package memql

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// strayLineStoredTraitSource is a stored trait whose source carries a line the
// grammar it was stored under accepted and read nothing from: an annotation
// no receiver takes. memql#5359 refuses it at parse (annotation_unknown), so a
// durably promoted row carrying one stops compiling at re-hydration.
const strayLineStoredTraitSource = "@bogusStoredAnnotation\ntrait storedStrayLine = row => row.status == \"active\"\n"

// TestRehydrationNamesAStoredRowARuleRefuses (memql#5426): a durably promoted
// row that a named rule of the language now refuses is quarantined with a
// report that names the row by its id, bundle and owner, the rule, and what to
// do -- delete a line the stored grammar never read, correct and re-promote,
// or demote -- instead of the stamp guard's "run memqlmigrate --rewrite",
// which has no rewrite for a stray line. The quarantine entry carries the id
// and the rule too.
func TestRehydrationNamesAStoredRowARuleRefuses(t *testing.T) {
	eng := stampTestEngine(t)
	row := AuthoringConstructRow{
		Id:             "v1:authoring:construct:stray1",
		Kind:           "trait",
		Name:           "storedStrayLine",
		BundleId:       "authoring:bundle:stray1",
		OwnerUserId:    "u-owner",
		Source:         strayLineStoredTraitSource,
		Origin:         "trainingns/concepts.memql",
		Status:         "active",
		GrammarVersion: "2026.07-some-earlier-epoch",
	}
	err := eng.recompileAndPromoteRow(context.Background(), row)
	var refusal *RehydrationRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("want a *RehydrationRefusal, got %v", err)
	}
	if refusal.RuleCode() != "annotation_unknown" {
		t.Errorf("code = %q, want the parser's annotation_unknown", refusal.RuleCode())
	}
	msg := err.Error()
	for _, want := range []string{
		`authored trait "storedStrayLine" (row v1:authoring:construct:stray1, bundle authoring:bundle:stray1, owner u-owner)`,
		`was stored under grammar "2026.07-some-earlier-epoch"`,
		"unknown annotation @bogusStoredAnnotation",
		"is deleted with no change in what the construct does",
		"promote the bundle again, or demote the construct",
		"durable authored construct quarantined at re-hydration",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the report does not say %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "for the intervening grammar epics") {
		t.Errorf("a named rule's refusal must not be sent to a rewrite that does not exist:\n%s", msg)
	}
	if n := len(eng.loadReport.Quarantined); n != 1 {
		t.Fatalf("want one quarantine entry, got %d", n)
	}
	q := eng.loadReport.Quarantined[0]
	if q.Id != row.Id || q.Code != "annotation_unknown" {
		t.Errorf("the quarantine entry does not carry the row id and the rule: %+v", q)
	}
	if eng.loadReport.HasProblems() {
		t.Error("a quarantined stored row must not become a strict-boot problem")
	}
}
