package tools_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

func policyFrom(t *testing.T, body string) *tools.Policy {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := tools.LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	return p
}

// THE VERSION MUST NOT MOVE. The engine admits exactly 1 and refuses
// anything else BY VALUE ("unsupported schemaVersion %d"), so bumping
// it for an additive field fails every registration on the fleet at
// once -- and the failure is a handshake refusal, not a missing field,
// so every machine goes offline rather than losing one capability.
//
// This is the same rule Displays shipped under, written down where the
// next additive field will be added.
func TestCapabilityDescriptorSchemaVersionStaysOne(t *testing.T) {
	raw, err := tools.CapabilityDescriptorJSONFor(nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got["schemaVersion"] != float64(1) {
		t.Fatalf("schemaVersion = %v, want 1 -- see the engine's component/worker/capability_descriptor.go, "+
			"which refuses any other value and would fail every registration on the fleet",
			got["schemaVersion"])
	}
}
