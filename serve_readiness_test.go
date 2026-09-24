package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/stretchr/testify/require"
)

// closedAddress returns a loopback address nothing listens on.
func closedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return address
}

// TestStartedMeansTheEndpointAnswers pins the contract a dependent is started
// on: STARTED is not returned while the endpoint refuses connections, and is
// returned once it answers, whatever the status.
func TestStartedMeansTheEndpointAnswers(t *testing.T) {
	runtime, env := startTestRuntime(t)
	address := closedAddress(t)
	runtime.serveAddress = "http://" + address

	served := make(chan struct{})
	go func() {
		time.Sleep(700 * time.Millisecond)
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Errorf("cannot bind the late listener: %v", err)
			return
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}), ReadHeaderTimeout: time.Second}
		t.Cleanup(func() { _ = server.Close() })
		close(served)
		_ = server.Serve(listener)
	}()

	began := time.Now()
	resp, err := runtime.Start(context.Background(), &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())
	select {
	case <-served:
	default:
		t.Fatal("STARTED was returned before anything listened on the endpoint")
	}
	require.GreaterOrEqual(t, time.Since(began), 600*time.Millisecond)
	require.Len(t, env.started(), 1)
	require.False(t, env.started()[0].isStopped())
}

// TestStartAcceptsAnyHTTPAnswer: a redirect or an error status is still an
// endpoint serving HTTP.
func TestStartAcceptsAnyHTTPAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)

	runtime, _ := startTestRuntime(t)
	// The carrier may hold host:port without a scheme.
	runtime.serveAddress = strings.TrimPrefix(server.URL, "http://")

	resp, err := runtime.Start(context.Background(), &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())
}

// TestStartFailsWhenTheAppExitsBeforeServing: a process that dies before its
// endpoint answers is a failed Start carrying what it printed, not a STARTED
// the supervisor revokes later.
func TestStartFailsWhenTheAppExitsBeforeServing(t *testing.T) {
	runtime, env := startTestRuntime(t)
	runtime.serveAddress = "http://" + closedAddress(t)

	go func() {
		for len(env.started()) == 0 || !env.started()[0].isStarted() {
			time.Sleep(10 * time.Millisecond)
		}
		proc := env.started()[0]
		_, _ = proc.output.Write([]byte("ModuleNotFoundError: No module named 'src.main'\n"))
		proc.die(errors.New("exit status 1"))
	}()

	resp, err := runtime.Start(context.Background(), &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_ERROR, resp.GetStatus().GetState())
	require.Contains(t, resp.GetStatus().GetMessage(), "exited before its HTTP endpoint")
	require.Contains(t, resp.GetStatus().GetMessage(), "ModuleNotFoundError")
	require.Nil(t, currentRunner(runtime), "a process that never served must not stay the service's runner")
}

// TestStartFailsWhenTheEndpointNeverAnswers: a live process whose endpoint
// never answers is stopped and reported, bounded by the serve timeout.
func TestStartFailsWhenTheEndpointNeverAnswers(t *testing.T) {
	runtime, env := startTestRuntime(t)
	runtime.serveAddress = "http://" + closedAddress(t)
	runtime.serveTimeout = 600 * time.Millisecond

	resp, err := runtime.Start(context.Background(), &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_ERROR, resp.GetStatus().GetState())
	require.Contains(t, resp.GetStatus().GetMessage(), "did not answer within")
	require.Len(t, env.started(), 1)
	require.True(t, env.started()[0].isStopped(), "the process that never served must be stopped")
	require.Nil(t, currentRunner(runtime))

	// The next Start is a fresh attempt, not an answer from the failed one.
	runtime.serveAddress = ""
	resp, err = runtime.Start(context.Background(), &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())
	require.Len(t, env.started(), 2)
}
