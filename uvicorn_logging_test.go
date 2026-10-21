package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUvicornLoggingDropsProbesAndNothingElse pins the two files the agent
// hands uvicorn: a logging configuration that is uvicorn's own default with
// one filter on the access handler, and the filter module that drops a probe
// path and keeps every other request line.
func TestUvicornLoggingDropsProbesAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeUvicornLogging(dir))

	raw, err := os.ReadFile(filepath.Join(dir, uvicornLogConfigFile))
	require.NoError(t, err)
	var config map[string]any
	require.NoError(t, json.Unmarshal(raw, &config))
	filters := config["filters"].(map[string]any)["probes"].(map[string]any)
	require.Equal(t, "codefly_uvicorn_logging.ProbeFilter", filters["()"])
	access := config["handlers"].(map[string]any)["access"].(map[string]any)
	require.Equal(t, []any{"probes"}, access["filters"], "only the access handler is filtered")
	require.NotContains(t, config["handlers"].(map[string]any)["default"], "filters", "uvicorn's own messages are never dropped")
	for _, logger := range []string{"uvicorn", "uvicorn.error", "uvicorn.access"} {
		require.Contains(t, config["loggers"], logger, "uvicorn's default loggers are restated whole")
	}

	module, err := os.ReadFile(filepath.Join(dir, uvicornLoggingModule))
	require.NoError(t, err)
	require.Contains(t, string(module), "class ProbeFilter(logging.Filter)")

	probe := regexp.MustCompile(probePaths)
	for _, path := range []string{"/healthz", "/readyz", "/livez", "/health", "/ready", "/live", "/ping"} {
		require.True(t, probe.MatchString(path), path)
	}
	for _, path := range []string{"/", "/v1/model/profiles", "/healthz/deep", "/api/health-report", "/pingback"} {
		require.False(t, probe.MatchString(path), path)
	}

	// Rewritten whole on every call: a stale copy never survives an agent upgrade.
	require.NoError(t, os.WriteFile(filepath.Join(dir, uvicornLoggingModule), []byte("stale"), 0o644))
	require.NoError(t, writeUvicornLogging(dir))
	module, err = os.ReadFile(filepath.Join(dir, uvicornLoggingModule))
	require.NoError(t, err)
	require.NotEqual(t, "stale", string(module))
}
