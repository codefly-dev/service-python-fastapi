package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReleaseDeclaresOnePublisherAndArchiveSBOMs(t *testing.T) {
	read := func(path string, target any) {
		t.Helper()
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(payload, target); err != nil {
			t.Fatal(err)
		}
	}
	var manifest struct {
		Release struct{ Owner, Workflow string }
	}
	read("agent.codefly.yaml", &manifest)
	if manifest.Release.Owner != "workflow" || manifest.Release.Workflow != "releaser.yml" {
		t.Fatal("the tag workflow must be the sole artifact publisher")
	}
	var config struct {
		SBOMs []struct {
			Artifacts string
			Documents []string
			Disable   bool
		} `yaml:"sboms"`
	}
	read(".goreleaser.yaml", &config)
	if len(config.SBOMs) != 1 || config.SBOMs[0].Artifacts != "archive" || config.SBOMs[0].Disable || len(config.SBOMs[0].Documents) != 1 || config.SBOMs[0].Documents[0] != "${artifact}.sbom.json" {
		t.Fatal("each published archive must carry its canonical SBOM")
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				ID, Uses, Run string
				Env           map[string]string
			}
		}
	}
	read(filepath.Join(".github", "workflows", manifest.Release.Workflow), &workflow)
	installed, mounted := false, false
	for _, step := range workflow.Jobs["release"].Steps {
		if step.ID == "syft" && strings.HasPrefix(step.Uses, "anchore/sbom-action/download-syft@") {
			installed = true
		}
		if strings.Contains(step.Run, `-v "$SYFT_PATH:/usr/local/bin/syft:ro"`) && step.Env["SYFT_PATH"] == "${{ steps.syft.outputs.cmd }}" {
			mounted = true
		}
	}
	if !installed || !mounted {
		t.Fatal("the canonical cross build must have Syft installed and mounted")
	}
}
