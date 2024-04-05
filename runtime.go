package main

import (
	"context"
	"fmt"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/shared"
	"github.com/hashicorp/go-multierror"
	"path"
	"strings"

	"github.com/codefly-dev/core/runners"

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
	runner       runners.Runner
	otherRunners []runners.Runner

	address string
	port    uint16
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

func (s *Runtime) GenerateOpenAPI(ctx context.Context) error {
	var generator runners.Runner
	if s.Runtime.Container() {
		runner, err := runners.NewDocker(ctx, runtimeImage)
		if err != nil {
			return err
		}
		err = runner.Init(ctx)
		if err != nil {
			return err
		}
		runner.WithMount(s.sourceLocation, "/app")
		runner.WithMount(s.Local("openapi"), "/openapi")
		runner.WithMount(s.Local("service.codefly.yaml"), "/service.codefly.yaml")
		runner.WithMount(s.DockerEnvPath(), "/venv")
		runner.WithWorkDir("/app")
		runner.WithOut(s.Wool)
		runner.WithCommand("poetry", "run", "python", "openapi.py")
		generator = runner
	}
	if s.Runtime.Native() {
		runner, err := runners.NewProcess(ctx, "poetry", "run", "python", "openapi.py")
		if err != nil {
			return err
		}
		runner.WithDir(s.sourceLocation)
		runner.WithOut(s.Wool)
		generator = runner
	}
	s.otherRunners = append(s.otherRunners, generator)
	err := generator.Run(ctx)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot generate open api")
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

	s.Runtime.Scope = req.Scope

	s.sourceLocation = s.Local("src")

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
	return path.Join(s.sourceLocation, ".venv.container")
}

func (s *Runtime) dockerInitRunner(ctx context.Context) (runners.Runner, error) {
	runner, err := runners.NewDocker(ctx, runtimeImage)
	if err != nil {
		return nil, err
	}

	_, err = shared.CheckDirectoryOrCreate(ctx, s.DockerEnvPath())
	if err != nil {
		return nil, err
	}

	err = runner.Init(ctx)
	if err != nil {
		return nil, err
	}

	runner.WithMount(s.sourceLocation, "/app")
	runner.WithMount(s.DockerEnvPath(), "/venv")
	runner.WithWorkDir("/app")
	runner.WithCommand("poetry", "install", "--no-root")
	runner.WithOut(s.Wool)
	return runner, nil
}

func (s *Runtime) nativeInitRunner(ctx context.Context) (runners.Runner, error) {
	runner, err := runners.NewProcess(ctx, "poetry", "install", "--no-root")
	if err != nil {
		return nil, err
	}
	runner.WithDir(s.sourceLocation)
	runner.WithEnvironmentVariables("POETRY_VIRTUALENVS_IN_PROJECT=1")
	runner.WithOut(s.Wool)
	return runner, nil

}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)

	s.NetworkMappings = req.ProposedNetworkMappings

	// Add to environment variables
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

	s.LogForward("installing poetry dependencies")
	var runner runners.Runner
	if s.Runtime.Container() {
		runner, err = s.dockerInitRunner(ctx)
	}
	if s.Runtime.Native() {
		runner, err = s.nativeInitRunner(ctx)
	}
	if err != nil {
		return s.Runtime.InitError(err)
	}
	s.otherRunners = append(s.otherRunners, runner)
	err = runner.Run(ctx)
	if err != nil {
		s.Wool.Error("cannot run poetry install", wool.ErrField(err))
		return s.Runtime.InitError(err)
	}
	s.Ready()
	s.Wool.Debug("successful init of runner")

	s.LogForward("generating Open API document")
	err = s.GenerateOpenAPI(ctx)
	if err != nil {
		return s.Base.Runtime.InitError(err)
	}

	s.Wool.Debug("generate Open API done")

	return s.Runtime.InitResponse()
}

func (s *Runtime) dockerStartRunner(ctx context.Context) (runners.Runner, error) {
	// runner for the write endpoint
	runner, err := runners.NewDocker(ctx, runtimeImage)
	if err != nil {
		return nil, err
	}

	err = runner.Init(ctx)
	if err != nil {
		return nil, err
	}
	runner.WithPort(runners.DockerPortMapping{Container: s.port, Host: s.port})
	runner.WithName(s.Global())

	runner.WithMount(s.sourceLocation, "/app")
	runner.WithMount(s.DockerEnvPath(), "/venv")
	runner.WithMount(s.Local("service.codefly.yaml"), "/service.codefly.yaml")
	runner.WithMount(s.Local("openapi"), "/openapi")

	runner.WithEnvironmentVariables(s.EnvironmentVariables.All()...)
	runner.WithEnvironmentVariables("PYTHONUNBUFFERED=0")
	runner.WithOut(s.Wool)
	runner.WithCommand("poetry", "run", "uvicorn", "main:app", "--reload", "--host", "0.0.0.0", "--port", fmt.Sprintf("%d", s.port))
	return runner, nil
}

func (s *Runtime) nativeStartRunner(ctx context.Context) (runners.Runner, error) {
	runner, err := runners.NewProcess(ctx, "poetry", "run", "uvicorn", "main:app", "--reload", "--host", "localhost", "--port", fmt.Sprintf("%d", s.port))
	if err != nil {
		return nil, err
	}
	runner.WithDir(s.sourceLocation)
	runner.WithDebug(s.Settings.Debug)
	runner.WithEnvironmentVariables(s.EnvironmentVariables.All()...)
	runner.WithEnvironmentVariables("POETRY_VIRTUALENVS_IN_PROJECT=1")
	runner.WithEnvironmentVariables("PYTHONUNBUFFERED=0")
	runner.WithOut(s.Wool)
	return runner, nil
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogStartRequest(req)

	s.EnvironmentVariables.SetRunning(true)

	if s.runner != nil && s.Settings.Watch {
		// Use the hot-reloading
		return s.Runtime.StartResponse()
	}

	s.Wool.Debug("env", wool.Field("envs", s.EnvironmentVariables.All()))

	runningContext := s.Wool.Inject(context.Background())

	var runner runners.Runner
	var err error
	if s.Runtime.Container() {
		runner, err = s.dockerStartRunner(ctx)
	}
	if s.Runtime.Native() {
		runner, err = s.nativeStartRunner(ctx)
	}
	if err != nil {
		return s.Runtime.StartError(err)
	}
	s.runner = runner

	if s.Settings.Watch {
		conf := services.NewWatchConfiguration(requirements)
		err = s.SetupWatcher(ctx, conf, s.EventHandler)
		if err != nil {
			s.Wool.Warn("error in watcher", wool.ErrField(err))
		}
	}

	s.LogForward("starting poetry app")
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
		err := s.runner.Stop()
		if err != nil {
			agg = multierror.Append(agg, err)
		}
	}
	for _, run := range s.otherRunners {
		err := run.Stop()
		if err != nil {
			agg = multierror.Append(agg, err)
			s.Wool.Warn("error stopping runner", wool.ErrField(err))
		}
	}
	s.Wool.Debug("runner stopped")
	err := s.Base.Stop()
	if err != nil {
		if err != nil {
			agg = multierror.Append(agg, err)
			s.Wool.Warn("error stopping runner", wool.ErrField(err))
		}
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
	if strings.Contains(event.Path, "swagger.json") {
		return nil
	}
	if strings.HasSuffix(event.Path, ".py") {
		// Dealt with the uvicorn
		return nil
	}
	// Now, only start
	// TODO: handle change of swagger
	s.Runtime.DesiredStart()
	return nil
}
