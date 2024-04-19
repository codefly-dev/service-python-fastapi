package main

import (
	"context"
	"fmt"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/shared"
	"github.com/hashicorp/go-multierror"
	"path"
	"strings"

	runners "github.com/codefly-dev/core/runners/base"

	"github.com/codefly-dev/core/configurations"

	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/agents/services"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"

	"github.com/codefly-dev/core/agents/helpers/code"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
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
	proc, err := s.runnerEnvironment.NewProcess("poetry", "run", "python", "openapi.py")
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create openapi runner")
	}
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
		return s.Base.Runtime.LoadError(err)
	}

	s.sourceLocation = s.Local("src")

	s.Runtime.Scope = req.Scope

	s.LogForward("running in %s mode", s.Runtime.Scope)

	s.Environment = req.Environment

	s.EnvironmentVariables.SetEnvironment(s.Environment)
	s.EnvironmentVariables.SetNetworkScope(s.Runtime.Scope)

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	s.restEndpoint, err = configurations.FindRestEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	return s.Base.Runtime.LoadResponse()
}

func (s *Runtime) DockerEnvPath() string {
	return path.Join(s.Location, "container/.venv")
}

func (s *Runtime) CreateRunnerEnvironment(ctx context.Context) error {
	s.Wool.Debug("creating runner environment in", wool.DirField(s.sourceLocation))
	if s.Runtime.Container() {
		s.Wool.Debug("running in container")

		dockerEnv, err := runners.NewDockerEnvironment(ctx, runtimeImage, s.sourceLocation, s.UniqueWithProject())
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create docker runner")
		}
		dockerEnv.WithPause()
		err = dockerEnv.Clear(ctx)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot clear the docker environment")
		}
		// Need to bind the ports
		instance, err := configurations.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.restEndpoint, s.Runtime.Scope)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot find network instance")
		}
		dockerEnv.WithPort(ctx, uint16(instance.Port))
		dockerEnv.WithMount(s.Local("openapi"), "/openapi")
		dockerEnv.WithMount(s.Local("service.codefly.yaml"), "/service.codefly.yaml")
		envPath := s.DockerEnvPath()
		_, err = shared.CheckDirectoryOrCreate(ctx, envPath)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create docker venv environment")
		}
		dockerEnv.WithMount(s.DockerEnvPath(), "/venv")

		s.cacheLocation, err = s.LocalDirCreate(ctx, ".cache/container")
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create cache location")
		}
		s.runnerEnvironment = dockerEnv
	} else {
		s.Wool.Debug("running locally")
		localEnv, err := runners.NewLocalEnvironment(ctx, s.sourceLocation)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create local runner")
		}
		s.cacheLocation, err = s.LocalDirCreate(ctx, ".cache/local")
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create cache location")
		}
		s.runnerEnvironment = localEnv
	}

	s.runnerEnvironment.WithEnvironmentVariables(s.EnvironmentVariables.All()...)
	s.runnerEnvironment.WithEnvironmentVariables(configurations.Env("PYTHONUNBUFFERED", 1))
	return nil
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)

	s.NetworkMappings = req.ProposedNetworkMappings

	// Filter configurations for the scope
	confs := configurations.FilterConfigurations(req.DependenciesConfigurations, s.Runtime.Scope)
	err := s.EnvironmentVariables.AddConfigurations(confs...)

	// Networking
	net, err := s.Runtime.NetworkInstance(ctx, s.NetworkMappings, s.restEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.LogForward("will run on http://localhost:%d", net.Port)
	s.port = uint16(net.Port)

	err = s.CreateRunnerEnvironment(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	err = s.runnerEnvironment.Init(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	// poetry install
	deps := builders.NewDependencies("poetry", builders.NewDependency(s.Local(s.sourceLocation, "pyproject.toml"))).WithCache(s.cacheLocation)
	depUpdate, err := deps.Updated(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	if depUpdate {
		s.LogForward("installing poetry dependencies")
		proc, err := s.runnerEnvironment.NewProcess("poetry", "install", "--no-root")
		if err != nil {
			return s.Runtime.InitError(err)
		}
		err = proc.Run(ctx)
		if err != nil {
			s.Wool.Error("cannot run poetry install", wool.ErrField(err))
			return s.Runtime.InitError(err)
		}
		err = deps.UpdateCache(ctx)
		if err != nil {
			return s.Runtime.InitError(err)
		}
	}
	s.Ready()

	s.Wool.Debug("successful init of runner")

	openAPI := builders.NewDependencies("api", builders.NewDependency(s.Local(s.sourceLocation, "main.py"))).WithCache(s.cacheLocation)
	openApiUpdate, err := openAPI.Updated(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	if openApiUpdate {
		s.LogForward("generating Open API document")
		err = s.GenerateOpenAPI(ctx)
		if err != nil {
			return s.Base.Runtime.InitError(err)
		}
		err = openAPI.UpdateCache(ctx)
	}
	s.Wool.Debug("generate Open API done")

	return s.Runtime.InitResponse()
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogStartRequest(req)

	if s.runner != nil && s.Settings.Watch {
		// Use the hot-reloading
		return s.Runtime.StartResponse()
	}

	proc, err := s.runnerEnvironment.NewProcess("poetry", "run", "uvicorn", "main:app", "--reload", "--host", "0.0.0.0", "--port", fmt.Sprintf("%d", s.port))
	if err != nil {
		return s.Runtime.StartError(err)
	}
	proc.WithOutput(s.Logger)
	proc.WithEnvironmentVariables(configurations.Env(configurations.RunningPrefix, "true"))
	s.runner = proc

	if s.Settings.Watch {
		conf := services.NewWatchConfiguration(requirements)
		err = s.SetupWatcher(ctx, conf, s.EventHandler)
		if err != nil {
			s.Wool.Warn("error in watcher", wool.ErrField(err))
		}
	}

	s.LogForward("starting poetry app")
	runningContext := s.Wool.Inject(context.Background())
	err = s.runner.Start(runningContext)
	if err != nil {
		return s.Runtime.StartError(err, wool.Field("in", "runner"))
	}

	s.Wool.Debug("start done")
	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Base.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()
	var agg error
	s.Wool.Debug("stopping service")
	if s.runner != nil {
		err := s.runner.Stop(ctx)
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
		return s.Base.Runtime.StopError(agg)
	}
	return s.Runtime.StopResponse()
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	//TODO implement me
	panic("implement me")
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
