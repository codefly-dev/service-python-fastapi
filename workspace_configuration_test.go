package main

import (
	"context"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceConfigurationReachesServiceProcess(t *testing.T) {
	runtime, env, req := initStartTestRuntime(t)
	req.WorkspaceConfigurations = []*basev0.Configuration{{
		Origin: resources.ConfigurationWorkspace,
		Infos: []*basev0.ConfigurationInformation{{
			Name: "gateway",
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: "profile", Value: "local/fake-text"},
				{Key: "token", Value: "synthetic-test-token", Secret: true},
			},
		}},
	}}
	initialized, err := runtime.Init(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, runtimev0.InitStatus_READY, initialized.GetStatus().GetState())
	started, err := runtime.Start(context.Background(), &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, started.GetStatus().GetState())
	t.Cleanup(func() { _, _ = runtime.Stop(context.Background(), &runtimev0.StopRequest{}) })
	values := map[string]string{}
	for _, variable := range uvicornProc(t, env).envs {
		values[variable.Key] = variable.ValueAsString()
	}
	require.Equal(t, "local/fake-text", values["CODEFLY__WORKSPACE_CONFIGURATION__GATEWAY__PROFILE"])
	require.Equal(t, "synthetic-test-token", values["CODEFLY__WORKSPACE_SECRET_CONFIGURATION__GATEWAY__TOKEN"])
}
