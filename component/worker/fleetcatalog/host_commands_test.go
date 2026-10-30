package fleetcatalog

import (
	"context"
	"testing"
)

// The router reads the owner's "Host commands" switch from the registration
// row, and the replica holding the machine's stream reads the same row through
// workerservice.OwnerFeaturesFromRow before it admits a command. The two must
// agree on what "off" means, or the router would offer a machine that always
// refuses, or skip one that would have run. Every hop test builds Candidate
// values by hand, so this is the only test that holds the mapping itself.
func TestWorkersForOwnerReadsHostCommandsOffFromTheRow(t *testing.T) {
	row := func(id string, features any) map[string]any {
		r := map[string]any{"id": id, "name": id, "ownerUserId": "alice"}
		if features != nil {
			r["ownerFeatures"] = features
		}
		return r
	}
	rows := []any{
		// No setting was ever saved: the owner's existing access is retained.
		row("never-set", nil),
		row("on", map[string]any{"headless": true, "apps": false, "computerUse": false}),
		row("off", map[string]any{"headless": false, "apps": true, "computerUse": true}),
		// Other features off say nothing about Host commands.
		row("only-others-off", map[string]any{"apps": false, "computerUse": false}),
		// A saved setting that is not an object never enables a feature.
		row("malformed", "invalid"),
		row("not-a-boolean", map[string]any{"headless": "true"}),
	}
	want := map[string]bool{
		"never-set":       false,
		"on":              false,
		"off":             true,
		"only-others-off": false,
		"malformed":       true,
		"not-a-boolean":   true,
	}

	candidates, err := (&EngineStore{Engine: &rowsFixture{rows: rows}}).WorkersForOwner(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != len(want) {
		t.Fatalf("read %d machines, want %d", len(candidates), len(want))
	}
	for _, c := range candidates {
		expected, known := want[c.RegistrationId]
		if !known {
			t.Fatalf("unexpected machine %q", c.RegistrationId)
		}
		if c.HostCommandsOff != expected {
			t.Errorf("%s: HostCommandsOff = %v, want %v", c.RegistrationId, c.HostCommandsOff, expected)
		}
	}
}
