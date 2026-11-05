package campaigns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/compose"
)

// compose_start_test.go -- starting, cancelling and retrying an email
// composition (campaignComposeEmail, campaignCancelComposition and
// campaignRetryComposition), driven against the package's fake engine with the
// work runtime and the app doors stood in for.
//
// The goal's run is the real one (compose_run.go), with the composer stood in
// as compose_run_test.go stands it in, so a test can show that a composition
// started here is written in place by its own run.

const (
	// The sentences a person reads, spelled out here and not taken from the
	// handlers so that a change of wording is a change of a test.
	composeNeedsBrand    = "Set up this organization's brand first: Email, Settings, Brand."
	composeNeedsApp      = "Open Cockpit with Claude Code signed in, then try again."
	composeDescription   = "Describe the email in 1 to 8000 characters."
	composeTooManyFiles  = "An email is composed from at most 64 attached files. Attach fewer."
	composeBadFile       = "Attach files from your Library."
	composeUnknownLayout = "choose one of the built-in layouts"
	composeCancelReason  = "cancelled by person"
	composeNothingToStop = "MemQL is not writing this email, so there is nothing to cancel."
	composeNotFailed     = "Only an email MemQL could not finish can be retried."
	composeUnavailable   = "This node cannot start MemQL writing an email: its work runtime and app doors are not wired."
	composeStartFailed   = "MemQL could not start writing this email: the work runtime is not answering."
)

// startedGoal is one goal the starter opened, with whether a template lock was
// held when it was asked.
type startedGoal struct {
	goal      CompositionGoal
	goalID    string
	locksHeld int
}

// recordingStarter is a GoalStarter that keeps what it is asked and answers
// goal ids in order. fail, when set, is its answer instead. before, when set,
// runs once the goal is open and before it answers: a run that starts first,
// or a person who cancels meanwhile.
type recordingStarter struct {
	mu     sync.Mutex
	gate   *layoutGateLog
	opened []startedGoal
	fail   error
	before func(g CompositionGoal, goalID string)
}

func (s *recordingStarter) StartComposition(_ context.Context, g CompositionGoal) (string, error) {
	_, held := s.gate.snapshot()
	s.mu.Lock()
	goalID := fmt.Sprintf("v1:work:goal:started-%d", len(s.opened)+1)
	s.opened = append(s.opened, startedGoal{goal: g, goalID: goalID, locksHeld: held})
	fail, before := s.fail, s.before
	s.mu.Unlock()
	if fail != nil {
		return "", fail
	}
	if before != nil {
		before(g, goalID)
	}
	return goalID, nil
}

func (s *recordingStarter) snapshot() []startedGoal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.opened)
}

// cancelledGoal is one goal the canceller was asked to stop, with the
// composition's state as it read at that moment.
type cancelledGoal struct {
	goalID, reason, state string
	locksHeld             int
}

// recordingCanceller is a GoalCanceller that keeps what it is asked. fail, when
// set, is its answer (a goal the caller cannot read, say).
type recordingCanceller struct {
	mu        sync.Mutex
	gate      *layoutGateLog
	state     func(goalID string) string
	cancelled []cancelledGoal
	fail      error
}

func (c *recordingCanceller) CancelGoal(_ context.Context, goalID, reason string) error {
	_, held := c.gate.snapshot()
	state := ""
	if c.state != nil {
		state = c.state(goalID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelled = append(c.cancelled, cancelledGoal{goalID: goalID, reason: reason, state: state, locksHeld: held})
	return c.fail
}

func (c *recordingCanceller) snapshot() []cancelledGoal {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.cancelled)
}

// fakeApps is an AppReachability whose answer a test sets, and which keeps who
// it was asked about.
type fakeApps struct {
	mu        sync.Mutex
	reachable bool
	err       error
	asked     []string
}

func (a *fakeApps) HasReachableApp(_ context.Context, userID string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, userID)
	return a.reachable, a.err
}

func (a *fakeApps) askedAbout() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.asked)
}

// startWorld is a run world with the composition's goals and app doors stood
// in for.
type startWorld struct {
	*runWorld
	starter   *recordingStarter
	canceller *recordingCanceller
	apps      *fakeApps
}

func newStartWorld(t *testing.T) *startWorld {
	t.Helper()
	rw := newRunWorld(t)
	sw := &startWorld{runWorld: rw, starter: &recordingStarter{gate: rw.gate},
		canceller: &recordingCanceller{gate: rw.gate}, apps: &fakeApps{reachable: true}}
	// The state of the composition writing for the goal, as the canceller is
	// asked to stop it.
	sw.canceller.state = func(goalID string) string {
		for _, row := range sw.engine.rowsNow() {
			if c, ok := compositionFromRow(row); ok && sameGoal(c.GoalID, goalID) {
				return c.State
			}
		}
		return ""
	}
	rw.w.SetCompositionGoals(sw.starter, sw.canceller, sw.apps)
	return sw
}

// rowsNow is every stored template, as copies.
func (e *composeEngine) rowsNow() []map[string]any {
	e.rowsMu.Lock()
	defer e.rowsMu.Unlock()
	out := make([]map[string]any, 0, len(e.rows))
	for _, row := range e.rows {
		copied := map[string]any{}
		for k, v := range row {
			copied[k] = v
		}
		out = append(out, copied)
	}
	return out
}

// composeArgs is a "Write it" call; each mutate changes the arguments.
func composeArgs(mutate ...func(map[string]any)) map[string]any {
	args := map[string]any{
		"accountId": templateOrg, "name": "Spring news", "layoutId": "builtin:article",
		"description": "Tell them about spring.", "fileIds": []any{}, "referenceFileId": "",
	}
	for _, m := range mutate {
		m(args)
	}
	return args
}

// decodeAnswer decodes the one node a builtin answered, which must be kind.
func decodeAnswer(t *testing.T, nodes []memorynodes.MemoryNode, kind string) map[string]any {
	t.Helper()
	if len(nodes) != 1 || nodes[0].ID != kind {
		t.Fatalf("expected one %s node, got %+v", kind, nodes)
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatalf("decoding the answer: %v", err)
	}
	return out
}

func (sw *startWorld) compose(args map[string]any) (map[string]any, error) {
	sw.t.Helper()
	nodes, err := sw.w.handleComposeEmail(importCtx(), args, 0)
	if err != nil {
		return nil, err
	}
	return decodeAnswer(sw.t, nodes, "campaignCompositionStarted"), nil
}

func (sw *startWorld) mustCompose(args map[string]any) (templateID, goalID string) {
	sw.t.Helper()
	out, err := sw.compose(args)
	if err != nil {
		sw.t.Fatalf("campaignComposeEmail: %v", err)
	}
	return str(out, "templateId"), str(out, "goalId")
}

func (sw *startWorld) cancel(templateID string) (map[string]any, error) {
	sw.t.Helper()
	nodes, err := sw.w.handleCancelComposition(importCtx(), map[string]any{"templateId": templateID}, 0)
	if err != nil {
		return nil, err
	}
	return decodeAnswer(sw.t, nodes, "campaignCompositionCancelled"), nil
}

func (sw *startWorld) retry(templateID string) (map[string]any, error) {
	sw.t.Helper()
	nodes, err := sw.w.handleRetryComposition(importCtx(), map[string]any{"templateId": templateID}, 0)
	if err != nil {
		return nil, err
	}
	return decodeAnswer(sw.t, nodes, "campaignCompositionStarted"), nil
}

// compositionOf is the stored composition of the template.
func (sw *startWorld) compositionOf(templateID string) composition {
	sw.t.Helper()
	c, ok := compositionFromRow(sw.engine.row(templateID))
	if !ok {
		sw.t.Fatalf("template %s has no composition", templateID)
	}
	return c
}

// nothingDone fails the test when anything was written or a goal was started.
func (sw *startWorld) nothingDone(t *testing.T, what string) {
	t.Helper()
	if n := len(sw.engine.writes()); n != 0 {
		t.Errorf("%s: %d writes", what, n)
	}
	if n := len(sw.engine.rowsNow()); n != 0 {
		t.Errorf("%s: %d templates", what, n)
	}
	if n := len(sw.starter.snapshot()); n != 0 {
		t.Errorf("%s: %d goals started", what, n)
	}
}

func TestComposeEmailRefusesWithoutBrandKit(t *testing.T) {
	sw := newStartWorld(t)
	_, err := sw.compose(composeArgs())
	if !errors.Is(err, errComposeNeedsBrand) || err.Error() != composeNeedsBrand {
		t.Fatalf("err = %v", err)
	}
	sw.nothingDone(t, "without a brand kit")
	// The brand is looked at before the apps: an organization with no kit is
	// sent to make one, whoever's machine is on.
	if asked := sw.apps.askedAbout(); len(asked) != 0 {
		t.Errorf("the apps were asked about %v", asked)
	}
	// Start from the layout still works without one (Task 14's builtin).
	if _, err := sw.w.handleCreateTemplateFromLayout(importCtx(), map[string]any{"accountId": templateOrg, "name": "Plain", "layoutId": "builtin:article"}, 0); err != nil {
		t.Errorf("start from the layout without a kit: %v", err)
	}
}

func TestComposeEmailRefusesWithoutReachableApp(t *testing.T) {
	sw := newStartWorld(t)
	sw.withKit()
	sw.apps.reachable = false
	_, err := sw.compose(composeArgs())
	if !errors.Is(err, errComposeNeedsApp) || err.Error() != composeNeedsApp {
		t.Fatalf("err = %v", err)
	}
	sw.nothingDone(t, "with no app")
	if asked := sw.apps.askedAbout(); len(asked) != 1 || asked[0] != testOwner {
		t.Errorf("the apps were asked about %v, want the caller", asked)
	}

	// The apps could not be asked: that is said as what it is, and nothing is
	// written either.
	sw = newStartWorld(t)
	sw.withKit()
	sw.apps.err = errors.New("the fleet cannot be read")
	if _, err := sw.compose(composeArgs()); err == nil || errors.Is(err, errComposeNeedsApp) || !strings.Contains(err.Error(), "the fleet cannot be read") {
		t.Fatalf("err = %v", err)
	}
	sw.nothingDone(t, "when the apps cannot be asked")

	// A node with nothing to open a goal with, or to ask the apps, refuses by
	// name rather than guessing.
	for name, install := range map[string]func(*startWorld){
		"no starter": func(sw *startWorld) { sw.w.SetCompositionGoals(nil, sw.canceller, sw.apps) },
		"no apps":    func(sw *startWorld) { sw.w.SetCompositionGoals(sw.starter, sw.canceller, nil) },
	} {
		sw := newStartWorld(t)
		sw.withKit()
		install(sw)
		if _, err := sw.compose(composeArgs()); err == nil || err.Error() != composeUnavailable {
			t.Errorf("%s: err = %v", name, err)
		}
		sw.nothingDone(t, name)
	}
}

func TestComposeEmailValidatesTheRequestFirst(t *testing.T) {
	files := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = fmt.Sprintf("file-%d", i)
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"no description", func(a map[string]any) { a["description"] = "" }, composeDescription},
		{"blank description", func(a map[string]any) { a["description"] = " \n\t " }, composeDescription},
		{"long description", func(a map[string]any) { a["description"] = strings.Repeat("é", 8001) }, composeDescription},
		{"65 files", func(a map[string]any) { a["fileIds"] = files(65) }, composeTooManyFiles},
		{"a file that is no id", func(a map[string]any) { a["fileIds"] = []any{"file-1", "../etc/passwd"} }, composeBadFile},
		{"a file that is no string", func(a map[string]any) { a["fileIds"] = []any{"file-1", 7} }, composeBadFile},
		{"a reference that is no id", func(a map[string]any) { a["referenceFileId"] = "a b" }, composeBadFile},
		{"no name", func(a map[string]any) { a["name"] = "  " }, chooseTemplate},
		{"long name", func(a map[string]any) { a["name"] = strings.Repeat("x", 201) }, chooseTemplate},
		{"control in the name", func(a map[string]any) { a["name"] = "a\nb" }, chooseTemplate},
		{"no organization", func(a map[string]any) { a["accountId"] = "" }, chooseTemplate},
		{"unknown layout", func(a map[string]any) { a["layoutId"] = "builtin:nope" }, composeUnknownLayout},
		{"organization layout", func(a map[string]any) { a["layoutId"] = "org:abc" }, composeUnknownLayout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sw := newStartWorld(t)
			sw.withKit()
			_, err := sw.compose(composeArgs(tc.mutate))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			sw.nothingDone(t, tc.name)
			if asked := sw.apps.askedAbout(); len(asked) != 0 {
				t.Errorf("the apps were asked about %v before the request was read", asked)
			}
		})
	}

	// At the limits: 8000 characters and 64 files, one of them named twice and
	// once canonically, are taken.
	sw := newStartWorld(t)
	sw.withKit()
	list := append(files(64), "file-3", "v1:library:file:file-5", "")
	id, _ := sw.mustCompose(composeArgs(func(a map[string]any) {
		a["description"], a["fileIds"] = strings.Repeat("é", 8000), list
	}))
	got := sw.compositionOf(id).Request
	if len(got.FileIDs) != 64 || got.FileIDs[0] != "file-0" || got.FileIDs[63] != "file-63" {
		t.Errorf("file ids = %d %v", len(got.FileIDs), got.FileIDs)
	}

	// Someone who may not write templates in the organization starts nothing.
	sw = newStartWorld(t)
	sw.withKit()
	sw.engine.capable = func(string, string, string) bool { return false }
	if _, err := sw.compose(composeArgs()); !errors.Is(err, errTemplateNeedsWrite) {
		t.Fatalf("without write permission: err = %v", err)
	}
	sw.nothingDone(t, "without write permission")
}

func TestComposeEmailCreatesWritingTemplateAndStartsGoal(t *testing.T) {
	sw := newStartWorld(t)
	kit := sw.withKit()
	hero := previewPNG(t, 6, 4)
	heroID := sw.addFile("hero.png", "image/png", hero)
	copyID := sw.addFile("copy.txt", "text/plain", []byte("Spring is here."))
	referenceID := sw.addFile("finished.png", "image/png", previewPNG(t, 8, 8))

	out, err := sw.compose(composeArgs(func(a map[string]any) {
		a["description"] = "  Tell them about spring.  "
		a["fileIds"] = []any{heroID, " " + copyID + " ", "v1:library:file:" + heroID}
		a["referenceFileId"] = referenceID
	}))
	if err != nil {
		t.Fatal(err)
	}
	id, goal := str(out, "templateId"), str(out, "goalId")

	// One goal, the caller's, for the organization and this template, opened
	// while no template lock was held.
	opened := sw.starter.snapshot()
	if len(opened) != 1 {
		t.Fatalf("%d goals started", len(opened))
	}
	g := opened[0]
	if g.goal.OwnerUserID != testOwner || g.goal.AccountID != templateOrg || g.goal.TemplateID != id || goal != g.goalID {
		t.Errorf("goal = %+v, answered %q", g.goal, goal)
	}
	if !strings.Contains(g.goal.Statement, "Spring news") || !strings.Contains(g.goal.Statement, "Article") {
		t.Errorf("statement = %q", g.goal.Statement)
	}
	if g.locksHeld != 0 {
		t.Errorf("the goal was opened with %d template locks held", g.locksHeld)
	}

	// The template is the layout with the brand applied, a draft, and MemQL is
	// writing it under this goal with the request as it was asked (each file
	// once, bare).
	row := sw.engine.row(id)
	if !sameJSON(t, row["document"], expectedLayoutDocument(t, "builtin:article", &kit)) || str(row, "status") != "draft" || str(row, "name") != "Spring news" {
		t.Errorf("the template is not the brand-applied layout as a draft: status %q", str(row, "status"))
	}
	c := sw.compositionOf(id)
	if c.State != compositionWriting || !sameGoal(c.GoalID, goal) || !c.StartedAt.Equal(templateNow) || c.Error != "" || !c.FinishedAt.IsZero() {
		t.Errorf("composition = %+v", c)
	}
	want := compositionRequest{LayoutID: "builtin:article", Name: "Spring news", Description: "Tell them about spring.",
		FileIDs: []string{heroID, copyID}, ReferenceFileID: referenceID}
	if !sameJSON(t, c.Request, want) {
		t.Errorf("request = %+v, want %+v", c.Request, want)
	}
	// In step: the composition starts from the template as it is now, and the
	// answer names that revision.
	revision := templateTime(row["createdAt"])
	if !c.BaseRevision.Equal(revision) || str(out, "revision") != revision.Format(time.RFC3339Nano) {
		t.Errorf("base revision %v, row %v, answered %s", c.BaseRevision, revision, str(out, "revision"))
	}
	if n := len(sw.engine.writes()); n != 2 {
		t.Errorf("%d writes, want the template and the goal", n)
	}

	// The goal's run takes it from there, and its session's email is saved over
	// the template, not beside it.
	sw.ai.session = sw.submitting(articleComposition(compose.EmailAssetHandle(hero)))
	if _, err := sw.run(runContext(goal), id); err != nil {
		t.Fatalf("run: %v", err)
	}
	if done := sw.compositionOf(id); done.State != compositionDone {
		t.Errorf("after the run: %s %q", done.State, done.Error)
	}
	if n := len(sw.engine.rowsNow()); n != 1 {
		t.Errorf("%d templates: the run saved a copy", n)
	}
}

// The goal's run can claim the composition before the start records the goal:
// the start then leaves it as the run wrote it, so the template stays at the
// revision the run's composition starts from.
func TestComposeEmailLeavesTheGoalItsRunClaimedFirst(t *testing.T) {
	sw := newStartWorld(t)
	sw.withKit()
	sw.starter.before = func(g CompositionGoal, goalID string) {
		if _, claimed, err := sw.w.claimComposition(importCtx(), g.TemplateID, goalID); err != nil || !claimed {
			t.Errorf("the run's claim: claimed=%v err=%v", claimed, err)
		}
	}
	out, err := sw.compose(composeArgs())
	if err != nil {
		t.Fatal(err)
	}
	id, goal := str(out, "templateId"), str(out, "goalId")
	if n := len(sw.engine.writes()); n != 2 {
		t.Errorf("%d writes, want the template and the run's claim only", n)
	}
	row := sw.engine.row(id)
	c := sw.compositionOf(id)
	revision := templateTime(row["createdAt"])
	if !sameGoal(c.GoalID, goal) || !c.BaseRevision.Equal(revision) || str(out, "revision") != revision.Format(time.RFC3339Nano) {
		t.Errorf("composition goal %q base %v, row %v, answered %s", c.GoalID, c.BaseRevision, revision, str(out, "revision"))
	}
	sw.ai.session = sw.submitting(articleComposition(""))
	if _, err := sw.run(runContext(goal), id); err != nil {
		t.Fatalf("run: %v", err)
	}
	if done := sw.compositionOf(id); done.State != compositionDone || len(sw.engine.rowsNow()) != 1 {
		t.Errorf("after the run: %s, %d templates", done.State, len(sw.engine.rowsNow()))
	}
}

func TestComposeEmailMarksFailedWhenGoalCannotStart(t *testing.T) {
	sw := newStartWorld(t)
	kit := sw.withKit()
	sw.starter.fail = errors.New("the work runtime is not answering")
	_, err := sw.compose(composeArgs())
	if err == nil || !strings.HasPrefix(err.Error(), composeStartFailed) {
		t.Fatalf("err = %v", err)
	}
	rows := sw.engine.rowsNow()
	if len(rows) != 1 {
		t.Fatalf("%d templates, want the draft kept", len(rows))
	}
	// The template stays as the brand-applied layout, so the person can retry it
	// or write the email by hand; its composition says why it stopped.
	id := bare(str(rows[0], "id"))
	if !sameJSON(t, rows[0]["document"], expectedLayoutDocument(t, "builtin:article", &kit)) || str(rows[0], "status") != "draft" {
		t.Error("the draft is not the brand-applied layout")
	}
	c := sw.compositionOf(id)
	if c.State != compositionFailed || c.Error != composeStartFailed || c.GoalID != "" || !c.FinishedAt.Equal(templateNow) {
		t.Errorf("composition = %s %q goal %q finished %v", c.State, c.Error, c.GoalID, c.FinishedAt)
	}

	// No goal at all is a goal that cannot start too.
	sw = newStartWorld(t)
	sw.withKit()
	sw.w.SetCompositionGoals(goalStarterFunc(func(context.Context, CompositionGoal) (string, error) { return " ", nil }), sw.canceller, sw.apps)
	if _, err := sw.compose(composeArgs()); err == nil || !strings.HasPrefix(err.Error(), "MemQL could not start writing this email") {
		t.Fatalf("an empty goal: err = %v", err)
	}
	if rows := sw.engine.rowsNow(); len(rows) != 1 {
		t.Fatalf("%d templates", len(rows))
	} else if c, _ := compositionFromRow(rows[0]); c.State != compositionFailed {
		t.Errorf("an empty goal left the composition %s", c.State)
	}

	// A failed start can be retried.
	sw.w.SetCompositionGoals(sw.starter, sw.canceller, sw.apps)
	if _, err := sw.retry(bare(str(sw.engine.rowsNow()[0], "id"))); err != nil {
		t.Errorf("retrying a start that failed: %v", err)
	}
}

// goalStarterFunc is a GoalStarter made of a function.
type goalStarterFunc func(context.Context, CompositionGoal) (string, error)

func (f goalStarterFunc) StartComposition(ctx context.Context, g CompositionGoal) (string, error) {
	return f(ctx, g)
}

func TestCancelCompositionMarksFailedAndCancelsGoal(t *testing.T) {
	sw := newStartWorld(t)
	kit := sw.withKit()
	id, goal := sw.mustCompose(composeArgs())
	sw.w.now = func() time.Time { return templateNow.Add(time.Minute) }

	out, err := sw.cancel(id)
	if err != nil {
		t.Fatal(err)
	}
	// The goal is asked to stop, as the person, while the composition still
	// reads writing and no template lock is held; then the composition fails.
	cancelled := sw.canceller.snapshot()
	if len(cancelled) != 1 || !sameGoal(cancelled[0].goalID, goal) || cancelled[0].reason != composeCancelReason {
		t.Fatalf("cancelled = %+v", cancelled)
	}
	if cancelled[0].state != compositionWriting || cancelled[0].locksHeld != 0 {
		t.Errorf("the goal was cancelled with the composition %s and %d locks held", cancelled[0].state, cancelled[0].locksHeld)
	}
	c := sw.compositionOf(id)
	if c.State != compositionFailed || c.Error != compositionCancelled || !c.FinishedAt.Equal(templateNow.Add(time.Minute)) || !sameGoal(c.GoalID, goal) {
		t.Errorf("composition = %s %q finished %v goal %q", c.State, c.Error, c.FinishedAt, c.GoalID)
	}
	row := sw.engine.row(id)
	if str(out, "templateId") != id || str(out, "state") != compositionFailed || str(out, "revision") != templateTime(row["createdAt"]).Format(time.RFC3339Nano) {
		t.Errorf("answer = %v", out)
	}
	// The draft stays as the brand-applied layout, and the cancelled run's late
	// submit is refused.
	if !sameJSON(t, row["document"], expectedLayoutDocument(t, "builtin:article", &kit)) {
		t.Error("cancelling changed the email")
	}
	if _, err := sw.toolFor("submit", id, goal, articleComposition("")); !errors.Is(err, errCompositionNotWriting) {
		t.Errorf("a late submit: err = %v", err)
	}

	// Only a writing composition can be cancelled: nothing is asked or written.
	writes := len(sw.engine.writes())
	if _, err := sw.cancel(id); err == nil || err.Error() != composeNothingToStop {
		t.Errorf("cancelling again: err = %v", err)
	}
	plain, err := sw.w.handleCreateTemplateFromLayout(importCtx(), map[string]any{"accountId": templateOrg, "name": "Plain", "layoutId": "builtin:article"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	plainID := decodeSaved(t, plain).TemplateID
	writes++
	if _, err := sw.cancel(plainID); err == nil || err.Error() != composeNothingToStop {
		t.Errorf("cancelling a template MemQL is not writing: err = %v", err)
	}
	if _, err := sw.cancel("no-such-template"); err == nil || err.Error() != composeMissing {
		t.Errorf("cancelling a template that does not exist: err = %v", err)
	}
	if n := len(sw.engine.writes()); n != writes || len(sw.canceller.snapshot()) != 1 {
		t.Errorf("refused cancels wrote %d times and asked %d goals to stop", n-writes, len(sw.canceller.snapshot())-1)
	}
}

func TestCancelCompositionStopsWhatIsWritingWhateverTheGoal(t *testing.T) {
	// The goal is not recorded yet (the start is still opening it, or stopped
	// before it could): there is no goal to ask, and the composition fails.
	sw := newStartWorld(t)
	sw.withKit()
	id := sw.start("builtin:article", nil, "")
	unrecorded := sw.compositionOf(id)
	unrecorded.GoalID = ""
	if _, err := sw.w.writeComposition(importCtx(), templateOrg, id, unrecorded); err != nil {
		t.Fatal(err)
	}
	if _, err := sw.cancel(id); err != nil {
		t.Fatal(err)
	}
	if c := sw.compositionOf(id); c.State != compositionFailed || c.Error != compositionCancelled {
		t.Errorf("composition = %s %q", c.State, c.Error)
	}
	if n := len(sw.canceller.snapshot()); n != 0 {
		t.Errorf("%d goals asked to stop", n)
	}

	// The start records its goal while the cancel is on its way, between the
	// cancel's first read and its lock: that goal is stopped too.
	sw = newStartWorld(t)
	sw.withKit()
	id = sw.start("builtin:article", nil, "")
	unrecorded = sw.compositionOf(id)
	unrecorded.GoalID = ""
	if _, err := sw.w.writeComposition(importCtx(), templateOrg, id, unrecorded); err != nil {
		t.Fatal(err)
	}
	gate, hooked := sw.w.templateGate, false
	sw.w.templateGate = func(ctx context.Context, key string) (func(), error) {
		if !hooked {
			// The cancel's lock, first: the start's write comes before it.
			hooked = true
			recorded := sw.compositionOf(id)
			recorded.GoalID = "v1:work:goal:recorded-meanwhile"
			if _, err := sw.w.writeComposition(importCtx(), templateOrg, id, recorded); err != nil {
				t.Error(err)
			}
		}
		return gate(ctx, key)
	}
	if _, err := sw.cancel(id); err != nil {
		t.Fatal(err)
	}
	if c := sw.compositionOf(id); c.State != compositionFailed || c.Error != compositionCancelled {
		t.Errorf("composition = %s %q", c.State, c.Error)
	}
	if cancelled := sw.canceller.snapshot(); len(cancelled) != 1 || !sameGoal(cancelled[0].goalID, "recorded-meanwhile") || cancelled[0].reason != composeCancelReason {
		t.Errorf("cancelled = %+v, want the goal recorded meanwhile", cancelled)
	}

	// The run finishes while the cancel is on its way: the email it saved and
	// the composition's done stay, and the answer says so.
	sw = newStartWorld(t)
	sw.withKit()
	id, goal := sw.mustCompose(composeArgs())
	gate, hooked = sw.w.templateGate, false
	sw.w.templateGate = func(ctx context.Context, key string) (func(), error) {
		if !hooked {
			hooked = true
			if _, err := sw.toolFor("submit", id, goal, articleComposition("")); err != nil {
				t.Errorf("the run's submit: %v", err)
			}
		}
		return gate(ctx, key)
	}
	out, err := sw.cancel(id)
	if err != nil {
		t.Fatal(err)
	}
	if c := sw.compositionOf(id); c.State != compositionDone || c.Error != "" || str(out, "state") != compositionDone {
		t.Errorf("composition = %s %q, answered %v", c.State, c.Error, out)
	}
	if subject := str(sw.engine.row(id), "subject"); subject != "Spring is here" {
		t.Errorf("the saved email was lost: subject %q", subject)
	}

	// The goal will not stop for this caller (a colleague who cannot read the
	// composer's goal): the composition is cancelled all the same, so no tool
	// of that run reaches the template again.
	sw = newStartWorld(t)
	sw.withKit()
	sw.canceller.fail = errors.New("work: no goal is readable by this caller")
	id, goal = sw.mustCompose(composeArgs())
	if _, err := sw.cancel(id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if c := sw.compositionOf(id); c.State != compositionFailed || c.Error != compositionCancelled {
		t.Errorf("composition = %s %q", c.State, c.Error)
	}
	if _, err := sw.toolFor("context", id, goal, nil); !errors.Is(err, errCompositionNotWriting) {
		t.Errorf("the run's tool after the cancel: err = %v", err)
	}

	// A caller who may not write the template cancels nothing.
	sw = newStartWorld(t)
	sw.withKit()
	id, _ = sw.mustCompose(composeArgs())
	writes := len(sw.engine.writes())
	sw.engine.capable = func(string, string, string) bool { return false }
	if _, err := sw.cancel(id); !errors.Is(err, errTemplateNeedsWrite) {
		t.Errorf("without write permission: err = %v", err)
	}
	if len(sw.engine.writes()) != writes || len(sw.canceller.snapshot()) != 0 {
		t.Error("a refused cancel wrote or stopped something")
	}
	if c := sw.compositionOf(id); c.State != compositionWriting {
		t.Errorf("composition = %s", c.State)
	}
}

// A person who cancels while the start is still opening the goal: the start
// finds its composition cancelled when it comes to record the goal, leaves it
// so, and stops the goal nothing will use.
func TestComposeEmailStopsItsGoalWhenCancelledWhileStarting(t *testing.T) {
	sw := newStartWorld(t)
	sw.withKit()
	sw.starter.before = func(g CompositionGoal, _ string) {
		if _, err := sw.cancel(g.TemplateID); err != nil {
			t.Errorf("cancel while starting: %v", err)
		}
	}
	out, err := sw.compose(composeArgs())
	if err != nil {
		t.Fatal(err)
	}
	id, goal := str(out, "templateId"), str(out, "goalId")
	c := sw.compositionOf(id)
	if c.State != compositionFailed || c.Error != compositionCancelled || c.GoalID != "" {
		t.Errorf("composition = %s %q goal %q", c.State, c.Error, c.GoalID)
	}
	cancelled := sw.canceller.snapshot()
	if len(cancelled) != 1 || !sameGoal(cancelled[0].goalID, goal) || cancelled[0].reason != composeCancelReason {
		t.Errorf("cancelled = %+v, want the start's own goal", cancelled)
	}
	if n := len(sw.engine.writes()); n != 2 {
		t.Errorf("%d writes, want the template and the cancel", n)
	}
}

func TestRetryCompositionStartsNewGoalWithRecordedRequest(t *testing.T) {
	sw := newStartWorld(t)
	sw.withKit()
	hero := previewPNG(t, 6, 4)
	heroID := sw.addFile("hero.png", "image/png", hero)
	referenceID := sw.addFile("finished.png", "image/png", previewPNG(t, 8, 8))
	id, first := sw.mustCompose(composeArgs(func(a map[string]any) {
		a["fileIds"], a["referenceFileId"] = []any{heroID, "gone-1"}, referenceID
	}))
	recorded := sw.compositionOf(id).Request

	// The run saves nothing, and the composition fails with the files it left
	// out noted.
	if _, err := sw.run(runContext(first), id); err == nil || !strings.Contains(err.Error(), compositionFailedCode) {
		t.Fatalf("the first run: err = %v", err)
	}
	failed := sw.compositionOf(id)
	if failed.State != compositionFailed || len(failed.Notes) == 0 {
		t.Fatalf("the first run left %s with notes %v", failed.State, failed.Notes)
	}
	// The person edited the draft by hand meanwhile: a retry composes over it,
	// in place, rather than beside it.
	edited := templateNow.Add(30 * time.Minute)
	sw.engine.edit(id, func(row map[string]any) {
		row["subject"], row["createdAt"] = "Edited by hand", edited.Format(time.RFC3339Nano)
	})

	later := templateNow.Add(time.Hour)
	sw.w.now = func() time.Time { return later }
	// While the new goal is being opened, the composition is writing and names
	// no goal: the first run's calls are refused already.
	sw.starter.before = func(CompositionGoal, string) {
		if _, err := sw.toolFor("context", id, first, nil); !errors.Is(err, errCompositionNotWriting) {
			t.Errorf("the old goal's tool while the retry starts: err = %v", err)
		}
	}
	out, err := sw.retry(id)
	sw.starter.before = nil
	if err != nil {
		t.Fatal(err)
	}
	second := str(out, "goalId")
	opened := sw.starter.snapshot()
	if len(opened) != 2 || second != opened[1].goalID || sameGoal(second, first) {
		t.Fatalf("goals %+v, answered %q", opened, second)
	}
	if g := opened[1].goal; g.OwnerUserID != testOwner || g.AccountID != templateOrg || g.TemplateID != id || !strings.Contains(g.Statement, "Spring news") || opened[1].locksHeld != 0 {
		t.Errorf("the retry's goal = %+v (locks held %d)", g, opened[1].locksHeld)
	}
	// Writing again under the new goal, from now, with the request it recorded,
	// no error, no notes, and in step with the template.
	c := sw.compositionOf(id)
	row := sw.engine.row(id)
	revision := templateTime(row["createdAt"])
	if c.State != compositionWriting || !sameGoal(c.GoalID, second) || !c.StartedAt.Equal(later) || c.Error != "" || !c.FinishedAt.IsZero() || len(c.Notes) != 0 {
		t.Errorf("composition = %+v", c)
	}
	if !sameJSON(t, c.Request, recorded) {
		t.Errorf("request = %+v, want the recorded %+v", c.Request, recorded)
	}
	if !c.BaseRevision.Equal(revision) || str(out, "revision") != revision.Format(time.RFC3339Nano) || str(out, "templateId") != id {
		t.Errorf("base %v, row %v, answer %v", c.BaseRevision, revision, out)
	}

	// The first run's late calls are refused; the new run writes in place.
	if _, err := sw.toolFor("context", id, first, nil); !errors.Is(err, errCompositionNotWriting) {
		t.Errorf("the old goal's tool: err = %v", err)
	}
	sw.ai.session = sw.submitting(articleComposition(compose.EmailAssetHandle(hero)))
	if _, err := sw.run(runContext(second), id); err != nil {
		t.Fatalf("the retried run: %v", err)
	}
	if done := sw.compositionOf(id); done.State != compositionDone || !sameGoal(done.GoalID, second) || len(sw.engine.rowsNow()) != 1 {
		t.Errorf("after the retried run: %s goal %q, %d templates", done.State, done.GoalID, len(sw.engine.rowsNow()))
	}
}

// A run of an older goal that starts late (its start answered uncertainly, say)
// can claim a retried composition before the retry records its own goal. The
// retry's goal is the composition's all the same, and the template stays in
// step with it: the new run's email lands in place.
func TestRetryCompositionTakesBackWhatAStaleRunClaimed(t *testing.T) {
	sw := newStartWorld(t)
	sw.withKit()
	id, first := sw.mustCompose(composeArgs())
	if _, err := sw.cancel(id); err != nil {
		t.Fatal(err)
	}
	sw.starter.before = func(g CompositionGoal, _ string) {
		if _, claimed, err := sw.w.claimComposition(importCtx(), g.TemplateID, first); err != nil || !claimed {
			t.Errorf("the stale run's claim: claimed=%v err=%v", claimed, err)
		}
	}
	out, err := sw.retry(id)
	if err != nil {
		t.Fatal(err)
	}
	second := str(out, "goalId")
	c := sw.compositionOf(id)
	revision := templateTime(sw.engine.row(id)["createdAt"])
	if c.State != compositionWriting || !sameGoal(c.GoalID, second) || !c.BaseRevision.Equal(revision) {
		t.Fatalf("composition %s goal %q base %v, row %v", c.State, c.GoalID, c.BaseRevision, revision)
	}
	if _, err := sw.toolFor("context", id, first, nil); !errors.Is(err, errCompositionNotWriting) {
		t.Errorf("the stale goal's tool: err = %v", err)
	}
	sw.ai.session = sw.submitting(articleComposition(""))
	if _, err := sw.run(runContext(second), id); err != nil {
		t.Fatalf("the retried run: %v", err)
	}
	if done := sw.compositionOf(id); done.State != compositionDone || len(sw.engine.rowsNow()) != 1 {
		t.Errorf("after the retried run: %s, %d templates", done.State, len(sw.engine.rowsNow()))
	}
}

func TestRetryCompositionRefusesUnlessItCanRunAgain(t *testing.T) {
	failedTemplate := func(t *testing.T) (*startWorld, string) {
		t.Helper()
		sw := newStartWorld(t)
		sw.withKit()
		id, _ := sw.mustCompose(composeArgs())
		if _, err := sw.cancel(id); err != nil {
			t.Fatal(err)
		}
		return sw, id
	}
	refused := func(t *testing.T, sw *startWorld, id, want string) {
		t.Helper()
		writes, goals, asked := len(sw.engine.writes()), len(sw.starter.snapshot()), len(sw.apps.askedAbout())
		_, err := sw.retry(id)
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if len(sw.engine.writes()) != writes || len(sw.starter.snapshot()) != goals {
			t.Error("a refused retry wrote or started something")
		}
		// What cannot be retried is said first, whatever the person's machines.
		if want == composeNotFailed && len(sw.apps.askedAbout()) != asked {
			t.Error("the apps were asked about a composition that cannot be retried")
		}
	}

	t.Run("writing", func(t *testing.T) {
		sw := newStartWorld(t)
		sw.withKit()
		id, _ := sw.mustCompose(composeArgs())
		refused(t, sw, id, composeNotFailed)
	})
	t.Run("done", func(t *testing.T) {
		sw := newStartWorld(t)
		sw.withKit()
		id, goal := sw.mustCompose(composeArgs())
		sw.ai.session = sw.submitting(articleComposition(""))
		if _, err := sw.run(runContext(goal), id); err != nil {
			t.Fatal(err)
		}
		refused(t, sw, id, composeNotFailed)
	})
	t.Run("no composition", func(t *testing.T) {
		sw := newStartWorld(t)
		plain, err := sw.w.handleCreateTemplateFromLayout(importCtx(), map[string]any{"accountId": templateOrg, "name": "Plain", "layoutId": "builtin:article"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		sw.withKit()
		refused(t, sw, decodeSaved(t, plain).TemplateID, composeNotFailed)
	})
	t.Run("no brand kit", func(t *testing.T) {
		sw, id := failedTemplate(t)
		sw.engine.brandKit = nil
		refused(t, sw, id, composeNeedsBrand)
	})
	t.Run("no app", func(t *testing.T) {
		sw, id := failedTemplate(t)
		sw.apps.reachable = false
		refused(t, sw, id, composeNeedsApp)
	})
	t.Run("no write permission", func(t *testing.T) {
		sw, id := failedTemplate(t)
		sw.engine.capable = func(string, string, string) bool { return false }
		refused(t, sw, id, templateNeeds)
	})
	t.Run("the goal cannot start", func(t *testing.T) {
		sw, id := failedTemplate(t)
		sw.starter.fail = errors.New("the work runtime is not answering")
		if _, err := sw.retry(id); err == nil || !strings.HasPrefix(err.Error(), composeStartFailed) {
			t.Fatalf("err = %v", err)
		}
		if c := sw.compositionOf(id); c.State != compositionFailed || c.Error != composeStartFailed {
			t.Errorf("composition = %s %q", c.State, c.Error)
		}
	})
}

func TestComposeStartBuiltinsLoadAsSDKBuiltins(t *testing.T) {
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.Logger = quietLogger()
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine init: %v", err)
	}
	if err := eng.RegisterIntegration(newTestWorker(t, &fakeEngine{}, &recordingSender{})); err != nil {
		t.Fatal(err)
	}
	if err := eng.AuditIntegrationExecutors(); err != nil {
		t.Fatalf("executor audit: %v", err)
	}
	for builtin, executor := range map[string]string{
		"campaignComposeEmail":      "integration.campaigns.composeEmail",
		"campaignCancelComposition": "integration.campaigns.cancelComposition",
		"campaignRetryComposition":  "integration.campaigns.retryComposition",
	} {
		fn, err := eng.Functions().Get(builtin)
		if err != nil || fn == nil || !fn.IsBuiltin() || fn.Executor != executor || fn.ServerOnly {
			t.Errorf("builtin %s: %+v %v", builtin, fn, err)
		}
	}
}
