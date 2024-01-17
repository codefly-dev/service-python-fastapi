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
	"github.com/codefly-dev/core/agents/network"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
)

type Runtime struct {
	*Service

	// internal
	EnvironmentVariables *configurations.EnvironmentVariableManager
	runner               *runners.Runner
	port                 string
	address              string
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

	var err error
	s.NetworkMappings, err = s.Network(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	// Self-mapping
	envs, err := network.ConvertToEnvironmentVariables(s.NetworkMappings)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.EnvironmentVariables.Add(envs...)

	// Setup the port
	address := s.NetworkMappings[0].Addresses[0]
	s.port = strings.Split(address, ":")[1]
	s.address = address

	for _, providerInfo := range req.ProviderInfos {
		envs := configurations.ProviderInformationAsEnvironmentVariables(providerInfo)
		s.EnvironmentVariables.Add(envs...)
	}

	runner := &runners.Runner{
		Dir:   s.Location,
		Bin:   "poetry",
		Args:  []string{"install"},
		Debug: s.Settings.Debug,
	}

	err = runner.Run(ctx)
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

	others, err := network.ConvertToEnvironmentVariables(req.NetworkMappings)
	if err != nil {
		return s.Runtime.StartError(err, wool.Field("in", "convert to environment variables"))
	}

	s.EnvironmentVariables.Add(others...)
	_, _ = s.Wool.Forward([]byte(fmt.Sprintf("running on %s", s.address)))

	s.runner = &runners.Runner{
		Dir:   s.Location,
		Bin:   "poetry",
		Args:  []string{"run", "uvicorn", "main:app", "--reload", "--host", "localhost", "--port", s.port},
		Debug: s.Settings.Debug,
	}

	s.runner.Envs = s.EnvironmentVariables.Get()

	if s.Settings.Watch {
		conf := services.NewWatchConfiguration(requirements)
		err := s.SetupWatcher(ctx, conf, s.EventHandler)
		if err != nil {
			s.Wool.Warn("error in watcher", wool.ErrField(err))
		}
	}

	// Create a new context as the runner will be running in the background
	runningContext := context.Background()
	runningContext = s.Wool.Inject(runningContext)

	out, err := s.runner.Start(runningContext)
	if err != nil {
		return s.Runtime.StartError(err, wool.Field("in", "runner"))
	}

	go func() {
		for event := range out.Events {
			if event.Err != nil && event.Message != "" {
				s.Wool.Error("event", wool.Field("event", event))
			}
		}
	}()

	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Base.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()

	s.Wool.Debug("stopping service")
	err := s.runner.Kill(ctx)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot kill go")
	}

	err = s.Base.Stop()
	if err != nil {
		return nil, err
	}
	return &runtimev0.StopResponse{}, nil
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
	s.WantRestart()
	return nil
}
