package main

import (
	"context"
	"fmt"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/shared"
	"github.com/hashicorp/go-multierror"
	"path"
	"strings"

	runners "github.com/codefly-dev/core/runners/base"

	"github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/agents/services"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"

	"github.com/codefly-dev/core/agents/helpers/code"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
)

type Runtime struct {
	*Service
	Environment *basev0.Environment

	// internal
	runnerEnvironment runners.RunnerEnvironment
	runner            runners.Proc

	address string
	port    uint16

	cacheLocation string // Local
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

func (s *Runtime) GenerateOpenAPI(ctx context.Context) error {
	proc, err := s.runnerEnvironment.NewProcess("poetry", "run", "python", "src/openapi.py")
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create openapi runner")
	}
	proc.WithDir(s.sourceLocation)

	err = proc.Run(ctx)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot run openapi")
	}
	return nil
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Runtime.LoadError(err)
	}

	s.Runtime.SetEnvironment(req.Environment)

	s.sourceLocation = s.Local("code")
	s.Wool.Debug("code location", wool.DirField(s.sourceLocation))

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Runtime.LoadError(err)
	}

	s.RestEndpoint, err = resources.FindRestEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Runtime.LoadError(err)
	}

	return s.Runtime.LoadResponse()
}

func (s *Runtime) DockerEnvPath() string {
	return path.Join(s.Location, ".cache/container/.venv")
}

func (s *Runtime) CreateRunnerEnvironment(ctx context.Context) error {
	s.Wool.Debug("creating runner environment in", wool.DirField(s.Identity.WorkspacePath))
	if s.Runtime.IsContainerRuntime() {
		dockerEnv, err := runners.NewDockerEnvironment(ctx, runtimeImage, s.Identity.WorkspacePath, s.UniqueWithWorkspace())
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create docker runner")
		}
		dockerEnv.WithPause()

		// Need to bind the ports
		instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.RestEndpoint, resources.NewNativeNetworkAccess())
		if err != nil {
			return s.Wool.Wrapf(err, "cannot find network instance")
		}
		dockerEnv.WithPort(ctx, uint16(instance.Port))

		envPath := s.DockerEnvPath()

		_, err = shared.CheckDirectoryOrCreate(ctx, envPath)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create docker venv environment")
		}

		s.Wool.Debug("docker environment", wool.DirField(envPath))
		dockerEnv.WithMount(s.DockerEnvPath(), "/venv")

		s.cacheLocation, err = s.LocalDirCreate(ctx, ".cache/container")
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create cache location")
		}
		s.runnerEnvironment = dockerEnv
	} else {
		localEnv, err := runners.NewNativeEnvironment(ctx, s.sourceLocation)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create local runner")
		}
		s.cacheLocation, err = s.LocalDirCreate(ctx, ".cache/local")
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create cache location")
		}
		s.runnerEnvironment = localEnv
	}

	s.runnerEnvironment.WithEnvironmentVariables(ctx, s.EnvironmentVariables.All()...)
	s.runnerEnvironment.WithEnvironmentVariables(ctx, resources.Env("PYTHONUNBUFFERED", 1))
	return nil
}

func (s *Runtime) SetRuntimeContext(ctx context.Context, runtimeContext *basev0.RuntimeContext) error {
	if runtimeContext.Kind == resources.RuntimeContextFree || runtimeContext.Kind == resources.RuntimeContextNative {
		if languages.HasPythonPoetryRuntime(nil) {
			s.Runtime.RuntimeContext = resources.NewRuntimeContextNative()
			return nil
		}
	}
	s.Runtime.RuntimeContext = resources.NewRuntimeContextContainer()
	return nil
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)

	err := s.SetRuntimeContext(ctx, req.RuntimeContext)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.Wool.Forwardf("starting execution environment in %s mode", s.Runtime.RuntimeContext.Kind)

	s.EnvironmentVariables.SetRuntimeContext(s.Runtime.RuntimeContext)

	s.NetworkMappings = req.ProposedNetworkMappings

	err = s.EnvironmentVariables.AddEndpoints(ctx, s.NetworkMappings, resources.NetworkAccessFromRuntimeContext(s.Runtime.RuntimeContext))
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if s.runnerEnvironment == nil {
		err = s.CreateRunnerEnvironment(ctx)
		if err != nil {
			return s.Runtime.InitErrorf(err, "cannot create runner environment")
		}
	}

	s.Wool.Debug("init for runner environment")
	err = s.runnerEnvironment.Init(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	err = s.EnvironmentVariables.AddConfigurations(req.Configuration)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	// Filter resources for the scope
	confs := resources.FilterConfigurations(req.DependenciesConfigurations, s.Runtime.RuntimeContext)
	err = s.EnvironmentVariables.AddConfigurations(confs...)

	// Networking
	net, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.RestEndpoint, resources.NewNativeNetworkAccess())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.Infof("will run on %s", net.Address)
	s.port = uint16(net.Port)

	hasPyProject, err := shared.FileExists(ctx, path.Join(s.sourceLocation, "pyproject.toml"))
	if err != nil {
		return s.Runtime.InitError(err)
	}
	if !hasPyProject {
		return s.Runtime.InitErrorf(nil, "no pyproject.toml found")
	}
	// poetry install
	s.Wool.Debug("computing dependency")
	deps := builders.NewDependencies("poetry", builders.NewDependency(path.Join(s.sourceLocation, "pyproject.toml"))).WithCache(s.cacheLocation)

	depUpdate, err := deps.Updated(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.Wool.Debug("computing dependency: done")

	if depUpdate {
		// if there is a .lock. update
		hasLock, err := shared.FileExists(ctx, path.Join(s.sourceLocation, "poetry.lock"))
		if err != nil {
			return s.Runtime.InitError(err)
		}
		var proc runners.Proc
		if !hasLock {
			s.Infof("installing poetry dependencies")
			proc, err = s.runnerEnvironment.NewProcess("poetry", "install", "--no-root")
			if err != nil {
				return s.Runtime.InitErrorf(err, "cannot create poetry install process")
			}
		} else {
			s.Infof("updating poetry dependencies")
			proc, err = s.runnerEnvironment.NewProcess("poetry", "update")
			if err != nil {
				return s.Runtime.InitErrorf(err, "cannot create poetry update process")
			}
		}
		proc.WithDir(s.sourceLocation)
		err = proc.Run(ctx)
		if err != nil {
			return s.Runtime.InitErrorf(err, "cannot run poetry install")
		}

		err = deps.UpdateCache(ctx)
		if err != nil {
			return s.Runtime.InitErrorf(err, "cannot update cache")
		}

	}
	s.Ready()

	s.Wool.Debug("successful init of runner")

	openAPI := builders.NewDependencies("api", builders.NewDependency(path.Join(s.sourceLocation, "src/main.py"))).WithCache(s.cacheLocation)
	openApiUpdate, err := openAPI.Updated(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if openApiUpdate {
		s.Infof("generating Open API document")
		err = s.GenerateOpenAPI(ctx)
		if err != nil {
			return s.Runtime.InitErrorf(err, "cannot generate Open API document")
		}
		err = openAPI.UpdateCache(ctx)
		s.Wool.Debug("generate Open API done")
	}

	return s.Runtime.InitResponse()
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogStartRequest(req)

	if s.runner != nil && s.Settings.HotReload {
		// Use the hot-reloading
		return s.Runtime.StartResponse()
	}

	proc, err := s.runnerEnvironment.NewProcess(
		"poetry", "run", "uvicorn", "src.main:app", "--reload", "--host", "0.0.0.0", "--port", fmt.Sprintf("%d", s.port))
	if err != nil {
		return s.Runtime.StartError(err)
	}

	proc.WithOutput(s.Logger)
	proc.WithDir(s.sourceLocation)

	s.EnvironmentVariables.SetRunning()

	proc.WithEnvironmentVariables(ctx, s.EnvironmentVariables.All()...)
	proc.WithEnvironmentVariables(ctx, s.EnvironmentVariables.Secrets()...)

	s.runner = proc

	if s.Settings.HotReload {
		conf := services.NewWatchConfiguration(requirements)
		err = s.SetupWatcher(ctx, conf, s.EventHandler)
		if err != nil {
			s.Wool.Warn("error in watcher", wool.ErrField(err))
		}
	}

	s.Infof("starting poetry app")
	runningContext := s.Wool.Inject(context.Background())
	err = s.runner.Start(runningContext)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	s.Wool.Debug("start done")
	return s.Runtime.StartResponse()
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.runnerEnvironment.WithBinary("codefly")
	if err != nil {
		return s.Runtime.TestError(err)
	}

	proc, err := s.runnerEnvironment.NewProcess("poetry", "run", "pytest", "tests", "-v", "-s")
	if err != nil {
		return s.Runtime.TestError(err)
	}

	proc.WithOutput(s.Logger)
	proc.WithDir(s.sourceLocation)

	s.Infof("testing poetry app")
	testingContext := s.Wool.Inject(context.Background())

	// poetry runs pytest
	proc.WaitOn("pytest")

	err = proc.Run(testingContext)
	if err != nil {
		return s.Runtime.TestError(err)
	}
	return s.Runtime.TestResponse()

}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()

	ctx = s.Wool.Inject(ctx)

	var agg error
	s.Wool.Debug("stopping service")
	if s.runner != nil {
		err := s.runner.Stop(ctx)
		if err != nil {
			agg = multierror.Append(agg, err)
		}
		s.runner = nil
	}
	if s.runnerEnvironment != nil {
		err := s.runnerEnvironment.Shutdown(ctx)
		if err != nil {
			agg = multierror.Append(agg, err)
		}
	}
	s.Wool.Debug("runner stopped")
	err := s.Base.Stop()
	if err != nil {
		agg = multierror.Append(agg, err)
		s.Wool.Warn("error stopping runner", wool.ErrField(err))
	}
	if agg != nil {
		return s.Runtime.StopError(agg)
	}
	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()

	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("Destroying service")

	// Remove cache
	s.Wool.Debug("removing cache")
	err := shared.EmptyDir(ctx, s.cacheLocation)
	if err != nil {
		return s.Runtime.DestroyError(err)
	}

	// Get the runner environment
	if s.Runtime.IsContainerRuntime() {
		s.Wool.Debug("running in container")

		dockerEnv, err := runners.NewDockerEnvironment(ctx, runtimeImage, s.sourceLocation, s.Runtime.UniqueWithWorkspace())
		if err != nil {
			return s.Runtime.DestroyError(err)
		}
		err = dockerEnv.Shutdown(ctx)
		if err != nil {
			return s.Runtime.DestroyError(err)
		}
	}
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Communicate(ctx context.Context, req *agentv0.Engage) (*agentv0.InformationRequest, error) {
	return s.Base.Communicate(ctx, req)
}

/* Details

 */

func (s *Runtime) EventHandler(event code.Change) error {
	if strings.Contains(event.Path, "api.json") {
		return nil
	}
	// TODO: OpenAPI changes...
	if strings.HasSuffix(event.Path, ".py") {
		// Dealt with the uvicorn
		return nil
	}
	// Now, only start
	// TODO: handle change of swagger
	s.Runtime.DesiredStart()
	return nil
}
