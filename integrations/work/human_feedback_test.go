package work

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/compose/emaildoc"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

func TestExpiredScopeIsRenewedOnceAndAnActiveGrantIsReused(t *testing.T) {
	i, eng := newTestIntegration(t)
	// Production locks by key, every key of one call in one acquisition; the
	// renewal's locks must not be replaced by a single process mutex here.
	locks := map[string]*sync.Mutex{}
	var mutex sync.Mutex
	i.decisionGate = func(_ context.Context, keys ...string) (func(), error) {
		var held []*sync.Mutex
		for _, key := range keys {
			mutex.Lock()
			m := locks[key]
			if m == nil {
				m = &sync.Mutex{}
				locks[key] = m
			}
			mutex.Unlock()
			m.Lock()
			held = append(held, m)
		}
		return func() {
			for n := len(held) - 1; n >= 0; n-- {
				held[n].Unlock()
			}
		}, nil
	}
	ctx := common.ContextWithRun(callerContext("u-alice"), common.RunContext{RunId: "r1", GoalId: "g1", OwnerUserId: "u-alice", StepKey: "reason"})
	eng.reply("workRunForOwner", map[string]any{"id": "r1", "ownerUserId": "u-alice", "status": "running"})
	subject := map[string]any{"scope": "observe", "summary": "Read the requested file", "agentId": "agent"}
	id, err := i.AskComputerScope(ctx, "u-alice", "r1", subject)
	if err != nil {
		t.Fatal(err)
	}
	eng.replyWhen("workApprovalForOwner", id, map[string]any{"id": id, "decision": "approved", "expiresAt": testNow.Add(-time.Second).Format(time.RFC3339Nano)})
	renewed, err := i.AskComputerScope(ctx, "u-alice", "r1", subject)
	if err != nil || renewed == id {
		t.Fatalf("expired grant reused: %s %v", renewed, err)
	}
	eng.replyWhen("workApprovalForOwner", renewed, map[string]any{"id": renewed, "decision": "approved", "expiresAt": testNow.Add(time.Hour).Format(time.RFC3339Nano)})
	again, err := i.AskComputerScope(ctx, "u-alice", "r1", subject)
	if err != nil || again != renewed || len(eng.callsTo("createWorkApproval")) != 2 {
		t.Fatalf("grant renewal duplicated: %s %v", again, err)
	}
}
func TestFeedbackValidatesRecordedAnswersAgainstTheQuestion(t *testing.T) {
	options := []map[string]any{{"label": "PDF", "value": "pdf"}, {"label": "Text", "value": "txt"}}
	for _, tc := range []struct {
		kind, decision string
		answer         map[string]any
		valid          bool
	}{
		{"choice", "answered", map[string]any{"text": "A spreadsheet instead"}, true},
		{"multi", "answered", map[string]any{"text": "A spreadsheet instead"}, true},
		{"choice", "answered", map[string]any{"text": "Other", "value": "invented"}, false},
		{"text", "answered", map[string]any{"text": "PDF please"}, true}, {"text", "answered", map[string]any{"text": " "}, false},
		{"choice", "answered", map[string]any{"value": "pdf"}, true}, {"choice", "approved", nil, false}, {"choice", "answered", map[string]any{"value": "invented"}, false},
		{"multi", "answered", map[string]any{"values": []string{"pdf", "txt"}}, true}, {"multi", "answered", map[string]any{"values": []string{"pdf", "pdf"}}, false},
	} {
		err := validateFeedbackAnswer(map[string]any{"kind": tc.kind, "options": options}, tc.decision, tc.answer)
		if (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	i, eng := newTestIntegration(t)
	subject := map[string]any{"scope": "full"}
	eng.reply("workApprovalsForOwner", map[string]any{"id": "q", "runId": "r", "ownerUserId": "u-alice", "kind": "scopeElevation", "subject": subject, "artifactHash": workstate.ArtifactHash(subject), "expiresAt": testNow.Add(-time.Second).Format(time.RFC3339Nano)})
	if _, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{"approvalId": "q", "decision": "approved"}, 0); err == nil || len(eng.callsTo("decideWorkApproval")) != 0 {
		t.Fatal("expired computer access was approved")
	}
}

// replyFirst answers `name` with these rows when the rendered query contains
// `contains`, ahead of every reply added before: a row that changed.
func (e *recordingEngine) replyFirst(name, contains string, rows ...map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.conditional = append([]conditionalReply{{name: name, contains: contains, rows: rows}}, e.conditional...)
}

// TestARenewalLocksItsChainInOneCallAndWalksItAgainUnderTheLocks: the chain's
// receipts are locked with ONE call of the decision gate, in chain order, and the
// chain is walked again under them. A grant that expired between the two walks
// takes the chain past the keys held, and the renewal walks and locks it again
// rather than park on a receipt it does not hold.
func TestARenewalLocksItsChainInOneCallAndWalksItAgainUnderTheLocks(t *testing.T) {
	i, eng := newTestIntegration(t)
	subject := map[string]any{"scope": "observe", "summary": "Read the requested file", "agentId": "agent"}
	chain := renewalChainIDs("r1", "reason", subject, 2)
	eng.reply("workRunForOwner", map[string]any{"id": "r1", "ownerUserId": "u-alice", "status": "running"})
	eng.replyWhen("workApprovalForOwner", chain[0], map[string]any{"id": chain[0], "decision": "approved", "expiresAt": testNow.Add(time.Second).Format(time.RFC3339Nano)})
	var calls [][]string
	i.decisionGate = func(_ context.Context, keys ...string) (func(), error) {
		calls = append(calls, append([]string(nil), keys...))
		if len(calls) == 1 {
			// The grant expires while the first walk's locks are being taken.
			eng.replyFirst("workApprovalForOwner", chain[0], map[string]any{"id": chain[0], "decision": "approved", "expiresAt": testNow.Add(-time.Second).Format(time.RFC3339Nano)})
		}
		return func() {}, nil
	}
	ctx := common.ContextWithRun(callerContext("u-alice"), common.RunContext{RunId: "r1", GoalId: "g1", OwnerUserId: "u-alice", StepKey: "reason"})

	got, err := i.AskComputerScope(ctx, "u-alice", "r1", subject)

	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || !slices.Equal(calls[0], chain[:1]) || !slices.Equal(calls[1], chain) {
		t.Fatalf("the decision gate was called with %v, want [%v] and then %v in one call", calls, chain[:1], chain)
	}
	if got != chain[1] {
		t.Errorf("the renewal answered %s, want the successor %s", got, chain[1])
	}
	if creates := eng.callsTo("createWorkApproval"); len(creates) != 1 || !strings.Contains(creates[0].Query, chain[1]) {
		t.Errorf("the renewal created %v, want exactly the successor", creates)
	}
}

// TestARenewalWhoseChainKeepsGrowingGivesUp: a chain that is longer under its
// locks every time it is walked is not chased for ever; the renewal stops and asks
// for another try, having written nothing.
func TestARenewalWhoseChainKeepsGrowingGivesUp(t *testing.T) {
	i, eng := newTestIntegration(t)
	subject := map[string]any{"scope": "observe", "summary": "Read the requested file", "agentId": "agent"}
	chain := renewalChainIDs("r1", "reason", subject, renewalLockAttempts+2)
	eng.reply("workRunForOwner", map[string]any{"id": "r1", "ownerUserId": "u-alice", "status": "running"})
	expired := func(id string) map[string]any {
		return map[string]any{"id": id, "decision": "approved", "expiresAt": testNow.Add(-time.Second).Format(time.RFC3339Nano)}
	}
	eng.replyWhen("workApprovalForOwner", chain[0], map[string]any{"id": chain[0], "decision": "approved", "expiresAt": testNow.Add(time.Second).Format(time.RFC3339Nano)})
	calls := 0
	i.decisionGate = func(_ context.Context, keys ...string) (func(), error) {
		calls++
		// The last receipt the walk reached expires, and the one after it appears
		// already expired: every walk under the locks is one longer.
		last := keys[len(keys)-1]
		eng.replyFirst("workApprovalForOwner", last, expired(last))
		return func() {}, nil
	}
	ctx := common.ContextWithRun(callerContext("u-alice"), common.RunContext{RunId: "r1", GoalId: "g1", OwnerUserId: "u-alice", StepKey: "reason"})

	_, err := i.AskComputerScope(ctx, "u-alice", "r1", subject)

	if err == nil || !strings.Contains(err.Error(), "try again") {
		t.Fatalf("a chain that kept growing answered %v, want to be asked again", err)
	}
	if calls != renewalLockAttempts {
		t.Errorf("the chain was locked %d times, want %d", calls, renewalLockAttempts)
	}
	if len(eng.callsTo("createWorkApproval")) != 0 || len(eng.callsTo("updateWorkRun")) != 0 {
		t.Errorf("a renewal that gave up wrote: %s", eng.summary())
	}
}

// askingRun is a run of u-alice's at step "reason" that may park on a question.
func askingRun(t *testing.T) (*Integration, *recordingEngine, context.Context) {
	t.Helper()
	i, eng := newTestIntegration(t)
	eng.reply("workRunForOwner", map[string]any{"id": "r1", "ownerUserId": "u-alice", "status": "running"})
	ctx := common.ContextWithRun(callerContext("u-alice"), common.RunContext{RunId: "r1", GoalId: "g1", OwnerUserId: "u-alice", StepKey: "reason"})
	return i, eng, ctx
}

// gridWireframe is a built-in layout as every picker draws it: the layout
// grid, and a choice of layouts in Ask (emaildoc.BuiltinWireframe).
func gridWireframe(t *testing.T, id string) string {
	t.Helper()
	svg, err := emaildoc.BuiltinWireframe(id)
	if err != nil {
		t.Fatal(err)
	}
	return svg
}

// storedOptions are the options a question was stored with, as maps.
func storedOptions(t *testing.T, approval map[string]any, key string) []map[string]any {
	t.Helper()
	raw, _ := approval[key].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		option, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s holds %v, want objects", key, approval[key])
		}
		out = append(out, option)
	}
	return out
}

// TestFeedbackLayoutsPresentationRequiresBuiltinLayoutIDs: a choice of layouts
// offers built-in layouts and nothing else. Every refusal names what to fix and
// writes nothing, so no question the server could not draw reaches a person.
func TestFeedbackLayoutsPresentationRequiresBuiltinLayoutIDs(t *testing.T) {
	choice := func(values ...string) []map[string]any {
		out := []map[string]any{}
		for _, value := range values {
			out = append(out, map[string]any{"label": "Layout " + value, "value": value})
		}
		return out
	}
	for _, tc := range []struct {
		name, kind, presentation string
		options                  []map[string]any
		want                     string
	}{
		{"an unknown built-in", "choice", "layouts", choice("builtin:article", "builtin:nope"), "builtin:nope"},
		{"a bare layout name", "choice", "layouts", choice("article"), `"article"`},
		{"an organization layout", "choice", "layouts", choice("builtin:letter", "org:abc123"), "org:abc123"},
		{"several answers", "multi", "layouts", choice("builtin:article", "builtin:letter"), "kind choice"},
		{"a written answer", "text", "layouts", nil, "kind choice"},
		{"an unknown presentation", "choice", "grid", choice("builtin:article"), "presentation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, eng, ctx := askingRun(t)
			_, err := i.AskFeedbackPresented(ctx, "u-alice", "r1", "Which layout should the email use?", tc.kind, tc.presentation, tc.options, time.Time{}, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("asked with %v; want a refusal naming %s", err, tc.want)
			}
			if len(eng.callsTo("createWorkApproval")) != 0 || len(eng.callsTo("updateWorkRun")) != 0 {
				t.Errorf("a refused question wrote: %s", eng.summary())
			}
		})
	}

	i, eng, ctx := askingRun(t)
	if _, err := i.AskFeedbackPresented(ctx, "u-alice", "r1", "Which layout should the email use?", "choice", "layouts", choice("builtin:article", "builtin:three-reasons", "builtin:letter"), time.Time{}, false); err != nil {
		t.Fatalf("a choice of three built-in layouts was refused: %v", err)
	}
	if len(eng.callsTo("createWorkApproval")) != 1 {
		t.Fatalf("a choice of built-in layouts raised %s, want one question", eng.summary())
	}
}

// TestFeedbackLayoutsPresentationAddsServerWireframes: the server draws each
// layout option's wireframe as the layout grid draws it; a wireframe the model
// supplied is dropped and replaced, and the labels stay as the model wrote them.
// Each value is stored as the exact layout id it names, so the answer that
// comes back is one campaignComposeEmail takes as it is. The question itself --
// what the answer is checked against and what its id is hashed from -- holds
// the model's options without any drawing.
func TestFeedbackLayoutsPresentationAddsServerWireframes(t *testing.T) {
	i, eng, ctx := askingRun(t)
	forged := `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`
	options := []map[string]any{
		{"label": "Article, closest to your notes", "value": "builtin:article", "wireframe": forged},
		{"label": "Steps", "value": "builtin:steps"},
		{"label": "Letter", "value": " builtin:letter ", "wireframe": ""},
	}
	ids := []string{"builtin:article", "builtin:steps", "builtin:letter"}

	id, err := i.AskFeedbackPresented(ctx, "u-alice", "r1", "Which layout should the email use?", "choice", "layouts", options, time.Time{}, false)
	if err != nil {
		t.Fatal(err)
	}

	approval := eng.callTo(t, "createWorkApproval").Args(t)
	if approval["approvalId"] != id {
		t.Fatalf("raised %v, answered %s", approval["approvalId"], id)
	}
	stored := storedOptions(t, approval, "options")
	if len(stored) != len(options) {
		t.Fatalf("stored %d options, want %d", len(stored), len(options))
	}
	for n, option := range stored {
		if option["value"] != ids[n] || option["label"] != options[n]["label"] {
			t.Errorf("option %d stored as %v, want the model's label and the layout id %s", n, option, ids[n])
		}
		if option["wireframe"] != gridWireframe(t, ids[n]) {
			t.Errorf("option %s carries %q, want the server's wireframe of the layout", ids[n], option["wireframe"])
		}
		if len(option) != 3 {
			t.Errorf("option %s stored with %v, want label, value and wireframe", ids[n], option)
		}
	}
	if raw, _ := emaildoc.Layout("builtin:steps"); stored[1]["wireframe"] == emaildoc.Wireframe(raw) {
		t.Error("Steps was drawn with one step, not at its minimum of two as the layout grid draws it")
	}

	subject, _ := approval["subject"].(map[string]any)
	if subject["presentation"] != "layouts" {
		t.Errorf("subject %v does not say the choice is of layouts", subject)
	}
	for n, option := range storedOptions(t, subject, "options") {
		if _, drawn := option["wireframe"]; drawn {
			t.Errorf("the question's own option %v carries a drawing; it is hashed into the question's id", option)
		}
		if option["value"] != ids[n] {
			t.Errorf("the question's own option %v, want the layout id %s", option, ids[n])
		}
	}
	if err := validateFeedbackAnswer(subject, "answered", map[string]any{"value": "builtin:letter"}); err != nil {
		t.Errorf("the answer naming the padded layout's id was refused: %v", err)
	}
	if approval["artifactHash"] != workstate.ArtifactHash(subject) {
		t.Error("the question's hash is not its subject's")
	}
	if options[0]["wireframe"] != forged || len(options[1]) != 2 || options[2]["value"] != " builtin:letter " {
		t.Error("the caller's options were changed")
	}
}

// TestFeedbackWithoutPresentationIsAskedAsBefore: a question with no
// presentation is stored byte for byte as it always was -- the same subject,
// the same id -- except that a wireframe the model put on an option is never
// stored: only the server draws one.
func TestFeedbackWithoutPresentationIsAskedAsBefore(t *testing.T) {
	options := []map[string]any{{"label": "PDF", "value": "pdf"}, {"label": "Text", "value": "txt"}}
	subject, err := workstate.NormalizeApprovalSubject(map[string]any{"question": "Which format?", "kind": "choice", "options": options})
	if err != nil {
		t.Fatal(err)
	}
	want := workstate.HumanQuestionID("r1", "reason", "feedback", workstate.ArtifactHash(subject), "")

	for _, tc := range []struct {
		name    string
		options []map[string]any
	}{
		{"as given", options},
		{"with a supplied wireframe", []map[string]any{{"label": "PDF", "value": "pdf", "wireframe": "<svg/>"}, {"label": "Text", "value": "txt"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, eng, ctx := askingRun(t)
			id, err := i.AskFeedback(ctx, "u-alice", "r1", "Which format?", "choice", tc.options, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if id != want {
				t.Errorf("asked as %s, want the id the question always had, %s", id, want)
			}
			approval := eng.callTo(t, "createWorkApproval").Args(t)
			stored, _ := approval["subject"].(map[string]any)
			if _, ok := stored["presentation"]; ok || len(stored) != 3 {
				t.Errorf("subject %v, want question, kind and options only", stored)
			}
			for _, list := range [][]map[string]any{storedOptions(t, approval, "options"), storedOptions(t, stored, "options")} {
				if len(list) != len(options) {
					t.Fatalf("stored %v, want %d options", list, len(options))
				}
				for _, option := range list {
					if _, drawn := option["wireframe"]; drawn {
						t.Errorf("a plain question stored %v", option)
					}
				}
			}
		})
	}
}
