package baseparser

import (
	"strings"
	"testing"
)

// TestRetiredConstructAnnotationRole is the #2709 bury gate (the #2631
// ruling): @role was documented-only, load-rejected, and never enforced --
// the vocabulary is deleted so no reader mistakes it for access control.
// The gate emits the pointed BURY message, not a generic unknown-annotation
// error.
func TestRetiredConstructAnnotationRole(t *testing.T) {
	allowed := map[string]bool{"description": true, "public": true}
	src := "@role(\"admin\")\nquery auditEvent queryAdminDashboard {\n  filter isActiveRecord\n}\n"
	err := ValidateConstructAnnotations(src, "query", allowed)
	if err == nil {
		t.Fatalf("@role must be rejected with the bury message (#2709)")
	}
	if !strings.Contains(err.Error(), "#2709") || !strings.Contains(err.Error(), "never enforced") {
		t.Errorf("rejection must carry the bury hint, got: %v", err)
	}
}
