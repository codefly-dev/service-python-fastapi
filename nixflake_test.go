package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnsureNixFlakeProvisionsDevShell(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, ensureNixFlake(dir))

	flake, err := os.ReadFile(filepath.Join(dir, "flake.nix"))
	require.NoError(t, err)
	lock, err := os.ReadFile(filepath.Join(dir, "flake.lock"))
	require.NoError(t, err)

	// The runtime reaches these through the materialized devShell: python3 to
	// run uvicorn, uv to sync dependencies. A shell without them starts an
	// agent that cannot run the service it just provisioned.
	require.Contains(t, string(flake), "pkgs.python3")
	require.Contains(t, string(flake), "pkgs.uv")
	require.Contains(t, string(lock), `"nixpkgs"`)
}

func TestEnsureNixFlakeKeepsUserFlake(t *testing.T) {
	dir := t.TempDir()
	user := []byte("# a user's own flake\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "flake.nix"), user, 0o644))

	require.NoError(t, ensureNixFlake(dir))

	got, err := os.ReadFile(filepath.Join(dir, "flake.nix"))
	require.NoError(t, err)
	require.Equal(t, user, got)
	// The lock pins nixpkgs for the embedded flake only; writing it beside a
	// user's flake would pin inputs that flake never declared.
	require.NoFileExists(t, filepath.Join(dir, "flake.lock"))
}
