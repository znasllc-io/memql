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
	want := []string{"constructId", "constructName", "distinctBindings", "dryEvidence", "procedureHash", "recordedFrom", "shadowMatches", "target", "title"}
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

// Every key is always there, even when the lift had no bindings, no
// provenance or no target to give: a card that shows a field on one promotion
// and omits it on the next reads as a different kind of decision.
func TestThePromotionSubjectAlwaysCarriesEveryKey(t *testing.T) {
	p := promotionProposal()
	p.DistinctBindings, p.RecordedFrom = nil, nil
	a := ProcedurePromotionApproval(p, t0)
	for _, k := range []string{"distinctBindings", "recordedFrom"} {
		m, ok := a.Subject[k].(map[string]any)
		if !ok || m == nil || len(m) != 0 {
			t.Fatalf("subject[%s] = %#v, want an empty object rather than an absent or null one", k, a.Subject[k])
		}
	}
	if got, ok := a.Subject["target"].(string); !ok || got != "" {
		t.Fatalf("subject[target] = %#v, want an empty string where no target was given", a.Subject["target"])
	}
	if got, ok := a.Subject["dryEvidence"].(bool); !ok || got {
		t.Fatalf("subject[dryEvidence] = %#v, want false where it was not set", a.Subject["dryEvidence"])
	}
}

// Where it would run is part of what a person approves: a canary on their own
// machine acts on their files, one in the workbench acts in a sandbox. And a
// machine-local procedure's shadow matches were DRY -- the calls it would make
// compared with the app's, nothing run -- which a person saying yes should
// know. A target that is absent, or one this build does not know, makes no
// claim about where; the subject carries what the question claimed, so the
// card and the question cannot disagree.
func TestThePromotionQuestionSaysWhereItWouldRunAndWhetherItHasRun(t *testing.T) {
	const (
		ask     = `Promote "Export last month's invoices" to canary? `
		machine = ask + "It matched the app 5 times, and would now run on your machine with the app standing by."
		bench   = ask + "It matched the app 5 times, and would now run in the workbench, a sandbox in your cluster, with the app standing by."
		nowhere = ask + "It matched the app 5 times beside it, and would now run for real with the app standing by."
		dry     = " Those matches compared the commands it would run with the app's own; it has not run by itself yet."
	)
	for _, tc := range []struct {
		name, target  string
		dryEvidence   bool
		want, subject string
	}{
		{"on the machine, compared dry", "machine", true, machine + dry, "machine"},
		{"on the machine", "machine", false, machine, "machine"},
		{"in the workbench", "workbench", false, bench, "workbench"},
		{"no target given", "", false, nowhere, ""},
		{"a target this build does not know", "Machine", false, nowhere, ""},
		{"no target given, compared dry", "", true, nowhere + dry, ""},
	} {
		p := promotionProposal()
		p.Target, p.DryEvidence = tc.target, tc.dryEvidence
		a := ProcedurePromotionApproval(p, t0)
		if a.Question != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, a.Question, tc.want)
		}
		if a.Subject["target"] != tc.subject || a.Subject["dryEvidence"] != tc.dryEvidence {
			t.Errorf("%s: subject target=%#v dryEvidence=%#v, want %q and %v", tc.name, a.Subject["target"], a.Subject["dryEvidence"], tc.subject, tc.dryEvidence)
		}
	}
}

// The count reads as English, the way usedPhrase spells a use count: an
// operator may set m to one, and "matched the app 1 times" is the sentence a
// reader stops at.
func TestThePromotionQuestionSpellsASingleMatchAsOnce(t *testing.T) {
	for _, target := range []string{"machine", "workbench", ""} {
		p := promotionProposal()
		p.Target, p.ShadowMatches = target, 1
		q := ProcedurePromotionApproval(p, t0).Question
		if !strings.Contains(q, "It matched the app once") || strings.Contains(q, "1 times") {
			t.Errorf("target %q: question %q, want the single match spelled once", target, q)
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

// The question is read by a person, not parsed by Go. A goal statement that
// carries its own quotation marks reads as it was written -- never with the
// backslashes Go's quoting adds -- and one that ran over several lines is one
// line in a question.
func TestThePromotionQuestionQuotesTheGoalAsAPersonWritesIt(t *testing.T) {
	for _, tc := range []struct{ title, want string }{
		{`Export the "Q3" invoices`, `Promote "Export the "Q3" invoices" to canary?`},
		{"Reconcile the ledger\nfor last month", `Promote "Reconcile the ledger for last month" to canary?`},
	} {
		a := ProcedurePromotionApproval(PromotionProposal{
			ConstructId: "c1", ConstructName: "learnedProcedure_abc_l1", ProcedureHash: "sha256:x",
			ShadowRunId: "r1", ShadowMatches: 5, Title: tc.title,
		}, time.Unix(0, 0))
		if !strings.HasPrefix(a.Question, tc.want) {
			t.Errorf("title %q: question %q, want it to open %q", tc.title, a.Question, tc.want)
		}
		if strings.ContainsAny(a.Question, "\\\n") {
			t.Errorf("title %q: question %q carries an escape or a line break", tc.title, a.Question)
		}
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

// TestThePromotionReasonReadsAtTheLowestValues: an operator may set m and k to
// one, and the evidence line a person reads must still be a sentence.
func TestThePromotionReasonReadsAtTheLowestValues(t *testing.T) {
	cases := []struct {
		p    PromotionProposal
		want string
	}{
		{PromotionProposal{ShadowMatches: 1}, "matched the app once in shadow, with no parameter to vary"},
		{PromotionProposal{ShadowMatches: 1, DistinctBindings: map[string]int{"h1": 1}}, "matched the app once in shadow, across at least 1 distinct binding of every parameter"},
		{PromotionProposal{ShadowMatches: 5, DistinctBindings: map[string]int{"h1": 2, "h2": 3}}, "matched the app 5 consecutive times in shadow, across at least 2 distinct bindings of every parameter"},
	}
	for _, c := range cases {
		if got := promotionReason(c.p); got != c.want {
			t.Errorf("promotionReason(%+v) = %q, want %q", c.p, got, c.want)
		}
	}
}
