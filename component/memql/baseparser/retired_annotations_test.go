package baseparser

import (
	"strings"
	"testing"
)

// TestRetiredConstructAnnotationInternal is the #2708 retirement gate (the
// #2620 ruling, 2026.08 epoch): construct-level @internal is recognised only
// to emit a migration-hint error, the @use* precedent -- never a generic
// unknown-annotation message, and never silently accepted.
func TestRetiredConstructAnnotationInternal(t *testing.T) {
	allowed := map[string]bool{"description": true, "public": true}
	src := "@internal\nquery globalVariable userDefaults {\n  filter isActiveRecord\n}\n"
	err := ValidateConstructAnnotations(src, "query", allowed)
	if err == nil {
		t.Fatalf("construct-level @internal must be rejected (retired, #2708)")
	}
	if !strings.Contains(err.Error(), "#2708") || !strings.Contains(err.Error(), "retired") {
		t.Errorf("rejection must carry the retirement hint, got: %v", err)
	}
	// The allow-list path is unchanged for live annotations.
	if err := ValidateConstructAnnotations("@public\nquery g x {\n}\n", "query", allowed); err != nil {
		t.Errorf("live annotation must pass: %v", err)
	}
}
