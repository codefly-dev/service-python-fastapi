package main

import (
	"context"
	"fmt"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"strconv"
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
	EnvironmentVariables *configurations.EnvironmentVariableManager
	runner               *runners.Runner

	address         string
	Port            int
	NetworkMappings []*basev0.NetworkMapping
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

	err = s.GenerateOpenAPI(ctx)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	err = s.LoadEndpoints(ctx)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	s.EnvironmentVariables = configurations.NewEnvironmentVariableManager()

	return s.Base.Runtime.LoadResponse()
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.NetworkMappings = req.NetworkMappings

	envs, err := configurations.ExtractEndpointEnvironmentVariables(ctx, s.NetworkMappings)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	s.EnvironmentVariables.Add(envs...)

	net, err := configurations.GetMappingInstance(s.NetworkMappings)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	s.address = net.Address
	s.Port = net.Port
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
	runner.WithDir(s.Location).WithDebug(s.Settings.Debug)

	err = runner.Run()
	if err != nil {
		s.Wool.Error("cannot init the go runner", wool.ErrField(err))
		return s.Runtime.InitError(err)
	}
	s.Ready()
	s.Wool.Info("successful init of runner")

	return s.Runtime.InitResponse()
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

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

	runningContext := s.Wool.Inject(context.Background())
	runner, err := runners.NewRunner(runningContext, "poetry", "run", "uvicorn", "main:app", "--reload", "--host", "localhost", "--Port", strconv.Itoa(s.Port))
	if err != nil {
		return s.Runtime.StartError(err, wool.Field("in", "runner"))
	}
	s.runner = runner
	s.runner.WithDir(s.Location).WithDebug(s.Settings.Debug).WithEnvs(s.EnvironmentVariables.Get())

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

	err = runners.WaitForPortUnbound(ctx, s.Port)
	if err != nil {
		s.Wool.Warn("cannot wait for port to be free", wool.ErrField(err))
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
