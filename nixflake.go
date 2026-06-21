package main

// nixflake.go — provisions the nix devShell for the nix runtime.
//
// In nix mode the agent runs the FastAPI service through a NixEnvironment rooted
// at the service source dir (CreateRunnerEnvironment), which materializes the
// devShell from a flake.nix there. User projects don't ship one, so this embeds
// a codefly flake (python3 + uv) and writes it into the source dir when absent —
// a user-supplied flake.nix is respected (never overwritten).

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed nix/flake.nix
var nixFlakeNix string

//go:embed nix/flake.lock
var nixFlakeLock string

// ensureNixFlake writes the embedded flake (python3 + uv) into dir unless a
// flake.nix already exists there.
func ensureNixFlake(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "flake.nix")); err == nil {
		return nil // user (or a prior run) already provides one
	}
	if err := os.WriteFile(filepath.Join(dir, "flake.nix"), []byte(nixFlakeNix), 0o644); err != nil {
		return fmt.Errorf("write flake.nix: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "flake.lock"), []byte(nixFlakeLock), 0o644); err != nil {
		return fmt.Errorf("write flake.lock: %w", err)
	}
	return nil
}
