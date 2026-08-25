package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	agenttesting "github.com/codefly-dev/core/agents/testing"
)

// TestSettingsParseGRPCServer proves the nested grpc-server block round-trips
// through the flat inline-embedded Settings and defaults to disabled.
func TestSettingsParseGRPCServer(t *testing.T) {
	var enabled Settings
	require.NoError(t, yaml.Unmarshal([]byte(`
python-version: "3.12"
grpc-server:
  enabled: true
  proto: proto/api.proto
`), &enabled))
	require.True(t, enabled.GRPCServer.Enabled)
	require.Equal(t, "proto/api.proto", enabled.GRPCServer.Proto)

	var absent Settings
	require.NoError(t, yaml.Unmarshal([]byte(`python-version: "3.12"`), &absent))
	require.False(t, absent.GRPCServer.Enabled)
}

func confirmAnswer(value bool) *agentv0.Answer {
	return &agentv0.Answer{Value: &agentv0.Answer_Confirm{Confirm: &agentv0.ConfirmAnswer{Confirmed: value}}}
}

// createServiceForTest drives Load + Create in a temp workspace, mirroring the
// setup in main_test but stopping before Runtime so it needs no docker/network.
func createServiceForTest(t *testing.T, answers map[string]*agentv0.Answer) (*Builder, string) {
	t.Helper()
	ctx := context.Background()
	tmpDir, err := os.MkdirTemp("testdata", "grpc")
	require.NoError(t, err)
	tmpDir = shared.MustSolvePath(tmpDir)
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	serviceName := fmt.Sprintf("svc-%v", time.Now().UnixMilli())
	service := resources.Service{Name: serviceName, Version: "0.0.0"}
	require.NoError(t, service.SaveAtDir(ctx, path.Join(tmpDir, fmt.Sprintf("mod/%s", service.Name))))

	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Version:             service.Version,
		Module:              "mod",
		Workspace:           "test",
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: fmt.Sprintf("mod/%s", service.Name),
	}

	svc := NewService()
	builder := NewBuilder(svc)

	communicate := answers != nil
	_, err = builder.Load(ctx, &builderv0.LoadRequest{Identity: identity, CreationMode: &builderv0.CreationMode{Communicate: communicate}})
	require.NoError(t, err)
	if communicate {
		builder.answers = answers
	}

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)
	return builder, path.Join(tmpDir, fmt.Sprintf("mod/%s", service.Name))
}

// TestCreateRESTOnlyByDefault pins the untouched REST-only layout: one
// endpoint, no proto tree, and no gRPC dependencies or boot code leaking into
// the scaffold.
func TestCreateRESTOnlyByDefault(t *testing.T) {
	builder, root := createServiceForTest(t, nil)

	require.False(t, builder.FastAPI.Settings.GRPCServer.Enabled)
	require.Equal(t, 1, len(builder.Endpoints))
	require.Nil(t, builder.FastAPI.GRPCEndpoint)

	_, err := os.Stat(filepath.Join(root, "code/proto/api.proto"))
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(root, "code/src/rpc/server.py"))
	require.True(t, os.IsNotExist(err))

	pyproject, err := os.ReadFile(filepath.Join(root, "code/pyproject.toml"))
	require.NoError(t, err)
	require.NotContains(t, string(pyproject), "grpcio")

	main, err := os.ReadFile(filepath.Join(root, "code/src/main.py"))
	require.NoError(t, err)
	require.NotContains(t, string(main), "CODEFLY_GRPC_PORT")
}

// TestCreateWithGRPCServer proves opting in scaffolds the proto contract, the
// server/servicer seam, the dependencies and boot code, and advertises a
// second gRPC endpoint alongside REST.
func TestCreateWithGRPCServer(t *testing.T) {
	builder, root := createServiceForTest(t, map[string]*agentv0.Answer{
		HotReload:      confirmAnswer(false),
		PublicEndpoint: confirmAnswer(false),
		GRPCServer:     confirmAnswer(true),
	})

	require.True(t, builder.FastAPI.Settings.GRPCServer.Enabled)
	require.Equal(t, defaultProtoPath, builder.FastAPI.Settings.GRPCServer.Proto)

	require.Equal(t, 2, len(builder.Endpoints))
	require.NotNil(t, builder.FastAPI.GRPCEndpoint)
	require.Equal(t, standards.GRPC, builder.FastAPI.GRPCEndpoint.Api)

	for _, rel := range []string{
		"code/proto/api.proto",
		"code/proto/buf.gen.yaml",
		"code/src/rpc/server.py",
		"code/src/rpc/servicer.py",
		"code/tests/rpc/test_grpc.py",
		"code/.gitignore",
	} {
		_, err := os.Stat(filepath.Join(root, rel))
		require.NoError(t, err, "expected scaffolded file %s", rel)
	}

	pyproject, err := os.ReadFile(filepath.Join(root, "code/pyproject.toml"))
	require.NoError(t, err)
	require.Contains(t, string(pyproject), "grpcio")
	require.Contains(t, string(pyproject), "grpcio-tools")

	main, err := os.ReadFile(filepath.Join(root, "code/src/main.py"))
	require.NoError(t, err)
	require.Contains(t, string(main), "CODEFLY_GRPC_PORT")
	require.Contains(t, string(main), "from src.rpc.server import serve")
}

// TestDeploymentRendersGRPCPort proves the gRPC container/service ports appear
// only when the deployment parameters opt in.
func TestDeploymentRendersGRPCPort(t *testing.T) {
	enabled := agenttesting.AssertKustomizeTemplates(t, deploymentFS, Parameters{GRPCEnabled: true, GRPCPort: 9090})
	svc, err := os.ReadFile(filepath.Join(enabled, "base", "service.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(svc), "grpc-port")
	require.Contains(t, string(svc), "9090")

	deployment, err := os.ReadFile(filepath.Join(enabled, "base", "deployment.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(deployment), "containerPort: 9090")

	disabled := agenttesting.AssertKustomizeTemplates(t, deploymentFS, Parameters{})
	svc, err = os.ReadFile(filepath.Join(disabled, "base", "service.yaml"))
	require.NoError(t, err)
	require.NotContains(t, string(svc), "grpc-port")
}
