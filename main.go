package main

import (
	"context"
	"embed"
	"github.com/codefly-dev/core/builders"
	v0 "github.com/codefly-dev/core/generated/go/base/v0"

	"google.golang.org/grpc/codes"

	"google.golang.org/grpc/status"

	"github.com/codefly-dev/core/templates"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
)

// Agent version
var agent = shared.Must(resources.LoadFromFs[resources.Agent](shared.Embed(infoFS)))

var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("code/src").WithPathSelect(shared.NewSelect("*.py")))

const HotReload = "hot-reload"
const PublicEndpoint = "public-endpoint"

type Settings struct {
	HotReload      bool `yaml:"hot-reload"`
	PublicEndpoint bool `yaml:"public-endpoint"`
}

var runtimeImage = &resources.DockerImage{Name: "codeflydev/python-poetry", Tag: "0.0.9"}

type Service struct {
	*services.Base

	sourceLocation string

	// Settings
	*Settings
	RestEndpoint *v0.Endpoint
}

func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {
	defer s.Wool.Catch()

	readme, err := templates.ApplyTemplateFrom(ctx, shared.Embed(readmeFS), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &agentv0.AgentInformation{
		RuntimeRequirements: []*agentv0.Runtime{
			{Type: agentv0.Runtime_PYTHON},
			{Type: agentv0.Runtime_PYTHON_POETRY},
		},
		Capabilities: []*agentv0.Capability{
			{Type: agentv0.Capability_BUILDER},
			{Type: agentv0.Capability_RUNTIME},
			{Type: agentv0.Capability_HOT_RELOAD},
		},
		Languages: []*agentv0.Language{
			{Type: agentv0.Language_PYTHON},
		},
		Protocols: []*agentv0.Protocol{
			{Type: agentv0.Protocol_HTTP},
		},
		ReadMe: readme,
	}, nil
}

func NewService() *Service {
	return &Service{
		Base:     services.NewServiceBase(context.Background(), agent),
		Settings: &Settings{},
	}
}

func main() {
	agents.Register(
		services.NewServiceAgent(agent.Of(resources.ServiceAgent), NewService()),
		services.NewBuilderAgent(agent.Of(resources.RuntimeServiceAgent), NewBuilder()),
		services.NewRuntimeAgent(agent.Of(resources.BuilderServiceAgent), NewRuntime()))
}

//go:embed agent.codefly.yaml
var infoFS embed.FS

//go:embed templates/agent
var readmeFS embed.FS
