package memql

import (
	"strings"
	"testing"
)

// TestCheckPluginContractCompat covers the load-time compatibility gate: the
// current version is accepted, anything else (newer-than-core, undeclared, or a
// retired older major) is rejected.
func TestCheckPluginContractCompat(t *testing.T) {
	if err := CheckPluginContractCompat(PluginContractVersion); err != nil {
		t.Fatalf("current contract version %d should be compatible, got: %v", PluginContractVersion, err)
	}
	if err := CheckPluginContractCompat(PluginContractVersion + 1); err == nil {
		t.Fatal("a pack needing a newer-than-core contract version must be rejected")
	}
	if err := CheckPluginContractCompat(0); err == nil {
		t.Fatal("a pack that declares no contract version (0) must be rejected")
	}
	if err := CheckPluginContractCompat(-3); err == nil {
		t.Fatal("a negative contract version must be rejected")
	}
	// A retired older major is only reachable once the version advances past 1.
	if PluginContractVersion > 1 {
		if err := CheckPluginContractCompat(PluginContractVersion - 1); err == nil {
			t.Fatal("a pack built against a retired older contract major must be rejected")
		}
	}
}

// TestPluginRegistrationValidateContract checks the registration-level wrapper
// the loader calls, including that it names the offending plug-in.
func TestPluginRegistrationValidateContract(t *testing.T) {
	good := PluginRegistration{Name: "good", RequiresContractVersion: PluginContractVersion}
	if err := good.ValidateContract(); err != nil {
		t.Fatalf("compatible registration was rejected: %v", err)
	}

	bad := PluginRegistration{Name: "stale-pack", RequiresContractVersion: PluginContractVersion + 1}
	err := bad.ValidateContract()
	if err == nil {
		t.Fatal("incompatible registration was accepted")
	}
	if !strings.Contains(err.Error(), "stale-pack") {
		t.Fatalf("error should name the plug-in, got: %v", err)
	}
}

// TestRegisterPluginDefaultsToCurrentContract verifies RegisterPlugin stamps
// the current contract version, and RegisterPluginForContract honors an
// explicit one. Uniquely-named so it does not collide with real registrations.
func TestRegisterPluginDefaultsToCurrentContract(t *testing.T) {
	noopFactory := func(PluginContext) (IntegrationProvider, error) { return nil, nil }

	const defaultName = "test_default_contract_plugin"
	RegisterPlugin(defaultName, noopFactory)
	if got := findRegistration(t, defaultName).RequiresContractVersion; got != PluginContractVersion {
		t.Fatalf("RegisterPlugin should stamp current version %d, got %d", PluginContractVersion, got)
	}

	const pinnedName = "test_pinned_contract_plugin"
	RegisterPluginForContract(pinnedName, 7, noopFactory)
	if got := findRegistration(t, pinnedName).RequiresContractVersion; got != 7 {
		t.Fatalf("RegisterPluginForContract should stamp 7, got %d", got)
	}
}

func findRegistration(t *testing.T, name string) PluginRegistration {
	t.Helper()
	for _, r := range RegisteredPlugins() {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("registration %q not found", name)
	return PluginRegistration{}
}
