package work

// The one human approval on the ladder (epic memql#5408, task memql#5410;
// design record D3, D15).

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func promotionProposal() PromotionProposal {
	return PromotionProposal{
		OwnerUserId:      "v1:identity:user:u1",
		ConstructId:      "v1:authoring:construct:c1",
		ConstructName:    "exportInvoices",
		ProcedureHash:    "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		ShadowRunId:      "v1:work:run:r9",
		ShadowMatches:    5,
		DistinctBindings: map[string]int{"s0.path": 2, "s1.month": 3},
		RecordedFrom: map[string]any{
			"app": "claude-code", "model": "claude-x", "effort": "high",
			"sessionIds": []string{"sess1", "sess2"}, "runIds": []string{"v1:work:run:r1", "v1:work:run:r2"},
		},
		Title: "Export last month's invoices",
	}
}

// D15: "the one human approval is a v1:work:approval of kind
// procedurePromotion whose artifact hash pins the construct version". The
// hash IS the construct's procedureHash -- over its source, its template and
// its preconditions -- so resume refuses when any of the three changed after
// the person said yes, and a changed procedure is a new candidate rather than
// something approved in its absence. The approval names the shadow run whose
// comparison met the threshold, because every approval but the routing
// review's parks or points at a run.
func TestProcedurePromotionApprovalPinsTheProcedureHash(t *testing.T) {
	p := promotionProposal()
	a := ProcedurePromotionApproval(p, t0)
	// The literal, because it is the value v1:work:approval.kind's enum must
	// carry: a constant renamed in Go and not in the DSL is refused at write.
	if a.Kind != "procedurePromotion" {
		t.Fatalf("kind = %q, want procedurePromotion", a.Kind)
	}
	if a.ArtifactHash != p.ProcedureHash {
		t.Fatalf("ArtifactHash = %q, want the construct version %q", a.ArtifactHash, p.ProcedureHash)
	}
	if a.RunId != p.ShadowRunId {
		t.Fatalf("RunId = %q, want the shadow run %q", a.RunId, p.ShadowRunId)
	}
	if !a.RequestedAt.Equal(t0) {
		t.Fatalf("RequestedAt = %v, want %v", a.RequestedAt, t0)
	}
	if !a.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt = %v: a promotion waits for its person; the procedure stays in shadow meanwhile", a.ExpiresAt)
	}

	var keys []string
	for k := range a.Subject {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"constructId", "constructName", "distinctBindings", "procedureHash", "recordedFrom", "shadowMatches", "title"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("subject keys = %v, want exactly %v", keys, want)
	}
	if a.Subject["constructId"] != p.ConstructId || a.Subject["constructName"] != p.ConstructName ||
		a.Subject["procedureHash"] != p.ProcedureHash || a.Subject["title"] != p.Title || a.Subject["shadowMatches"] != 5 {
		t.Fatalf("subject does not carry the proposal: %+v", a.Subject)
	}
	if got := a.Subject["distinctBindings"]; !reflect.DeepEqual(got, map[string]any{"s0.path": 2, "s1.month": 3}) {
		t.Fatalf("distinctBindings = %#v, want the per-parameter counts", got)
	}
	if got := a.Subject["recordedFrom"].(map[string]any)["app"]; got != "claude-code" {
		t.Fatalf("recordedFrom.app = %v: the approval must say where the procedure was recorded from (D9)", got)
	}
	if err := ValidateApprovalKind(a); err != nil {
		t.Fatalf("the built approval must pass the writer's own check: %v", err)
	}
}

// Every one of the seven keys is always there, even when the lift had no
// bindings or no provenance to give: a card that shows a field on one
// promotion and omits it on the next reads as a different kind of decision.
func TestThePromotionSubjectAlwaysCarriesItsSevenKeys(t *testing.T) {
	p := promotionProposal()
	p.DistinctBindings, p.RecordedFrom = nil, nil
	a := ProcedurePromotionApproval(p, t0)
	for _, k := range []string{"distinctBindings", "recordedFrom"} {
		m, ok := a.Subject[k].(map[string]any)
		if !ok || m == nil || len(m) != 0 {
			t.Fatalf("subject[%s] = %#v, want an empty object rather than an absent or null one", k, a.Subject[k])
		}
	}
}

// The subject is a copy. The ladder keeps working on the maps it proposed
// from, and an approval row whose evidence changed after it was raised would
// show a person figures they were never asked about.
func TestThePromotionSubjectDoesNotShareTheProposalsMaps(t *testing.T) {
	p := promotionProposal()
	a := ProcedurePromotionApproval(p, t0)
	p.DistinctBindings["s0.path"] = 99
	p.RecordedFrom["app"] = "someone-else"
	if got := a.Subject["distinctBindings"].(map[string]any)["s0.path"]; got != 2 {
		t.Fatalf("the approval's distinctBindings followed the proposal to %v", got)
	}
	if got := a.Subject["recordedFrom"].(map[string]any)["app"]; got != "claude-code" {
		t.Fatalf("the approval's recordedFrom followed the proposal to %v", got)
	}
}

// Approve or keep in shadow, and no third answer. "Not now" would decide
// nothing and leave the same evidence proposing again; a decline is recorded
// and spends the streak (ladder.go), which is what makes it final.
func TestThePromotionOffersApproveAndDeclineAndNoThird(t *testing.T) {
	a := ProcedurePromotionApproval(promotionProposal(), t0)
	if len(a.Options) != 2 {
		t.Fatalf("expected two options, got %d: %+v", len(a.Options), a.Options)
	}
	values := map[any]bool{}
	for _, o := range a.Options {
		values[o["value"]] = true
		if o["label"] == "" || o["label"] == nil {
			t.Fatalf("an option without a label: %+v", o)
		}
	}
	if !values["approved"] || !values["rejected"] {
		t.Fatalf("the options must be approve and decline, got %+v", a.Options)
	}
	if a.Question == "" {
		t.Fatal("the approval must put a question to the person")
	}
}

// ValidateApprovalKind already requires a run for every kind but the routing
// review's; the promotion carries the shadow run, and one without it is
// refused like any other kind that names no run.
func TestValidateApprovalKindAcceptsAProcedurePromotionWithARun(t *testing.T) {
	if err := ValidateApprovalKind(ApprovalRequest{Kind: ApprovalKindProcedurePromotion, RunId: "v1:work:run:r9"}); err != nil {
		t.Fatalf("a promotion naming its shadow run was refused: %v", err)
	}
	err := ValidateApprovalKind(ApprovalRequest{Kind: ApprovalKindProcedurePromotion})
	if !errors.Is(err, ErrApprovalNeedsRun) {
		t.Fatalf("a promotion naming no run: err = %v, want ErrApprovalNeedsRun", err)
	}
	p := promotionProposal()
	p.ShadowRunId = ""
	if err := ValidateApprovalKind(ProcedurePromotionApproval(p, time.Now())); !errors.Is(err, ErrApprovalNeedsRun) {
		t.Fatalf("a promotion built with no shadow run must be refused by the writer's check; got %v", err)
	}
}

// TestThePromotionQuestionNamesTheGoalNotTheConstruct: the question is what
// a person reads in their inbox, and a learned procedure's construct name is
// derived from a digest (learnedProcedure_<sig>_l1). The goal it serves is
// the name they recognise; the construct name is only the fallback.
func TestThePromotionQuestionNamesTheGoalNotTheConstruct(t *testing.T) {
	cases := []struct {
		title, name, want, never string
	}{
		{"Reconcile the ledger", "learnedProcedure_abc_l1", `"Reconcile the ledger"`, "learnedProcedure_abc_l1"},
		{"", "learnedProcedure_abc_l1", "learnedProcedure_abc_l1", `""`},
		{"  ", "", "this learned procedure", `""`},
	}
	for _, tc := range cases {
		a := ProcedurePromotionApproval(PromotionProposal{
			OwnerUserId: "u1", ConstructId: "c1", ConstructName: tc.name, ProcedureHash: "sha256:x",
			ShadowRunId: "r1", ShadowMatches: 5, Title: tc.title,
		}, time.Unix(0, 0))
		if !strings.Contains(a.Question, tc.want) {
			t.Errorf("title %q, name %q: question %q does not name %s", tc.title, tc.name, a.Question, tc.want)
		}
		if strings.Contains(a.Question, tc.never) {
			t.Errorf("title %q, name %q: question %q names %s", tc.title, tc.name, a.Question, tc.never)
		}
	}
}
