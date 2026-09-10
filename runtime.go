package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/codefly-dev/core/agents/helpers/code"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	dockerrun "github.com/codefly-dev/core/runners/dockerrun"
	pythonhelpers "github.com/codefly-dev/core/runners/python"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"

	pythonruntime "github.com/codefly-dev/service-python/pkg/runtime"
)

// Runtime is the FastAPI specialization of the generic Python Runtime.
//
// Embedding:
//
//	*pythonruntime.Runtime — inherits Test (uv run pytest), Lint (uv run ruff),
//	                         Build (no-op), Information, and the services.Base
//	                         chain via *pythonservice.Service promotion.
//	FastAPI               — fastapi-specific state (RestEndpoint, HotReload
//	                         setting) accessed explicitly as s.FastAPI.X.
//
// Overridden methods: Load, Init, Start, Stop, Destroy — fastapi adds Docker
// runner env, port binding, uvicorn, OpenAPI regeneration, watchers.
// Inherited methods: Test, Lint, Build — the generic uv-based implementations
// are already what fastapi needs.
type Runtime struct {
	*pythonruntime.Runtime

	// FastAPI is the fastapi-layer service state. Access fastapi-specific
	// fields via s.FastAPI.*; generic fields flow through the embedded
	// Runtime (which itself embeds *pythonservice.Service).
	//
	// The persistent Python REPL machinery (exec / repl-reset commands,
	// state preservation across calls) is inherited from the generic
	// *pythonruntime.Runtime — no fastapi-specific REPL state here.
	// Specializations that need to override REPL behavior can define
	// their own cmdExec method to shadow the generic one.
	FastAPI *Service

	// internal
	runnerEnvironment runners.RunnerEnvironment
	runner            *runnerHandle

	// runnerMu serializes the runner lifecycle. Start, Stop and the supervisor
	// goroutines are concurrent and all replace s.runner; without it two
	// overlapping Starts can each stop the same process and leave one of their
	// replacements running, unreachable, on the bound port.
	//
	// It guards the rest of the runner state too — runnerEnvironment,
	// cacheLocation and the runtime context — which Init publishes while Stop
	// and Destroy are reading and releasing it.
	runnerMu sync.Mutex

	// runnerCreateMu single-flights the creation of the runner environment.
	// runnerMu cannot do that job: publishing is cheap but building is a
	// docker pull, and holding runnerMu across it would park every Stop.
	// Without it two concurrent Inits each build an environment, one gets
	// published and the other is left initialized — holding a docker client
	// and a log stream — with nothing referencing it to shut it down.
	runnerCreateMu sync.Mutex

	// startInputs renders the StartRequest fields the running process was
	// launched with, so a later Start can tell whether they still match.
	startInputs string

	// appliedOverrides counts, per key, how many override entries this agent
	// has handed to the environment manager. The manager appends overrides and
	// never removes them, so the counts are what lets a withdrawn key be
	// filtered back out of the process environment.
	appliedOverrides map[string]int

	port uint16

	// grpcPort is the mapped port for the service-owned gRPC listener, passed
	// to the Python process as CODEFLY_GRPC_PORT. Zero when gRPC is disabled.
	grpcPort uint16

	cacheLocation string

	// destroyed marks the runtime as torn down, guarded by runnerMu. Destroy
	// cannot stop a Start that is already on its way — the watcher's debounce
	// can still fire one after StopWatcher — so Start consults this rather
	// than launching a process against resources Destroy is removing.
	destroyed bool
}

func NewRuntime(svc *Service) *Runtime {
	return &Runtime{
		Runtime: pythonruntime.New(svc.Service),
		FastAPI: svc,
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if err := s.Base.Load(ctx, req.Identity, s.FastAPI.Settings); err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	s.Base.Runtime.SetEnvironment(req.Environment)

	// FastAPI layout: Python source lives under <service>/code. Push onto
	// the generic Service.SourceLocation so the inherited Test / Lint see it.
	s.Service.SourceLocation = s.Local("code")
	s.Wool.Debug("code location", wool.DirField(s.Service.SourceLocation))

	endpoints, err := s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}
	s.Endpoints = endpoints

	s.FastAPI.RestEndpoint, err = resources.FindRestEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	if s.FastAPI.Settings.GRPCServer.Enabled {
		s.FastAPI.GRPCEndpoint, err = resources.FindGRPCEndpoint(ctx, s.Endpoints)
		if err != nil {
			return s.Base.Runtime.LoadError(err)
		}
	}

	// Inherit the persistent Python REPL commands (exec, repl-reset)
	// from the generic python runtime. FastAPI adds no REPL-specific
	// behavior on top — same pattern go-grpc uses when inheriting from
	// generic go: call the generic setup, override only where needed.
	s.Runtime.RegisterReplCommands()

	return s.Base.Runtime.LoadResponse()
}

func (s *Runtime) DockerEnvPath() string {
	return path.Join(s.Location, ".cache/container/.venv")
}

// resolveRuntimeImage returns the image the service runs in: the settings
// override when one is set, else the codefly-built default. Strict pinning —
// :latest and untagged refs are rejected so builds stay reproducible.
func (s *Runtime) resolveRuntimeImage() (*resources.DockerImage, error) {
	override := s.FastAPI.Settings.RuntimeImage
	if override == "" {
		return runtimeImage, nil
	}
	parsed, err := resources.ParsePinnedImage(override)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "invalid docker-image override in service.codefly.yaml")
	}
	s.Wool.Info("using docker-image override (not recommended)", wool.Field("image", parsed.FullName()))
	return parsed, nil
}

// CreateRunnerEnvironment builds the execution environment and the cache
// location that belongs to it, publishes both under runnerMu and returns them
// so the caller can keep working against locals.
func (s *Runtime) CreateRunnerEnvironment(ctx context.Context) (runners.RunnerEnvironment, string, error) {
	s.Wool.Debug("creating runner environment in", wool.DirField(s.Identity.WorkspacePath))
	image, err := s.resolveRuntimeImage()
	if err != nil {
		return nil, "", err
	}

	var env runners.RunnerEnvironment
	var cacheLocation string

	switch {
	case s.Base.Runtime.IsContainerRuntime():
		dockerEnv, err := dockerrun.NewDockerEnvironment(ctx, image, s.Identity.WorkspacePath, s.UniqueWithWorkspace())
		if err != nil {
			return nil, "", s.Wool.Wrapf(err, "cannot create docker runner")
		}
		dockerEnv.WithPause()
		// Run as the invoking host user. uv sync writes uv.lock into the
		// bind-mounted source and populates the venv; as root those files
		// become root-owned on the host, and a later Init that hashes uv.lock
		// for its dependency cache then fails with "permission denied" on any
		// host where the user isn't root (e.g. Linux CI).
		dockerEnv.WithUser(fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))

		instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.FastAPI.RestEndpoint, resources.NewNativeNetworkAccess())
		if err != nil {
			return nil, "", s.Wool.Wrapf(err, "cannot find network instance")
		}
		dockerEnv.WithPort(ctx, uint16(instance.Port))

		if s.FastAPI.GRPCEndpoint != nil {
			grpcInstance, grpcErr := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.FastAPI.GRPCEndpoint, resources.NewNativeNetworkAccess())
			if grpcErr != nil {
				return nil, "", s.Wool.Wrapf(grpcErr, "cannot find grpc network instance")
			}
			dockerEnv.WithPort(ctx, uint16(grpcInstance.Port))
		}

		envPath := s.DockerEnvPath()
		if _, err = shared.CheckDirectoryOrCreate(ctx, envPath); err != nil {
			return nil, "", s.Wool.Wrapf(err, "cannot create docker venv environment")
		}

		s.Wool.Debug("docker environment", wool.DirField(envPath))
		// uv stores the venv under $UV_PROJECT_ENVIRONMENT or .venv by default.
		// Mount the persistent venv dir so it survives container restarts.
		dockerEnv.WithMount(s.DockerEnvPath(), "/venv")

		cacheLocation, err = s.LocalDirCreate(ctx, ".cache/container")
		if err != nil {
			return nil, "", s.Wool.Wrapf(err, "cannot create cache location")
		}
		// uv's download cache defaults to $HOME/.cache/uv; the host user has no
		// home inside the image, so give uv a writable, host-owned cache mount.
		uvCache, err := s.LocalDirCreate(ctx, ".cache/container/uv")
		if err != nil {
			return nil, "", s.Wool.Wrapf(err, "cannot create uv cache location")
		}
		dockerEnv.WithMount(uvCache, "/uv-cache")
		dockerEnv.WithEnvironmentVariables(ctx, resources.Env("UV_CACHE_DIR", "/uv-cache"))
		env = dockerEnv

	case s.Base.Runtime.IsNixRuntime():
		// Provision the devShell (python3 + uv) when the project doesn't ship a
		// flake.nix, so NewNixEnvironment has something to materialize.
		if err := ensureNixFlake(s.Service.SourceLocation); err != nil {
			return nil, "", s.Wool.Wrapf(err, "cannot provision nix flake")
		}
		nixEnv, err := runners.NewNixEnvironment(ctx, s.Service.SourceLocation)
		if err != nil {
			return nil, "", s.Wool.Wrapf(err, "cannot create nix runner")
		}
		cacheLocation, err = s.LocalDirCreate(ctx, ".cache/nix")
		if err != nil {
			return nil, "", s.Wool.Wrapf(err, "cannot create cache location")
		}
		// Enable materialized-env caching — nix print-dev-env is run once
		// and the result is persisted under the plugin's cacheLocation.
		// Subsequent starts skip nix evaluation entirely (see nix_runner.go).
		nixEnv.WithCacheDir(cacheLocation)
		env = nixEnv

	default:
		localEnv, err := runners.NewNativeEnvironment(ctx, s.Service.SourceLocation)
		if err != nil {
			return nil, "", s.Wool.Wrapf(err, "cannot create local runner")
		}
		cacheLocation, err = s.LocalDirCreate(ctx, ".cache/local")
		if err != nil {
			return nil, "", s.Wool.Wrapf(err, "cannot create cache location")
		}
		env = localEnv
	}

	allEnvs, err := s.EnvironmentVariables.All()
	if err != nil {
		return nil, "", s.Wool.Wrapf(err, "cannot get environment variables")
	}
	env.WithEnvironmentVariables(ctx, allEnvs...)
	env.WithEnvironmentVariables(ctx, resources.Env("PYTHONUNBUFFERED", 1))

	s.runnerMu.Lock()
	defer s.runnerMu.Unlock()
	s.runnerEnvironment = env
	s.cacheLocation = cacheLocation
	// Share with Code / Tooling so AST analysis, grep, uv sync follow whatever
	// mode the plugin is in. The lock orders this against Stop, which clears
	// the same field — without it a publish can land after a teardown and
	// leave Code holding a shut-down environment. The setter is what orders it
	// against the readers: Code, Tooling and the REPL (service-python pkg/code,
	// pkg/runtime/commands) run on their own goroutines and cannot take runnerMu.
	s.FastAPI.Service.SetActiveEnvironment(env)
	return env, cacheLocation, nil
}

// runnerEnv returns the execution environment and cache location Init works
// against, creating them when the runtime holds none.
//
// The pair is read once under runnerMu and then carried as locals: Stop and
// Destroy own the same fields, and Init's runner section — a docker pull, a
// nix evaluation, `uv sync` — is far too long to hold the lock across, so a
// teardown can land anywhere inside it.
//
// runnerCreateMu spans the check and the creation so the two are atomic with
// respect to each other; runnerMu is taken only for the read and the publish.
func (s *Runtime) runnerEnv(ctx context.Context) (runners.RunnerEnvironment, string, error) {
	s.runnerCreateMu.Lock()
	defer s.runnerCreateMu.Unlock()

	s.runnerMu.Lock()
	env, cacheLocation := s.runnerEnvironment, s.cacheLocation
	s.runnerMu.Unlock()

	if env != nil {
		return env, cacheLocation, nil
	}
	return s.CreateRunnerEnvironment(ctx)
}

// SetRuntimeContext publishes the runtime context under runnerMu. Init calls
// it while Destroy reads the same field through IsContainerRuntime and Start
// reads it through applyStartInputs, both holding the lock.
func (s *Runtime) SetRuntimeContext(_ context.Context, runtimeContext *basev0.RuntimeContext) error {
	s.runnerMu.Lock()
	defer s.runnerMu.Unlock()
	s.Base.Runtime.RuntimeContext = pythonhelpers.SetPythonRuntimeContext(runtimeContext)
	return nil
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Base.Runtime.LogInitRequest(req)

	// Init opens a new lifecycle, so a runtime torn down earlier accepts work
	// again — otherwise Init would succeed and every later Start refuse.
	s.runnerMu.Lock()
	s.destroyed = false
	s.runnerMu.Unlock()

	if err := s.SetRuntimeContext(ctx, req.RuntimeContext); err != nil {
		return s.Base.Runtime.InitError(err)
	}

	s.Wool.Forwardf("starting execution environment in %s mode", s.Base.Runtime.RuntimeContext.Kind)

	s.EnvironmentVariables.SetRuntimeContext(s.Base.Runtime.RuntimeContext)
	s.NetworkMappings = req.ProposedNetworkMappings

	if err := s.EnvironmentVariables.AddEndpoints(ctx, s.NetworkMappings, resources.NetworkAccessFromRuntimeContext(s.Base.Runtime.RuntimeContext)); err != nil {
		return s.Base.Runtime.InitError(err)
	}

	env, cacheLocation, err := s.runnerEnv(ctx)
	if err != nil {
		return s.Base.Runtime.InitErrorf(err, "cannot create runner environment")
	}

	s.Wool.Debug("init for runner environment")
	if err := env.Init(ctx); err != nil {
		return s.Base.Runtime.InitError(err)
	}

	if err := s.EnvironmentVariables.AddConfigurations(ctx, req.Configuration); err != nil {
		return s.Base.Runtime.InitError(err)
	}

	confs := resources.FilterConfigurations(req.DependenciesConfigurations, s.Base.Runtime.RuntimeContext)
	if err := s.EnvironmentVariables.AddConfigurations(ctx, confs...); err != nil {
		return s.Base.Runtime.InitError(err)
	}

	net, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.FastAPI.RestEndpoint, resources.NewNativeNetworkAccess())
	if err != nil {
		return s.Base.Runtime.InitError(err)
	}

	s.Infof("will run on %s", net.Address)
	s.port = uint16(net.Port)

	if s.FastAPI.GRPCEndpoint != nil {
		grpcNet, grpcErr := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.FastAPI.GRPCEndpoint, resources.NewNativeNetworkAccess())
		if grpcErr != nil {
			return s.Base.Runtime.InitError(grpcErr)
		}
		s.grpcPort = uint16(grpcNet.Port)
		s.Infof("grpc will run on %s", grpcNet.Address)
	}

	hasPyProject, err := shared.FileExists(ctx, path.Join(s.Service.SourceLocation, "pyproject.toml"))
	if err != nil {
		return s.Base.Runtime.InitError(err)
	}
	if !hasPyProject {
		return s.Base.Runtime.InitErrorf(nil, "no pyproject.toml found")
	}

	// uv sync: one command, reads pyproject.toml + uv.lock (creates lock if
	// missing). Cached on pyproject.toml + uv.lock so we only re-run when
	// dependencies actually change.
	s.Wool.Debug("computing uv dependency cache")
	deps := builders.NewDependencies("uv",
		builders.NewDependency(path.Join(s.Service.SourceLocation, "pyproject.toml")),
		builders.NewDependency(path.Join(s.Service.SourceLocation, "uv.lock")),
	).WithCache(cacheLocation)

	depUpdate, err := deps.Updated(ctx)
	if err != nil {
		return s.Base.Runtime.InitError(err)
	}

	if depUpdate {
		s.Infof("syncing uv environment")
		proc, err := env.NewProcess("uv", "sync")
		if err != nil {
			return s.Base.Runtime.InitErrorf(err, "cannot create uv sync process")
		}
		proc.WithDir(s.Service.SourceLocation)
		if err := proc.Run(ctx); err != nil {
			return s.Base.Runtime.InitErrorf(err, "cannot run uv sync")
		}
		if err := deps.UpdateCache(ctx); err != nil {
			return s.Base.Runtime.InitErrorf(err, "cannot update cache")
		}
	}
	s.Wool.Debug("successful init of runner")

	openAPI := builders.NewDependencies("api",
		builders.NewDependency(path.Join(s.Service.SourceLocation, "src/main.py"))).WithCache(cacheLocation)
	openApiUpdate, err := openAPI.Updated(ctx)
	if err != nil {
		return s.Base.Runtime.InitError(err)
	}

	if openApiUpdate {
		s.Infof("generating Open API document")
		if err := s.GenerateOpenAPI(ctx, env); err != nil {
			return s.Base.Runtime.InitErrorf(err, "cannot generate Open API document")
		}
		if err := openAPI.UpdateCache(ctx); err != nil {
			s.Wool.Warn("cannot update openapi cache", wool.ErrField(err))
		}
		s.Wool.Debug("generate Open API done")
	}

	return s.Base.Runtime.InitResponse()
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.runnerMu.Lock()
	defer s.runnerMu.Unlock()

	s.Base.Runtime.LogStartRequest(req)

	if s.destroyed {
		return s.Base.Runtime.StartError(s.Wool.NewError("service has been destroyed (Init must run before it can start again)"))
	}

	inputs := startInputs(req)

	if s.runner != nil {
		switch {
		case !s.runnerAlive(ctx, s.runner):
			// The process died between two Starts — the orchestrator asking
			// again is the chance to bring the service back, not something to
			// answer from a stale handle.
			s.Infof("fastapi app is no longer running, starting it again")
		case inputs == s.startInputs:
			// A running process keeps the environment it was launched with and
			// uvicorn --reload already covers source edits, so an unchanged
			// request describes exactly what is running: nothing to do.
			return s.Base.Runtime.StartResponse()
		default:
			// Different inputs only reach the service through a new process:
			// --reload re-executes the app inside the process it was launched
			// in, which keeps that process's environment.
			s.Infof("start inputs changed, restarting fastapi app")
		}
		if err := s.stopRunner(ctx); err != nil {
			return s.Base.Runtime.StartError(err)
		}
		// The start below registers a new watcher, which would otherwise leave
		// this one running with no way to reach it.
		s.Base.StopWatcher()
	}

	if s.runnerEnvironment == nil {
		// Init must run before Start; without it NewProcess below nil-derefs
		// and panics the agent. Fail loudly with a clear error instead.
		return s.Base.Runtime.StartError(s.Wool.NewError("runner environment not initialized (Init must run before Start)"))
	}

	proc, err := s.runnerEnvironment.NewProcess("uv", uvicornArgs(s.port, s.FastAPI.Settings.HotReload)...)
	if err != nil {
		return s.Base.Runtime.StartError(err)
	}

	if inputs != s.startInputs {
		if err := s.applyStartInputs(ctx, req); err != nil {
			return s.Base.Runtime.StartErrorf(err, "applying start request")
		}
	}

	startEnvs, err := s.processEnvironment(req)
	if err != nil {
		return s.Base.Runtime.StartErrorf(err, "getting environment variables")
	}

	tail := &tailWriter{max: runnerTailLines}
	proc.WithOutput(io.MultiWriter(s.Logger, tail))
	proc.WithDir(s.Service.SourceLocation)

	proc.WithEnvironmentVariables(ctx, startEnvs...)
	if s.grpcPort != 0 {
		// The FastAPI lifespan boots the grpc.aio listener on this port
		// (src/rpc/server.py); in container/k8s the app falls back to the
		// standard gRPC port when the variable is unset. The agent owns the
		// mapped port, so it is applied after the overrides and wins over a
		// `--set` naming the same key.
		proc.WithEnvironmentVariables(ctx, resources.Env("CODEFLY_GRPC_PORT", s.grpcPort))
	}

	// The process must outlive this RPC, so it runs under a background context
	// — cancellable, so Stop, Destroy and a replacement can tell the supervisor
	// below that the exit it is about to see was asked for.
	runningContext, cancel := context.WithCancel(s.Wool.Inject(context.Background()))
	handle := &runnerHandle{proc: proc, cancel: cancel, tail: tail}

	s.runner = handle
	s.startInputs = inputs

	if s.FastAPI.Settings.HotReload {
		conf := services.NewWatchConfiguration(requirements)
		if err := s.SetupWatcher(ctx, conf, s.EventHandler); err != nil {
			s.Wool.Warn("error in watcher", wool.ErrField(err))
		}
	}

	s.Infof("starting fastapi app via uv")
	if err := proc.Start(runningContext); err != nil {
		cancel()
		s.runner = nil // failed to start — don't leave a dead proc on the struct
		return s.Base.Runtime.StartError(err)
	}

	// STARTED is recorded before the supervisor is armed: revoking a status
	// that was never committed would leave the orchestrator reading STARTED for
	// a process that is already gone.
	resp, err := s.Base.Runtime.StartResponse()
	go s.superviseRunner(runningContext, handle)

	s.Wool.Debug("start done")
	return resp, err
}

// uvicornArgs renders the `uv run` invocation. --reload is conditional: it
// makes uvicorn fork a supervisor that re-executes the app on source edits,
// which is not what a service configured without hot reload asked for.
func uvicornArgs(port uint16, hotReload bool) []string {
	args := []string{"run", "uvicorn", "src.main:app", "--host", "0.0.0.0", "--port", fmt.Sprintf("%d", port)}
	if hotReload {
		args = append(args, "--reload")
	}
	return args
}

// runnerHandle is one launched process: the proc itself, the cancel function of
// the context it runs under, and the tail of its output. Handles are never
// reused — every Start that replaces a process installs a new one, and the
// supervisor compares identity to tell whether the exit it observed still
// describes the service.
type runnerHandle struct {
	proc   runners.Proc
	cancel context.CancelFunc
	tail   *tailWriter

	// exited is set by the supervisor as soon as the process is gone, so a
	// Start landing before the supervisor reaches the mutex still sees a dead
	// generation rather than a live-looking handle.
	exited atomic.Bool
}

// runnerAlive reports whether the process behind the handle is still up. The
// runner is asked directly because Start can land in the window between the
// process dying and its supervisor being scheduled. A probe that cannot answer
// counts as dead: the agent must not answer STARTED for a process whose
// liveness it could not confirm.
func (s *Runtime) runnerAlive(ctx context.Context, handle *runnerHandle) bool {
	if handle.exited.Load() {
		return false
	}
	running, err := handle.proc.IsRunning(ctx)
	if err != nil {
		s.Wool.Warn("cannot tell whether the fastapi app is running", wool.ErrField(err))
		return false
	}
	return running
}

// stopRunner ends the current generation. It must be called with runnerMu held.
// Cancelling before the kill is what tells the supervisor the exit was asked
// for; dropping the handle first means even a failed Stop cannot leave a
// generation that still owns the service's status.
func (s *Runtime) stopRunner(ctx context.Context) error {
	handle := s.runner
	if handle == nil {
		return nil
	}
	s.runner = nil
	handle.cancel()
	return handle.proc.Stop(ctx)
}

// superviseRunner turns the death of a uvicorn process into a revoked STARTED.
// Start commits the status and returns; without this the agent would keep
// reporting a service that exited on an import error, and `codefly run` has no
// other way to learn about it (it polls Information).
//
// This is process supervision, not application readiness: with --reload the
// uvicorn supervisor survives an application that fails to import, so a live
// process is a weaker claim than a serving one. Readiness stays with the
// orchestrator's probes against the endpoints the service declares.
func (s *Runtime) superviseRunner(ctx context.Context, handle *runnerHandle) {
	err := handle.proc.Wait(ctx)
	handle.exited.Store(true)
	if ctx.Err() != nil {
		// Stop, Destroy or a replacement cancelled this generation: the exit
		// was asked for.
		return
	}
	s.reportRunnerExit(handle, err)
}

// reportRunnerExit revokes STARTED for an unexpected exit — a non-zero one, and
// a clean one just the same, since nothing asked this process to stop.
func (s *Runtime) reportRunnerExit(handle *runnerHandle, err error) {
	s.runnerMu.Lock()
	defer s.runnerMu.Unlock()
	if s.runner != handle {
		// A replacement already owns the service; this generation's death says
		// nothing about the process that is running now.
		return
	}
	s.runner = nil

	message := "fastapi app exited unexpectedly"
	if err != nil {
		message = fmt.Sprintf("%s: %v", message, err)
	}
	if tail := handle.tail.Tail(); tail != "" {
		message = fmt.Sprintf("%s\n%s", message, tail)
	}
	s.Wool.Error(message)
	s.Base.Runtime.MarkRunnerExited(s.Wool.NewError("%s", message))
}

// runnerTailLines is how much process output the agent keeps to explain an
// unexpected exit. A Python traceback is the diagnostic that matters here and
// fits well inside it.
const runnerTailLines = 20

// tailWriter mirrors process output into a bounded ring of its most recent
// lines. An exit status alone says nothing about why uvicorn died; the
// traceback it printed on the way out does.
type tailWriter struct {
	mu      sync.Mutex
	lines   []string
	partial string
	max     int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	segments := strings.Split(w.partial+string(p), "\n")
	w.partial = segments[len(segments)-1]
	for _, line := range segments[:len(segments)-1] {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		w.lines = append(w.lines, line)
	}
	if len(w.lines) > w.max {
		w.lines = slices.Clone(w.lines[len(w.lines)-w.max:])
	}
	return len(p), nil
}

func (w *tailWriter) Tail() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	lines := w.lines
	if strings.TrimSpace(w.partial) != "" {
		lines = append(slices.Clone(lines), w.partial)
	}
	return strings.Join(lines, "\n")
}

// startInputs renders the StartRequest fields this agent turns into process
// environment: the per-service overrides of `codefly run --set`, the fixture
// selector, and the addresses of the endpoints this service depends on.
// req.Specs carries agent-specific runtime knobs and fastapi defines none, so
// it is deliberately absent — a request differing only there produces an
// identical process and must not cost a restart.
func startInputs(req *runtimev0.StartRequest) string {
	lines := []string{fmt.Sprintf("fixture %s", req.GetFixture())}
	for _, key := range slices.Sorted(maps.Keys(req.GetOverrides())) {
		lines = append(lines, fmt.Sprintf("override %s=%s", key, req.GetOverrides()[key]))
	}
	var dependencies []string
	for _, mapping := range req.GetDependenciesNetworkMappings() {
		endpoint := mapping.GetEndpoint()
		for _, instance := range mapping.GetInstances() {
			dependencies = append(dependencies, fmt.Sprintf("dependency %s/%s/%s/%s %s %s",
				endpoint.GetModule(), endpoint.GetService(), endpoint.GetName(), endpoint.GetApi(),
				instance.GetAccess().GetKind(), instance.GetAddress()))
		}
	}
	slices.Sort(dependencies)
	return strings.Join(append(lines, dependencies...), "\n")
}

// applyStartInputs folds the StartRequest into the environment manager. Callers
// must only invoke it when the inputs actually changed: the manager appends
// overrides and endpoints without ever removing any, so re-folding an unchanged
// request would grow those slices on every restart.
func (s *Runtime) applyStartInputs(ctx context.Context, req *runtimev0.StartRequest) error {
	overrides := req.GetOverrides()
	s.EnvironmentVariables.AddOverrides(overrides)
	if s.appliedOverrides == nil {
		s.appliedOverrides = make(map[string]int, len(overrides))
	}
	for key := range overrides {
		s.appliedOverrides[key]++
	}

	// Unconditional: the manager falls back to the environment-level fixture
	// when this one is empty, so passing "" is how a withdrawn fixture reverts.
	s.EnvironmentVariables.SetFixture(req.GetFixture())

	networkAccess := resources.NetworkAccessFromRuntimeContext(s.Base.Runtime.RuntimeContext)
	if unresolved := unresolvedDependencies(req.GetDependenciesNetworkMappings(), networkAccess); len(unresolved) > 0 {
		// AddEndpoints drops these silently, which surfaces much later as a
		// missing variable inside the service rather than as a start problem.
		s.Wool.Warn("dependency endpoints carry no address reachable from this runtime context and are not exported",
			wool.Field("access", networkAccess.GetKind()),
			wool.Field("endpoints", strings.Join(unresolved, ", ")))
	}
	if err := s.EnvironmentVariables.AddEndpoints(ctx, req.GetDependenciesNetworkMappings(), networkAccess); err != nil {
		return s.Wool.Wrapf(err, "cannot add dependency endpoints")
	}

	s.EnvironmentVariables.SetRunning()
	return nil
}

// unresolvedDependencies returns the dependency mappings that carry no instance
// reachable from networkAccess, named module/service/endpoint. It mirrors the
// filter AddEndpoints applies, which reports nothing when a mapping contributes
// no address.
func unresolvedDependencies(mappings []*basev0.NetworkMapping, networkAccess *basev0.NetworkAccess) []string {
	var unresolved []string
	for _, mapping := range mappings {
		if mapping == nil {
			continue
		}
		matched := false
		for _, instance := range mapping.GetInstances() {
			if instance.GetAccess().GetKind() == networkAccess.GetKind() {
				matched = true
				break
			}
		}
		if !matched {
			endpoint := mapping.GetEndpoint()
			unresolved = append(unresolved, fmt.Sprintf("%s/%s/%s",
				endpoint.GetModule(), endpoint.GetService(), endpoint.GetName()))
		}
	}
	return unresolved
}

// processEnvironment returns what the service process runs with. All() already
// carries the secret configuration values alongside the plain ones, so this is
// the complete set and the override resolution below covers secrets too.
func (s *Runtime) processEnvironment(req *runtimev0.StartRequest) ([]*resources.EnvironmentVariable, error) {
	envs, err := s.EnvironmentVariables.All()
	if err != nil {
		return nil, err
	}
	withdrawn := make(map[string]int, len(s.appliedOverrides))
	for key, count := range s.appliedOverrides {
		if _, current := req.GetOverrides()[key]; !current {
			withdrawn[key] = count
		}
	}
	return withOverrides(envs, req.GetOverrides(), withdrawn), nil
}

// withOverrides resolves the keys this agent's overrides participate in, and
// leaves every other entry exactly as the environment manager emitted it.
//
// The manager emits overrides ahead of the configuration-derived variables
// (secrets included), so a configuration would otherwise shadow a `--set`
// naming the same key: every earlier entry a current override claims is
// dropped, and the override value is appended at the end.
//
// The manager also never forgets an override. withdrawn carries, per key, how
// many stale entries an earlier start left behind; those are the first entries
// the manager emits for that key, so dropping exactly that many reveals the
// configuration value underneath — the value the service would have seen had
// the override never been set.
func withOverrides(envs []*resources.EnvironmentVariable, overrides map[string]string, withdrawn map[string]int) []*resources.EnvironmentVariable {
	if len(overrides) == 0 && len(withdrawn) == 0 {
		return envs
	}
	remaining := maps.Clone(withdrawn)
	resolved := make([]*resources.EnvironmentVariable, 0, len(envs)+len(overrides))
	for _, env := range envs {
		if _, claimed := overrides[env.Key]; claimed {
			continue
		}
		if remaining[env.Key] > 0 {
			remaining[env.Key]--
			continue
		}
		resolved = append(resolved, env)
	}
	for _, key := range slices.Sorted(maps.Keys(overrides)) {
		resolved = append(resolved, resources.Env(key, overrides[key]))
	}
	return resolved
}

// Test is INHERITED from *pythonruntime.Runtime (uv run pytest).
// Lint is INHERITED from *pythonruntime.Runtime (uv run ruff check).
// Build is INHERITED (no-op for Python).

// endExecution releases the execution resources the runtime holds: the file
// watcher, the current process generation, and the runner environment. It must
// be called with runnerMu held.
//
// The watcher goes first because it is the only thing that can ask for a
// replacement process, and a teardown racing it can be handed back the very
// thing it is tearing down. The remaining steps then all run even when an
// earlier one fails, so one stubborn resource cannot strand the rest, and the
// environment is forgotten only once it is actually down — a caller retrying
// after a failure must still be able to reach it.
//
// It reports whether the owned environment was actually shut down, which in
// container mode is what tells Destroy the container is already gone.
func (s *Runtime) endExecution(ctx context.Context) (bool, error) {
	// Cancel the watcher and let its Start goroutine's deferred close of Events
	// run exactly once — never close Events here, or it races that goroutine
	// into a "close of closed channel" panic.
	s.Base.StopWatcher()

	var errs []error
	if err := s.stopRunner(ctx); err != nil {
		errs = append(errs, s.Wool.Wrapf(err, "cannot stop the fastapi app"))
	}
	released := false
	if s.runnerEnvironment != nil {
		if err := s.runnerEnvironment.Shutdown(ctx); err != nil {
			errs = append(errs, s.Wool.Wrapf(err, "cannot shut down the runner environment"))
		} else {
			// A shut-down environment cannot serve another process — a docker
			// one has closed its client — so drop it and let a later Init build
			// a fresh one rather than hand Code and the REPL a dead handle.
			released = true
			s.runnerEnvironment = nil
			s.FastAPI.Service.SetActiveEnvironment(nil)
		}
	}
	return released, errors.Join(errs...)
}

// Stop releases the execution resources and keeps everything on disk, so a
// later Init and Start bring the same service back.
func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.runnerMu.Lock()
	defer s.runnerMu.Unlock()

	s.Wool.Debug("stopping service")
	if _, err := s.endExecution(ctx); err != nil {
		return s.Base.Runtime.StopError(err)
	}
	return s.Base.Runtime.StopResponse()
}

// Destroy ends execution and then removes what this service owns on the host.
// It assumes nothing about what ran before it: `codefly` shutdown calls Destroy
// directly, with no Stop in between, so ending execution is Destroy's own job
// rather than a precondition on its callers. Safe before Init, after a failed
// Init, after Stop, and repeated.
func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("Destroying service")

	var errs []error

	s.runnerMu.Lock()
	s.destroyed = true
	released, err := s.endExecution(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	cacheLocation := s.cacheLocation
	isContainer := s.Base.Runtime.IsContainerRuntime()
	s.runnerMu.Unlock()

	// A container outlives the agent that started it, so a Destroy whose
	// runtime holds no environment — a fresh agent, or an Init that never ran —
	// still has to reach it by name. Shutting the owned environment down has
	// already removed it, and reaching for docker again there would only invent
	// a way for a finished teardown to fail.
	if isContainer && !released {
		if err := s.destroyContainerByIdentity(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	// Last, and only once nothing is executing any more. In container mode the
	// venv and the uv cache are bind-mounted out of this directory, so wiping
	// it while the container is still up pulls the interpreter out from under
	// the running service. An empty location means Init never claimed one —
	// nothing owned to clear, and EmptyDir rejects the empty path as an error.
	if cacheLocation != "" {
		s.Wool.Debug("removing cache")
		if err := shared.EmptyDir(ctx, cacheLocation); err != nil {
			errs = append(errs, s.Wool.Wrapf(err, "cannot remove cache"))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return s.Base.Runtime.DestroyError(err)
	}
	return s.Base.Runtime.DestroyResponse()
}

// destroyContainerByIdentity removes the service's container by the name it is
// registered under, for a Destroy holding no environment of its own.
func (s *Runtime) destroyContainerByIdentity(ctx context.Context) error {
	s.Wool.Debug("running in container")
	image, err := s.resolveRuntimeImage()
	if err != nil {
		// The container is found by name, never by image, so an override this
		// agent cannot parse must not be what leaves it running.
		s.Wool.Warn("cannot resolve the runtime image, falling back to the default", wool.ErrField(err))
		image = runtimeImage
	}
	dockerEnv, err := dockerrun.NewDockerEnvironment(ctx, image, s.Service.SourceLocation, s.Base.Runtime.UniqueWithWorkspace())
	if err != nil {
		return s.Wool.Wrapf(err, "cannot reach the container environment")
	}
	if err := dockerEnv.Shutdown(ctx); err != nil {
		return s.Wool.Wrapf(err, "cannot shut down the container environment")
	}
	return nil
}

func (s *Runtime) EventHandler(event code.Change) error {
	if strings.Contains(event.Path, "api.json") {
		return nil
	}
	if strings.HasSuffix(event.Path, ".py") {
		// uvicorn --reload picks these up; no action needed here.
		return nil
	}
	s.Base.Runtime.DesiredStart()
	return nil
}

// GenerateOpenAPI runs the project's src/openapi.py under uv to regenerate
// the OpenAPI spec. Convention: the project ships a small openapi.py that
// imports src.main and dumps the schema. See templates/factory.
func (s *Runtime) GenerateOpenAPI(ctx context.Context, env runners.RunnerEnvironment) error {
	proc, err := env.NewProcess("uv", "run", "python", "src/openapi.py")
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create openapi runner")
	}
	proc.WithDir(s.Service.SourceLocation)
	proc.WithEnvironmentVariables(ctx, resources.Env("PYTHONPATH", s.Service.SourceLocation))

	if err := proc.Run(ctx); err != nil {
		return s.Wool.Wrapf(err, "cannot run openapi")
	}
	return nil
}
