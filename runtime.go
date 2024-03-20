package main

import (
	"context"
	"fmt"
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

	runner *runners.Runner

	address string
	port    int32
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

	s.sourceLocation = s.Local("src")

	s.Environment = req.Environment

	s.EnvironmentVariables = s.LoadEnvironmentVariables(req.Environment)

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

	return s.Base.Runtime.LoadResponse()
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Focus("init")

	s.NetworkMappings = req.ProposedNetworkMappings

	envs, err := configurations.ExtractEndpointEnvironmentVariables(ctx, s.NetworkMappings)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.EnvironmentVariables.Add(envs...)

	net, err := configurations.FindNetworkMapping(s.restEndpoint, s.NetworkMappings)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	s.address = net.Address
	s.port = net.Port
	s.LogForward("will run on: %s", net.Address)

	for _, providerInfo := range req.ProviderInfos {
		envs_ := configurations.ProviderInformationAsEnvironmentVariables(providerInfo)
		s.EnvironmentVariables.Add(envs_...)
	}

	runner, err := runners.NewRunner(ctx, "poetry", "install")
	if err != nil {
		s.Wool.Error("cannot init the go runner", wool.ErrField(err))
		return s.Runtime.InitError(err)
	}
	runner.WithDir(s.sourceLocation)
	runner.WithDebug(s.Settings.Debug)

	err = runner.Run()
	if err != nil {
		s.Wool.Error("cannot init the go runner", wool.ErrField(err))
		return s.Runtime.InitError(err)
	}
	s.Ready()
	s.Wool.Focus("successful init of runner")

	return s.Runtime.InitResponse()
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Focus("start: go")

	others, err := configurations.ExtractEndpointEnvironmentVariables(ctx, req.OtherNetworkMappings)
	if err != nil {
		return s.Runtime.StartError(err, wool.Field("in", "convert to environment variables"))
	}

	s.EnvironmentVariables.Add(others...)
	_, _ = s.Wool.Forward([]byte(fmt.Sprintf("running on %s", s.address)))

	if s.runner != nil && s.Settings.Watch {
		// Use the hot-reloading
		return s.Runtime.StartResponse()
	}

	s.Wool.Focus("env", wool.Field("envs", s.EnvironmentVariables.Get()))

	runningContext := s.Wool.Inject(context.Background())
	runner, err := runners.NewRunner(runningContext, "poetry", "run", "uvicorn", "main:app", "--reload", "--host", "localhost", "--port", fmt.Sprintf("%d", s.port))
	if err != nil {
		return s.Runtime.StartError(err, wool.Field("in", "runner"))
	}

	s.runner = runner
	s.runner.WithDir(s.sourceLocation)
	s.runner.WithDebug(s.Settings.Debug)
	s.runner.WithEnvs(s.EnvironmentVariables.Get()...)
	s.runner.WithEnvs("PYTHONUNBUFFERED=1")

	if s.Settings.Watch {
		conf := services.NewWatchConfiguration(requirements)
		err = s.SetupWatcher(ctx, conf, s.EventHandler)
		if err != nil {
			s.Wool.Warn("error in watcher", wool.ErrField(err))
		}
	}

	err = s.runner.Start()
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
