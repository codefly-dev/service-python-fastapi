package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"os"

	"github.com/codefly-dev/core/agents/communicate"
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
// (Dockerfile and build plan preparation),
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

	// UVImage is the pinned uv distribution the builder stage copies its uv
	// from, so the tool that reads pyproject.toml and uv.lock is a declared
	// input of the recipe rather than whatever the base image was built with.
	UVImage string

	// ServicePrefix, ProjectWorkdir and Carried describe the build context.
	// They are empty / defaulted when the recipe builds the service directory
	// itself, which renders the Dockerfile this agent has always rendered.
	ServicePrefix  string
	ProjectWorkdir string
	Carried        []CarriedSource
}

func (s *Builder) BuildCapabilities(context.Context, *builderv0.BuildCapabilitiesRequest) (*builderv0.BuildCapabilitiesResponse, error) {
	return &builderv0.BuildCapabilitiesResponse{BuildxSelection: true}, nil
}

func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	defer s.Wool.Catch()
	if req.GetOutputDirectory() == "" {
		return s.Base.Builder.BuildError(errors.New("output_directory is required for CLI image builds"))
	}
	dockerRequest, err := s.Base.Builder.DockerBuildRequest(ctx, req)
	if err != nil {
		return s.Base.Builder.BuildError(err)
	}
	ctx = s.Wool.Inject(ctx)
	outputDir := req.GetOutputDirectory()
	emitted, err := services.PrepareRecipeDestination(builderFS, outputDir)
	if err != nil {
		return s.Base.Builder.BuildError(err)
	}
	// A `[tool.uv.sources]` path resolves against the project directory, and the
	// default context is the service directory and nothing beside it: carry what
	// leaves the project into an assembled context so the builder stage resolves
	// what the host resolves. A service with no such path assembles nothing.
	assembled, err := assembleUVContext(s.Location, s.SourceLocation, outputDir)
	if err != nil {
		return s.Base.Builder.BuildError(err)
	}
	docker := DockerTemplating{
		Builder:        runtimeImage.FullName(),
		Components:     requirements.All(),
		UVImage:        uvImage,
		ProjectWorkdir: defaultProjectWorkdir,
	}
	if assembled != nil {
		docker.ServicePrefix = assembled.ServicePrefix
		docker.ProjectWorkdir = assembled.ProjectWorkdir
		docker.Carried = assembled.Carried
	}
	if err := s.Base.Templates(ctx, docker, services.WithBuilder(builderFS).WithDestination("%s", outputDir)); err != nil {
		return s.Base.Builder.BuildError(err)
	}
	image := s.DockerImage(dockerRequest).FullName()
	if assembled == nil {
		// Only the Dockerfile and its ignore were written, and the application's
		// inputs are the service's own tree: the plan claims what it emitted and
		// the caller builds the service directory.
		return s.Base.Builder.SingleImageBuildResponse(req, image, emitted)
	}
	// The whole destination was assembled by this build, so the plan claims all
	// of it and the caller builds the assembled tree rather than the service
	// directory — which is what RECIPE_INVENTORY_SCOPE_TREE declares.
	plan, err := services.BuildDockerBuildPlan(outputDir, []*builderv0.DockerBuildRecipe{{
		Name:         "app",
		Dockerfile:   "Dockerfile",
		Context:      ".",
		Dockerignore: dockerignoreOf(emitted),
		Image:        image,
		Platforms:    services.RecipeBuildPlatforms(),
	}})
	if err != nil {
		return s.Base.Builder.BuildError(err)
	}
	s.Base.Builder.WithBuildPlan(plan)
	return s.Base.Builder.BuildResponse()
}

// dockerignoreOf names the ignore file only when this build rendered one.
// Probing the destination instead would adopt a stale file: the executor copies
// the referenced ignore to the path buildx discovers, so one left behind by an
// older template set would silently keep files out of the image.
func dockerignoreOf(emitted []string) string {
	for _, name := range emitted {
		if name == "dockerignore" {
			return name
		}
	}
	return ""
}

// SBOM serves both evidence scopes. Source inventory stays with the generic
// Python builder, which reads uv's lockfile; it describes the resolved
// dependency set and says nothing about the OS packages of a shipped image.
//
// Image scope goes to core's shared scanner. This agent emits a build recipe
// and never runs buildx, so the digests only exist on the caller's side and
// arrive as explicit subjects.
func (s *Builder) SBOM(ctx context.Context, req *builderv0.SBOMRequest) (*builderv0.SBOMResponse, error) {
	defer s.Wool.Catch()
	if req.GetScope() != builderv0.SBOMScope_SBOM_SCOPE_IMAGE {
		return s.Builder.SBOM(ctx, req)
	}
	ctx = s.Wool.Inject(ctx)
	return s.imageSBOM(ctx, req.GetSubjects())
}

// imageSBOM preserves each caller-supplied immutable image identity and source.
// Core refuses unpinned subjects and never substitutes another scanner source.
func (s *Builder) imageSBOM(ctx context.Context, subjects []*builderv0.ImageSubject) (*builderv0.SBOMResponse, error) {
	return s.Base.Builder.SBOMImages(ctx, subjects)
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
	Probes string
	// GRPCEnabled adds the gRPC containerPort and Service port to the rendered
	// manifests. GRPCPort is the fixed in-cluster port the grpc.aio listener
	// binds (the app defaults to it when CODEFLY_GRPC_PORT is unset).
	GRPCEnabled bool
	GRPCPort    int
	// AutomountServiceAccountToken projects the ServiceAccount token into the
	// pod. It stays false because the identity path is the metadata server or a
	// webhook-projected token and the app does not call the API server. Setting
	// it true requires the pod annotation codefly.dev/api-server-access: required.
	AutomountServiceAccountToken bool
}

// The container runs with readOnlyRootFilesystem, so the template reserves one
// scratch volume and mounts it at the path HOME also points at.
const (
	scratchVolumeName = "tmp"
	scratchMountPath  = "/tmp"
)

// preparePodOverlay normalizes the overlay the way the render depends on and
// rejects a ConfigMount that collides with the scratch mount.
//
// Core validates a mount against the other mounts, but only this agent knows
// the volume name and container path its own template reserves. A colliding
// mount renders a duplicate pod volume name (or a second volumeMount on the
// same path) that core's static conformance accepts and the API server rejects
// at apply — and shadowing the scratch mount would leave the app with no
// writable storage at all.
func preparePodOverlay(overlay *services.PodTemplateOverlay) error {
	if overlay == nil {
		return nil
	}
	// The template renders VolumeName and ReadOnly verbatim; unnormalized they
	// are empty and nil, which render an unnamed volume and a "<nil>" string
	// that survive YAML parsing and static conformance.
	overlay.DefaultConfigMounts()
	for _, mount := range overlay.ConfigMounts {
		if mount.VolumeName == scratchVolumeName {
			return fmt.Errorf("config mount %q collides with the reserved scratch volume %q", mount.ConfigMapName, scratchVolumeName)
		}
		if mount.MountPath == scratchMountPath {
			return fmt.Errorf("config mount %q would shadow the scratch mount at %s", mount.ConfigMapName, scratchMountPath)
		}
	}
	return overlay.Validate()
}

// Deploy renders and applies k8s manifests.
func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	// The current Core manifest profile requires all three intents. Require
	// an authored startup/restart policy instead of inventing one from readiness.
	plan := resources.PlanEndpointProbes(s.FastAPI.RestEndpoint)
	if plan.Startup == nil || plan.Liveness == nil {
		return nil, fmt.Errorf("the Kubernetes manifest profile requires explicit endpoint health.startup and health.liveness; readiness is not a restart policy")
	}
	probes, err := deploymentProbes(s.FastAPI.RestEndpoint)
	if err != nil {
		return nil, err
	}
	for _, endpoint := range s.Endpoints {
		if endpoint.GetName() != s.FastAPI.RestEndpoint.GetName() && endpoint.GetHealth() != nil {
			return nil, fmt.Errorf("declared health on additional endpoint %q requires a combined deployment probe; refusing to ignore it", endpoint.GetName())
		}
	}
	// No producer populates the overlay yet — the deployment request carries no
	// identity or config-mount payload (codefly-dev/core#594, codefly-dev/cli#757).
	// The check sits on the path the overlay will arrive on, so wiring the source
	// cannot skip it.
	var overlay *services.PodTemplateOverlay
	if err = preparePodOverlay(overlay); err != nil {
		return nil, err
	}
	return s.Base.Builder.DeployKustomize(ctx, req, services.KustomizeDeployment{
		EnvironmentVariables: s.EnvironmentVariables,
		Templates:            deploymentFS,
		PodOverlay:           overlay,
		Inputs: services.DeploymentInputs{
			OwnConfiguration:         true,
			DependencyConfigurations: true,
		},
		Parameters: Parameters{
			Probes:      probes,
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
	// New scaffolds explicitly declare the policy supported by their /version
	// route. Existing authored services are loaded, never rewritten by Deploy.
	endpoint.Health = &resources.Health{
		Readiness: &resources.Probe{Kind: resources.ProbeKindHTTP, Path: "/version", Statuses: []string{"200"}},
		Startup:   &resources.Probe{Kind: resources.ProbeKindTransport, Period: "2s", FailureThreshold: 30},
		Liveness:  &resources.Probe{Kind: resources.ProbeKindTransport},
	}

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
	api.Health, err = endpoint.Health.Proto(api.GetApi())
	if err != nil {
		return s.Wool.Wrapf(err, "cannot declare scaffold health")
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
