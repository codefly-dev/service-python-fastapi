package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// createdBuilder loads and creates a fastapi service in a fresh temp directory
// and returns the builder together with its service location.
func createdBuilder(t *testing.T) (*Builder, string) {
	t.Helper()
	ctx := context.Background()

	tmpDir, err := os.MkdirTemp("testdata", "recipe")
	require.NoError(t, err)
	tmpDir = shared.MustSolvePath(tmpDir)
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	service := resources.Service{Name: fmt.Sprintf("svc-%v", time.Now().UnixMilli()), Version: "0.0.0"}
	require.NoError(t, service.SaveAtDir(ctx, path.Join(tmpDir, fmt.Sprintf("mod/%s", service.Name))))
	service.WithModule("mod")

	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Version:             service.Version,
		Module:              "mod",
		Workspace:           "test",
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: fmt.Sprintf("mod/%s", service.Name),
	}

	builder := NewBuilder(NewService())
	_, err = builder.Load(ctx, &builderv0.LoadRequest{Identity: identity, CreationMode: &builderv0.CreationMode{Communicate: false}})
	require.NoError(t, err)
	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	return builder, builder.Local("builder")
}

func recipeBuildRequest(outputDir string) *builderv0.BuildRequest {
	return &builderv0.BuildRequest{
		BuildContext: &builderv0.BuildContext{
			Kind: &builderv0.BuildContext_DockerBuildContext{
				DockerBuildContext: &builderv0.DockerBuildContext{DockerRepository: "example.com/test"},
			},
		},
		OutputDirectory: outputDir,
	}
}

func recipeClient(t *testing.T, builder *Builder) *services.BuilderAgent {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	builderv0.RegisterBuilderServer(server, builder)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	return services.NewBuilderAgentClient(conn)
}

func TestBuildEmitsRecipePlan(t *testing.T) {
	for _, selection := range []string{"default", "explicit", "cache-selected"} {
		t.Run(selection, func(t *testing.T) {
			builder, conventionalDir := createdBuilder(t)
			outputDir := t.TempDir()
			client := recipeClient(t, builder)
			t.Setenv("PATH", t.TempDir())
			for _, executable := range []string{"docker", "buildx"} {
				_, err := exec.LookPath(executable)
				require.Error(t, err)
			}
			req := recipeBuildRequest(outputDir)
			docker := req.GetBuildContext().GetDockerBuildContext()
			if selection != "default" {
				docker.BuildxBuilder = "cli-selected"
			}
			if selection == "cache-selected" {
				docker.Cache = &builderv0.BuildCacheOptions{Backend: "registry", Scope: "service", Imports: []string{"example.com/test/cache"}, Exports: []string{"example.com/test/cache"}}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			capabilities, err := client.BuildCapabilities(ctx, &builderv0.BuildCapabilitiesRequest{})
			require.NoError(t, err)
			require.True(t, capabilities.GetBuildxSelection())
			resp, err := client.Build(ctx, req)
			require.NoError(t, err)
			require.Equal(t, builderv0.BuildStatus_SUCCESS, resp.GetState().GetState(), resp.GetState().GetMessage())
			require.Empty(t, resp.GetBuildxBuilder())
			require.Empty(t, resp.GetCacheContractVersion())
			plan := resp.GetResult().GetDockerBuildPlan()
			require.NotNil(t, plan)
			require.Nil(t, resp.GetResult().GetDockerBuildResult())
			require.Len(t, plan.GetRecipes(), 1)
			recipe := plan.GetRecipes()[0]
			require.Equal(t, "Dockerfile", recipe.GetDockerfile())
			require.Equal(t, ".", recipe.GetContext())
			require.Equal(t, []string{"linux/amd64", "linux/arm64"}, recipe.GetPlatforms())
			require.NotEmpty(t, recipe.GetImage())
			require.NoError(t, services.VerifyDockerBuildPlan(outputDir, plan))
			require.NoFileExists(t, path.Join(conventionalDir, "Dockerfile"))
			content, err := os.ReadFile(path.Join(outputDir, recipe.GetDockerfile()))
			require.NoError(t, err)
			require.Contains(t, string(content), "COPY code/pyproject.toml code/uv.lock ./")
			require.Contains(t, string(content), "COPY --chown=appuser code/src code/src")
			require.NoError(t, os.WriteFile(path.Join(outputDir, "Dockerfile"), []byte("tampered"), 0600))
			require.Error(t, services.VerifyDockerBuildPlan(outputDir, plan))
		})
	}
}

func TestBuildRejectsInvalidOutputDirectoryBeforePreparation(t *testing.T) {
	for _, outputDir := range []string{"", "relative"} {
		t.Run(fmt.Sprintf("output=%q", outputDir), func(t *testing.T) {
			builder, conventionalDir := createdBuilder(t)
			require.NoError(t, os.MkdirAll(conventionalDir, 0755))
			dockerfile := path.Join(conventionalDir, "Dockerfile")
			require.NoError(t, os.WriteFile(dockerfile, []byte("untouched"), 0600))
			client := recipeClient(t, builder)
			t.Setenv("PATH", t.TempDir())
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			req := recipeBuildRequest(outputDir)
			req.GetBuildContext().GetDockerBuildContext().BuildxBuilder = "cli-selected"
			resp, err := client.Build(ctx, req)
			require.NoError(t, err)
			require.Equal(t, builderv0.BuildStatus_ERROR, resp.GetState().GetState())
			require.Contains(t, resp.GetState().GetMessage(), "output_directory")
			require.Nil(t, resp.GetResult())
			content, err := os.ReadFile(dockerfile)
			require.NoError(t, err)
			require.Equal(t, "untouched", string(content))
		})
	}
}
