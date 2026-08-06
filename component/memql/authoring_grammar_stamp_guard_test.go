package memql

import (
	"context"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// memql#3089 DoD item 5: prove the grammar-stamp guard FIRES.
//
// The guard (authoring_promote_durable.go) exists so a durably-promoted row
// authored under an older grammar is quarantined with the migration command
// named, instead of surfacing whatever downstream parse error the stale source
// happens to produce -- and quarantines do not fail boot, so without the guard
// such a construct simply disappears on the next restart with an ERROR log and
// nothing else.
//
// It had never been tested, and it had also never fired: GrammarVersion did not
// move for six weeks across six grammar narrowings, so `row.GrammarVersion !=
// languageParser.GrammarVersion` was false every time. #3089 bumps the constant;
// these tests pin the behaviour that bump exists to enable, so the guard cannot
// go dormant again without something going red.
//
// No database: the stamp comparison is the first statement in
// recompileAndPromoteRow, and the quarantine path it defers to needs only the
// engine's load report.

// stampGuardEngine builds the minimum engine the guard path touches.
func stampGuardEngine() *MemQLEngine {
	return &MemQLEngine{
		functions:  newFunctionRegistry(),
		specs:      newSpecRegistry(),
		loadReport: newLoadReport(),
	}
}

// stampGuardRow is a row whose SOURCE is deliberately valid under the current
// grammar. That matters: if the source were invalid, a passing test could not
// distinguish "the stamp guard refused it" from "the recompile failed anyway",
// which is precisely the confusion the guard exists to remove.
func stampGuardRow(stamp string) AuthoringConstructRow {
	return AuthoringConstructRow{
		Id:              "v1:authoring:construct:stamp-probe",
		OwnerUserId:     "v1:identity:user:probe",
		BundleId:        "v1:authoring:bundle:probe",
		Kind:            "query",
		Name:            "stampProbe",
		TargetNamespace: "probe",
		Source:          "query space stampProbe {\n  filter x==1\n}\n",
		Status:          "active",
		GrammarVersion:  stamp,
	}
}

func TestGrammarStampGuardFiresOnAMismatchedStamp(t *testing.T) {
	e := stampGuardEngine()
	row := stampGuardRow("2026.01-some-older-epoch")

	err := e.recompileAndPromoteRow(context.Background(), row)
	if err == nil {
		t.Fatal("a row stamped with an older grammar epoch was accepted. The stamp guard is the " +
			"only thing that turns 'this row predates a grammar move' into an actionable " +
			"diagnostic; without it the stale source produces a raw parse error and the " +
			"construct vanishes on the next restart (memql#3089).")
	}

	msg := err.Error()
	// The diagnostic has to carry BOTH versions and the remedy. An error that
	// merely says "mismatch" leaves the operator exactly where the raw parse
	// error left them, which is the whole point of the guard.
	for _, want := range []string{
		"2026.01-some-older-epoch",    // what the row was authored under
		languageParser.GrammarVersion, // what this engine runs
		"memqlmigrate --rewrite",      // the remedy, named
		"stampProbe",                  // which construct
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the quarantine diagnostic must contain %q, or it is not actionable.\n  got: %v", want, msg)
		}
	}

	// And it must quarantine rather than fail the walk -- one rotted stored row
	// must not brick a fleet.
	if got := len(e.loadReport.Quarantined); got != 1 {
		t.Errorf("expected exactly 1 quarantine entry, got %d -- the guard returned an error but "+
			"the row was not recorded, so an operator has an ERROR log and no structured signal", got)
	}
}

func TestGrammarStampGuardAcceptsTheCurrentStamp(t *testing.T) {
	// The converse, and the one that makes the test above mean something: a row
	// stamped with the CURRENT grammar must not be refused by the stamp check.
	// Without this, a guard that refused everything would pass the mismatch test
	// while bricking every durable row.
	e := stampGuardEngine()
	row := stampGuardRow(languageParser.GrammarVersion)

	err := e.recompileAndPromoteRow(context.Background(), row)
	if err != nil && strings.Contains(err.Error(), "was authored under grammar") {
		t.Fatalf("a row stamped with the CURRENT grammar was refused by the stamp guard: %v", err)
	}
	// Any other error is out of scope here -- this test pins the stamp
	// comparison, not the whole recompile pipeline.
}

func TestGrammarStampGuardSkipsLegacyUnstampedRows(t *testing.T) {
	// Legacy rows predate the stamp and carry "". They must proceed to the
	// recompile, which decides -- refusing them would quarantine every row
	// written before #2361 on the first boot after this change.
	e := stampGuardEngine()
	row := stampGuardRow("")

	err := e.recompileAndPromoteRow(context.Background(), row)
	if err != nil && strings.Contains(err.Error(), "was authored under grammar") {
		t.Fatalf("an unstamped legacy row was refused by the stamp guard: %v", err)
	}
}

// TestGrammarStampGuardWouldHaveBeenDormant is the regression that ties this
// file to memql#3089's actual cause.
//
// The guard was correct code that could never fire, because the constant it
// compares against had not moved in six weeks of grammar narrowings. A test
// asserting the guard works on a synthetic mismatch would have passed
// throughout that period and told nobody anything.
//
// So this asserts the property that was actually violated: the shipped
// GrammarVersion is not the value it was stuck at while the narrowings landed.
// If a future change reverts the constant, this fires and names the reason.
func TestGrammarStampGuardWouldHaveBeenDormant(t *testing.T) {
	const stuckAt = "2026.08-doc-comment-descriptions"
	if languageParser.GrammarVersion == stuckAt {
		t.Fatalf("GrammarVersion is back to %q, the value it was stuck at from 2026-07-21 while six "+
			"grammar narrowings landed unbumped (memql#3089). Every durably-promoted row authored "+
			"under any of those narrowings then rehydrates with an equal stamp, the guard does not "+
			"fire, and the construct disappears on the next restart with a raw parse error. If a "+
			"revert is deliberate, this test is the thing to argue with -- not to delete.", stuckAt)
	}
}
