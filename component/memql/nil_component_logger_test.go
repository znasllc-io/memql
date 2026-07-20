package memql

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestQuarantineOnComponentLessEngine is the #2674 regression: Logger is
// promoted through the embedded *component.Component, so on the
// internal-test engine idiom (&MemQLEngine{...} with no Component) the
// guard `if e.Logger != nil` DEREFERENCES the nil Component and panics
// with SIGSEGV before the nil check can help. Production engines always
// carry a Component, so the blast radius was test-only -- but a panic in
// the quarantine path masks whatever failure routed a row there.
func TestQuarantineOnComponentLessEngine(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry(), loadReport: &LoadReport{}}

	// Must not panic, and must still record the quarantine entry.
	e.quarantineRehydratedConstruct(AuthoringConstructRow{
		Kind:        "query",
		Name:        "rottedQuery",
		BundleId:    "b1",
		OwnerUserId: "u1",
	}, errors.New("stored source failed to recompile"))

	if got := len(e.loadReport.Quarantined); got != 1 {
		t.Fatalf("quarantine entry count = %d, want 1", got)
	}
	if e.loadReport.Quarantined[0].Name != "rottedQuery" {
		t.Errorf("quarantined entry = %+v", e.loadReport.Quarantined[0])
	}
}

// promotedLoggerGuardRe finds a Logger nil-check reached THROUGH an
// engine value (`e.Logger`, `m.engine.Logger`) -- the promoted-field
// form that dereferences a possibly-nil embedded *component.Component.
// Direct struct fields (`deps.Logger` on AuthoredRuntimeDeps) are not
// promoted and are excluded by construction.
var promotedLoggerGuardRe = regexp.MustCompile(`\b(?:e|m\.engine)\.Logger != nil`)

// componentGuardedRe matches the correct form: the Component checked
// before the promoted Logger, anywhere earlier on the line.
var componentGuardedRe = regexp.MustCompile(`\b(?:e|m\.engine)\.Component != nil`)

// TestNoUnguardedPromotedLoggerChecks keeps the #2674 class out of the
// package: every `e.Logger != nil` guard must be preceded by an
// `e.Component != nil` check on the same line, or the guard itself
// panics on a Component-less engine.
func TestNoUnguardedPromotedLoggerChecks(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(".", name))
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if !promotedLoggerGuardRe.MatchString(line) {
				continue
			}
			if componentGuardedRe.MatchString(line) {
				continue
			}
			offenders = append(offenders, fmt.Sprintf("%s:%d: %s", name, i+1, strings.TrimSpace(line)))
		}
	}
	if len(offenders) > 0 {
		t.Errorf("%d unguarded promoted-Logger check(s) -- Logger is promoted through the embedded *component.Component, so this panics on a Component-less engine (#2674); guard with `e.Component != nil && e.Logger != nil`:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
