package memql

// grammar_stamp_guard_3089_test.go -- memql#3089.
//
// The durable-rehydration grammar-stamp guard, proven to FIRE, and proven not to
// destroy anything on the way.
//
// Both halves are needed because the guard had two states and neither was the
// intended one:
//
//   - BEFORE any of this, the stamp never moved (GrammarVersion sat unbumped
//     across six grammar moves), so `row.GrammarVersion != GrammarVersion` was
//     always false and the guard was dead. A row with stale source surfaced the
//     raw parse error and vanished on the next restart with an ERROR log --
//     exactly the failure mode the guard was built to prevent.
//   - The parked attempt (PR #3134) bumped the version, and because the stamp was
//     compared BEFORE any compile was attempted, every durable
//     v1:authoring:construct row carrying the prior stamp was unregistered on
//     first boot even though its source still parsed. That is worse than a dead
//     guard: a bump silently deleted working constructs.
//
// The fix inverts the ordering -- compile first, and let a stale stamp EXPLAIN a
// failure rather than cause one. These tests pin both consequences.

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// stampTestEngine builds an engine with a discard logger and a fresh load report,
// matching TestRehydrationQuarantine's setup.
func stampTestEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	eng, err := New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	eng.loadReport = newLoadReport()
	// PromoteAuthoredConstruct registers into the shared spec registry, which
	// engine_bootstrap.go populates on a real boot. Without it the promote fails
	// with "spec registry is not initialized" -- which would make the
	// survives-a-bump assertion below unable to distinguish "the stamp guard
	// blocked it" from "the harness has no registry".
	eng.specs = newSpecRegistry()
	return eng
}

// A stored spec whose source is VALID under the current grammar.
const validStoredSpecSource = "spec activeRowTrait storedValidSpec {\n  return status == \"active\"\n}\n"

// A stored spec whose source does NOT compile under the current grammar -- the
// "row predates a grammar move" rot case.
const rottedStoredSpecSource = "spec activeRowTrait storedRottedSpec {\n  return status ==== \"x\" &&&& true\n}\n"

// TestStampGuard_ValidStoredConstructSurvivesABump is the destructive-bump
// regression, and it is the reason the ordering was inverted rather than worked
// around with an epoch allowlist or a restamp pass.
//
// A row stamped with a PRIOR grammar whose source still compiles must register.
// Under the old ordering this returned an error before the compiler ever ran, so
// a GrammarVersion bump unregistered every stored construct in the fleet.
func TestStampGuard_ValidStoredConstructSurvivesABump(t *testing.T) {
	eng := stampTestEngine(t)

	row := AuthoringConstructRow{
		Kind:        "spec",
		Name:        "storedValidSpec",
		BundleId:    "authoring:bundle:stamp1",
		OwnerUserId: "u-owner",
		Source:      validStoredSpecSource,
		Status:      "active",
		// Deliberately a PRIOR epoch: this is the state every durable row in a
		// running deployment is in the moment the engine's constant moves.
		GrammarVersion: "2026.07-some-earlier-epoch",
	}

	if err := eng.recompileAndPromoteRow(context.Background(), row); err != nil {
		t.Fatalf("a stored construct whose source STILL COMPILES must not be lost to a grammar bump.\n"+
			"  The stamp is a diagnosis, not a gate -- comparing it before attempting the compile is\n"+
			"  what made a bump destructive (memql#3089). got: %v", err)
	}
	if n := len(eng.loadReport.Quarantined); n != 0 {
		t.Errorf("a valid stored construct must not be quarantined by a stale stamp, got %d entries: %+v", n, eng.loadReport.Quarantined)
	}
	if _, live := eng.specs.Lookup("storedValidSpec"); !live {
		t.Error("the construct compiled and was not quarantined, so it must be registered and callable")
	}
}

// TestStampGuard_FiresOnAStaleStampWithRottedSource is the guard doing its job:
// the quarantine names the migration command instead of surfacing whatever
// downstream parse error the stale source produced.
func TestStampGuard_FiresOnAStaleStampWithRottedSource(t *testing.T) {
	eng := stampTestEngine(t)

	row := AuthoringConstructRow{
		Kind:           "spec",
		Name:           "storedRottedSpec",
		BundleId:       "authoring:bundle:stamp2",
		OwnerUserId:    "u-owner",
		Source:         rottedStoredSpecSource,
		Status:         "active",
		GrammarVersion: "2026.07-some-earlier-epoch",
	}

	err := eng.recompileAndPromoteRow(context.Background(), row)
	if err == nil {
		t.Fatal("a stored row whose source no longer compiles must be quarantined")
	}
	msg := err.Error()
	for _, want := range []string{
		"authored under grammar",
		"2026.07-some-earlier-epoch",
		languageParser.GrammarVersion,
		"memqlmigrate",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the quarantine reason must name the grammar mismatch and the migration command; missing %q in: %s", want, msg)
		}
	}
	// The underlying compile error is WRAPPED, not replaced. The migration
	// command is the headline because it is the actionable part, but an operator
	// still needs to see what actually failed -- the parked attempt's version
	// returned before compiling and so had nothing to show.
	if !strings.Contains(msg, "recompile") {
		t.Errorf("the underlying recompile failure must be retained alongside the diagnosis, got: %s", msg)
	}

	if n := len(eng.loadReport.Quarantined); n != 1 {
		t.Fatalf("expected exactly 1 quarantine entry, got %d: %+v", n, eng.loadReport.Quarantined)
	}
	if !strings.Contains(eng.loadReport.Quarantined[0].Err, "memqlmigrate") {
		t.Errorf("the RECORDED quarantine reason (not just the returned error) must name the migration command, got: %s", eng.loadReport.Quarantined[0].Err)
	}
	// Unchanged posture: a rotted stored row must never fail strict boot.
	if eng.loadReport.HasProblems() {
		t.Error("a quarantined stored row must not become a strict-boot problem")
	}
}

// TestStampGuard_CurrentStampKeepsItsOwnError is the other side of the
// diagnosis. A row stamped with the CURRENT grammar that fails to compile has a
// real defect in its source or a missing dependency -- blaming a grammar move
// that did not happen would point the reader somewhere useless.
func TestStampGuard_CurrentStampKeepsItsOwnError(t *testing.T) {
	eng := stampTestEngine(t)

	err := eng.recompileAndPromoteRow(context.Background(), AuthoringConstructRow{
		Kind:           "spec",
		Name:           "storedRottedSpec",
		BundleId:       "authoring:bundle:stamp3",
		OwnerUserId:    "u-owner",
		Source:         rottedStoredSpecSource,
		Status:         "active",
		GrammarVersion: languageParser.GrammarVersion,
	})
	if err == nil {
		t.Fatal("a stored row whose source does not compile must still be quarantined")
	}
	if strings.Contains(err.Error(), "memqlmigrate") {
		t.Errorf("a row stamped with the CURRENT grammar must not be blamed on a grammar move -- no migration will fix it. got: %v", err)
	}
	if !strings.Contains(err.Error(), "recompile") {
		t.Errorf("it must keep its own compile error, got: %v", err)
	}
}

// TestStampGuard_UnstampedLegacyRowIsNotBlamed covers the pre-#2361 rows. With
// no stamp, nothing is known about which grammar the source was written against,
// so naming a migration would be a guess.
func TestStampGuard_UnstampedLegacyRowIsNotBlamed(t *testing.T) {
	eng := stampTestEngine(t)

	err := eng.recompileAndPromoteRow(context.Background(), AuthoringConstructRow{
		Kind:        "spec",
		Name:        "storedRottedSpec",
		BundleId:    "authoring:bundle:stamp4",
		OwnerUserId: "u-owner",
		Source:      rottedStoredSpecSource,
		Status:      "active",
		// No GrammarVersion: a legacy row.
	})
	if err == nil {
		t.Fatal("a rotted legacy row must still be quarantined")
	}
	if strings.Contains(err.Error(), "authored under grammar") {
		t.Errorf("an UNSTAMPED row must not be given a grammar diagnosis -- there is no stamp to compare. got: %v", err)
	}

	// And an unstamped row whose source is VALID must register, unchanged.
	eng2 := stampTestEngine(t)
	if err := eng2.recompileAndPromoteRow(context.Background(), AuthoringConstructRow{
		Kind:        "spec",
		Name:        "storedValidSpec",
		BundleId:    "authoring:bundle:stamp5",
		OwnerUserId: "u-owner",
		Source:      validStoredSpecSource,
		Status:      "active",
	}); err != nil {
		t.Fatalf("an unstamped legacy row with valid source must register: %v", err)
	}
}

// TestGrammarVersionActuallyMoved is the small factual assertion the whole issue
// turns on: the constant is no longer the value it was stuck at.
//
// It is not a tautology. The stamp guard is only meaningful if the engine's
// grammar epoch is capable of differing from a stored row's, and for six weeks it
// was not -- the comparison was structurally always-equal. If the constant is
// ever reverted to the stuck value, the guard is dead again and this says so.
func TestGrammarVersionActuallyMoved(t *testing.T) {
	const stuck = "2026.08-doc-comment-descriptions"
	if languageParser.GrammarVersion == stuck {
		t.Errorf("GrammarVersion is back to %q, the value it was stuck at from 2026-07-21 through at least six grammar moves. The stamp guard cannot fire while the engine's epoch never differs from a stored row's (memql#3089).", stuck)
	}
}
