package main

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/codefly-dev/core/agents/helpers/code"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	pythonhelpers "github.com/codefly-dev/core/runners/python"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"

	pythonruntime "github.com/codefly-dev/service-python/pkg/runtime"
)

// Runtime is the FastAPI specialization of the generic Python Runtime.
//
// Embedding:
//   *pythonruntime.Runtime — inherits Test (uv run pytest), Lint (uv run ruff),
//                            Build (no-op), Information, and the services.Base
//                            chain via *pythonservice.Service promotion.
//   FastAPI               — fastapi-specific state (RestEndpoint, HotReload
//                            setting) accessed explicitly as s.FastAPI.X.
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

	port uint16

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
		dockerEnv, err := runners.NewDockerEnvironment(ctx, image, s.Identity.WorkspacePath, s.UniqueWithWorkspace())
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create docker runner")
		}
		dockerEnv.WithPause()

		instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.FastAPI.RestEndpoint, resources.NewNativeNetworkAccess())
		if err != nil {
			return s.Wool.Wrapf(err, "cannot find network instance")
		}
		dockerEnv.WithPort(ctx, uint16(instance.Port))

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
		s.runnerEnvironment = dockerEnv

	case s.Base.Runtime.IsNixRuntime():
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

	s.Base.Runtime.LogStartRequest(req)

	if s.runner != nil && s.FastAPI.Settings.HotReload {
		return s.Base.Runtime.StartResponse()
	}

	proc, err := s.runnerEnvironment.NewProcess(
		"uv", "run", "uvicorn", "src.main:app",
		"--reload", "--host", "0.0.0.0", "--port", fmt.Sprintf("%d", s.port))
	if err != nil {
		return s.Base.Runtime.StartError(err)
	}

	proc.WithOutput(s.Logger)
	proc.WithDir(s.Service.SourceLocation)

	s.EnvironmentVariables.SetRunning()

	startEnvs, err := s.EnvironmentVariables.All()
	if err != nil {
		return s.Base.Runtime.StartErrorf(err, "getting environment variables")
	}
	proc.WithEnvironmentVariables(ctx, startEnvs...)
	proc.WithEnvironmentVariables(ctx, s.EnvironmentVariables.Secrets()...)

	s.runner = proc

	if s.FastAPI.Settings.HotReload {
		conf := services.NewWatchConfiguration(requirements)
		if err := s.SetupWatcher(ctx, conf, s.EventHandler); err != nil {
			s.Wool.Warn("error in watcher", wool.ErrField(err))
		}
	}

	s.Infof("starting fastapi app via uv")
	runningContext := s.Wool.Inject(context.Background())
	if err := s.runner.Start(runningContext); err != nil {
		return s.Base.Runtime.StartError(err)
	}

	s.Wool.Debug("start done")
	return s.Base.Runtime.StartResponse()
}

// Test is INHERITED from *pythonruntime.Runtime (uv run pytest).
// Lint is INHERITED from *pythonruntime.Runtime (uv run ruff check).
// Build is INHERITED (no-op for Python).

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

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
	if s.Watcher != nil {
		s.Watcher.Pause()
	}
	if s.Events != nil {
		close(s.Events)
		s.Events = nil
	}
	return s.Base.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("Destroying service")

	s.Wool.Debug("removing cache")
	if err := shared.EmptyDir(ctx, s.cacheLocation); err != nil {
		return s.Base.Runtime.DestroyError(err)
	}

	if s.Base.Runtime.IsContainerRuntime() {
		s.Wool.Debug("running in container")
		dockerEnv, err := runners.NewDockerEnvironment(ctx, runtimeImage, s.Service.SourceLocation, s.Base.Runtime.UniqueWithWorkspace())
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
