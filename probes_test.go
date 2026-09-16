package main

import (
	"encoding/json"
	"encoding/pem"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func healthEndpoint(t *testing.T, secured bool, health *resources.Health) *basev0.Endpoint {
	t.Helper()
	endpoint, err := (&resources.Endpoint{Name: "http", Service: "worker", Module: "example", Visibility: "private", API: "rest", Secured: secured, Health: health}).Proto()
	require.NoError(t, err)
	return endpoint
}

func renderedProbe(t *testing.T, endpoint *basev0.Endpoint) map[string]any {
	t.Helper()
	probes, err := deploymentProbes(endpoint)
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(probes), &result))
	return result
}

func TestDeploymentUsesDeclaredHTTPHealthAndIndependentIntents(t *testing.T) {
	endpoint := healthEndpoint(t, true, &resources.Health{Readiness: &resources.Probe{
		Kind: "http", Path: "/readyz", Statuses: []string{"200"}, BodyContains: "ready",
		InitialDelay: "2s", Period: "3s", Timeout: "4s", FailureThreshold: 5, SuccessThreshold: 2,
	}})
	container := renderedProbe(t, endpoint)
	require.NotContains(t, container, "startupProbe")
	require.NotContains(t, container, "livenessProbe")
	probe := container["readinessProbe"].(map[string]any)
	require.EqualValues(t, 2, probe["initialDelaySeconds"])
	require.EqualValues(t, 3, probe["periodSeconds"])
	require.EqualValues(t, 4, probe["timeoutSeconds"])
	require.EqualValues(t, 5, probe["failureThreshold"])
	require.EqualValues(t, 2, probe["successThreshold"])
	command := probe["exec"].(map[string]any)["command"].([]any)
	var config localProbe
	require.NoError(t, json.Unmarshal([]byte(command[4].(string)), &config))
	require.True(t, config.Secured)
	require.Equal(t, "/readyz", config.Path)
	require.Equal(t, [][2]uint32{{200, 200}}, config.Statuses)
	require.Equal(t, "ready", config.Body)
}

func TestDeploymentLegacyReadinessOnlyConnects(t *testing.T) {
	container := renderedProbe(t, healthEndpoint(t, false, nil))
	require.NotContains(t, container, "startupProbe")
	require.NotContains(t, container, "livenessProbe")
	command := container["readinessProbe"].(map[string]any)["exec"].(map[string]any)["command"].([]any)
	var config localProbe
	require.NoError(t, json.Unmarshal([]byte(command[4].(string)), &config))
	require.Equal(t, "transport", config.Kind)
	require.Empty(t, config.Path)
}

func TestDeploymentRefusesUnrepresentableTimingAndPredicates(t *testing.T) {
	for _, probe := range []*resources.Probe{
		{Kind: "http", Path: "/readyz", Period: "1500ms"},
		{Kind: "agent"},
	} {
		_, err := deploymentProbes(healthEndpoint(t, false, &resources.Health{Readiness: probe}))
		require.Error(t, err)
	}
	_, err := deploymentProbes(healthEndpoint(t, false, &resources.Health{Liveness: &resources.Probe{Kind: "transport", SuccessThreshold: 2}}))
	require.Error(t, err)
}

func runLocalProbe(t *testing.T, config localProbe, target string, certFile string) error {
	t.Helper()
	endpoint, err := url.Parse(target)
	require.NoError(t, err)
	config.Port, err = strconv.Atoi(endpoint.Port())
	require.NoError(t, err)
	raw, err := json.Marshal(config)
	require.NoError(t, err)
	python, err := exec.LookPath("python3")
	require.NoError(t, err, "probe conformance requires Python")
	command := exec.Command(python, "-I", "-c", probeScript, string(raw))
	command.Env = append(os.Environ(), "UVICORN_SSL_CERTFILE="+certFile)
	return command.Run()
}

func TestActualProbePreservesHTTPPredicateAndDoesNotFollowRedirects(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	var redirects atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirects.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/readyz", r.URL.Path)
		w.Header().Set("Location", target.URL)
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte("ready"))
	}))
	defer server.Close()
	config := localProbe{Kind: "http", Path: "/readyz", Statuses: [][2]uint32{{200, 200}}, Body: "ready", Timeout: 1}
	require.NoError(t, runLocalProbe(t, config, server.URL, ""))
	for _, code := range []int{201, 302, 503} {
		status.Store(int32(code))
		require.Error(t, runLocalProbe(t, config, server.URL, ""))
	}
	require.Zero(t, redirects.Load())
	status.Store(200)
	config.Body = "missing"
	require.Error(t, runLocalProbe(t, config, server.URL, ""))
}

func TestActualTLSProbePinsTheMountedServingCertificate(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(200) }))
	defer server.Close()
	certFile := filepath.Join(t.TempDir(), "serving.crt")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600))
	config := localProbe{Kind: "http", Secured: true, Path: "/readyz", Statuses: [][2]uint32{{200, 200}}, Timeout: 1}
	require.NoError(t, runLocalProbe(t, config, server.URL, certFile))
	require.EqualValues(t, 1, requests.Load())
	require.Error(t, runLocalProbe(t, config, server.URL, "/missing/certificate"))
	different := append([]byte(nil), server.Certificate().Raw...)
	different[len(different)-1] ^= 1
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: different}), 0600))
	require.Error(t, runLocalProbe(t, config, server.URL, certFile))
	require.EqualValues(t, 1, requests.Load())

	// A malformed or mismatched mounted certificate cannot send application data.
	require.NoError(t, os.WriteFile(certFile, []byte("not a certificate"), 0600))
	require.Error(t, runLocalProbe(t, config, server.URL, certFile))
	require.EqualValues(t, 1, requests.Load())
}

func TestActualProbeHasBoundedResponseTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) }))
	defer server.Close()
	start := time.Now()
	require.Error(t, runLocalProbe(t, localProbe{Kind: "http", Path: "/readyz", Statuses: [][2]uint32{{200, 200}}, Timeout: 1}, server.URL, ""))
	require.Less(t, time.Since(start), 1900*time.Millisecond)
}

func deploymentTestParameters(t *testing.T, parameters Parameters) Parameters {
	t.Helper()
	health := &resources.Health{
		Readiness: &resources.Probe{Kind: "http", Path: "/version", Statuses: []string{"200"}},
		Startup:   &resources.Probe{Kind: "transport", FailureThreshold: 30, Period: "2s"},
		Liveness:  &resources.Probe{Kind: "transport"},
	}
	var err error
	parameters.Probes, err = deploymentProbes(healthEndpoint(t, false, health))
	require.NoError(t, err)
	return parameters
}

func TestBuilderLoadsAuthoredTLSAndHealthBeforeProjection(t *testing.T) {
	original, _ := createdBuilder(t)
	service := *original.Base.Service
	service.Endpoints = []*resources.Endpoint{{
		Name: "http", API: "rest", Secured: true, Visibility: "private",
		Health: &resources.Health{
			Readiness: &resources.Probe{Kind: "http", Path: "/readyz", Statuses: []string{"200"}},
			Startup:   &resources.Probe{Kind: "transport"},
			Liveness:  &resources.Probe{Kind: "transport"},
		},
	}}
	require.NoError(t, service.SaveAtDir(t.Context(), original.Location))
	identity, err := original.Identity.Proto()
	require.NoError(t, err)
	loaded := NewBuilder(NewService())
	_, err = loaded.Load(t.Context(), &builderv0.LoadRequest{Identity: identity})
	require.NoError(t, err)
	endpoint := loaded.FastAPI.RestEndpoint
	require.True(t, resources.EndpointSecured(endpoint))
	require.Equal(t, "/readyz", endpoint.GetHealth().GetReadiness().GetHttp().GetPath())
	probes, err := deploymentProbes(endpoint)
	require.NoError(t, err)
	require.Contains(t, probes, "/readyz")
	require.NotContains(t, probes, "/version")
}
