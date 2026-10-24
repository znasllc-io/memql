package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestComposedEntryCarriesNoClientId is the shared-registry half of the
// rule on ClusterConfig.ClientId: ~/.memql/clusters.yaml is read by the
// VS Code extension and other tools, a client id is per TOOL, and a
// `client_id: cockpit` the cockpit stamped on every entry was taken as an
// override by the editor and broke its sign-in. The entry `memql cluster
// add` composes must reach the file with no client_id key at all -- and
// the cockpit must still resolve its own client from it.
func TestComposedEntryCarriesNoClientId(t *testing.T) {
	composed := ComposeFromDomain("staging.example.com")
	if composed.ClientId != "" {
		t.Fatalf("ComposeFromDomain set ClientId = %q; the cockpit's default must not be stored", composed.ClientId)
	}

	// Write it the way `cluster add` does -- through the registry
	// transaction -- and look at the BYTES, not a decoded struct: a
	// decode would hide a key that is present but empty.
	path := filepath.Join(t.TempDir(), "clusters.yaml")
	if err := updateClustersAt(t.Context(), path, func(f *ClustersFile) error {
		f.Clusters = append(f.Clusters, composed)
		return nil
	}); err != nil {
		t.Fatalf("updateClustersAt: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "client_id") {
		t.Fatalf("composed entry wrote a client_id into the shared registry:\n%s", data)
	}

	var loaded ClustersFile
	if err := yaml.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := loaded.Get(composed.Name)
	if !ok {
		t.Fatalf("entry %q not found after write:\n%s", composed.Name, data)
	}
	// The literal, not DefaultClientId: the engine registers the cockpit
	// as "cockpit", and a renamed constant must fail here.
	if id := got.EffectiveClientId(); id != "cockpit" {
		t.Errorf("EffectiveClientId() = %q for an entry with no client_id, want %q", id, "cockpit")
	}
	if got.NeedsAuth() {
		t.Error("NeedsAuth() = true for an endpoint + issuer entry with no client_id; the cockpit signs in as its default client")
	}
}

// TestExplicitClientIdIsStillRead keeps the operator override working
// for the cockpit itself: a client_id somebody wrote by hand (or a
// cluster's discovery document named) is what the cockpit signs in as,
// and a cockpit load/save cycle hands it back unchanged.
func TestExplicitClientIdIsStillRead(t *testing.T) {
	const onDisk = `clusters:
    - name: staging
      endpoint: https://api.staging.example.com
      issuer: https://identity.staging.example.com
      client_id: operator-client
`
	var loaded ClustersFile
	if err := yaml.Unmarshal([]byte(onDisk), &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if id := loaded.Clusters[0].EffectiveClientId(); id != "operator-client" {
		t.Errorf("EffectiveClientId() = %q, want the explicit operator-client", id)
	}

	saved, err := yaml.Marshal(&loaded)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(saved), "client_id: operator-client") {
		t.Errorf("an explicit client_id did not survive a cockpit save:\n%s", saved)
	}
}

func TestEffectiveClientIdTreatsBlankAsAbsent(t *testing.T) {
	if id := (ClusterConfig{ClientId: "   "}).EffectiveClientId(); id != DefaultClientId {
		t.Errorf("EffectiveClientId() = %q for a blank client_id, want %q", id, DefaultClientId)
	}
}

func TestStoredClientIdOmitsOnlyTheDefault(t *testing.T) {
	for in, want := range map[string]string{
		"":                "",
		"cockpit":         "",
		" cockpit ":       "",
		"operator-client": "operator-client",
	} {
		if got := StoredClientId(in); got != want {
			t.Errorf("StoredClientId(%q) = %q, want %q", in, got, want)
		}
	}
}
