package annotations

import "testing"

// TestKeywordArgsAreDerivedFromPlacements: KeywordArgs is the editor's view of
// the keyword keys, keyed by annotation name. It is derived from the
// placements, so it cannot name a key the check refuses or miss one it
// accepts.
func TestKeywordArgsAreDerivedFromPlacements(t *testing.T) {
	want := map[string][]ArgSpec{}
	for _, p := range Placements() {
		if len(p.Keys) == 0 {
			continue
		}
		if _, seen := want[p.Name]; seen {
			continue
		}
		want[p.Name] = p.Keys
	}
	if len(KeywordArgs) != len(want) {
		t.Errorf("KeywordArgs has %d names, the keyword placements %d", len(KeywordArgs), len(want))
	}
	for name, keys := range want {
		got := KeywordArgsFor(name)
		if len(got) != len(keys) {
			t.Errorf("KeywordArgsFor(%q) = %d keys, the placement declares %d", name, len(got), len(keys))
			continue
		}
		for i := range keys {
			if got[i] != keys[i] {
				t.Errorf("KeywordArgsFor(%q)[%d] = %+v, want %+v", name, i, got[i], keys[i])
			}
		}
	}
	if KeywordArgsFor("nonexistent-annotation") != nil {
		t.Error("KeywordArgsFor should return nil for an unmodeled annotation")
	}
	// The two keys the old hand table was missing (memql#5359 survey).
	for name, key := range map[string]string{"handler": "url", "relationship": "fieldSource"} {
		found := false
		for _, k := range KeywordArgsFor(name) {
			found = found || k.Name == key
		}
		if !found {
			t.Errorf("KeywordArgsFor(%q) lacks %q, which the parser reads", name, key)
		}
	}
}
