// Binary service-python-fastapi is the FastAPI specialization of the
// generic Python agent. It composes the reusable pkg/* types from
// github.com/codefly-dev/service-python and adds FastAPI-specific Runtime
// and Builder behavior (uvicorn, OpenAPI generation, hot reload).
// Dependency management is uv (opinionated — no poetry).
package main

import (
	"context"
	"embed"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/builders"
	v0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
	"github.com/codefly-dev/core/toolbox/lang"

	pythoncode "github.com/codefly-dev/service-python/pkg/code"
	pythonruntime "github.com/codefly-dev/service-python/pkg/runtime"
	pythonservice "github.com/codefly-dev/service-python/pkg/service"
	pythontooling "github.com/codefly-dev/service-python/pkg/tooling"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Agent version.
var agent = shared.Must(resources.LoadFromFs[resources.Agent](shared.Embed(infoFS)))

var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("code/src").WithPathSelect(shared.NewSelect("*.py")))

const (
	HotReload      = "hot-reload"
	PublicEndpoint = "public-endpoint"
)

// Settings inherits the generic Python Settings (PythonVersion) and adds
// FastAPI-specific fields. `yaml:",inline"` means the YAML shape is flat:
//
//	python-version: "3.12"   # from generic
//	hot-reload: true         # from this layer
//	public-endpoint: true    # from this layer
type Settings struct {
	pythonservice.Settings `yaml:",inline"`

	HotReload      bool `yaml:"hot-reload"`
	PublicEndpoint bool `yaml:"public-endpoint"`

	// RuntimeImage overrides the default codefly-built runtime image.
	// Format: "name:tag". Plain "name" and ":latest" are rejected —
	// pinning is enforced. Leave empty to use codeflydev/python:<ver>
	// (recommended; companion is rebuilt + pinned on every codefly
	// release). Field named RuntimeImage (not DockerImage) to avoid
	// colliding with services.Base.DockerImage(req).
	RuntimeImage string `yaml:"docker-image"`
}

// runtimeImage is the codefly-built Python runtime companion —
// python:3.13.1-alpine3.21 + codefly CLI + uv 0.5.29. Built from
// core/companions/python/. Users can override via the Python settings
// (DockerImage field) but NOT recommended — the companion image is
// the mode-consistent default and gets the same tool set as every
// other codefly-built image.
var runtimeImage = &resources.DockerImage{Name: "codeflydev/python", Tag: "0.0.1"}

// Service is the FastAPI specialization. It embeds the generic Python
// Service so methods defined on *pythonservice.Service (and transitively
// *services.Base: Wool, Logger, Location, Identity, …) are promoted.
//
// FastAPI-specific: a richer Settings (with HotReload / PublicEndpoint)
// and the REST endpoint handle. FastAPI source lives under ./code — the
// Builder/Runtime set Service.SourceLocation to s.Local("code") during
// Load.
type Service struct {
	*pythonservice.Service

	// Shadows the generic Service.Settings with a richer type that still
	// inherits the generic fields (via inline embedding). `s.Settings` is
	// the FastAPI settings; `s.Service.Settings` is the generic view.
	Settings *Settings

	RestEndpoint *v0.Endpoint
}

// GetAgentInformation overrides the generic info to advertise HTTP protocol
// and hot-reload capability. Runtime requirement is the same NIX advertised
// by the generic; dep tooling is uv (no poetry).
func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {
	defer s.Wool.Catch()

	readme, err := templates.ApplyTemplateFrom(ctx, shared.Embed(readmeFS), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &agentv0.AgentInformation{
		RuntimeRequirements: []*agentv0.Runtime{
			{Type: agentv0.Runtime_PYTHON},
			{Type: agentv0.Runtime_NIX},
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

// NewService constructs a FastAPI Service. The embedded generic Service
// carries the identity and services.Base.
func NewService() *Service {
	generic := pythonservice.New(agent.Of(resources.ServiceAgent))
	settings := &Settings{}
	generic.Settings = &settings.Settings // keep the generic view pointing at the inlined half
	return &Service{
		Service:  generic,
		Settings: settings,
	}
}

func main() {
	svc := NewService()

	// Code + Tooling are inherited wholesale from the generic Python layer.
	// FastAPI has no Python analysis behavior to add beyond what generic
	// already does (uv/AST/ruff/pytest). If that changes, wrap them here.
	code := pythoncode.New(svc.Service)
	genericRuntime := pythonruntime.New(svc.Service)
	tooling := pythontooling.New(code, genericRuntime)

	agents.Serve(agents.PluginRegistration{
		Agent:   svc,
		Runtime: NewRuntime(svc),
		Builder: NewBuilder(svc),
		Code:    code,
		Tooling: tooling,
		// Phase B migration: expose the unified Toolbox surface
		// alongside Tooling. Mind can switch consumer-side via
		// lang.ToolingFromToolbox(toolboxClient); other Toolbox
		// consumers (MCP transcoder, future codefly tools) work
		// without further changes.
		Toolbox: lang.NewToolboxFromTooling(agent.Name, agent.Version, tooling),
	})
}

//go:embed agent.codefly.yaml
var infoFS embed.FS

//go:embed templates/agent
var readmeFS embed.FS
