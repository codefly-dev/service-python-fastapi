package main

import (
	"context"
	"fmt"
	v0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/shared"
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

	// internal
	runner runners.Runner

	address string
	port    uint16
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
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

	switch s.Runtime.Scope {
	case v0.RuntimeScope_Container:
		s.Wool.Info("running in container mode")
	case v0.RuntimeScope_Native:
		s.Wool.Info("running in native mode")
	}

	s.Environment = req.Environment

	s.EnvironmentVariables.SetEnvironment(s.Environment)
	s.EnvironmentVariables.SetRuntimeScope(s.Runtime.Scope)

	s.Wool.Focus("generate Open API")
	err = s.GenerateOpenAPI(ctx)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	s.Wool.Focus("generate Open API done")

	err = s.LoadEndpoints(ctx, configurations.IsLocal(req.Environment))
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

	w := s.Wool.In("init")

	w.Focus("input",
		wool.Field("configurations", configurations.MakeConfigurationSummary(req.Configuration)),
		wool.Field("other configurations", configurations.MakeManyConfigurationSummary(req.DependenciesConfigurations)),
		wool.Field("network", configurations.MakeManyNetworkMappingSummary(req.ProposedNetworkMappings)))

	s.NetworkMappings = req.ProposedNetworkMappings

	// Add to environment variables
	err := s.EnvironmentVariables.AddConfigurations(req.DependenciesConfigurations...)

	envs := s.EnvironmentVariables.Get()
	w.Focus("env", wool.Field("envs", envs))
	// Networking
	net, err := s.Runtime.NetworkInstance(s.NetworkMappings, s.restEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	w.Focus("network instance", wool.Field("instance", net))

	s.LogForward("will run on http://localhost:%d", net.Port)
	s.port = uint16(net.Port)

	var runner runners.Runner
	switch s.Runtime.Scope {
	case v0.RuntimeScope_Container:
		runner, err = s.dockerInitRunner(ctx)
	case v0.RuntimeScope_Native:
		runner, err = s.nativeInitRunner(ctx)
	}

	if err != nil {
		return s.Runtime.InitError(err)
	}
	err = runner.Run(ctx)
	if err != nil {
		s.Wool.Error("cannot run poetry install", wool.ErrField(err))
		return s.Runtime.InitError(err)
	}
	s.Ready()
	s.Wool.Focus("successful init of runner")

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

	runner.WithEnvironmentVariables(s.EnvironmentVariables.Get()...)
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
	runner.WithEnvironmentVariables(s.EnvironmentVariables.Get()...)
	runner.WithEnvironmentVariables("POETRY_VIRTUALENVS_IN_PROJECT=1")
	runner.WithEnvironmentVariables("PYTHONUNBUFFERED=0")
	runner.WithOut(s.Wool)
	return runner, nil
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	//
	//others, err := configurations.ExtractEndpointEnvironmentVariables(ctx, req.OtherNetworkMappings)
	//if err != nil {
	//	return s.Runtime.StartError(err, wool.Field("in", "convert to environment variables"))
	//}
	//
	//s.EnvironmentVariables.Add(others...)
	//_, _ = s.Wool.Forward([]byte(fmt.Sprintf("running on %s", s.address)))

	if s.runner != nil && s.Settings.Watch {
		// Use the hot-reloading
		return s.Runtime.StartResponse()
	}

	s.Wool.Focus("env", wool.Field("envs", s.EnvironmentVariables.Get()))

	runningContext := s.Wool.Inject(context.Background())

	var runner runners.Runner
	var err error
	switch s.Runtime.Scope {
	case v0.RuntimeScope_Container:
		runner, err = s.dockerStartRunner(ctx)
	case v0.RuntimeScope_Native:
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

	err = s.runner.Start(runningContext)
	if err != nil {
		return s.Runtime.StartError(err, wool.Field("in", "runner"))
	}

	s.Wool.Focus("start done")
	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Base.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()
	s.Wool.Debug("stopping service")
	if s.runner == nil {
		return s.Runtime.StopResponse()
	}
	err := s.runner.Stop()
	if err != nil {
		return s.Runtime.StopError(err)
	}

	err = s.Base.Stop()
	if err != nil {
		return s.Runtime.StopError(err)
	}
	return s.Runtime.StopResponse()
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
