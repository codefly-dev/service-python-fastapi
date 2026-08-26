package main

import (
	"context"
	"embed"
	"errors"
	"os"
	"path/filepath"

	"github.com/codefly-dev/core/agents/communicate"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/agents/services/upgrade"
	"github.com/codefly-dev/core/companions/proto"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/templates"
	"github.com/codefly-dev/core/wool"

	pythonbuilder "github.com/codefly-dev/service-python/pkg/builder"
)

// Builder is the FastAPI specialization of the generic Python Builder.
//
// Embedding chain:
//
//	*pythonbuilder.Builder  — promotes Init / Update / Deploy (no-op)
//	                          plus the services.Base chain via
//	                          *pythonservice.Service.
//	FastAPI *Service        — fastapi-specific state: richer Settings
//	                          and the REST endpoint.
//
// Inherited: Init.
// Overridden: Load (fastapi puts source under ./code, discovers REST
// endpoint), Update (applies builder templates), Sync (gRPC codegen for
// declared dependencies and the optional service-owned gRPC server), Build
// (custom DockerTemplating + docker build),
// Deploy (k8s), Create (two-question Communicate + REST endpoint).
type Builder struct {
	*pythonbuilder.Builder

	FastAPI *Service

	answers map[string]*agentv0.Answer
}

// NewBuilder composes a fastapi Builder from the generic Python Builder.
func NewBuilder(svc *Service) *Builder {
	return &Builder{
		Builder: pythonbuilder.New(svc.Service),
		FastAPI: svc,
	}
}

// Load overrides generic to place source under <service>/code and to
// discover the REST endpoint.
func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()

	// Call generic first — it loads Settings, Endpoints, handles CreationMode.
	resp, err := s.Builder.Load(ctx, req)
	if err != nil {
		return resp, err
	}
	requirements.Localize(s.Location)

	// Override SourceLocation: fastapi source lives in ./code, not the
	// service root (generic's default).
	s.Service.SourceLocation = s.Local("code")

	// In creation mode, regenerate GETTING_STARTED from the fastapi template
	// (generic has no templates).
	if req.CreationMode != nil {
		gs, tmplErr := s.renderGettingStarted(ctx)
		if tmplErr != nil {
			return s.Base.Builder.LoadError(tmplErr)
		}
		s.Base.Builder.GettingStarted = gs
		return s.Base.Builder.LoadResponse()
	}

	s.FastAPI.RestEndpoint, err = resources.FindRestEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Base.Builder.LoadError(err)
	}

	return s.Base.Builder.LoadResponse()
}

func (s *Builder) renderGettingStarted(ctx context.Context) (string, error) {
	return renderFromFactory(ctx, s.Information)
}

// Init is INHERITED from *pythonbuilder.Builder (records dep endpoints).

// Update re-applies builder templates. Generic has this as a no-op.
func (s *Builder) Update(ctx context.Context, _ *builderv0.UpdateRequest) (*builderv0.UpdateResponse, error) {
	defer s.Wool.Catch()
	if err := s.Base.Templates(ctx, services.WithBuilder(builderFS)); err != nil {
		return nil, s.Wool.Wrapf(err, "cannot copy and apply template")
	}
	return &builderv0.UpdateResponse{}, nil
}

// Sync generates gRPC client stubs for declared dependencies.
func (s *Builder) Sync(ctx context.Context, _ *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	w := s.Wool.In("sync")
	w.Debug("dependencies",
		wool.Field("dependencies", s.Base.Service.ServiceDependencies),
		wool.Field("endpoints", resources.MakeManyEndpointSummary(s.DependencyEndpoints)))

	for _, dep := range s.Base.Service.ServiceDependencies {
		ep, err := resources.FindGRPCEndpointFromService(ctx, dep, s.DependencyEndpoints)
		if err != nil {
			return s.Base.Builder.SyncError(err)
		}
		if ep == nil {
			continue
		}
		w.Info("generating grpc code", wool.Field("dependency", dep))
		if err := proto.GenerateGRPC(ctx, languages.PYTHON, s.Local("code/src/external/%s", dep.Unique()), dep.Unique(), ep); err != nil {
			return s.Base.Builder.SyncError(err)
		}
	}

	if s.FastAPI.Settings.GRPCServer.Enabled {
		if err := s.syncGRPCServer(ctx); err != nil {
			return s.Base.Builder.SyncError(err)
		}
	}
	return s.Base.Builder.SyncResponse()
}

// syncGRPCServer regenerates the Python protobuf + grpc.aio server stubs from
// the service-owned proto contract via Buf. Buf reads proto/ under the service
// root (matching the endpoint contract location) and writes the stubs into the
// Python source tree. Generation is cached on the proto tree, so it re-runs
// deterministically only when the contract changes.
func (s *Builder) syncGRPCServer(ctx context.Context) error {
	buf, err := proto.NewBuf(ctx, s.Location)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create proto generator")
	}
	buf.WithGeneratedDirs(s.Local("code/src/rpc/_generated"))
	if err := buf.Generate(ctx); err != nil {
		return s.Wool.Wrapf(err, "cannot generate grpc server code")
	}
	return nil
}

// Env + DockerTemplating structs are the template context for the
// builder Dockerfile.
type Env struct {
	Key   string
	Value string
}

type DockerTemplating struct {
	Builder         string
	Components      []string
	RuntimePackages []string
	Envs            []Env
}

// Build produces the service Docker image. Generic is a no-op; fastapi
// renders a Dockerfile and either builds it in-process (legacy) or, when the
// CLI owns the build, emits a reproducible recipe for the CLI to build.
func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	defer s.Wool.Catch()
	dockerRequest, err := s.Base.Builder.DockerBuildRequest(ctx, req)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "can only do docker build request")
	}

	image := s.DockerImage(dockerRequest)
	s.Wool.Debug("building docker image", wool.Field("image", image.FullName()))
	ctx = s.Wool.Inject(ctx)

	docker := DockerTemplating{
		Builder:    runtimeImage.FullName(),
		Components: requirements.All(),
	}

	if err := shared.DeleteFile(ctx, s.Local("builder/Dockerfile")); err != nil {
		return nil, s.Wool.Wrapf(err, "cannot remove dockerfile")
	}
	if err := s.Base.Templates(ctx, docker, services.WithBuilder(builderFS)); err != nil {
		return nil, s.Wool.Wrapf(err, "cannot copy and apply template")
	}

	// When the caller owns the build (a non-empty output_directory), emit a
	// reproducible build recipe rather than building in-process. The rendered
	// Dockerfile already lives in that directory, so the CLI runs docker buildx
	// against it and publishes a multi-arch manifest list.
	if outputDir := req.GetOutputDirectory(); outputDir != "" {
		plan, err := singleImageBuildPlan(outputDir, image.FullName())
		if err != nil {
			return s.Base.Builder.BuildError(err)
		}
		s.Base.Builder.WithBuildPlan(plan)
		return s.Base.Builder.BuildResponse()
	}

	builder, err := dockerhelpers.NewBuilder(dockerhelpers.BuilderConfiguration{
		Root:        s.Location,
		Dockerfile:  "builder/Dockerfile",
		Destination: image,
		Output:      s.Wool,
	})
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create builder")
	}
	if _, err := builder.Build(ctx); err != nil {
		return nil, s.Wool.Wrapf(err, "cannot build image")
	}

	s.Base.Builder.WithDockerImages(image)
	return s.Base.Builder.BuildResponse()
}

// singleImageBuildPlan inventories the recipe the CLI-owned build consumes: the
// builder/Dockerfile rendered into outputDirectory, built with the service
// directory as its context and targeting a linux/amd64 + linux/arm64 manifest
// list so a consumer never needs the agent toolchain to rebuild. Paths are
// encoded as the CLI's recipe executor resolves them — the Dockerfile relative
// to outputDirectory (which the CLI sets to <service>/builder), the context
// relative to the service directory.
//
// This stands in for services.SingleImageBuildPlan / services.RecipeBuildPlatforms,
// which are not in a released core yet (they land with the shared recipe runners
// in core#336); it can be replaced once that release lands and its recipe layout
// is confirmed to match the CLI executor.
func singleImageBuildPlan(outputDirectory, image string) (*builderv0.DockerBuildPlan, error) {
	recipe := &builderv0.DockerBuildRecipe{
		Name:       "app",
		Dockerfile: "Dockerfile",
		Context:    ".",
		Image:      image,
		Platforms:  []string{"linux/amd64", "linux/arm64"},
	}
	if info, err := os.Stat(filepath.Join(outputDirectory, "dockerignore")); err == nil && info.Mode().IsRegular() {
		recipe.Dockerignore = "dockerignore"
	}
	return services.BuildDockerBuildPlan(outputDirectory, []*builderv0.DockerBuildRecipe{recipe})
}

// Upgrade bumps Python dependencies in requirements.txt (pip list
// --outdated + rewrite + pip install --upgrade). --major allows major
// version jumps; --dry-run skips the write.
func (s *Builder) Upgrade(ctx context.Context, req *builderv0.UpgradeRequest) (*builderv0.UpgradeResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	dir := s.Local("code")
	res, err := upgrade.Python(ctx, dir, upgrade.Options{
		IncludeMajor: req.IncludeMajor,
		DryRun:       req.DryRun,
		Only:         req.Only,
	})
	if err != nil {
		return s.Base.Builder.UpgradeError(err)
	}
	return s.Base.Builder.UpgradeResponse(res.Changes, res.LockfileDiff)
}

// Parameters is the template parameter set for the k8s deployment.
type Parameters struct {
	// GRPCEnabled adds the gRPC containerPort and Service port to the rendered
	// manifests. GRPCPort is the fixed in-cluster port the grpc.aio listener
	// binds (the app defaults to it when CODEFLY_GRPC_PORT is unset).
	GRPCEnabled bool
	GRPCPort    int
}

// Deploy renders and applies k8s manifests.
func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	return s.Base.Builder.DeployKustomize(ctx, req, services.KustomizeDeployment{
		EnvironmentVariables: s.EnvironmentVariables,
		Templates:            deploymentFS,
		Inputs: services.DeploymentInputs{
			OwnConfiguration:         true,
			DependencyConfigurations: true,
		},
		Parameters: Parameters{
			GRPCEnabled: s.FastAPI.Settings.GRPCServer.Enabled,
			GRPCPort:    int(standards.Port(standards.GRPC)),
		},
	})
}

// Options returns the two-question set shown during `codefly add service`.
func (s *Builder) Options() []*agentv0.Question {
	return []*agentv0.Question{
		communicate.NewConfirm(&agentv0.Message{Name: PublicEndpoint, Message: "Expose API as public", Description: "is that directly accessible from the internet?"}, true),
		communicate.NewConfirm(&agentv0.Message{Name: HotReload, Message: "Code hot-reload (Recommended)?", Description: "codefly can restart your service when code changes are detected 🔎"}, true),
		communicate.NewConfirm(&agentv0.Message{Name: GRPCServer, Message: "Add a gRPC server?", Description: "runs a grpc.aio listener alongside FastAPI with a proto contract ⚙️"}, false),
	}
}

// CreateConfiguration is the template context passed to factory templates.
type CreateConfiguration struct {
	*services.Information
	Image *resources.DockerImage
	Envs  []string

	// GRPCEnabled gates the gRPC boot in src/main.py and the grpcio
	// dependencies in pyproject.toml. False keeps the REST-only scaffold.
	GRPCEnabled bool
}

// Create applies factory templates, scaffolds src/tests dirs, and
// creates the REST endpoint.
func (s *Builder) Create(ctx context.Context, _ *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if s.Base.Builder.CreationMode != nil && s.Base.Builder.CreationMode.Communicate && s.answers != nil {
		if err := s.populateSettingsFromAnswers(); err != nil {
			return s.Base.Builder.CreateError(err)
		}
	} else {
		if err := s.populateSettingsFromDefaults(); err != nil {
			return s.Base.Builder.CreateError(err)
		}
	}

	create := CreateConfiguration{Information: s.Information, Envs: []string{}, GRPCEnabled: s.FastAPI.Settings.GRPCServer.Enabled}
	if err := s.Base.Templates(ctx, create, services.WithFactory(factoryFS)); err != nil {
		return s.Base.Builder.CreateError(err)
	}

	// The gRPC scaffold (proto contract, buf config, grpc.aio server + user
	// servicer seam) is applied from a separate tree only when opted in, so a
	// REST-only service keeps its generated layout untouched.
	if s.FastAPI.Settings.GRPCServer.Enabled {
		grpc := services.WithTemplate(grpcFS, "grpc", "").WithOverride(shared.SkipAll())
		if err := s.Base.Templates(ctx, create, grpc); err != nil {
			return s.Base.Builder.CreateError(err)
		}
	}

	// Scaffold package + tests dirs with empty __init__.py.
	if _, err := shared.CheckDirectoryOrCreate(ctx, s.Local("code/src")); err != nil {
		return s.Base.Builder.CreateError(err)
	}
	if _, err := shared.CheckDirectoryOrCreate(ctx, s.Local("code/tests")); err != nil {
		return s.Base.Builder.CreateError(err)
	}
	if err := shared.CreateFile(ctx, s.Local("code/src/__init__.py")); err != nil {
		return s.Base.Builder.CreateError(err)
	}
	if err := shared.CreateFile(ctx, s.Local("code/tests/__init__.py")); err != nil {
		return s.Base.Builder.CreateError(err)
	}
	if err := s.CreateEndpoints(ctx); err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create endpoints")
	}
	return s.Base.Builder.CreateResponse(ctx, s.FastAPI.Settings)
}

// CreateEndpoints materializes the REST endpoint.
//
// openapi/api.swagger.json is generated at Runtime time by src/openapi.py
// (uv run python src/openapi.py). At Create time it doesn't exist yet —
// that specific error is expected and swallowed with a debug log. Any
// other LoadRestAPI failure (malformed JSON, permission, …) propagates.
func (s *Builder) CreateEndpoints(ctx context.Context) error {
	openapiFile := s.Local("openapi/api.swagger.json")
	endpoint := s.Base.BaseEndpoint(standards.REST)
	if s.FastAPI.Settings.PublicEndpoint {
		endpoint.Visibility = resources.VisibilityPublic
	}

	rest, loadErr := resources.LoadRestAPI(ctx, shared.Pointer(openapiFile))
	if loadErr != nil {
		if !errors.Is(loadErr, os.ErrNotExist) && !isFileNotExistErr(loadErr) {
			return s.Wool.Wrapf(loadErr, "cannot load rest api")
		}
		s.Wool.Debug("openapi spec not generated yet (expected at Create time)",
			wool.Field("path", openapiFile))
	}

	api, err := resources.NewAPI(ctx, endpoint, resources.ToRestAPI(rest))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create openapi api")
	}
	s.FastAPI.RestEndpoint = api
	s.Endpoints = []*basev0.Endpoint{s.FastAPI.RestEndpoint}

	if s.FastAPI.Settings.GRPCServer.Enabled {
		grpcEndpoint, grpcErr := s.grpcEndpoint(ctx)
		if grpcErr != nil {
			return grpcErr
		}
		s.FastAPI.GRPCEndpoint = grpcEndpoint
		s.Endpoints = append(s.Endpoints, grpcEndpoint)
	}
	return nil
}

// grpcEndpoint builds the service-owned gRPC endpoint from the proto contract.
// The proto path is resolved relative to the service root — the same location
// core's LoadEndpoints re-reads it from — so the endpoint keeps its RPCs across
// reloads and stays consumable by dependent services. It inherits the same
// public/private visibility as the REST endpoint.
func (s *Builder) grpcEndpoint(ctx context.Context) (*basev0.Endpoint, error) {
	protoPath := s.Local("%s", s.FastAPI.Settings.GRPCServer.Proto)
	grpc, err := resources.LoadGrpcAPI(ctx, shared.Pointer(protoPath))
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot load grpc proto %q", protoPath)
	}
	endpoint := s.Base.BaseEndpoint(standards.GRPC)
	if s.FastAPI.Settings.PublicEndpoint {
		endpoint.Visibility = resources.VisibilityPublic
	}
	api, err := resources.NewAPI(ctx, endpoint, resources.ToGrpcAPI(grpc))
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create grpc api")
	}
	return api, nil
}

// isFileNotExistErr matches the bespoke "file does not exist" error string
// that resources.LoadRestAPI returns for missing files — it doesn't wrap
// os.ErrNotExist, so errors.Is can't catch it directly.
func isFileNotExistErr(err error) bool {
	return err != nil && err.Error() == "file does not exist"
}

func (s *Builder) populateSettingsFromAnswers() error {
	var err error
	if s.FastAPI.Settings.HotReload, err = communicate.Confirm(s.answers, HotReload); err != nil {
		return err
	}
	if s.FastAPI.Settings.PublicEndpoint, err = communicate.Confirm(s.answers, PublicEndpoint); err != nil {
		return err
	}
	if s.FastAPI.Settings.GRPCServer.Enabled, err = communicate.Confirm(s.answers, GRPCServer); err != nil {
		return err
	}
	s.applyGRPCDefaults()
	return nil
}

// applyGRPCDefaults fills the proto path when the server is enabled but the
// path was left blank (interactive answers and defaults never set it).
func (s *Builder) applyGRPCDefaults() {
	if s.FastAPI.Settings.GRPCServer.Enabled && s.FastAPI.Settings.GRPCServer.Proto == "" {
		s.FastAPI.Settings.GRPCServer.Proto = defaultProtoPath
	}
}

func (s *Builder) populateSettingsFromDefaults() error {
	opts := s.Options()
	var err error
	if s.FastAPI.Settings.HotReload, err = communicate.GetDefaultConfirm(opts, HotReload); err != nil {
		return err
	}
	if s.FastAPI.Settings.PublicEndpoint, err = communicate.GetDefaultConfirm(opts, PublicEndpoint); err != nil {
		return err
	}
	if s.FastAPI.Settings.GRPCServer.Enabled, err = communicate.GetDefaultConfirm(opts, GRPCServer); err != nil {
		return err
	}
	s.applyGRPCDefaults()
	return nil
}

func (s *Builder) Communicate(stream builderv0.Builder_CommunicateServer) error {
	asker := communicate.NewQuestionAsker(stream)
	answers, err := asker.RunSequence(s.Options())
	if err != nil {
		return err
	}
	s.answers = answers
	return nil
}

// renderFromFactory renders templates/factory/GETTING_STARTED.md.tmpl
// from the embedded factory FS. Kept at binary level because //go:embed
// can't reach up from pkg.
func renderFromFactory(ctx context.Context, info *services.Information) (string, error) {
	return templates.ApplyTemplateFrom(ctx, shared.Embed(factoryFS), "templates/factory/GETTING_STARTED.md", info)
}

//go:embed templates/factory
var factoryFS embed.FS

// all: so the scaffold's dotfiles (code/.gitignore) are embedded — go:embed
// skips names beginning with "." without it.
//
//go:embed all:templates/grpc
var grpcFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
