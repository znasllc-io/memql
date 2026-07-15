package annotations

import "testing"

// TestKeywordArgsReferenceKnownAnnotations guards the arg-schema extension: every
// annotation modeled in KeywordArgs must be a real annotation (accepted by at
// least one receiver and carrying a Docs entry), and every arg spec must be
// well-formed. This keeps the new SoT for annotation arguments from drifting
// away from the name/receiver/doc surface it augments.
func TestKeywordArgsReferenceKnownAnnotations(t *testing.T) {
	known := make(map[string]bool)
	for _, names := range ByReceiver {
		for _, n := range names {
			known[n] = true
		}
	}

	for name, specs := range KeywordArgs {
		if !known[name] {
			t.Errorf("KeywordArgs models %q which no receiver accepts", name)
		}
		if _, ok := Docs[name]; !ok {
			t.Errorf("KeywordArgs annotation %q has no Docs entry", name)
		}
		if len(specs) == 0 {
			t.Errorf("KeywordArgs[%q] is empty", name)
		}
		seen := make(map[string]bool)
		for _, s := range specs {
			if s.Name == "" || s.Type == "" {
				t.Errorf("KeywordArgs[%q] has a malformed arg: %+v", name, s)
			}
			if seen[s.Name] {
				t.Errorf("KeywordArgs[%q] has a duplicate arg %q", name, s.Name)
			}
			seen[s.Name] = true
		}
	}

	if KeywordArgsFor("nonexistent-annotation") != nil {
		t.Error("KeywordArgsFor should return nil for an unmodeled annotation")
	}
}
