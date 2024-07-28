package main

import (
	"context"
	"embed"
	"github.com/codefly-dev/core/agents/communicate"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/templates"
	"github.com/codefly-dev/core/wool"
)

type Builder struct {
	*Service
}

func NewBuilder() *Builder {
	return &Builder{
		Service: NewService(),
	}
}
func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()

	err := s.Builder.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return nil, err
	}

	requirements.Localize(s.Location)

	s.sourceLocation = s.Local("code")

	if req.CreationMode != nil {
		s.Builder.CreationMode = req.CreationMode
		s.Builder.GettingStarted, err = templates.ApplyTemplateFrom(ctx, shared.Embed(factoryFS), "templates/factory/GETTING_STARTED.md", s.Information)
		if err != nil {
			return s.Builder.LoadError(err)
		}

		if req.CreationMode.Communicate {
			// communication on CreateResponse
			err = s.Communication.Register(ctx, communicate.New[builderv0.CreateRequest](s.createCommunicate()))
			if err != nil {
				return s.Builder.LoadError(err)
			}
		}
		return s.Builder.LoadResponse()
	}

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	s.RestEndpoint, err = resources.FindRestEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	return s.Builder.LoadResponse()
}

func (s *Builder) Init(ctx context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	defer s.Wool.Catch()

	s.Builder.LogInitRequest(req)

	//hash, err := requirements.Hash(ctx)
	//if err != nil {
	//	return s.Builder.InitError(err)
	//}

	return s.Builder.InitResponse()
}

func (s *Builder) Update(ctx context.Context, req *builderv0.UpdateRequest) (*builderv0.UpdateResponse, error) {
	defer s.Wool.Catch()

	err := s.Base.Templates(nil, services.WithBuilder(builderFS))
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot copy and apply template")
	}

	return &builderv0.UpdateResponse{}, nil
}

func (s *Builder) Sync(ctx context.Context, req *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
	defer s.Wool.Catch()

	return s.Builder.SyncResponse()
}

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

func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	defer s.Wool.Catch()
	dockerRequest, err := s.Builder.DockerBuildRequest(ctx, req)
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

	err = shared.DeleteFile(ctx, s.Local("builder/Dockerfile"))
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot remove dockerfile")
	}

	err = s.Templates(ctx, docker, services.WithBuilder(builderFS))
	if err != nil {
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
	_, err = builder.Build(ctx)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot build image")
	}

	s.Builder.WithDockerImages(image)
	return s.Builder.BuildResponse()
}

type Parameters struct {
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	s.Builder.LogDeployRequest(req, s.Wool.Debug)

	s.EnvironmentVariables.SetRunning()

	var k *builderv0.KubernetesDeployment
	var err error
	if k, err = s.Builder.KubernetesDeploymentRequest(ctx, req); err != nil {
		return s.Builder.DeployError(err)
	}

	err = s.EnvironmentVariables.AddConfigurations(ctx, req.Configuration)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	err = s.EnvironmentVariables.AddConfigurations(ctx, req.DependenciesConfigurations...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	cm, err := services.EnvsAsConfigMapData(s.EnvironmentVariables.Configurations()...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	secrets, err := services.EnvsAsSecretData(s.EnvironmentVariables.Secrets()...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	params := services.DeploymentParameters{
		ConfigMap:  cm,
		SecretMap:  secrets,
		Parameters: Parameters{},
	}

	err = s.Builder.KustomizeDeploy(ctx, req.Environment, k, deploymentFS, params)

	return s.Builder.DeployResponse()
}

func (s *Builder) Options() []*agentv0.Question {
	return []*agentv0.Question{communicate.NewConfirm(&agentv0.Message{Name: PublicEndpoint, Message: "Expose API as public", Description: "is that directly accessible from the internet?"}, true),
		communicate.NewConfirm(&agentv0.Message{Name: HotReload, Message: "Code hot-reload (Recommended)?", Description: "codefly can restart your service when code changes are detected 🔎"}, true),
	}
}

func (s *Builder) createCommunicate() *communicate.Sequence {
	return communicate.NewSequence(s.Options()...)
}

type CreateConfiguration struct {
	*services.Information
	Image *resources.DockerImage
	Envs  []string
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()

	ctx = s.Wool.Inject(ctx)

	if s.Builder.CreationMode.Communicate {
		session, err := s.Communication.Done(ctx, communicate.Channel[builderv0.CreateRequest]())
		if err != nil {
			return s.Builder.CreateError(err)
		}

		s.Settings.HotReload, err = session.Confirm(HotReload)
		if err != nil {
			return s.Builder.CreateError(err)
		}
		s.Settings.PublicEndpoint, err = session.Confirm(PublicEndpoint)
		if err != nil {
			return s.Builder.CreateError(err)
		}
	} else {
		var err error
		s.Settings.HotReload, err = communicate.GetDefaultConfirm(s.Options(), HotReload)
		if err != nil {
			return s.Builder.CreateError(err)
		}
		s.Settings.PublicEndpoint, err = communicate.GetDefaultConfirm(s.Options(), PublicEndpoint)
		if err != nil {
			return s.Builder.CreateError(err)
		}
	}

	create := CreateConfiguration{
		Information: s.Information,
		Envs:        []string{},
	}

	err := s.Templates(ctx, create, services.WithFactory(factoryFS))
	if err != nil {
		return s.Builder.CreateError(err)
	}

	// Create __init__.py
	_, err = shared.CheckDirectoryOrCreate(ctx, s.Local("code/src"))
	if err != nil {
		return s.Builder.CreateError(err)
	}
	_, err = shared.CheckDirectoryOrCreate(ctx, s.Local("code/tests"))
	if err != nil {
		return s.Builder.CreateError(err)
	}
	err = shared.CreateFile(ctx, s.Local("code/src/__init__.py"))
	if err != nil {
		return s.Builder.CreateError(err)
	}
	err = shared.CreateFile(ctx, s.Local("code/tests/__init__.py"))
	if err != nil {
		return s.Builder.CreateError(err)
	}
	err = s.CreateEndpoints(ctx)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create endpoints")
	}

	return s.Builder.CreateResponse(ctx, s.Settings)
}

func (s *Builder) CreateEndpoints(ctx context.Context) error {
	openapiFile := s.Local("openapi/api.json")
	var err error
	endpoint := s.Base.BaseEndpoint(standards.REST)
	if s.Settings.PublicEndpoint {
		endpoint.Visibility = resources.VisibilityPublic
	}
	rest, err := resources.LoadRestAPI(ctx, shared.Pointer(openapiFile))
	s.RestEndpoint, err = resources.NewAPI(ctx, endpoint, resources.ToRestAPI(rest))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create openapi api")
	}
	s.Endpoints = []*basev0.Endpoint{s.RestEndpoint}
	return nil
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
