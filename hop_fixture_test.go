package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	"github.com/codefly-dev/authority/libraries/hop/go/hoptest"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// Every listener the scaffold generates verifies the hop's admission on every
// call, and refuses to start without the verifier's inputs — so a test that
// boots a scaffolded service has to compose an authority for it, exactly as a
// workspace does. hopAuthority is that authority: it signs Work Contexts with a
// key of its own, publishes its hop's admission key through the authority
// interface's Keys, and stamps a call the way the hop stamps one it let through
// (codefly-dev/authority libraries/hop/go/hoptest). Nothing is hand-built: the
// scaffold runs the authority library's verifier against the authority library's
// own fixture, so a green test is the real admission path.
//
// The module's replay store is this process's own, which the verifier accepts
// only in the run named exactly local — the one thing a developer run relaxes.

// hopLabel names the hop that signs for this fixture; the authority publishes
// its admission key under it.
const hopLabel = "python-fastapi"

// hopModule is the module a scaffolded test service belongs to, which is the
// audience the authority knows it by (CODEFLY__MODULE).
const hopModule = "mod"

type hopAuthority struct {
	*hoptest.Authority
	// address is where the authority's Keys answers, as a composition's
	// authority/endpoint names it.
	address string
}

// keysServer serves the one procedure a module reads: the published keys. Every
// other procedure is the authority's own and no module behind the hop calls it.
type keysServer struct {
	authorityv1.UnimplementedAuthorityServiceServer
	authority *hoptest.Authority
}

func (k keysServer) Keys(ctx context.Context, request *authorityv1.KeysRequest) (*authorityv1.KeysResponse, error) {
	return k.authority.Keys(ctx, request)
}

// serveHopAuthority starts the fixture authority on a loopback address. A
// plaintext endpoint is dialled only to loopback on a local run, which is what
// these tests are.
func serveHopAuthority(t *testing.T) *hopAuthority {
	t.Helper()
	authority := hoptest.New(t).PublishHop(hopLabel)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	authorityv1.RegisterAuthorityServiceServer(server, keysServer{authority: authority})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return &hopAuthority{Authority: authority, address: listener.Addr().String()}
}

// configurations is what the composition delivers: where the authority answers.
// Nothing about the hop itself is configured — its admission key comes from the
// same Keys read.
func (a *hopAuthority) configurations() []*basev0.Configuration {
	return []*basev0.Configuration{{
		Origin: resources.ConfigurationWorkspace,
		Infos: []*basev0.ConfigurationInformation{{
			Name:                "authority",
			ConfigurationValues: []*basev0.ConfigurationValue{{Key: "endpoint", Value: a.address}},
		}},
	}}
}

// environment is the same delivery for a process a test launches itself, in the
// variables core's environment-variable manager would inject.
func (a *hopAuthority) environment() []string {
	return []string{
		"CODEFLY__WORKSPACE_CONFIGURATION__AUTHORITY__ENDPOINT=" + a.address,
		"CODEFLY__ENVIRONMENT=local",
		"CODEFLY__MODULE=" + hopModule,
	}
}

// stamp is one call's headers as the hop writes them: the Work Context it
// checked, the identity it derived from it, and an admission signed for exactly
// this method. The method is "<VERB> <path>" for a REST call, as the hop's HTTP
// face binds it, and the full procedure for a Connect or gRPC one.
func (a *hopAuthority) stamp(t *testing.T, method string) http.Header {
	t.Helper()
	header := http.Header{}
	for key, values := range hoptest.Forward(t, a.Issue(t, hopModule), hopLabel, method) {
		for _, value := range values {
			header.Add(key, value)
		}
	}
	return header
}

// get is a stamped GET, the way a caller reaches a route behind the hop.
func (a *hopAuthority) get(t *testing.T, client *http.Client, address, path string) (*http.Response, error) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s%s", address, path), nil)
	require.NoError(t, err)
	request.Header = a.stamp(t, fmt.Sprintf("GET %s", path))
	return client.Do(request)
}
