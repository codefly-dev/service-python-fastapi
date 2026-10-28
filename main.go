// Binary service-python-fastapi is the FastAPI specialization of the
// generic Python agent. It composes the reusable pkg/* types from
// github.com/codefly-dev/service-python and adds FastAPI-specific Runtime
// and Builder behavior (uvicorn, OpenAPI generation, hot reload).
// Dependency management is uv (opinionated — no poetry).
package main

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/contract"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	v0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	runnersbase "github.com/codefly-dev/core/runners/base"
	pythonrunner "github.com/codefly-dev/core/runners/python"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
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
	GRPCServer     = "grpc-server"
	// ConnectEndpointSetting is go-grpc's spelling for the same face: a
	// listener serving Connect, gRPC-Web and gRPC over the service's gRPC server.
	ConnectEndpointSetting = "connect-endpoint"
)

// defaultProtoPath is where the service-owned proto contract lives, relative
// to the service root. It is core's standards.ProtoPath: LoadEndpoints
// (Builder + Runtime) and any dependent service re-derive the gRPC contract
// from that exact location, so the proto has to sit there — the same way the
// REST contract lives at openapi/api.swagger.json.
const defaultProtoPath = standards.ProtoPath

// GRPCServerSettings configures the optional service-owned grpc.aio listener.
// Disabled by default: an unset grpc-server block leaves the service with no
// gRPC listener and its generated layout unchanged.
type GRPCServerSettings struct {
	Enabled bool `yaml:"enabled"`

	// Proto is the proto contract path relative to the Python source dir.
	// Defaults to proto/api.proto when the server is enabled.
	Proto string `yaml:"proto"`
}

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

	// GRPCServer opts the service into a grpc.aio listener running in the same
	// process as the FastAPI app (see grpc-server:). Disabled by default.
	GRPCServer GRPCServerSettings `yaml:"grpc-server"`

	// ConnectEndpoint serves the service's gRPC API on a second listener that
	// speaks Connect and gRPC-Web (HTTP/1.1) and gRPC (HTTP/2) on one port:
	// the face a browser reaches through an L7 hop. Every call is transcoded
	// onto the grpc.aio server, so it needs grpc-server and adds no API of its
	// own. The agent writes the face (code/src/rpc/connect_gen.py) at sync,
	// removes it when the setting is off, and hands the process its port as
	// CODEFLY_CONNECT_PORT, locally and in the deployment, exactly when it is
	// on: the scaffold's src/main.py starts the face on that variable once the
	// gRPC server listens. Start refuses a run where nothing then listens on the
	// connect port. Off by default.
	ConnectEndpoint bool `yaml:"connect-endpoint"`

	// RuntimeImage overrides the default codefly-built runtime image.
	// Format: "name:tag". Plain "name" and ":latest" are rejected —
	// pinning is enforced. Leave empty to use ghcr.io/codefly-dev/python:<ver>
	// (recommended; companion releases are published separately, then
	// pinned by this agent). Field named RuntimeImage (not DockerImage) to avoid
	// colliding with services.Base.DockerImage(req).
	RuntimeImage string `yaml:"docker-image"`
}

// runtimeImage is the codefly-built Python runtime companion, built from
// core/companions/python/ and published by the CLI to ghcr.io/codefly-dev as
// a linux/amd64 + linux/arm64 image index. Users can override it through the
// Python settings (docker-image) but should not: the companion is the
// mode-consistent default and carries the same tool set as every other
// codefly-built image.
//
// It is pinned by the digest of the multi-platform INDEX, never of one
// platform's manifest: the build recipe is rendered for every target platform
// and each resolves its own entry from the index. The legacy
// docker.io/codeflydev/python:0.0.1 this replaces was a single amd64 manifest,
// so every linux/arm64 image build ran amd64 binaries under emulation and died
// on its first RUN ("Invalid ELF image for this architecture").
//
// Tag is the companion version of the core this agent links
// (core/companions/python/info.codefly.yaml); a test holds the two together,
// so bumping core without re-pinning here fails the build. Re-pin the digest
// from the registry when the tag moves:
//
//	docker buildx imagetools inspect ghcr.io/codefly-dev/python:<tag>
var runtimeImage = &resources.DockerImage{
	Repository: resources.ImageRegistry,
	Name:       "python",
	Tag:        "0.0.7",
	Digest:     "sha256:3b2a8f8d7854809212410db93d178d0fc5ce7b44e80ce334df0328cd6b0d156d",
}

// uvVersion is the uv the emitted build recipe resolves its lockfile with.
//
// The base image carries a uv of its own, installed when that image was built
// and stated nowhere a project or this agent can read. A build recipe is meant
// to be durable and reproducible, and a recipe whose resolver version is a
// property of its base image is neither: a project written against a newer
// `[tool.uv]` key resolves on the developer's machine and fails inside the
// builder stage with a TOML parse error naming a field the older uv does not
// know. Pinning it here is the same contract the Go recipe already keeps for
// its own toolchain, which states its Go and Alpine versions outright.
//
// Bump it deliberately, together with uvImage.
const uvVersion = "0.12.16"

// uvImage is the distribution the builder stage copies that uv from. astral
// publishes each release as a multi-architecture image holding the static
// binary, so the copy needs no package manager and no network beyond the
// registry the build already talks to.
const uvImage = "ghcr.io/astral-sh/uv:" + uvVersion

// Service is the FastAPI specialization. It embeds the generic Python
// Service so methods defined on *pythonservice.Service (and transitively
// *services.Base: Wool, Logger, Location, Identity, …) are promoted.
//
// FastAPI-specific: a richer Settings (with HotReload / PublicEndpoint)
// and the endpoint handles. FastAPI source lives under ./code — the
// Builder/Runtime set Service.SourceLocation to s.Local("code") during
// Load.
type Service struct {
	*pythonservice.Service

	// Shadows the generic Service.Settings with a richer type that still
	// inherits the generic fields (via inline embedding). `s.Settings` is
	// the FastAPI settings; `s.Service.Settings` is the generic view.
	Settings *Settings

	// HTTPEndpoint is the endpoint the app's HTTP listener (uvicorn) serves:
	// the service's REST endpoint, whose routes src/openapi.py documents, or a
	// plain HTTP one, which carries no API document (probes, a solution's own
	// routes). Nil for a service whose faces are gRPC and Connect alone.
	HTTPEndpoint *v0.Endpoint

	// GRPCEndpoint is the service-owned gRPC endpoint, present only when
	// Settings.GRPCServer.Enabled.
	GRPCEndpoint *v0.Endpoint

	// ConnectEndpoint is the Connect-family endpoint, present only when
	// Settings.ConnectEndpoint.
	ConnectEndpoint *v0.Endpoint
}

// findHTTPEndpoint returns the one endpoint the app's HTTP listener serves: a
// REST or a plain HTTP endpoint. None is not an error — a service whose faces
// are gRPC and Connect alone declares no HTTP endpoint — but two are, because
// the process has one HTTP listener and could serve only one of them.
func findHTTPEndpoint(ctx context.Context, endpoints []*v0.Endpoint) (*v0.Endpoint, error) {
	found := resources.FindEndpointsByAPI(ctx, standards.REST, endpoints)
	found = append(found, resources.FindEndpointsByAPI(ctx, standards.HTTP, endpoints)...)
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		return found[0], nil
	}
	names := make([]string, 0, len(found))
	for _, endpoint := range found {
		names = append(names, endpoint.GetName())
	}
	sort.Strings(names)
	return nil, fmt.Errorf("the app serves one HTTP listener, and the service declares %d HTTP endpoints (%s): keep one with api: rest or api: http", len(found), strings.Join(names, ", "))
}

// unservedError refuses a service nothing would serve: no HTTP endpoint, and
// no gRPC endpoint for a gRPC server to serve. A manifest that lost its
// endpoints would otherwise start a process nobody can reach, and Start would
// have nothing to wait on before it answered STARTED.
func (s *Service) unservedError(ctx context.Context, endpoints []*v0.Endpoint) error {
	if s.HTTPEndpoint != nil {
		return nil
	}
	if s.Settings.GRPCServer.Enabled && len(resources.FindEndpointsByAPI(ctx, standards.GRPC, endpoints)) > 0 {
		return nil
	}
	return fmt.Errorf("nothing would serve this service: it declares no HTTP endpoint (api: rest or api: http) and no gRPC endpoint with %s.enabled", GRPCServer)
}

// connectSettingError refuses a Connect face with no gRPC server to transcode
// onto: the face serves the gRPC server's API, never one of its own.
func (s *Settings) connectSettingError() error {
	if s.ConnectEndpoint && !s.GRPCServer.Enabled {
		return fmt.Errorf("%s needs %s.enabled: the Connect face serves the gRPC server's API", ConnectEndpointSetting, GRPCServer)
	}
	return nil
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

	advertisement := services.Advertisement{
		Backends: runnersbase.BackendSupport{
			Local:  pythonrunner.HasUVRuntime,
			Nix:    true,
			Docker: true,
		},
		Toolchains: []agentv0.Toolchain_Type{agentv0.Toolchain_PYTHON},
		HotReload:  true,
		Languages:  []agentv0.Language_Type{agentv0.Language_PYTHON},
		// The agent serves HTTP when a service declares a REST or HTTP endpoint
		// and gRPC when it opts in via grpc-server (and Connect beside it, over
		// HTTP); advertising both declares the capability, not that every
		// service exposes both.
		Protocols: []agentv0.Protocol_Type{agentv0.Protocol_HTTP, agentv0.Protocol_GRPC},
		ReadMe:    readme,
	}.Build()
	// The deployment path is core's DeployKustomize, which judges dependency
	// edges with the request's composition provenance on this core; the host
	// requires the live advertisement, not the linked core version.
	advertisement.Contract = contract.Current()
	advertisement.Contract.Capabilities = append(advertisement.Contract.Capabilities, contract.DeploymentCompositionProvenance)
	// The Runtime names what a native start ran on its StartStatus
	// (startedartifact.go), so a consumer pinning the build a process runs
	// reads it here instead of deriving a path from the python layout. The
	// capability is advertised because this agent calls
	// ReportStartedArtifact; an agent that does not must not advertise it.
	advertisement.Contract.Capabilities = append(advertisement.Contract.Capabilities, contract.RuntimeStartedArtifact)
	return advertisement, nil
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
