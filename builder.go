package main

import (
	"context"
	"embed"
	"errors"
	"os"

	"github.com/codefly-dev/core/agents/communicate"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/agents/services/audit"
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
// declared dependencies), Build (custom DockerTemplating + docker build),
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
	return s.Base.Builder.SyncResponse()
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
// renders a Dockerfile and runs docker build.
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

// Audit scans the Python project for vulnerabilities (pip-audit) and
// optionally reports outdated packages (pip list --outdated). Runs at
// the service code root (./code).
func (s *Builder) Audit(ctx context.Context, req *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	dir := s.Local("code")
	res, err := audit.Python(ctx, dir, req.IncludeOutdated)
	if err != nil {
		return s.Base.Builder.AuditError(err)
	}
	return s.Base.Builder.AuditResponse(res.Findings, res.Outdated, res.Tool, res.Language)
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
type Parameters struct{}

// Deploy renders and applies k8s manifests.
func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	s.Base.Builder.LogDeployRequest(req, s.Wool.Debug)
	s.EnvironmentVariables.SetRunning()

	k, err := s.Base.Builder.KubernetesDeploymentRequest(ctx, req)
	if err != nil {
		return s.Base.Builder.DeployError(err)
	}
	if err := s.EnvironmentVariables.AddConfigurations(ctx, req.Configuration); err != nil {
		return s.Base.Builder.DeployError(err)
	}
	if err := s.EnvironmentVariables.AddConfigurations(ctx, req.DependenciesConfigurations...); err != nil {
		return s.Base.Builder.DeployError(err)
	}

	confs, err := s.EnvironmentVariables.Configurations()
	if err != nil {
		return s.Base.Builder.DeployError(err)
	}
	cm, err := services.EnvsAsConfigMapData(confs...)
	if err != nil {
		return s.Base.Builder.DeployError(err)
	}
	secrets, err := services.EnvsAsSecretData(s.EnvironmentVariables.Secrets()...)
	if err != nil {
		return s.Base.Builder.DeployError(err)
	}
	params := services.DeploymentParameters{
		ConfigMap:  cm,
		SecretMap:  secrets,
		Parameters: Parameters{},
	}
	_ = s.Base.Builder.KustomizeDeploy(ctx, req.Environment, k, deploymentFS, params)
	return s.Base.Builder.DeployResponse()
}

// Options returns the two-question set shown during `codefly add service`.
func (s *Builder) Options() []*agentv0.Question {
	return []*agentv0.Question{
		communicate.NewConfirm(&agentv0.Message{Name: PublicEndpoint, Message: "Expose API as public", Description: "is that directly accessible from the internet?"}, true),
		communicate.NewConfirm(&agentv0.Message{Name: HotReload, Message: "Code hot-reload (Recommended)?", Description: "codefly can restart your service when code changes are detected 🔎"}, true),
	}
}

// CreateConfiguration is the template context passed to factory templates.
type CreateConfiguration struct {
	*services.Information
	Image *resources.DockerImage
	Envs  []string
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

	create := CreateConfiguration{Information: s.Information, Envs: []string{}}
	if err := s.Base.Templates(ctx, create, services.WithFactory(factoryFS)); err != nil {
		return s.Base.Builder.CreateError(err)
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
	return nil
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
	return nil
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

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
