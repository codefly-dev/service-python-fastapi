package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
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
func deploymentProbes(endpoint *basev0.Endpoint) (string, error) {
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
		value, err := kubernetesProbe(endpoint, intent.probe, intent.name)
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

func kubernetesProbe(endpoint *basev0.Endpoint, probe *basev0.Probe, intent string) (map[string]any, error) {
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
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	result["exec"] = map[string]any{"command": []string{"python", "-I", "-c", probeScript, string(raw)}}
	return result, nil
}
