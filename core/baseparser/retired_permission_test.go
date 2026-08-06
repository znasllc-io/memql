package baseparser

import (
	"strings"
	"testing"
)

// TestRetiredConstructAnnotationPermission is the #2713 bury gate (the #2631
// ruling's audited close-out): @permission was the @role twin -- documented,
// load-rejected, never enforced, its one help-payload reader dead -- so the
// vocabulary is deleted. The gate emits the pointed BURY message, not a
// generic unknown-annotation error.
func TestRetiredConstructAnnotationPermission(t *testing.T) {
	allowed := map[string]bool{"description": true, "public": true}
	src := "@permission(\"read:users\")\nquery user queryUserProfiles {\n  filter isActiveRecord\n}\n"
	err := ValidateConstructAnnotations(src, "query", allowed)
	if err == nil {
		t.Fatalf("@permission must be rejected with the bury message (#2713)")
	}
	if !strings.Contains(err.Error(), "#2713") || !strings.Contains(err.Error(), "never enforced") {
		t.Errorf("rejection must carry the bury hint, got: %v", err)
	}
}
