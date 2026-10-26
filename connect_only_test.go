package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v3"
)

// A Connect-only service declares its gRPC server and the Connect face over it,
// and no HTTP endpoint: no REST face, no OpenAPI document, no probe listener.
// These tests hold the agent to loading, syncing and starting one.

// TestFindHTTPEndpoint pins which endpoint the app's HTTP listener serves.
func TestFindHTTPEndpoint(t *testing.T) {
	ctx := context.Background()
	rest := &basev0.Endpoint{Name: "rest", Api: standards.REST}
	http := &basev0.Endpoint{Name: "probes", Api: standards.HTTP}
	grpc := &basev0.Endpoint{Name: "grpc", Api: standards.GRPC}
	connect := &basev0.Endpoint{Name: "connect", Api: standards.CONNECT}

	for name, tc := range map[string]struct {
		endpoints []*basev0.Endpoint
		want      *basev0.Endpoint
		refused   string
	}{
		"gRPC and Connect alone": {endpoints: []*basev0.Endpoint{grpc, connect}},
		"REST":                   {endpoints: []*basev0.Endpoint{rest, grpc}, want: rest},
		"plain HTTP":             {endpoints: []*basev0.Endpoint{http, grpc, connect}, want: http},
		"two listeners":          {endpoints: []*basev0.Endpoint{rest, http}, refused: "declares 2 HTTP endpoints (probes, rest)"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := findHTTPEndpoint(ctx, tc.endpoints)
			if tc.refused != "" {
				require.ErrorContains(t, err, tc.refused)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// createConnectOnlyService scaffolds a gRPC service and rewrites its manifest
// into a Connect-only one: connect-endpoint on, and the gRPC and Connect
// endpoints in place of the scaffold's REST endpoint.
func createConnectOnlyService(t *testing.T) (*Builder, string) {
	t.Helper()
	builder, root := createServiceForTest(t, map[string]*agentv0.Answer{
		HotReload:      confirmAnswer(false),
		PublicEndpoint: confirmAnswer(false),
		GRPCServer:     confirmAnswer(true),
	})
	manifest := filepath.Join(root, "service.codefly.yaml")
	content, err := os.ReadFile(manifest)
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, yaml.Unmarshal(content, &document))
	document["spec"].(map[string]any)["connect-endpoint"] = true
	document["endpoints"] = []map[string]any{
		{"name": "grpc", "api": "grpc", "visibility": "private"},
		{"name": "connect", "api": "connect", "visibility": "private"},
	}
	content, err = yaml.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifest, content, 0o644))
	return builder, root
}

func localEnvironment(t *testing.T) *basev0.Environment {
	t.Helper()
	environment, err := resources.LocalEnvironment().Proto()
	require.NoError(t, err)
	return environment
}

func endpointNames(endpoints []*basev0.Endpoint) []string {
	var names []string
	for _, endpoint := range endpoints {
		names = append(names, endpoint.GetName())
	}
	slices.Sort(names)
	return names
}

// TestConnectOnlyServiceLoads is the builder half of the regression: Load
// refused every service without a REST endpoint ("no rest endpoint found").
// The runtime loads it too, and Deploy, which probes the HTTP listener, says
// that it has none rather than that the health it would read is missing.
func TestConnectOnlyServiceLoads(t *testing.T) {
	ctx := context.Background()
	created, _ := createConnectOnlyService(t)
	identity, err := created.Identity.Proto()
	require.NoError(t, err)

	builder := NewBuilder(NewService())
	loaded, err := builder.Load(ctx, &builderv0.LoadRequest{Identity: identity})
	require.NoError(t, err)
	require.Equal(t, builderv0.LoadStatus_READY, loaded.GetState().GetState(), loaded.GetState().GetMessage())
	require.Equal(t, []string{"connect", "grpc"}, endpointNames(loaded.GetEndpoints()))
	require.Nil(t, builder.FastAPI.HTTPEndpoint)
	require.True(t, builder.FastAPI.Settings.ConnectEndpoint)

	_, err = builder.Deploy(ctx, &builderv0.DeploymentRequest{})
	require.ErrorContains(t, err, "the service declares none")

	runtime := NewRuntime(NewService())
	t.Cleanup(runtime.Base.StopWatcher)
	resp, err := runtime.Load(ctx, &runtimev0.LoadRequest{Identity: identity, Environment: localEnvironment(t), DisableCatch: true})
	require.NoError(t, err)
	require.NotEqual(t, runtimev0.LoadStatus_ERROR, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())
	require.Nil(t, runtime.FastAPI.HTTPEndpoint)
	require.Equal(t, "grpc", runtime.FastAPI.GRPCEndpoint.GetName())
	require.Equal(t, "connect", runtime.FastAPI.ConnectEndpoint.GetName())
}

// TestConnectOnlyServiceSyncs runs the builder's Sync on a loaded Connect-only
// service: the gRPC bindings and the Connect face are generated with no REST
// endpoint in sight. The bindings come from Buf in its companion container.
func TestConnectOnlyServiceSyncs(t *testing.T) {
	if testing.Short() {
		t.Skip("generates the gRPC bindings with Buf in a container")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("docker is not available, Buf cannot run: %v", err)
	}
	ownTestContainers(t)
	ctx := context.Background()
	created, root := createConnectOnlyService(t)
	identity, err := created.Identity.Proto()
	require.NoError(t, err)

	builder := NewBuilder(NewService())
	loaded, err := builder.Load(ctx, &builderv0.LoadRequest{Identity: identity})
	require.NoError(t, err)
	require.Equal(t, builderv0.LoadStatus_READY, loaded.GetState().GetState(), loaded.GetState().GetMessage())

	synced, err := builder.Sync(ctx, &builderv0.SyncRequest{})
	require.NoError(t, err)
	require.NotEqual(t, builderv0.SyncStatus_ERROR, synced.GetState().GetState(), synced.GetState().GetMessage())
	require.FileExists(t, filepath.Join(root, connectFacePath))
	generated, err := filepath.Glob(filepath.Join(root, "code", "src", "rpc", "_generated", "*_pb2_grpc.py"))
	require.NoError(t, err)
	require.NotEmpty(t, generated, "Sync wrote no gRPC bindings")
}

// connectOnlyMappings are the proposed mappings of a Connect-only service: its
// gRPC and Connect endpoints, each on a free loopback port.
func connectOnlyMappings(t *testing.T, grpcEndpoint, connectEndpoint *basev0.Endpoint) ([]*basev0.NetworkMapping, uint16) {
	t.Helper()
	mapping := func(endpoint *basev0.Endpoint, address func(uint16) string) (*basev0.NetworkMapping, uint16) {
		port := freePort(t)
		return &basev0.NetworkMapping{Endpoint: endpoint, Instances: []*basev0.NetworkInstance{{
			Access: resources.NewNativeNetworkAccess(), Hostname: "localhost", Port: uint32(port), Address: address(port),
		}}}, port
	}
	grpcMapping, _ := mapping(grpcEndpoint, func(port uint16) string { return fmt.Sprintf("localhost:%d", port) })
	connectMapping, connectPort := mapping(connectEndpoint, func(port uint16) string { return fmt.Sprintf("http://localhost:%d", port) })
	return []*basev0.NetworkMapping{grpcMapping, connectMapping}, connectPort
}

// TestConnectOnlyStartPublishesNoHTTPListener: Init resolves no HTTP endpoint
// for a service that declares none, so Start binds the app's HTTP listener to a
// loopback port nobody is told about and waits on the Connect face.
func TestConnectOnlyStartPublishesNoHTTPListener(t *testing.T) {
	isolateWorkingDir(t)
	ctx := context.Background()
	runtime, req := initTestRuntime(t)
	env := &fakeRunnerEnvironment{}
	runtime.runnerEnvironment = env
	runtime.cacheLocation = t.TempDir()

	grpcEndpoint := &basev0.Endpoint{Module: "mod", Service: "probe", Name: "grpc", Api: standards.GRPC}
	connectEndpoint := &basev0.Endpoint{Module: "mod", Service: "probe", Name: "connect", Api: standards.CONNECT}
	runtime.FastAPI.HTTPEndpoint = nil
	runtime.FastAPI.GRPCEndpoint = grpcEndpoint
	runtime.FastAPI.ConnectEndpoint = connectEndpoint
	var connectPort uint16
	req.ProposedNetworkMappings, connectPort = connectOnlyMappings(t, grpcEndpoint, connectEndpoint)
	var asked []string
	runtime.answers = func(_ context.Context, target string) error {
		asked = append(asked, target)
		return nil
	}

	initialized, err := runtime.Init(ctx, req)
	require.NoError(t, err)
	require.Equal(t, runtimev0.InitStatus_READY, initialized.GetStatus().GetState(), initialized.GetStatus().GetMessage())

	started, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, started.GetStatus().GetState(), started.GetStatus().GetMessage())
	t.Cleanup(func() { _, _ = runtime.Stop(context.Background(), &runtimev0.StopRequest{}) })
	args := uvicornProc(t, env).args
	require.Equal(t, "0", uvicornPort(t, env))
	require.Equal(t, "127.0.0.1", args[slices.Index(args, "--host")+1])
	require.Equal(t, []string{fmt.Sprintf("http://localhost:%d/", connectPort)}, asked)
}

// TestServeTargetWithoutHTTPEndpoint: the Connect face answers HTTP, so it is
// what Start waits on; a gRPC server alone is waited on for its handshake.
func TestServeTargetWithoutHTTPEndpoint(t *testing.T) {
	runtime := NewRuntime(NewService())
	runtime.grpcAddress = "localhost:9090"
	runtime.connectAddress = "http://localhost:8081"
	name, target, _ := runtime.serveTarget()
	require.Equal(t, "Connect face", name)
	require.Equal(t, "http://localhost:8081/", target)

	runtime.connectAddress = ""
	name, target, _ = runtime.serveTarget()
	require.Equal(t, "gRPC server", name)
	require.Equal(t, "localhost:9090", target)

	runtime.grpcAddress = ""
	_, target, _ = runtime.serveTarget()
	require.Empty(t, target, "nothing to wait on")
}

// TestGRPCAnswersNeedsAHandshake: a gRPC server answers; a listener that
// accepts and hangs up, as an engine's proxy does before anything listens
// behind it, does not.
func TestGRPCAnswersNeedsAHandshake(t *testing.T) {
	ctx := context.Background()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	require.NoError(t, grpcAnswers(ctx, listener.Addr().String()))
	require.NoError(t, grpcAnswers(ctx, "http://"+listener.Addr().String()), "a scheme in the address is not part of the target")

	hangup, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = hangup.Close() })
	go func() {
		for {
			conn, err := hangup.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	require.Error(t, grpcAnswers(ctx, hangup.Addr().String()))
}

// generateScaffoldStubs writes the gRPC bindings Sync generates with Buf the
// way the scaffold's own tests/rpc/conftest.py does when run directly: same
// plugins, no network plugin registry.
func generateScaffoldStubs(t *testing.T, code string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(code, "src", "rpc", "_generated"), 0o755))
	protoc := exec.Command("uv", "run", "python", "-m", "grpc_tools.protoc",
		"-I../proto", "--python_out=src/rpc/_generated", "--grpc_python_out=src/rpc/_generated", "../proto/api.proto")
	protoc.Dir = code
	out, err := protoc.CombinedOutput()
	require.NoError(t, err, "%s", out)
}

// TestConnectOnlyServiceStarts drives the runtime through Load, Init and Start
// on a scaffolded Connect-only service with a real uv: STARTED arrives, and
// the face it waited on answers a Connect call.
func TestConnectOnlyServiceStarts(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a scaffolded service with uv")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv is not installed: the scaffolded service cannot run")
	}
	ctx := context.Background()
	created, root := createConnectOnlyService(t)
	code := filepath.Join(root, "code")
	identity, err := created.Identity.Proto()
	require.NoError(t, err)
	// The face Sync writes, from the loaded manifest's settings.
	builder := NewBuilder(NewService())
	_, err = builder.Load(ctx, &builderv0.LoadRequest{Identity: identity})
	require.NoError(t, err)
	require.NoError(t, builder.syncConnectFace())
	generateScaffoldStubs(t, code)

	runtime := NewRuntime(NewService())
	t.Cleanup(func() { _, _ = runtime.Destroy(context.Background(), &runtimev0.DestroyRequest{}) })
	loaded, err := runtime.Load(ctx, &runtimev0.LoadRequest{Identity: identity, Environment: localEnvironment(t), DisableCatch: true})
	require.NoError(t, err)
	require.NotEqual(t, runtimev0.LoadStatus_ERROR, loaded.GetStatus().GetState(), loaded.GetStatus().GetMessage())

	mappings, connectPort := connectOnlyMappings(t, runtime.FastAPI.GRPCEndpoint, runtime.FastAPI.ConnectEndpoint)
	initialized, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext: resources.NewRuntimeContextNative(), ProposedNetworkMappings: mappings,
	})
	require.NoError(t, err)
	require.Equal(t, runtimev0.InitStatus_READY, initialized.GetStatus().GetState(), initialized.GetStatus().GetMessage())

	started, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, started.GetStatus().GetState(), started.GetStatus().GetMessage())

	request, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/api.EchoService/Echo", connectPort),
		strings.NewReader(`{"message":"connect only"}`))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, "%s", body)
	require.JSONEq(t, `{"message":"connect only"}`, string(body))

	_, err = runtime.Stop(ctx, &runtimev0.StopRequest{})
	require.NoError(t, err)
}

// rewriteManifest applies edit to a scaffolded service's manifest.
func rewriteManifest(t *testing.T, root string, edit func(document map[string]any)) {
	t.Helper()
	manifest := filepath.Join(root, "service.codefly.yaml")
	content, err := os.ReadFile(manifest)
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, yaml.Unmarshal(content, &document))
	edit(document)
	content, err = yaml.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifest, content, 0o644))
}

// TestUnservedServiceIsRefusedAtLoad: a manifest that lost its endpoints, or
// whose gRPC server has no endpoint to serve, is refused by both Loads by name
// instead of starting a process nothing reaches.
func TestUnservedServiceIsRefusedAtLoad(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		answers map[string]*agentv0.Answer
	}{
		"no endpoints, no gRPC server": {answers: nil},
		"gRPC server with no gRPC endpoint": {answers: map[string]*agentv0.Answer{
			HotReload: confirmAnswer(false), PublicEndpoint: confirmAnswer(false), GRPCServer: confirmAnswer(true),
		}},
	} {
		t.Run(name, func(t *testing.T) {
			created, root := createServiceForTest(t, tc.answers)
			rewriteManifest(t, root, func(document map[string]any) { delete(document, "endpoints") })
			identity, err := created.Identity.Proto()
			require.NoError(t, err)

			loaded, err := NewBuilder(NewService()).Load(ctx, &builderv0.LoadRequest{Identity: identity})
			require.NoError(t, err)
			require.Equal(t, builderv0.LoadStatus_ERROR, loaded.GetState().GetState())
			require.Contains(t, loaded.GetState().GetMessage(), "nothing would serve this service")

			runtime := NewRuntime(NewService())
			t.Cleanup(runtime.Base.StopWatcher)
			resp, err := runtime.Load(ctx, &runtimev0.LoadRequest{Identity: identity, Environment: localEnvironment(t), DisableCatch: true})
			require.NoError(t, err)
			require.Equal(t, runtimev0.LoadStatus_ERROR, resp.GetStatus().GetState())
			require.Contains(t, resp.GetStatus().GetMessage(), "nothing would serve this service")
		})
	}
}

// grpcOnlyInit stands a runtime up at Init for a service whose one face is its
// gRPC server, with the proposed mapping Init resolves it from.
func grpcOnlyInit(t *testing.T) (*Runtime, *fakeRunnerEnvironment, *runtimev0.InitRequest, string) {
	t.Helper()
	runtime, req := initTestRuntime(t)
	env := &fakeRunnerEnvironment{}
	runtime.runnerEnvironment = env
	runtime.cacheLocation = t.TempDir()
	grpcEndpoint := &basev0.Endpoint{Module: "mod", Service: "probe", Name: "grpc", Api: standards.GRPC}
	connectEndpoint := &basev0.Endpoint{Module: "mod", Service: "probe", Name: "connect", Api: standards.CONNECT}
	runtime.FastAPI.HTTPEndpoint = nil
	runtime.FastAPI.GRPCEndpoint = grpcEndpoint
	mappings, _ := connectOnlyMappings(t, grpcEndpoint, connectEndpoint)
	req.ProposedNetworkMappings = mappings[:1]
	return runtime, env, req, mappings[0].GetInstances()[0].GetAddress()
}

// TestGRPCOnlyStartWaitsOnTheGRPCServer: with neither an HTTP endpoint nor a
// Connect face, Start's readiness is the gRPC server's handshake at the address
// Init resolved for it.
func TestGRPCOnlyStartWaitsOnTheGRPCServer(t *testing.T) {
	isolateWorkingDir(t)
	ctx := context.Background()
	runtime, _, req, grpcAddress := grpcOnlyInit(t)
	var asked []string
	runtime.answers = func(_ context.Context, target string) error {
		asked = append(asked, target)
		return nil
	}
	initialized, err := runtime.Init(ctx, req)
	require.NoError(t, err)
	require.Equal(t, runtimev0.InitStatus_READY, initialized.GetStatus().GetState(), initialized.GetStatus().GetMessage())
	started, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, started.GetStatus().GetState(), started.GetStatus().GetMessage())
	t.Cleanup(func() { _, _ = runtime.Stop(context.Background(), &runtimev0.StopRequest{}) })
	require.Equal(t, []string{grpcAddress}, asked)
}

// TestInitDropsTheListenersAReloadRemoved: an Init after a reload that dropped
// the Connect endpoint publishes no Connect port or address, so Start neither
// hands the process the old port nor waits on the old face.
func TestInitDropsTheListenersAReloadRemoved(t *testing.T) {
	isolateWorkingDir(t)
	ctx := context.Background()
	runtime, env, req, grpcAddress := grpcOnlyInit(t)
	connectEndpoint := &basev0.Endpoint{Module: "mod", Service: "probe", Name: "connect", Api: standards.CONNECT}
	runtime.FastAPI.ConnectEndpoint = connectEndpoint
	withConnect, _ := connectOnlyMappings(t, runtime.FastAPI.GRPCEndpoint, connectEndpoint)
	req.ProposedNetworkMappings = append(req.ProposedNetworkMappings, withConnect[1])
	initialized, err := runtime.Init(ctx, req)
	require.NoError(t, err)
	require.Equal(t, runtimev0.InitStatus_READY, initialized.GetStatus().GetState(), initialized.GetStatus().GetMessage())
	require.NotEmpty(t, runtime.connectAddress)

	runtime.FastAPI.ConnectEndpoint = nil
	req.ProposedNetworkMappings = req.ProposedNetworkMappings[:1]
	initialized, err = runtime.Init(ctx, req)
	require.NoError(t, err)
	require.Equal(t, runtimev0.InitStatus_READY, initialized.GetStatus().GetState(), initialized.GetStatus().GetMessage())

	var asked []string
	runtime.answers = func(_ context.Context, target string) error {
		asked = append(asked, target)
		return nil
	}
	started, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, started.GetStatus().GetState(), started.GetStatus().GetMessage())
	t.Cleanup(func() { _, _ = runtime.Stop(context.Background(), &runtimev0.StopRequest{}) })
	require.Equal(t, []string{grpcAddress}, asked)
	require.Empty(t, processEnv(uvicornProc(t, env), "CODEFLY_CONNECT_PORT"))
}

// TestImageServesNoHTTPWithoutAnHTTPEndpoint: the image of a service with no
// HTTP endpoint binds uvicorn to loopback, never 0.0.0.0:8080.
func TestImageServesNoHTTPWithoutAnHTTPEndpoint(t *testing.T) {
	builder, _ := createdBuilder(t)
	builder.FastAPI.HTTPEndpoint = nil
	outputDir := t.TempDir()
	dockerfile := dockerfileOf(t, outputDir, emitRecipe(t, builder, outputDir))
	require.Contains(t, dockerfile, `CMD ["uvicorn", "src.main:app", "--host", "127.0.0.1", "--port", "0", "--log-config", "uvicorn-log-config.json"]`)
	require.NotContains(t, dockerfile, "0.0.0.0")
	require.NotContains(t, dockerfile, "8080")
}
