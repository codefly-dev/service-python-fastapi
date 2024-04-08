package main

import (
	"context"
	"embed"
	"github.com/codefly-dev/core/agents/communicate"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/configurations/standards"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
	"github.com/codefly-dev/core/shared"
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

	err := s.Base.Builder.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return nil, err
	}

	requirements.Localize(s.Location)

	s.sourceLocation = s.Local("src")

	// communication on CreateResponse
	err = s.Communication.Register(ctx, communicate.New[builderv0.CreateRequest](createCommunicate()))
	if err != nil {
		return s.Builder.LoadError(err)
	}

	s.Builder.GettingStarted, err = templates.ApplyTemplateFrom(ctx, shared.Embed(factoryFS), "templates/factory/GETTING_STARTED.md", s.Information)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	if req.AtCreate {
		return s.Builder.LoadResponse()
	}

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	s.restEndpoint, err = configurations.FindRestEndpoint(ctx, s.Endpoints)
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
	ctx = s.WoolAgent.Inject(ctx)

	docker := DockerTemplating{
		Builder:         runtimeImage.FullName(),
		Components:      requirements.All(),
		RuntimePackages: s.Settings.RuntimePackages,
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

type LoadBalancer struct {
	Enabled bool
	Host    string
}

type Parameters struct {
	LoadBalancer
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	s.Builder.LogDeployRequest(req, s.Wool.Debug)

	s.EnvironmentVariables.SetRunning(true)

	var k *builderv0.KubernetesDeployment
	var err error
	if k, err = s.Builder.KubernetesDeploymentRequest(ctx, req); err != nil {
		return s.Builder.DeployError(err)
	}

	err = s.EnvironmentVariables.AddConfigurations(req.Configuration)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	err = s.EnvironmentVariables.AddConfigurations(req.DependenciesConfigurations...)
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
		Parameters: Parameters{LoadBalancer{}},
	}
	if req.Deployment.LoadBalancer {
		inst, err := configurations.FindNetworkInstance(ctx, req.NetworkMappings, s.restEndpoint, basev0.NetworkScope_Public)
		if err != nil {
			return s.Builder.DeployError(err)
		}

		params.Parameters = Parameters{LoadBalancer{Host: inst.Host, Enabled: true}}
	}

	err = s.Builder.KustomizeDeploy(ctx, req.Environment, k, deploymentFS, params)

	return s.Builder.DeployResponse()
}

const Watch = "with-hot-reload"

func createCommunicate() *communicate.Sequence {
	return communicate.NewSequence(
		communicate.NewConfirm(&agentv0.Message{Name: Watch, Message: "Code hot-reload (Recommended)?", Description: "codefly can restart your service when code changes are detected 🔎"}, true),
	)
}

type CreateConfiguration struct {
	*services.Information
	Image *configurations.DockerImage
	Envs  []string
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()

	ctx = s.WoolAgent.Inject(ctx)

	session, err := s.Communication.Done(ctx, communicate.Channel[builderv0.CreateRequest]())
	if err != nil {
		return s.Builder.CreateError(err)
	}

	s.Settings.Watch, err = session.Confirm(Watch)
	if err != nil {
		return s.Builder.CreateError(err)
	}

	create := CreateConfiguration{
		Information: s.Information,
		Envs:        []string{},
	}
	err = s.Templates(ctx, create, services.WithFactory(factoryFS))
	if err != nil {
		return s.Base.Builder.CreateError(err)
	}

	err = s.CreateEndpoints(ctx)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create endpoints")
	}

	return s.Base.Builder.CreateResponse(ctx, s.Settings)
}

func (s *Builder) CreateEndpoints(ctx context.Context) error {
	openapiFile := s.Local("openapi/api.json")
	var err error
	endpoint := s.Base.Service.BaseEndpoint(standards.REST)
	rest, err := configurations.LoadRestAPI(ctx, openapiFile)
	s.restEndpoint, err = configurations.NewAPI(ctx, endpoint, configurations.ToRestAPI(rest))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create openapi api")
	}
	s.Endpoints = []*basev0.Endpoint{s.restEndpoint}
	return nil
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
