package envregistry

import (
	"reflect"
	"slices"
	"testing"
)

// A downstream CI credential must be seedable without making every cluster
// require it, generating a default value, or changing another entry's boot
// requirements. The existing seeder still needs an owner-supplied value.
func TestPipelinePackageReadSecretIsOptionalGlobal(t *testing.T) {
	const name = "NPM_PACKAGES_READ_TOKEN"
	m, err := LoadManifestFromBytes(embeddedManifest, "embedded snapshot")
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := m.Lookup(name)
	if !ok {
		t.Fatal("package-read credential is absent from the fixed seeder registry")
	}
	if entry.Scope != "global" || entry.Kind != "integration" || entry.Component != "pipelines" ||
		!entry.Optional || entry.Default != "" || len(entry.Required) != 0 {
		t.Fatalf("package-read credential must be optional, global and without boot/default authority: %+v", entry)
	}
	without := *m
	without.Secrets = nil
	count := 0
	for _, secret := range m.Secrets {
		if secret.Name == name {
			count++
			continue
		}
		without.Secrets = append(without.Secrets, secret)
	}
	if count != 1 {
		t.Fatalf("want exactly one secret entry, got %d", count)
	}
	for _, variable := range m.Variables {
		if variable.Name == name {
			t.Fatal("package credential must not be registered as a plaintext variable")
		}
	}
	if slices.Contains(m.Names(), name) || !reflect.DeepEqual(m.Names(), without.Names()) {
		t.Fatal("registering the credential changed the existing seal floor")
	}
	if !reflect.DeepEqual(m.InjectableDefaults(), without.InjectableDefaults()) {
		t.Fatal("registering the credential changed automatic default injection")
	}
	for _, node := range []string{"identity", "bff", "agent", "planner", "workbench", "mcp", "edge"} {
		if !reflect.DeepEqual(m.RequiredForNodeType(node), without.RequiredForNodeType(node)) {
			t.Errorf("registering the credential changed boot requirements for %s", node)
		}
	}
	if !isExternalRegistryName(name) {
		t.Fatal("downstream package credential must retain its external CI name")
	}
	for _, other := range []string{"NPM_PACKAGES_WRITE_TOKEN", "NPM_PACKAGES_READ_TOKEN_EXTRA", "NPM_NEW_SETTING"} {
		if isExternalRegistryName(other) {
			t.Errorf("exact credential exemption unexpectedly covers %s", other)
		}
	}
}
