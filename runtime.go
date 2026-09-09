package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"sync"

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
	runner            runners.Proc

	// runnerMu serializes the runner lifecycle. Start and Stop are concurrent
	// gRPC handlers and both replace s.runner; without it two overlapping
	// Starts can each stop the same process and leave one of their
	// replacements running, unreachable, on the bound port.
	runnerMu sync.Mutex

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

func (s *Runtime) CreateRunnerEnvironment(ctx context.Context) error {
	s.Wool.Debug("creating runner environment in", wool.DirField(s.Identity.WorkspacePath))
	// Resolve the runtime image: settings override (if any) takes priority,
	// else fall back to the codefly-built default. Strict pinning — we
	// reject :latest and untagged refs so builds stay reproducible.
	image := runtimeImage
	if override := s.FastAPI.Settings.RuntimeImage; override != "" {
		parsed, perr := resources.ParsePinnedImage(override)
		if perr != nil {
			return s.Wool.Wrapf(perr, "invalid docker-image override in service.codefly.yaml")
		}
		s.Wool.Info("using docker-image override (not recommended)", wool.Field("image", parsed.FullName()))
		image = parsed
	}

	switch {
	case s.Base.Runtime.IsContainerRuntime():
		dockerEnv, err := dockerrun.NewDockerEnvironment(ctx, image, s.Identity.WorkspacePath, s.UniqueWithWorkspace())
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create docker runner")
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
			return s.Wool.Wrapf(err, "cannot find network instance")
		}
		dockerEnv.WithPort(ctx, uint16(instance.Port))

		if s.FastAPI.GRPCEndpoint != nil {
			grpcInstance, grpcErr := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.FastAPI.GRPCEndpoint, resources.NewNativeNetworkAccess())
			if grpcErr != nil {
				return s.Wool.Wrapf(grpcErr, "cannot find grpc network instance")
			}
			dockerEnv.WithPort(ctx, uint16(grpcInstance.Port))
		}

		envPath := s.DockerEnvPath()
		if _, err = shared.CheckDirectoryOrCreate(ctx, envPath); err != nil {
			return s.Wool.Wrapf(err, "cannot create docker venv environment")
		}

		s.Wool.Debug("docker environment", wool.DirField(envPath))
		// uv stores the venv under $UV_PROJECT_ENVIRONMENT or .venv by default.
		// Mount the persistent venv dir so it survives container restarts.
		dockerEnv.WithMount(s.DockerEnvPath(), "/venv")

		s.cacheLocation, err = s.LocalDirCreate(ctx, ".cache/container")
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create cache location")
		}
		// uv's download cache defaults to $HOME/.cache/uv; the host user has no
		// home inside the image, so give uv a writable, host-owned cache mount.
		uvCache, err := s.LocalDirCreate(ctx, ".cache/container/uv")
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create uv cache location")
		}
		dockerEnv.WithMount(uvCache, "/uv-cache")
		dockerEnv.WithEnvironmentVariables(ctx, resources.Env("UV_CACHE_DIR", "/uv-cache"))
		s.runnerEnvironment = dockerEnv

	case s.Base.Runtime.IsNixRuntime():
		// Provision the devShell (python3 + uv) when the project doesn't ship a
		// flake.nix, so NewNixEnvironment has something to materialize.
		if err := ensureNixFlake(s.Service.SourceLocation); err != nil {
			return s.Wool.Wrapf(err, "cannot provision nix flake")
		}
		nixEnv, err := runners.NewNixEnvironment(ctx, s.Service.SourceLocation)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create nix runner")
		}
		s.cacheLocation, err = s.LocalDirCreate(ctx, ".cache/nix")
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create cache location")
		}
		// Enable materialized-env caching — nix print-dev-env is run once
		// and the result is persisted under the plugin's cacheLocation.
		// Subsequent starts skip nix evaluation entirely (see nix_runner.go).
		nixEnv.WithCacheDir(s.cacheLocation)
		s.runnerEnvironment = nixEnv

	default:
		localEnv, err := runners.NewNativeEnvironment(ctx, s.Service.SourceLocation)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create local runner")
		}
		s.cacheLocation, err = s.LocalDirCreate(ctx, ".cache/local")
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create cache location")
		}
		s.runnerEnvironment = localEnv
	}

	allEnvs, err := s.EnvironmentVariables.All()
	if err != nil {
		return s.Wool.Wrapf(err, "cannot get environment variables")
	}
	s.runnerEnvironment.WithEnvironmentVariables(ctx, allEnvs...)
	s.runnerEnvironment.WithEnvironmentVariables(ctx, resources.Env("PYTHONUNBUFFERED", 1))
	// Share with Code / Tooling so AST analysis, grep, uv sync follow
	// whatever mode the plugin is in.
	s.FastAPI.Service.ActiveEnv = s.runnerEnvironment
	return nil
}

func (s *Runtime) SetRuntimeContext(_ context.Context, runtimeContext *basev0.RuntimeContext) error {
	s.Base.Runtime.RuntimeContext = pythonhelpers.SetPythonRuntimeContext(runtimeContext)
	return nil
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Base.Runtime.LogInitRequest(req)

	if err := s.SetRuntimeContext(ctx, req.RuntimeContext); err != nil {
		return s.Base.Runtime.InitError(err)
	}

	s.Wool.Forwardf("starting execution environment in %s mode", s.Base.Runtime.RuntimeContext.Kind)

	s.EnvironmentVariables.SetRuntimeContext(s.Base.Runtime.RuntimeContext)
	s.NetworkMappings = req.ProposedNetworkMappings

	if err := s.EnvironmentVariables.AddEndpoints(ctx, s.NetworkMappings, resources.NetworkAccessFromRuntimeContext(s.Base.Runtime.RuntimeContext)); err != nil {
		return s.Base.Runtime.InitError(err)
	}

	if s.runnerEnvironment == nil {
		if err := s.CreateRunnerEnvironment(ctx); err != nil {
			return s.Base.Runtime.InitErrorf(err, "cannot create runner environment")
		}
	}

	s.Wool.Debug("init for runner environment")
	if err := s.runnerEnvironment.Init(ctx); err != nil {
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
	).WithCache(s.cacheLocation)

	depUpdate, err := deps.Updated(ctx)
	if err != nil {
		return s.Base.Runtime.InitError(err)
	}

	if depUpdate {
		s.Infof("syncing uv environment")
		proc, err := s.runnerEnvironment.NewProcess("uv", "sync")
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
		builders.NewDependency(path.Join(s.Service.SourceLocation, "src/main.py"))).WithCache(s.cacheLocation)
	openApiUpdate, err := openAPI.Updated(ctx)
	if err != nil {
		return s.Base.Runtime.InitError(err)
	}

	if openApiUpdate {
		s.Infof("generating Open API document")
		if err := s.GenerateOpenAPI(ctx); err != nil {
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

	inputs := startInputs(req)

	if s.runner != nil && s.FastAPI.Settings.HotReload {
		if inputs == s.startInputs {
			return s.Base.Runtime.StartResponse()
		}
		// Hot reload only covers source: uvicorn --reload re-executes the app
		// inside the process it was launched in, so that process keeps the
		// environment it was given. A request carrying different inputs
		// therefore needs a new process, or it would be accepted and never
		// take effect.
		s.Infof("start inputs changed, restarting fastapi app")
		if err := s.runner.Stop(ctx); err != nil {
			return s.Base.Runtime.StartError(err)
		}
		s.runner = nil
		// The restart registers a new watcher below, which would otherwise
		// leave this one running with no way to reach it.
		s.Base.StopWatcher()
	}

	if s.runnerEnvironment == nil {
		// Init must run before Start; without it NewProcess below nil-derefs
		// and panics the agent. Fail loudly with a clear error instead.
		return s.Base.Runtime.StartError(s.Wool.NewError("runner environment not initialized (Init must run before Start)"))
	}

	// Local patch (lodestar spike): honour spec.hot-reload instead of always passing --reload.
	args := []string{"run", "uvicorn", "src.main:app", "--host", "0.0.0.0", "--port", fmt.Sprintf("%d", s.port)}
	if s.FastAPI.Settings.HotReload {
		args = append(args, "--reload")
	}
	proc, err := s.runnerEnvironment.NewProcess("uv", args...)
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

	proc.WithOutput(s.Logger)
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

	s.runner = proc
	s.startInputs = inputs

	if s.FastAPI.Settings.HotReload {
		conf := services.NewWatchConfiguration(requirements)
		if err := s.SetupWatcher(ctx, conf, s.EventHandler); err != nil {
			s.Wool.Warn("error in watcher", wool.ErrField(err))
		}
	}

	s.Infof("starting fastapi app via uv")
	runningContext := s.Wool.Inject(context.Background())
	if err := s.runner.Start(runningContext); err != nil {
		s.runner = nil // failed to start — don't leave a dead proc on the struct
		return s.Base.Runtime.StartError(err)
	}

	s.Wool.Debug("start done")
	return s.Base.Runtime.StartResponse()
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

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.runnerMu.Lock()
	defer s.runnerMu.Unlock()

	s.Wool.Debug("stopping service")
	if s.runner != nil {
		if err := s.runner.Stop(ctx); err != nil {
			return s.Base.Runtime.StopError(err)
		}
		s.runner = nil
	}
	if s.runnerEnvironment != nil {
		if err := s.runnerEnvironment.Shutdown(ctx); err != nil {
			s.Wool.Warn("error shutting down runner environment", wool.ErrField(err))
		}
	}
	// Cancel the watcher and let its Start goroutine's deferred close of Events
	// run exactly once — Stop must not close Events itself, or it races that
	// goroutine into a "close of closed channel" panic.
	s.Base.StopWatcher()
	return s.Base.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("Destroying service")

	s.Wool.Debug("removing cache")
	if err := shared.EmptyDir(ctx, s.cacheLocation); err != nil {
		// Best-effort: a failed cache wipe must NOT short-circuit Destroy and
		// skip the container teardown below — that would leak the running
		// container (the far more expensive resource).
		s.Wool.Warn("cannot remove cache", wool.ErrField(err))
	}

	if s.Base.Runtime.IsContainerRuntime() {
		s.Wool.Debug("running in container")
		dockerEnv, err := dockerrun.NewDockerEnvironment(ctx, runtimeImage, s.Service.SourceLocation, s.Base.Runtime.UniqueWithWorkspace())
		if err != nil {
			return s.Base.Runtime.DestroyError(err)
		}
		if err := dockerEnv.Shutdown(ctx); err != nil {
			return s.Base.Runtime.DestroyError(err)
		}
	}
	return s.Base.Runtime.DestroyResponse()
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
func (s *Runtime) GenerateOpenAPI(ctx context.Context) error {
	proc, err := s.runnerEnvironment.NewProcess("uv", "run", "python", "src/openapi.py")
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
