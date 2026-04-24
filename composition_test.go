package main

import (
	"testing"

	"gopkg.in/yaml.v3"

	pythonservice "github.com/codefly-dev/service-python/pkg/service"
)

// TestServiceInheritsFromPython verifies that fastapi.Service embeds
// *pythonservice.Service and thereby inherits the services.Base chain
// (Wool, Logger, Location, Identity, Settings). If embedding is replaced
// with a named field, these field accesses break the test.
func TestServiceInheritsFromPython(t *testing.T) {
	svc := NewService()
	if svc == nil {
		t.Fatal("NewService returned nil")
	}
	if svc.Service == nil {
		t.Fatal("fastapi.Service does not embed *pythonservice.Service")
	}
	// Promoted through *pythonservice.Service → *services.Base.
	if svc.Base == nil {
		t.Error("services.Base not promoted through embedding chain")
	}
	// fastapi Settings shadows generic Settings; both should be reachable.
	if svc.Settings == nil {
		t.Error("fastapi Settings is nil")
	}
	if svc.Service.Settings == nil {
		t.Error("generic Settings (via embedded Service) is nil")
	}
}

// TestFastAPISettingsInheritsPython proves the YAML inline-embed:
// fastapi YAML carries python-version from the generic layer plus
// hot-reload / public-endpoint from fastapi.
func TestFastAPISettingsInheritsPython(t *testing.T) {
	src := []byte(`
python-version: "3.12"
hot-reload: true
public-endpoint: true
`)
	var s Settings
	if err := yaml.Unmarshal(src, &s); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if s.PythonVersion != "3.12" {
		t.Errorf("PythonVersion (from generic): got %q want 3.12", s.PythonVersion)
	}
	if !s.HotReload {
		t.Error("HotReload (fastapi) not populated")
	}
	if !s.PublicEndpoint {
		t.Error("PublicEndpoint (fastapi) not populated")
	}
}

// TestRuntimeInheritsTestFromGeneric verifies that fastapi.Runtime embeds
// *pythonruntime.Runtime so that Test / Lint / Build methods come from the
// generic layer without fastapi needing to reimplement them.
func TestRuntimeInheritsTestFromGeneric(t *testing.T) {
	svc := NewService()
	rt := NewRuntime(svc)
	if rt == nil {
		t.Fatal("NewRuntime returned nil")
	}
	if rt.Runtime == nil {
		t.Fatal("fastapi.Runtime does not embed *pythonruntime.Runtime")
	}
	// Generic runtime must be wired to the same Service the outer holds.
	if rt.Runtime.Service != svc.Service {
		t.Error("generic runtime Service != outer Service — shared state broken")
	}
}

// TestSettingsPointerSharing proves that the generic Service.Settings
// points at the embedded half of fastapi Settings so generic code paths
// (python's Runtime.Load) see the generic fields.
func TestSettingsPointerSharing(t *testing.T) {
	svc := NewService()
	// Mutate via fastapi's Settings and observe via generic's.
	svc.Settings.PythonVersion = "3.13"
	gen, ok := interface{}(svc.Service.Settings).(*pythonservice.Settings)
	if !ok {
		t.Fatalf("generic Settings is not *pythonservice.Settings: %T", svc.Service.Settings)
	}
	if gen.PythonVersion != "3.13" {
		t.Errorf("generic Settings did not reflect fastapi mutation: got %q", gen.PythonVersion)
	}
}
