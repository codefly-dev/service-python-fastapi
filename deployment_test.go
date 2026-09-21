package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	agenttesting "github.com/codefly-dev/core/agents/testing"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestDeploymentTemplates(t *testing.T) {
	destination := agenttesting.AssertKustomizeTemplates(t, deploymentFS, deploymentTestParameters(t, Parameters{}))

	rendered, err := os.ReadFile(filepath.Join(destination, "base", "deployment.yaml"))
	require.NoError(t, err)
	manifest := string(rendered)

	// Kubernetes rejects runAsNonRoot when the image user is non-numeric, so the
	// workload must pin a numeric UID matching the Dockerfile's appuser (1000).
	require.Contains(t, manifest, "runAsUser: 1000")
	// readOnlyRootFilesystem leaves the container without writable storage, so a
	// scratch mount at /tmp is required for the app to run.
	require.Contains(t, manifest, "mountPath: /tmp")
	// The scratch volume is bounded so a runaway writer evicts only this pod
	// rather than filling node ephemeral storage.
	require.Contains(t, manifest, "sizeLimit: 1Gi")
	// $HOME must point at the writable mount so ~/.cache writes don't hit the
	// read-only root filesystem.
	require.Contains(t, manifest, "value: /tmp")
}

// podSpec is the slice of the rendered Deployment the overlay lands on.
type podSpec struct {
	Metadata struct {
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		ServiceAccountName           string `yaml:"serviceAccountName"`
		AutomountServiceAccountToken *bool  `yaml:"automountServiceAccountToken"`
		Containers                   []struct {
			VolumeMounts []struct {
				Name      string `yaml:"name"`
				MountPath string `yaml:"mountPath"`
				ReadOnly  *bool  `yaml:"readOnly"`
			} `yaml:"volumeMounts"`
		} `yaml:"containers"`
		Volumes []struct {
			Name      string `yaml:"name"`
			ConfigMap *struct {
				Name     string `yaml:"name"`
				Optional *bool  `yaml:"optional"`
			} `yaml:"configMap"`
		} `yaml:"volumes"`
	} `yaml:"spec"`
}

type serviceAccount struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
}

// renderDeployment renders the deployment templates with an overlay and returns
// the workload's pod template, normalized the way DeployKustomize normalizes it
// before rendering.
func renderDeployment(t *testing.T, parameters Parameters, overlay *services.PodTemplateOverlay) (podSpec, string) {
	t.Helper()
	overlay.DefaultConfigMounts()
	destination := agenttesting.AssertKustomizeTemplatesWithOverlay(t, deploymentFS, deploymentTestParameters(t, parameters), overlay)

	rendered, err := os.ReadFile(filepath.Join(destination, "base", "deployment.yaml"))
	require.NoError(t, err)
	var deployment struct {
		Spec struct {
			Template podSpec `yaml:"template"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(rendered, &deployment))
	return deployment.Spec.Template, destination
}

func TestDeploymentWithoutOverlayIsUnchanged(t *testing.T) {
	pod, destination := renderDeployment(t, Parameters{}, nil)

	require.Empty(t, pod.Spec.ServiceAccountName)
	require.Equal(t, false, *pod.Spec.AutomountServiceAccountToken)
	require.Nil(t, pod.Metadata.Annotations)
	require.Equal(t, map[string]string{"app": "example-service", "sha": "1.2.3"}, pod.Metadata.Labels)
	// Only the scratch mount: an absent overlay must add no volume.
	require.Len(t, pod.Spec.Volumes, 1)
	require.Equal(t, "tmp", pod.Spec.Volumes[0].Name)
	require.Len(t, pod.Spec.Containers[0].VolumeMounts, 1)

	_, err := os.Stat(filepath.Join(destination, "base", "serviceaccount.yaml"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestDeploymentRendersServiceAccountWithAnnotations(t *testing.T) {
	pod, destination := renderDeployment(t, Parameters{}, &services.PodTemplateOverlay{
		ServiceAccount: &services.WorkloadServiceAccount{
			Name:        "model-gateway",
			Annotations: map[string]string{"iam.gke.io/gcp-service-account": "gateway@project.iam.gserviceaccount.com"},
		},
	})

	require.Equal(t, "model-gateway", pod.Spec.ServiceAccountName)
	// The workload takes its identity from a projected token, not from a
	// mounted API-server token.
	require.Equal(t, false, *pod.Spec.AutomountServiceAccountToken)

	rendered, err := os.ReadFile(filepath.Join(destination, "base", "serviceaccount.yaml"))
	require.NoError(t, err)
	var account serviceAccount
	require.NoError(t, yaml.Unmarshal(rendered, &account))
	require.Equal(t, "ServiceAccount", account.Kind)
	require.Equal(t, "model-gateway", account.Metadata.Name)
	require.Equal(t, "codefly-test", account.Metadata.Namespace)
	require.Equal(t, "gateway@project.iam.gserviceaccount.com", account.Metadata.Annotations["iam.gke.io/gcp-service-account"])
	require.Equal(t, "codefly", account.Metadata.Labels["app.kubernetes.io/managed-by"])

	kustomization, err := os.ReadFile(filepath.Join(destination, "base", "kustomization.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(kustomization), "serviceaccount.yaml")
}

func TestDeploymentStampsPodLabelsAndAnnotations(t *testing.T) {
	pod, _ := renderDeployment(t, Parameters{}, &services.PodTemplateOverlay{
		PodLabels:      map[string]string{"azure.workload.identity/use": "true"},
		PodAnnotations: map[string]string{"prometheus.io/scrape": "true"},
	})

	require.Equal(t, "true", pod.Metadata.Labels["azure.workload.identity/use"])
	// The selector label must survive the stamp, or the Deployment adopts no pods.
	require.Equal(t, "example-service", pod.Metadata.Labels["app"])
	require.Equal(t, "true", pod.Metadata.Annotations["prometheus.io/scrape"])
}

func TestDeploymentRendersConfigMount(t *testing.T) {
	pod, _ := renderDeployment(t, Parameters{}, &services.PodTemplateOverlay{
		ConfigMounts: []services.ConfigMount{
			{ConfigMapName: "trust-bundle", MountPath: "/etc/ssl/trust"},
		},
	})

	require.Len(t, pod.Spec.Volumes, 2)
	volume := pod.Spec.Volumes[1]
	require.Equal(t, "trust-bundle", volume.Name)
	require.Equal(t, "trust-bundle", volume.ConfigMap.Name)
	require.Equal(t, false, *volume.ConfigMap.Optional)

	mounts := pod.Spec.Containers[0].VolumeMounts
	require.Len(t, mounts, 2)
	require.Equal(t, "trust-bundle", mounts[1].Name)
	require.Equal(t, "/etc/ssl/trust", mounts[1].MountPath)
	// File config defaults to read-only.
	require.Equal(t, true, *mounts[1].ReadOnly)
}

func TestDeploymentRendersTwoConfigMounts(t *testing.T) {
	pod, _ := renderDeployment(t, Parameters{}, &services.PodTemplateOverlay{
		ConfigMounts: []services.ConfigMount{
			{ConfigMapName: "trust-bundle", MountPath: "/etc/ssl/trust"},
			{ConfigMapName: "model-routes", MountPath: "/etc/gateway/routes"},
		},
	})

	require.Len(t, pod.Spec.Volumes, 3)
	require.Equal(t, "trust-bundle", pod.Spec.Volumes[1].Name)
	require.Equal(t, "model-routes", pod.Spec.Volumes[2].Name)

	mounts := pod.Spec.Containers[0].VolumeMounts
	require.Len(t, mounts, 3)
	require.Equal(t, "/etc/ssl/trust", mounts[1].MountPath)
	require.Equal(t, "/etc/gateway/routes", mounts[2].MountPath)
}

func TestDeploymentRendersOptionalConfigMount(t *testing.T) {
	writable := false
	pod, _ := renderDeployment(t, Parameters{}, &services.PodTemplateOverlay{
		ConfigMounts: []services.ConfigMount{
			{ConfigMapName: "trust-bundle", MountPath: "/etc/ssl/trust", Optional: true, ReadOnly: &writable},
		},
	})

	// Optional lets the pod start before the ConfigMap is supplied per environment.
	require.Equal(t, true, *pod.Spec.Volumes[1].ConfigMap.Optional)
	require.Equal(t, false, *pod.Spec.Containers[0].VolumeMounts[1].ReadOnly)
}

// TestDeploymentAutomountServiceAccountTokenRejectsProjection pins the reason
// the parameter defaults to false and cannot yet be flipped: core's manifest
// conformance rejects a projected token for every workload, so a service that
// needs to call the API server is blocked in core, not here.
func TestDeploymentAutomountServiceAccountTokenRejectsProjection(t *testing.T) {
	_, destination := renderDeployment(t, Parameters{}, nil)

	path := filepath.Join(destination, "base", "deployment.yaml")
	conformant, err := os.ReadFile(path)
	require.NoError(t, err)
	projected := strings.Replace(string(conformant), "automountServiceAccountToken: false", "automountServiceAccountToken: true", 1)
	require.NotEqual(t, string(conformant), projected)
	require.NoError(t, os.WriteFile(path, []byte(projected), 0o600))

	validation := services.ValidateKubernetesManifestTree(t.Context(), destination, "test", "codefly-test",
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1, false, "", "")

	require.Equal(t, []string{"Deployment/example-service must set automountServiceAccountToken: false"}, validation.GetViolations())
}
