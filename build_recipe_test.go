package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/stretchr/testify/require"
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

// TestBuildEmitsRecipePlan proves that when the CLI provides an output
// directory, Build renders the Dockerfile there and returns a DockerBuildPlan
// (not the legacy DockerBuildResult) whose recipe the CLI executor can consume:
// the Dockerfile is resolved relative to the output directory, the context to
// the service directory, and the image targets a multi-arch manifest list.
func TestBuildEmitsRecipePlan(t *testing.T) {
	builder, outputDir := createdBuilder(t)

	resp, err := builder.Build(context.Background(), recipeBuildRequest(outputDir))
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_SUCCESS, resp.GetState().GetState(), resp.GetState().GetMessage())

	plan := resp.GetResult().GetDockerBuildPlan()
	require.NotNil(t, plan, "recipe path must emit a DockerBuildPlan")
	require.Nil(t, resp.GetResult().GetDockerBuildResult(), "recipe path must not emit a legacy DockerBuildResult")

	require.Len(t, plan.GetRecipes(), 1)
	recipe := plan.GetRecipes()[0]
	require.Equal(t, "Dockerfile", recipe.GetDockerfile())
	require.Equal(t, ".", recipe.GetContext())
	require.Equal(t, []string{"linux/amd64", "linux/arm64"}, recipe.GetPlatforms())
	require.NotEmpty(t, recipe.GetImage())

	// The plan must verify against the on-disk tree exactly as the CLI verifies
	// it before running docker buildx.
	require.NoError(t, services.VerifyDockerBuildPlan(outputDir, plan))

	// The Dockerfile the CLI builds must actually exist in the emitted tree.
	_, err = os.Stat(path.Join(outputDir, recipe.GetDockerfile()))
	require.NoError(t, err)
}
