package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/services/sbom"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

// imageSBOMRequest asks for image scope, optionally naming subjects.
func imageSBOMRequest(subjects ...*builderv0.ImageSubject) *builderv0.SBOMRequest {
	return &builderv0.SBOMRequest{
		Scope:    builderv0.SBOMScope_SBOM_SCOPE_IMAGE,
		Subjects: subjects,
	}
}

// emitRecipePlan runs a recipe build and returns the emitted plan together with
// the directory the Dockerfile was written to.
func emitRecipePlan(t *testing.T, builder *Builder) (*builderv0.DockerBuildPlan, string) {
	t.Helper()
	outputDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	resp, err := builder.Build(ctx, recipeBuildRequest(outputDir))
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_SUCCESS, resp.GetState().GetState(), resp.GetState().GetMessage())
	plan := resp.GetResult().GetDockerBuildPlan()
	require.NotNil(t, plan)
	return plan, outputDir
}

// TestImageSBOMWithoutSubjectsIsPreconditionFailure pins the answer this agent
// owes when it is asked to enumerate images it never builds. The CLI executes
// the emitted recipe, so the digest exists only on its side. Reporting
// UNSUPPORTED would deny an implementation that exists, and a no-image reason
// would deny an image the service really ships — both are the false coverage
// claim the contract exists to prevent.
func TestImageSBOMWithoutSubjectsIsPreconditionFailure(t *testing.T) {
	builder, _ := createdBuilder(t)

	resp, err := builder.SBOM(t.Context(), imageSBOMRequest())
	require.NoError(t, err)

	require.Equal(t, builderv0.SBOMStatus_ERROR, resp.GetState().GetState())
	require.Equal(t, basev0.FailureCode_FAILURE_CODE_PRECONDITION_FAILED, resp.GetState().GetFailure().GetCode())
	require.Equal(t, builderv0.SBOMScope_SBOM_SCOPE_IMAGE, resp.GetScope())
	require.Equal(t, builderv0.NoImageReason_NO_IMAGE_REASON_UNSPECIFIED, resp.GetNoImageReason())
	require.Empty(t, resp.GetImages())
}

// TestImageSBOMExpectationsMatchTheEmittedRecipe verifies the specialization's
// actual build-recipe path against the coverage helper the fleet grades every
// agent with: one subject per shipped platform, carrying the recipe's role and
// final image reference.
func TestImageSBOMExpectationsMatchTheEmittedRecipe(t *testing.T) {
	builder, _ := createdBuilder(t)
	plan, _ := emitRecipePlan(t, builder)

	expected := sbom.ExpectedFromBuildPlan(builder.Identity.Name, plan)

	require.Len(t, expected, 2)
	var platforms []string
	for _, subject := range expected {
		platforms = append(platforms, subject.GetPlatform())
		require.Equal(t, "app", subject.GetRole())
		require.Equal(t, builder.Identity.Name, subject.GetService())
		require.NotEmpty(t, subject.GetReference())
	}
	require.ElementsMatch(t, []string{"linux/amd64", "linux/arm64"}, platforms)
}

// TestImageSBOMCoverageRejectsFalseClaims is the release gate: every shape that
// could pass off "no evidence" as complete image coverage has to fail
// ValidateCoverage against the service's own declared build.
func TestImageSBOMCoverageRejectsFalseClaims(t *testing.T) {
	builder, _ := createdBuilder(t)
	plan, _ := emitRecipePlan(t, builder)
	expected := sbom.ExpectedFromBuildPlan(builder.Identity.Name, plan)

	t.Run("no-image reason contradicting the declared build", func(t *testing.T) {
		resp, err := builder.Base.Builder.SBOMNoImage(builderv0.NoImageReason_NO_IMAGE_REASON_EXTERNALLY_MANAGED, "vendor image")
		require.NoError(t, err)
		require.Error(t, sbom.ValidateCoverage(expected, resp))
	})

	t.Run("source inventory is not image coverage", func(t *testing.T) {
		resp := &builderv0.SBOMResponse{
			State: &builderv0.SBOMStatus{State: builderv0.SBOMStatus_COMPLETE},
			Scope: builderv0.SBOMScope_SBOM_SCOPE_SOURCE,
		}
		require.Error(t, sbom.ValidateCoverage(expected, resp))
	})

	t.Run("precondition failure never reads as coverage", func(t *testing.T) {
		resp, err := builder.SBOM(t.Context(), imageSBOMRequest())
		require.NoError(t, err)
		require.Error(t, sbom.ValidateCoverage(expected, resp))
	})

	t.Run("one platform does not cover a multi-architecture image", func(t *testing.T) {
		covered := expected[0]
		resp, err := builder.Base.Builder.SBOMImageResponse([]*builderv0.ImageSBOM{{
			Digest:   "sha256:" + strings.Repeat("a", 64),
			Platform: covered.GetPlatform(),
			Subjects: []*builderv0.ImageSubject{covered},
			Bom:      &agentv0.Bom{Components: []*agentv0.Component{{BomRef: "pkg:apk/alpine/musl@1.2.5"}}},
			Sha256:   strings.Repeat("b", 64),
		}})
		require.NoError(t, err)
		require.Error(t, sbom.ValidateCoverage(expected, resp))
	})
}

// TestImageSBOMPropagatesScanFailures proves an unreachable image is reported as
// an image-scope error rather than silently omitted from an otherwise complete
// response.
func TestImageSBOMPropagatesScanFailures(t *testing.T) {
	builder, _ := createdBuilder(t)

	subject := &builderv0.ImageSubject{
		// A digest-pinned reference on a closed port fails resolution fast, and
		// on a host without docker fails the scan instead — an error either way.
		Reference: "127.0.0.1:1/missing@sha256:" + strings.Repeat("0", 64),
		Service:   builder.Identity.Name,
		Role:      "app",
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	resp, err := builder.SBOM(ctx, imageSBOMRequest(subject))
	require.NoError(t, err)

	require.Equal(t, builderv0.SBOMStatus_ERROR, resp.GetState().GetState())
	require.Equal(t, builderv0.SBOMScope_SBOM_SCOPE_IMAGE, resp.GetScope())
	require.Empty(t, resp.GetImages())
	require.Error(t, sbom.ValidateCoverage([]*builderv0.ImageSubject{subject}, resp))
}

// TestImageSBOMInventoriesTheBuiltImage is the end-to-end check: it builds the
// specialization's real runtime image from the recipe this agent emits, scans
// it, and requires the inventory to carry both halves a source SBOM can never
// prove — the base image's OS packages and the application's installed
// dependencies — bound to the digest that was actually scanned.
func TestImageSBOMInventoriesTheBuiltImage(t *testing.T) {
	for _, tool := range []string{"docker", "uv", "syft"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: the real runtime image cannot be built and scanned", tool)
		}
	}

	builder, _ := createdBuilder(t)
	plan, outputDir := emitRecipePlan(t, builder)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()

	// The Dockerfile installs from a frozen lockfile, which the scaffold does
	// not ship: it is resolved on first run.
	lock := exec.CommandContext(ctx, "uv", "lock")
	lock.Dir = filepath.Join(builder.Location, "code")
	if out, err := lock.CombinedOutput(); err != nil {
		t.Skipf("uv lock failed, the image cannot be built: %v\n%s", err, out)
	}

	tag := fmt.Sprintf("codefly-fastapi-sbom-test:%d", time.Now().UnixMilli())
	// The recipe's context "." is the service directory; the Dockerfile lives
	// in the emitted output directory.
	build := exec.CommandContext(ctx, "docker", "build",
		"-f", filepath.Join(outputDir, plan.GetRecipes()[0].GetDockerfile()),
		"-t", tag, builder.Location)
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("docker build failed, the image cannot be scanned: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })

	// The image was loaded into the local daemon and never pushed, so its
	// immutable identity is the local image ID. The platform is left unstated:
	// the daemon holds exactly one, and it is read back rather than asserted.
	subject := &builderv0.ImageSubject{
		Reference: tag,
		Role:      plan.GetRecipes()[0].GetName(),
		Service:   builder.Identity.Name,
	}
	resp, err := builder.Base.Builder.SBOMImages(ctx, []*builderv0.ImageSubject{subject}, sbom.SourceDockerDaemon)
	require.NoError(t, err)
	require.Equal(t, builderv0.SBOMStatus_COMPLETE, resp.GetState().GetState(), resp.GetState().GetMessage())
	require.Equal(t, builderv0.SBOMScope_SBOM_SCOPE_IMAGE, resp.GetScope())
	require.Len(t, resp.GetImages(), 1)

	evidence := resp.GetImages()[0]
	require.True(t, strings.HasPrefix(evidence.GetDigest(), "sha256:"), evidence.GetDigest())
	require.NotEmpty(t, evidence.GetPlatform())
	require.NotEmpty(t, evidence.GetTool())
	require.NotEmpty(t, evidence.GetSha256())
	require.Equal(t, []*builderv0.ImageSubject{subject}, evidence.GetSubjects())

	names := map[string]bool{}
	for _, component := range evidence.GetBom().GetComponents() {
		names[strings.ToLower(component.GetName())] = true
	}

	// The CI log is where this evidence is retrieved from, so name the digest
	// and platform the inventory is bound to rather than only asserting them.
	t.Logf("image evidence: digest=%s platform=%s tool=%s components=%d document-sha256=%s",
		evidence.GetDigest(), evidence.GetPlatform(), evidence.GetTool(),
		len(evidence.GetBom().GetComponents()), evidence.GetSha256())

	// The alpine base layer: packages no lockfile inventory would ever list.
	var osPackages []string
	for _, name := range []string{"musl", "busybox", "alpine-baselayout", "ca-certificates-bundle"} {
		if names[name] {
			osPackages = append(osPackages, name)
		}
	}
	require.NotEmpty(t, osPackages, "image inventory carries no base OS packages")

	// The application dependencies as they are installed in the image.
	for _, name := range []string{"fastapi", "uvicorn", "pydantic"} {
		require.True(t, names[name], "image inventory is missing application dependency %s", name)
	}

	require.NoError(t, sbom.ValidateCoverage([]*builderv0.ImageSubject{subject}, resp))
}
