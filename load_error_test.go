package main

import (
	"path/filepath"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

// The generic Load reports its failure in the response's state. The fastapi
// Load must hand that failure back as it is, never go on to look for a REST
// endpoint in a service it could not load and report that one is missing.
func TestBuilderLoadSurfacesTheGenericLoadFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		request *builderv0.LoadRequest
		cause   string
	}{
		"no identity": {request: &builderv0.LoadRequest{}, cause: "requires a service identity"},
		"no service file": {request: &builderv0.LoadRequest{Identity: &basev0.ServiceIdentity{
			Name: "absent", Version: "0.0.0", Module: "mod", Workspace: "test",
			WorkspacePath: t.TempDir(), RelativeToWorkspace: filepath.Join("mod", "absent"),
		}}, cause: "absent"},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := NewBuilder(NewService()).Load(t.Context(), tc.request)
			require.NoError(t, err)
			require.Equal(t, builderv0.LoadStatus_ERROR, resp.GetState().GetState())
			require.Contains(t, resp.GetState().GetMessage(), tc.cause)
			require.NotContains(t, resp.GetState().GetMessage(), "rest endpoint")
		})
	}
}
