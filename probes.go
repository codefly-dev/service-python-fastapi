package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// probeScript is self-contained and executes with Python's isolated mode.
//
//go:embed probe.py
var probeScript string

type localProbe struct {
	Kind     string      `json:"kind"`
	Port     int         `json:"port"`
	Secured  bool        `json:"secured"`
	Timeout  int64       `json:"timeout"`
	Path     string      `json:"path"`
	Statuses [][2]uint32 `json:"statuses"`
	Body     string      `json:"body"`
}

// deploymentProbes preserves the producing endpoint's three separate intents.
// Kubernetes has only one probe per intent; predicates for additional listeners
// need an explicit combined implementation rather than being silently ignored.
//
// native selects Kubernetes' own probes (tcpSocket, httpGet) instead of the
// container-local exec probe. A restricted render is what a cell runs, and a
// cell's execution admission refuses any container with an exec probe: it runs
// a command of the delivering party's choosing inside a pod that already holds
// the workload identity. A native probe expresses less than probe.py (no body
// predicate, no redirect refusal, no certificate pinning, and the kubelet takes
// any 200–399 status as success), so a predicate it cannot express is refused
// at render rather than weakened.
func deploymentProbes(endpoint *basev0.Endpoint, native bool) (string, error) {
	if endpoint == nil {
		return "", fmt.Errorf("HTTP endpoint is required for deployment probes")
	}
	if err := resources.ValidateEndpointHealth(endpoint); err != nil {
		return "", err
	}
	plan := resources.PlanEndpointProbes(endpoint)
	rendered := make(map[string]any)
	for _, intent := range []struct {
		name  string
		probe *basev0.Probe
	}{
		{"readinessProbe", plan.Readiness}, {"startupProbe", plan.Startup}, {"livenessProbe", plan.Liveness},
	} {
		if intent.probe == nil {
			continue
		}
		value, err := kubernetesProbe(endpoint, intent.probe, intent.name, native)
		if err != nil {
			return "", fmt.Errorf("%s: %w", intent.name, err)
		}
		rendered[intent.name] = value
	}
	raw, err := yaml.Marshal(rendered)
	if err != nil {
		return "", err
	}
	return "          " + strings.ReplaceAll(strings.TrimSuffix(string(raw), "\n"), "\n", "\n          "), nil
}

func kubernetesProbe(endpoint *basev0.Endpoint, probe *basev0.Probe, intent string, native bool) (map[string]any, error) {
	config := localProbe{Port: 8080, Secured: resources.EndpointSecured(endpoint), Timeout: 1}
	switch predicate := probe.GetPredicate().(type) {
	case *basev0.Probe_Transport:
		config.Kind = "transport"
	case *basev0.Probe_Http:
		config.Kind, config.Path, config.Body = "http", predicate.Http.GetPath(), predicate.Http.GetBodyContains()
		// HTTP request lines cannot contain controls, spaces or non-ASCII bytes.
		for _, c := range config.Path {
			if c <= 32 || c >= 127 {
				return nil, fmt.Errorf("HTTP path is not a bounded ASCII request target")
			}
		}
		for _, status := range resources.AcceptedHTTPStatuses(predicate.Http) {
			config.Statuses = append(config.Statuses, [2]uint32{status.Min, status.Max})
		}
	default:
		return nil, fmt.Errorf("predicate %s cannot be represented by this HTTP listener's deployment probe", resources.ProbeKindOf(probe))
	}
	result := map[string]any{"failureThreshold": 1, "successThreshold": 1}
	// Kubernetes accepts whole seconds only. Do not round an authored duration.
	for _, item := range []struct {
		name     string
		duration time.Duration
	}{
		{"initialDelaySeconds", probe.GetTiming().GetInitialDelay().AsDuration()},
		{"periodSeconds", probe.GetTiming().GetPeriod().AsDuration()},
		{"timeoutSeconds", probe.GetTiming().GetTimeout().AsDuration()},
	} {
		if item.duration == 0 {
			continue
		}
		if item.duration%time.Second != 0 {
			return nil, fmt.Errorf("%s must be whole seconds for Kubernetes", item.name)
		}
		seconds := int64(item.duration / time.Second)
		result[item.name] = seconds
		if item.name == "timeoutSeconds" {
			config.Timeout = seconds
		}
	}
	if n := probe.GetTiming().GetFailureThreshold(); n > 0 {
		result["failureThreshold"] = n
	}
	if n := probe.GetTiming().GetSuccessThreshold(); n > 0 {
		if intent != "readinessProbe" && n != 1 {
			return nil, fmt.Errorf("Kubernetes requires startup/liveness success-threshold 1")
		}
		result["successThreshold"] = n
	}
	if native {
		handler, err := nativeProbe(config)
		if err != nil {
			return nil, err
		}
		for key, value := range handler {
			result[key] = value
		}
		return result, nil
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	result["exec"] = map[string]any{"command": []string{"python", "-I", "-c", probeScript, string(raw)}}
	return result, nil
}

// deploymentProbesFor renders the endpoint's probes for the requested output
// profile: native for a restricted render (what a cell runs), the
// container-local exec probe otherwise.
func deploymentProbesFor(endpoint *basev0.Endpoint, profile builderv0.KubernetesOutputProfile) (string, error) {
	return deploymentProbes(endpoint, restrictedOutput(profile))
}

// restrictedOutput reports whether the deployment selects the restricted,
// portable output contract a cell runs. An unparseable profile is treated as
// restricted, the stricter of the two.
func restrictedOutput(profile builderv0.KubernetesOutputProfile) bool {
	parsed, err := services.ParseOutputProfile(profile)
	if err != nil {
		return true
	}
	return parsed.Restricted()
}

// kubeletSuccess is the status range a kubelet httpGet probe takes as success.
var kubeletSuccess = [2]uint32{200, 399}

// nativeProbe expresses a predicate with the kubelet's own handler, or refuses
// it. A transport predicate is a TCP connect. An HTTP predicate is an httpGet on
// the same path, over HTTPS when the endpoint is secured; it is refused when it
// checks the response body, or accepts a status the kubelet would not, because
// the kubelet cannot check either.
func nativeProbe(config localProbe) (map[string]any, error) {
	switch config.Kind {
	case "transport":
		return map[string]any{"tcpSocket": map[string]any{"port": config.Port}}, nil
	case "http":
		if config.Body != "" {
			return nil, fmt.Errorf("a cell refuses an exec probe, and a native HTTP probe cannot check that the response body contains %q; drop the body predicate or probe a path whose status alone answers", config.Body)
		}
		for _, status := range config.Statuses {
			if status[0] < kubeletSuccess[0] || status[1] > kubeletSuccess[1] {
				return nil, fmt.Errorf("a cell refuses an exec probe, and a native HTTP probe takes only %d–%d as success, so it cannot accept %d–%d", kubeletSuccess[0], kubeletSuccess[1], status[0], status[1])
			}
		}
		get := map[string]any{"path": config.Path, "port": config.Port}
		if config.Secured {
			get["scheme"] = "HTTPS"
		}
		return map[string]any{"httpGet": get}, nil
	}
	return nil, fmt.Errorf("predicate kind %q has no native probe", config.Kind)
}
