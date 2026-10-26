package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

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
//	                          and the endpoint handles.
//
// Inherited: Init.
// Overridden: Load (fastapi puts source under ./code, finds the HTTP
// endpoint, if any), Update (applies builder templates), Sync (gRPC codegen for
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

// Load overrides generic to place source under <service>/code and to find
// the endpoint the app's HTTP listener serves, which a service whose faces are
// gRPC and Connect alone does not declare.
func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()

	// The generic load with this agent's Settings: it loads them, the
	// Endpoints, and handles CreationMode. The generic Load would decode the
	// spec into its own half of the settings alone, leaving grpc-server and
	// connect-endpoint unset on every loaded service, so Sync generated no
	// server code and Deploy rendered no gRPC or Connect port. It reports a
	// failure in the response's state, not the error: stop there, so the
	// response carries that cause.
	resp, err := s.Base.Builder.LoadService(ctx, req, services.BuilderLoad{Settings: s.FastAPI.Settings})
	s.Service.SourceLocation = s.Service.ResolveSourceLocation()
	if err != nil || resp.GetState().GetState() == builderv0.LoadStatus_ERROR {
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

	s.FastAPI.HTTPEndpoint, err = findHTTPEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Base.Builder.LoadError(err)
	}
	if err := s.FastAPI.unservedError(ctx, s.Endpoints); err != nil {
		return s.Base.Builder.LoadError(err)
	}
	if err := s.FastAPI.Settings.connectSettingError(); err != nil {
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
	if err := s.syncConnectFace(); err != nil {
		return s.Base.Builder.SyncError(err)
	}
	return s.Base.Builder.SyncResponse()
}

// connectFacePath is where the agent writes the Connect face, relative to the
// service root: beside the generated gRPC bindings, importable as
// src.rpc.connect_gen.
const connectFacePath = "code/src/rpc/connect_gen.py"

// connectFace is the Connect face every service with connect-endpoint gets,
// byte for byte: it reads the methods it serves from the descriptors the
// service's gRPC bindings register, so nothing in it is rendered per service.
//
//go:embed templates/connect/connect_gen.py
var connectFace []byte

// connectFaceMarker is the line every Connect face the agent wrote carries, and
// what lets Sync tell its own file from one the service wrote at that path.
const connectFaceMarker = "This code is generated by the python-fastapi agent (``connect-endpoint: true``)"

// syncConnectFace writes the Connect face when the service sets
// connect-endpoint and removes the one the agent wrote when it does not: the
// file is the setting's projection, and a face left behind is drift (the
// process serves it only with CODEFLY_CONNECT_PORT, which follows the setting).
func (s *Builder) syncConnectFace() error {
	if s.FastAPI.Settings.ConnectEndpoint {
		return s.writeConnectFace()
	}
	target := s.Local(connectFacePath)
	content, err := os.ReadFile(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return s.Wool.Wrapf(err, "cannot read %s", connectFacePath)
	}
	if !bytes.Contains(content, []byte(connectFaceMarker)) {
		return nil
	}
	if err := os.Remove(target); err != nil {
		return s.Wool.Wrapf(err, "cannot remove the Connect face")
	}
	return nil
}

// writeConnectFace writes the Connect face, replacing whatever is there: the
// file is the agent's, and a hand edit is drift the next sync removes.
func (s *Builder) writeConnectFace() error {
	if err := s.FastAPI.Settings.connectSettingError(); err != nil {
		return err
	}
	target := s.Local(connectFacePath)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return s.Wool.Wrapf(err, "cannot create %s", filepath.Dir(target))
	}
	if err := os.WriteFile(target, connectFace, 0o644); err != nil {
		return s.Wool.Wrapf(err, "cannot write the Connect face")
	}
	return nil
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

	// UVWorkspace is set when the project is a member of a uv workspace rooted
	// in the service: the recipe copies that workspace and syncs the project
	// from MemberWorkdir, its directory in the builder stage.
	UVWorkspace   *uvWorkspaceMembership
	MemberWorkdir string
	// IgnorePatterns are the dockerignore entries of a recipe that copies the
	// service tree whole.
	IgnorePatterns []string
	// GitSources are the repositories of the git commits the lock pins, which
	// the builder stage reads from the caller's mirrors (uvgitsources.go).
	GitSources []uvGitMirror
	// NoHTTPEndpoint starts uvicorn on a loopback port the system picks, as a
	// native start does (uvicornArgs): a service with no HTTP endpoint serves
	// no HTTP in the image either.
	NoHTTPEndpoint bool
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
	// A project that is a member of a uv workspace in the service is built from
	// that workspace, copied whole (see enclosingUVWorkspace); its path sources
	// must then stay inside the service the context carries.
	// uvicorn's logging configuration and its filter module, written into the
	// project exactly as a native start writes them, before any context is
	// assembled from it: the build context is the service directory (or the
	// assembled copy of it), and the Dockerfile copies them from code/ beside
	// src (uvicorn_logging.go).
	if err := writeUvicornLogging(s.SourceLocation); err != nil {
		return s.Base.Builder.BuildError(err)
	}
	membership, err := enclosingUVWorkspace(s.Location, s.SourceLocation)
	if err != nil {
		return s.Base.Builder.BuildError(err)
	}
	var assembled *assembledContext
	if membership != nil {
		if err := pathSourcesStayInService(s.Location, s.SourceLocation, membership); err != nil {
			return s.Base.Builder.BuildError(err)
		}
	} else {
		// A `[tool.uv.sources]` path resolves against the project directory, and
		// the default context is the service directory and nothing beside it:
		// carry what leaves the project into an assembled context so the builder
		// stage resolves what the host resolves. A service with no such path
		// assembles nothing.
		assembled, err = assembleUVContext(s.Location, s.SourceLocation, outputDir)
		if err != nil {
			return s.Base.Builder.BuildError(err)
		}
	}
	components, err := imageComponents(s.SourceLocation)
	if err != nil {
		return s.Base.Builder.BuildError(err)
	}
	gitSources, unmirrored, err := lockedGitSources(uvLockPath(s.Location, s.SourceLocation, membership))
	if err != nil {
		return s.Base.Builder.BuildError(err)
	}
	for _, source := range unmirrored {
		// The URL is not logged: a lock may still carry a credential in it.
		s.Wool.Warn("git source is not mirrored: the image build fetches it itself",
			wool.Field("package", source.Package), wool.Field("reason", source.Reason))
	}
	var options []services.RecipeOption
	if len(gitSources) > 0 {
		options = append(options, services.WithGitSourceDownloads(gitSources...))
	}
	docker := DockerTemplating{
		Builder:        runtimeImage.FullName(),
		Components:     components,
		UVImage:        uvImage,
		ProjectWorkdir: defaultProjectWorkdir,
		GitSources:     uvGitMirrors(gitSources),
		NoHTTPEndpoint: s.FastAPI.HTTPEndpoint == nil,
	}
	if assembled != nil {
		docker.ServicePrefix = assembled.ServicePrefix
		docker.ProjectWorkdir = assembled.ProjectWorkdir
		docker.Carried = assembled.Carried
	}
	if membership != nil {
		docker.UVWorkspace = membership
		docker.MemberWorkdir = path.Join(defaultProjectWorkdir, membership.Root, membership.Member)
		docker.IgnorePatterns = ephemeralIgnorePatterns()
	}
	if err := s.Base.Templates(ctx, docker, services.WithBuilder(builderFS).WithDestination("%s", outputDir)); err != nil {
		return s.Base.Builder.BuildError(err)
	}
	image := s.DockerImage(dockerRequest).FullName()
	if assembled == nil {
		// Only the Dockerfile and its ignore were written, and the application's
		// inputs are the service's own tree: the plan claims what it emitted and
		// the caller builds the service directory.
		return s.Base.Builder.SingleImageBuildResponse(req, image, emitted, options...)
	}
	// The whole destination was assembled by this build, so the plan claims all
	// of it and the caller builds the assembled tree rather than the service
	// directory — which is what RECIPE_INVENTORY_SCOPE_TREE declares.
	recipe := &builderv0.DockerBuildRecipe{
		Name:         "app",
		Dockerfile:   "Dockerfile",
		Context:      ".",
		Dockerignore: dockerignoreOf(emitted),
		Image:        image,
		Platforms:    services.RecipeBuildPlatforms(),
	}
	for _, option := range options {
		option(recipe)
	}
	plan, err := services.BuildDockerBuildPlan(outputDir, []*builderv0.DockerBuildRecipe{recipe})
	if err != nil {
		return s.Base.Builder.BuildError(err)
	}
	s.Base.Builder.WithBuildPlan(plan)
	return s.Base.Builder.BuildResponse()
}

// testsPackage is the test tree Create scaffolds beside src. It is importable
// locally but is not part of the application, so the image does not ship it.
const testsPackage = "tests"

// imageComponents is every service-relative source tree the runtime stage
// copies: code/src, which uvicorn imports the app from, and every other
// top-level Python package in the project directory.
//
// Locally the service runs as `uv run uvicorn src.main:app` from code/, so
// every package beside src is importable twice over — from the working
// directory uvicorn puts on sys.path, and from the project uv installs. The
// image installs dependencies only (`uv sync --no-install-project`) and runs
// from /app/code, so a sibling package it does not copy is a service that
// passes every local run and dies in the container with ModuleNotFoundError.
// Copying each package keeps the working directory the one thing that makes
// them importable in both places.
//
// A package is a directory holding __init__.py. Build outputs and tool caches
// are never packages of the application, and neither is the test tree.
func imageComponents(sourceDir string) ([]string, error) {
	components := requirements.All()
	shipped := map[string]bool{}
	for _, component := range components {
		shipped[component] = true
	}
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("cannot list the Python project at %s: %w", sourceDir, err)
	}
	var packages []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || name == testsPackage || ephemeralSourceDirectories[name] || strings.HasPrefix(name, ".") {
			continue
		}
		component := path.Join(filepath.Base(sourceDir), name)
		if shipped[component] {
			continue
		}
		marker, err := os.Stat(filepath.Join(sourceDir, name, "__init__.py"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !marker.Mode().IsRegular() {
			continue
		}
		packages = append(packages, component)
	}
	sort.Strings(packages)
	return append(components, packages...), nil
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
	// ConnectEnabled adds the Connect face's containerPort, Service port and
	// CODEFLY_CONNECT_PORT, the variable the app serves the face on.
	// ConnectPort is the fixed in-cluster port it binds.
	ConnectEnabled bool
	ConnectPort    int
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

	// The rendered pod is probed on the app's HTTP listener, the container's
	// port 8080, which a service with no HTTP endpoint does not publish: say so
	// rather than report the missing endpoint as missing health.
	listener := s.FastAPI.HTTPEndpoint
	if listener == nil {
		return nil, fmt.Errorf("the deployment probes the HTTP endpoint (api: rest or api: http), and the service declares none: declare the endpoint its probes answer on")
	}
	// The current Core manifest profile requires all three intents. Require
	// an authored startup/restart policy instead of inventing one from readiness.
	plan := resources.PlanEndpointProbes(listener)
	if plan.Startup == nil || plan.Liveness == nil {
		return nil, fmt.Errorf("the Kubernetes manifest profile requires explicit endpoint health.startup and health.liveness; readiness is not a restart policy")
	}
	// A restricted render is what a cell runs, and a cell's execution
	// admission refuses an exec probe: probe natively there.
	probes, err := deploymentProbesFor(listener, req.GetDeployment().GetKubernetes().GetProfile())
	if err != nil {
		return nil, err
	}
	for _, endpoint := range s.Endpoints {
		if endpoint.GetName() != listener.GetName() && endpoint.GetHealth() != nil {
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
			Probes:         probes,
			GRPCEnabled:    s.FastAPI.Settings.GRPCServer.Enabled,
			GRPCPort:       int(standards.Port(standards.GRPC)),
			ConnectEnabled: s.FastAPI.Settings.ConnectEndpoint,
			ConnectPort:    int(standards.Port(standards.CONNECT)),
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

// CreateEndpoints materializes the REST endpoint from the OpenAPI document
// the factory templates have just written (openapi/api.swagger.json); Init
// regenerates it with src/openapi.py from then on.
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
		// Since core v0.14.0 an endpoint declares two independent axes, and a
		// PUBLIC one must state both: visibility is reach, exposure is
		// addressing. "Expose API as public" has always meant reachable from
		// outside the workspace, which is exactly exposure public, so the old
		// single-axis answer projects onto both rather than leaving the second
		// unset. A manifest scaffolded without it is one core refuses to load,
		// and the service then reports no endpoints at all.
		endpoint.Visibility = resources.VisibilityPublic
		endpoint.Exposure = resources.ExposurePublic
	}

	rest, err := resources.LoadRestAPI(ctx, shared.Pointer(openapiFile))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot load rest api")
	}

	api, err := resources.NewAPI(ctx, endpoint, resources.ToRestAPI(rest))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create openapi api")
	}
	api.Health, err = endpoint.Health.Proto(api.GetApi())
	if err != nil {
		return s.Wool.Wrapf(err, "cannot declare scaffold health")
	}
	s.FastAPI.HTTPEndpoint = api
	s.Endpoints = []*basev0.Endpoint{api}

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
		// Since core v0.14.0 an endpoint declares two independent axes, and a
		// PUBLIC one must state both: visibility is reach, exposure is
		// addressing. "Expose API as public" has always meant reachable from
		// outside the workspace, which is exactly exposure public, so the old
		// single-axis answer projects onto both rather than leaving the second
		// unset. A manifest scaffolded without it is one core refuses to load,
		// and the service then reports no endpoints at all.
		endpoint.Visibility = resources.VisibilityPublic
		endpoint.Exposure = resources.ExposurePublic
	}
	api, err := resources.NewAPI(ctx, endpoint, resources.ToGrpcAPI(grpc))
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create grpc api")
	}
	return api, nil
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
