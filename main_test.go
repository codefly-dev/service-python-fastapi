package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/codefly-dev/core/agents"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"

	"github.com/codefly-dev/core/resources"

	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
)

func TestCreateToRunNative(t *testing.T) {
	if languages.HasPythonPoetryRuntime(nil) {
		testCreateToRun(t, resources.NewRuntimeContextNative())
	}
}

func TestCreateToRunDocker(t *testing.T) {
	testCreateToRun(t, resources.NewRuntimeContextContainer())
}

func testCreateToRun(t *testing.T, runtimeContext *basev0.RuntimeContext) {
	wool.SetGlobalLogLevel(wool.DEBUG)
	agents.LogToConsole()

	workspace := &resources.Workspace{Name: "test"}
	ctx := context.Background()
	tmpDir, err := os.MkdirTemp("testdata", runtimeContext.Kind)
	tmpDir = shared.MustSolvePath(tmpDir)
	require.NoError(t, err)
	defer func(path string) {
		err = os.RemoveAll(tmpDir)
		require.NoError(t, err)
	}(tmpDir)

	serviceName := fmt.Sprintf("svc-%v", time.Now().UnixMilli())
	service := resources.Service{Name: serviceName, Module: "mod", Version: "0.0.0"}
	err = service.SaveAtDir(ctx, tmpDir)
	require.NoError(t, err)

	identity := &basev0.ServiceIdentity{
		Name:      service.Name,
		Version:   service.Version,
		Module:    service.Module,
		Workspace: workspace.Name,
		Location:  tmpDir,
	}

	builder := NewBuilder()

	resp, err := builder.Load(ctx, &builderv0.LoadRequest{Identity: identity, CreationMode: &builderv0.CreationMode{Communicate: false}})
	require.NoError(t, err)
	require.NotNil(t, resp)

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	// Now run it

	// Create temporary network mappings
	networkManager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	networkManager.WithTemporaryPorts()

	runtime := NewRuntime()

	defer func() {
		_, err = runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
		require.NoError(t, err)
	}()

	env := resources.LocalEnvironment()

	_, err = runtime.Load(ctx, &runtimev0.LoadRequest{
		Identity:     identity,
		Environment:  shared.Must(env.Proto()),
		DisableCatch: true})
	require.NoError(t, err)

	require.Equal(t, 1, len(runtime.Endpoints))

	networkMappings, err := networkManager.GenerateNetworkMappings(ctx, env, workspace, runtime.Base.Service, runtime.Endpoints)
	require.NoError(t, err)
	require.Equal(t, 1, len(networkMappings))

	restRoutes, err := resources.ExtractRestRoutes(ctx, networkMappings, resources.NewPublicNetworkAccess())
	require.NoError(t, err)
	require.Equal(t, 1, len(restRoutes))

	testRun(t, runtime, ctx, identity, runtimeContext, networkMappings)

	_, err = runtime.Stop(ctx, &runtimev0.StopRequest{})
	require.NoError(t, err)

	// Running again should work
	testRun(t, runtime, ctx, identity, runtimeContext, networkMappings)

	// Test
	test, err := runtime.Test(ctx, &runtimev0.TestRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.TestStatus_SUCCESS, test.Status.State)

}

func testRun(t *testing.T, runtime *Runtime, ctx context.Context, identity *basev0.ServiceIdentity, runtimeContext *basev0.RuntimeContext, networkMappings []*basev0.NetworkMapping) {

	init, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          runtimeContext,
		ProposedNetworkMappings: networkMappings})
	require.NoError(t, err)
	require.NotNil(t, init)

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, init.NetworkMappings, runtime.RestEndpoint, resources.NewNativeNetworkAccess())
	require.NoError(t, err)

	_, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)

	client := http.Client{Timeout: 200 * time.Millisecond}
	// Loop and wait for 1 seconds up to do a HTTP request to localhost with /version path
	tries := 0
	for {
		if tries > 10 {
			t.Fatal("too many tries")
		}
		time.Sleep(time.Second)

		// HTTP
		response, err := client.Get(fmt.Sprintf("%s/version", instance.Address))
		if err != nil {
			tries++
			continue
		}

		defer response.Body.Close()

		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)

		var data map[string]interface{}
		err = json.Unmarshal(body, &data)
		require.NoError(t, err)

		version, ok := data["version"].(string)
		require.True(t, ok)
		require.Equal(t, identity.Version, version)
		break
	}
}
