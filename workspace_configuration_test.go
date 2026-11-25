package main

import (
	"context"
	"testing"

	basev1 "github.com/codefly-dev/core/generated/go/codefly/base/v1"
	runtimev1 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v1"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceConfigurationReachesServiceProcess(t *testing.T) {
	runtime, env, req := initStartTestRuntime(t)
	req.WorkspaceConfigurations = []*basev1.Configuration{{
		Origin: resources.ConfigurationWorkspace,
		Infos: []*basev1.ConfigurationInformation{{
			Name: "gateway",
			ConfigurationValues: []*basev1.ConfigurationValue{
				{Key: "profile", Value: "local/fake-text"},
				{Key: "token", Value: "synthetic-test-token", Secret: true},
			},
		}},
	}}
	initialized, err := runtime.Init(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, runtimev1.InitStatus_READY, initialized.GetStatus().GetState())
	started, err := runtime.Start(context.Background(), &runtimev1.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev1.StartStatus_STARTED, started.GetStatus().GetState())
	t.Cleanup(func() { _, _ = runtime.Stop(context.Background(), &runtimev1.StopRequest{}) })
	values := map[string]string{}
	for _, variable := range uvicornProc(t, env).envs {
		values[variable.Key] = variable.ValueAsString()
	}
	require.Equal(t, "local/fake-text", values["CODEFLY__WORKSPACE_CONFIGURATION__GATEWAY__PROFILE"])
	require.Equal(t, "synthetic-test-token", values["CODEFLY__WORKSPACE_SECRET_CONFIGURATION__GATEWAY__TOKEN"])
}
