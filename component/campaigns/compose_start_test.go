package campaigns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
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
	composeUnavailable   = "MemQL can't write emails on this server. Ask your administrator."
	composeStartFailed   = "MemQL could not start writing this email."
	composeAppsUnknown   = "MemQL could not check your apps right now. Try again in a moment."
	composeNotYours      = "Only the person who asked MemQL to write this email can retry it. Use New email to write it with your own files."
	composeStartStop     = "the composition could not start"

	// rawCause is what a stand-in for the work runtime or the app doors fails
	// with: infrastructure's words, which a person must never read.
	rawCause = "pq: connection reset by peer on goal insert"
)

// startedGoal is one goal the starter opened, with how many template locks were
// held when it was asked, and its context's error once it had opened the goal.
type startedGoal struct {
	goal      CompositionGoal
	goalID    string
	locksHeld int
	ctxErr    error
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

func (s *recordingStarter) StartComposition(ctx context.Context, g CompositionGoal) (string, error) {
	_, held := s.gate.snapshot()
	s.mu.Lock()
	n := len(s.opened)
	goalID := fmt.Sprintf("v1:work:goal:started-%d", n+1)
	s.opened = append(s.opened, startedGoal{goal: g, goalID: goalID, locksHeld: held})
	fail, before := s.fail, s.before
	s.mu.Unlock()
	if fail != nil {
		return "", fail
	}
	if before != nil {
		before(g, goalID)
	}
	s.mu.Lock()
	s.opened[n].ctxErr = ctx.Err()
	s.mu.Unlock()
	return goalID, nil
}

func (s *recordingStarter) snapshot() []startedGoal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.opened)
}

// cancelledGoal is one goal the canceller was asked to stop, with the
// composition's state as it read at that moment, and the error of the context
// it was asked on.
type cancelledGoal struct {
	goalID, reason, state string
	locksHeld             int
	ctxErr                error
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

func (c *recordingCanceller) CancelGoal(ctx context.Context, goalID, reason string) error {
	_, held := c.gate.snapshot()
	state := ""
	if c.state != nil {
		state = c.state(goalID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelled = append(c.cancelled, cancelledGoal{goalID: goalID, reason: reason, state: state, locksHeld: held, ctxErr: ctx.Err()})
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

// startAnswer decodes what a start or a retry answered: its kind
// (campaignCompositionStarted or campaignCompositionFailedToStart) and fields.
func startAnswer(t *testing.T, nodes []memorynodes.MemoryNode) (string, map[string]any) {
	t.Helper()
	if len(nodes) != 1 || (nodes[0].ID != "campaignCompositionStarted" && nodes[0].ID != "campaignCompositionFailedToStart") {
		t.Fatalf("expected one start answer, got %+v", nodes)
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatalf("decoding the answer: %v", err)
	}
	return nodes[0].ID, out
}

// logs sends the worker's warnings to a buffer the test reads.
func (sw *startWorld) logs() *syncBuffer {
	buf := &syncBuffer{}
	sw.w.logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return buf
}

// syncBuffer is a bytes.Buffer safe to write from one goroutine and read from
// another.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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

// colleagueCtx is a call from another member of the organization, who may
// write its templates.
func colleagueCtx() context.Context {
	return auth.ContextWithUserActor(context.Background(), "colleague-1")
}

// storedDocument is the template's stored document, as a copy a person can
// change.
func (sw *startWorld) storedDocument(templateID string) map[string]any {
	sw.t.Helper()
	raw, err := json.Marshal(sw.engine.row(templateID)["document"])
	if err != nil {
		sw.t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		sw.t.Fatal(err)
	}
	return doc
}

// personSaves saves content over the template as a person does in the editor
// (campaignSaveTemplate), held to the revision the template has now.
func (sw *startWorld) personSaves(ctx context.Context, templateID string, content map[string]any, action string) (savedTemplate, error) {
	sw.t.Helper()
	row := sw.engine.row(templateID)
	nodes, err := sw.w.handleSaveTemplate(ctx, map[string]any{
		"templateId": templateID, "accountId": templateOrg, "name": str(row, "name"), "content": content,
		"expectedRevision": templateTime(row["createdAt"]).UTC().Format(time.RFC3339Nano), "action": action,
	}, 0)
	if err != nil {
		return savedTemplate{}, err
	}
	return decodeSaved(sw.t, nodes), nil
}

// failedByItsRun starts a composition whose run saves nothing, so that it ends
// failed with the files it left out noted. It answers the template and the
// failed composition.
func (sw *startWorld) failedByItsRun() (string, composition) {
	sw.t.Helper()
	copyID := sw.addFile("copy.txt", "text/plain", []byte("Spring is here."))
	readmeID := sw.addFile("readme.pdf", "application/pdf", []byte("%PDF-1.4"))
	id, goal := sw.mustCompose(composeArgs(func(a map[string]any) { a["fileIds"] = []any{copyID, readmeID} }))
	if _, err := sw.run(runContext(goal), id); err == nil || !strings.Contains(err.Error(), compositionFailedCode) {
		sw.t.Fatalf("the run: err = %v", err)
	}
	failed := sw.compositionOf(id)
	if failed.State != compositionFailed || failed.Error == "" || len(failed.Notes) == 0 {
		sw.t.Fatalf("the run left %s %q with notes %v", failed.State, failed.Error, failed.Notes)
	}
	return id, failed
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

	// The apps could not be asked: the person reads a fixed sentence, never the
	// cause, which is logged; and nothing is written.
	sw = newStartWorld(t)
	sw.withKit()
	logged := sw.logs()
	sw.apps.err = errors.New(rawCause)
	_, err = sw.compose(composeArgs())
	if err == nil || err.Error() != composeAppsUnknown || strings.Contains(err.Error(), rawCause) {
		t.Fatalf("err = %v", err)
	}
	sw.nothingDone(t, "when the apps cannot be asked")
	if !strings.Contains(logged.String(), rawCause) {
		t.Error("the cause was not logged")
	}

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
	// The request names the person who asked: a retry is theirs alone.
	want := compositionRequest{LayoutID: "builtin:article", Name: "Spring news", Description: "Tell them about spring.",
		FileIDs: []string{heroID, copyID}, ReferenceFileID: referenceID, RequestedBy: testOwner}
	if !sameJSON(t, c.Request, want) {
		t.Errorf("request = %+v, want %+v", c.Request, want)
	}
	// In step: the composition starts from the template as it is now, and the
	// answer names that revision.
	revision := templateTime(row["createdAt"])
	if !c.BaseRevision.Equal(revision) || str(out, "revision") != revision.Format(time.RFC3339Nano) || str(out, "state") != compositionWriting {
		t.Errorf("base revision %v, row %v, answered %s %s", c.BaseRevision, revision, str(out, "revision"), str(out, "state"))
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
	logged := sw.logs()
	sw.starter.fail = errors.New(rawCause)
	nodes, err := sw.w.handleComposeEmail(importCtx(), composeArgs(), 0)
	if err != nil {
		t.Fatalf("a goal that cannot start is answered, not refused: %v", err)
	}
	kind, out := startAnswer(t, nodes)
	rows := sw.engine.rowsNow()
	if len(rows) != 1 {
		t.Fatalf("%d templates, want the draft kept", len(rows))
	}
	id := bare(str(rows[0], "id"))
	// The answer names the draft that was kept, failed, with the sentence the
	// person reads: a client opens that draft rather than making another.
	if kind != "campaignCompositionFailedToStart" || str(out, "templateId") != id || str(out, "state") != compositionFailed ||
		str(out, "error") != composeStartFailed || str(out, "revision") != templateTime(rows[0]["createdAt"]).Format(time.RFC3339Nano) {
		t.Errorf("answer = %s %v", kind, out)
	}
	// The template stays as the brand-applied layout, so the person can retry it
	// or write the email by hand; its composition says why it stopped.
	if !sameJSON(t, rows[0]["document"], expectedLayoutDocument(t, "builtin:article", &kit)) || str(rows[0], "status") != "draft" {
		t.Error("the draft is not the brand-applied layout")
	}
	c := sw.compositionOf(id)
	if c.State != compositionFailed || c.Error != composeStartFailed || c.GoalID != "" || !c.FinishedAt.Equal(templateNow) {
		t.Errorf("composition = %s %q goal %q finished %v", c.State, c.Error, c.GoalID, c.FinishedAt)
	}
	// It records the revision it started from, in step with the template, so
	// that a retry after somebody changed the template is not taken for one
	// over the template as it was.
	if revision := templateTime(rows[0]["createdAt"]); !c.BaseRevision.Equal(revision) {
		t.Errorf("base revision %v, row %v", c.BaseRevision, revision)
	}
	// The cause is logged, and nowhere a person reads it: not in the answer,
	// not anywhere on the template.
	answered, _ := plainJSON(out)
	stored, _ := plainJSON(sw.engine.row(id))
	if strings.Contains(string(answered), rawCause) || strings.Contains(string(stored), rawCause) {
		t.Errorf("the cause reached the person: answer %s", answered)
	}
	if !strings.Contains(logged.String(), rawCause) {
		t.Error("the cause was not logged")
	}

	// The person retries the draft that was kept: still one template.
	sw.starter.fail = nil
	retried, err := sw.retry(id)
	if err != nil || str(retried, "templateId") != id || str(retried, "state") != compositionWriting {
		t.Fatalf("retrying the kept draft: %v %v", retried, err)
	}
	if n := len(sw.engine.rowsNow()); n != 1 {
		t.Errorf("%d templates after the retry, want the one draft", n)
	}

	// No goal at all is a goal that cannot start too.
	sw = newStartWorld(t)
	sw.withKit()
	sw.w.SetCompositionGoals(goalStarterFunc(func(context.Context, CompositionGoal) (string, error) { return " ", nil }), sw.canceller, sw.apps)
	nodes, err = sw.w.handleComposeEmail(importCtx(), composeArgs(), 0)
	if err != nil {
		t.Fatalf("an empty goal: %v", err)
	}
	if kind, out := startAnswer(t, nodes); kind != "campaignCompositionFailedToStart" || str(out, "state") != compositionFailed || str(out, "error") != composeStartFailed {
		t.Errorf("an empty goal answered %s %v", kind, out)
	}
	if rows := sw.engine.rowsNow(); len(rows) != 1 {
		t.Fatalf("%d templates", len(rows))
	} else if c, _ := compositionFromRow(rows[0]); c.State != compositionFailed || c.Error != composeStartFailed {
		t.Errorf("an empty goal left the composition %s %q", c.State, c.Error)
	}
}

// The work runtime can open a goal and then fail to start its run, and answer
// both the goal and the error. The start fails the composition as for any goal
// that cannot open, and stops that goal: nothing will use it, and it would
// otherwise stay open.
func TestComposeEmailStopsAGoalThatOpenedHalfway(t *testing.T) {
	sw := newStartWorld(t)
	sw.withKit()
	logged := sw.logs()
	sw.w.SetCompositionGoals(goalStarterFunc(func(context.Context, CompositionGoal) (string, error) {
		return " v1:work:goal:half-open ", errors.New(rawCause)
	}), sw.canceller, sw.apps)
	// The composition never names that goal: the state the canceller reads is
	// the one template's.
	sw.canceller.state = func(string) string {
		for _, row := range sw.engine.rowsNow() {
			c, _ := compositionFromRow(row)
			return c.State
		}
		return ""
	}
	nodes, err := sw.w.handleComposeEmail(importCtx(), composeArgs(), 0)
	if err != nil {
		t.Fatalf("a goal that opened halfway is answered, not refused: %v", err)
	}
	kind, out := startAnswer(t, nodes)
	id := str(out, "templateId")
	if kind != "campaignCompositionFailedToStart" || str(out, "state") != compositionFailed || str(out, "error") != composeStartFailed {
		t.Errorf("answer = %s %v", kind, out)
	}
	if c := sw.compositionOf(id); c.State != compositionFailed || c.Error != composeStartFailed || c.GoalID != "" {
		t.Errorf("composition = %s %q goal %q", c.State, c.Error, c.GoalID)
	}
	cancelled := sw.canceller.snapshot()
	if len(cancelled) != 1 || !sameGoal(cancelled[0].goalID, "half-open") || cancelled[0].reason != composeStartStop || cancelled[0].state != compositionFailed {
		t.Errorf("cancelled = %+v, want the goal that opened, stopped once the composition is failed", cancelled)
	}
	if !strings.Contains(logged.String(), rawCause) {
		t.Error("the cause was not logged")
	}
}

// The goal is opened on a context the caller's going away does not reach: a
// page closed while the goal is being opened neither cuts the opening short (a
// goal left with no run) nor fails the composition with the cancellation.
func TestComposeEmailOpensItsGoalWhenTheCallerLeaves(t *testing.T) {
	sw := newStartWorld(t)
	sw.withKit()
	ctx, leave := context.WithCancel(importCtx())
	defer leave()
	sw.starter.before = func(CompositionGoal, string) { leave() }
	nodes, err := sw.w.handleComposeEmail(ctx, composeArgs(), 0)
	if err != nil {
		t.Fatal(err)
	}
	kind, out := startAnswer(t, nodes)
	opened := sw.starter.snapshot()
	if len(opened) != 1 || opened[0].ctxErr != nil {
		t.Fatalf("the goal was opened on the caller's context: %+v", opened)
	}
	c := sw.compositionOf(str(out, "templateId"))
	if kind != "campaignCompositionStarted" || str(out, "state") != compositionWriting || c.State != compositionWriting || !sameGoal(c.GoalID, opened[0].goalID) {
		t.Errorf("answer %s %v, composition %s goal %q", kind, out, c.State, c.GoalID)
	}
}

// A cancel is held to the run it read: when that run ended and the template was
// retried before the cancel took the lock, the retried run and its goal are
// left alone.
func TestCancelCompositionIsHeldToTheRunItRead(t *testing.T) {
	sw := newStartWorld(t)
	sw.withKit()
	id, first := sw.mustCompose(composeArgs())
	later := templateNow.Add(time.Hour)
	second := ""
	gate, hooked := sw.w.templateGate, false
	sw.w.templateGate = func(ctx context.Context, key string) (func(), error) {
		if !hooked {
			// Before the cancel's lock: the run ends, failed, and is retried.
			hooked = true
			ended := sw.compositionOf(id)
			ended.State, ended.Error, ended.FinishedAt = compositionFailed, "MemQL finished without saving the email.", templateNow
			if _, err := sw.w.writeComposition(importCtx(), templateOrg, id, ended); err != nil {
				t.Error(err)
			}
			sw.w.now = func() time.Time { return later }
			retried, err := sw.retry(id)
			if err != nil {
				t.Errorf("the retry: %v", err)
			}
			second = str(retried, "goalId")
		}
		return gate(ctx, key)
	}
	out, err := sw.cancel(id)
	if err != nil {
		t.Fatal(err)
	}
	c := sw.compositionOf(id)
	if second == "" || c.State != compositionWriting || !sameGoal(c.GoalID, second) || !c.StartedAt.Equal(later) || c.Error != "" {
		t.Errorf("the stale cancel touched the retried run: %s %q goal %q (retried as %q)", c.State, c.Error, c.GoalID, second)
	}
	if cancelled := sw.canceller.snapshot(); len(cancelled) != 1 || !sameGoal(cancelled[0].goalID, first) {
		t.Errorf("goals asked to stop: %+v, want only the run the cancel read", cancelled)
	}
	if str(out, "state") != compositionWriting {
		t.Errorf("answer = %v", out)
	}
}

// A cancel whose caller has gone away still stops the goal and fails the
// composition: once it acts, it finishes.
func TestCancelCompositionFinishesWhenTheCallerLeaves(t *testing.T) {
	sw := newStartWorld(t)
	sw.withKit()
	id, goal := sw.mustCompose(composeArgs())
	ctx, leave := context.WithCancel(importCtx())
	leave()
	if _, err := sw.w.handleCancelComposition(ctx, map[string]any{"templateId": id}, 0); err != nil {
		t.Fatal(err)
	}
	cancelled := sw.canceller.snapshot()
	if len(cancelled) != 1 || !sameGoal(cancelled[0].goalID, goal) || cancelled[0].ctxErr != nil {
		t.Errorf("cancelled = %+v, want the goal asked on a context the caller's leaving does not reach", cancelled)
	}
	if c := sw.compositionOf(id); c.State != compositionFailed || c.Error != compositionCancelled {
		t.Errorf("composition = %s %q", c.State, c.Error)
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
	if c.State != compositionFailed || c.Error != compositionCancelled || c.GoalID != "" || str(out, "state") != compositionFailed {
		t.Errorf("composition = %s %q goal %q, answered %v", c.State, c.Error, c.GoalID, out)
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
	if !c.BaseRevision.Equal(revision) || str(out, "revision") != revision.Format(time.RFC3339Nano) || str(out, "templateId") != id || str(out, "state") != compositionWriting {
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

// A retry over a draft nobody changed since the failure writes the email in
// place, however the composition failed: its run saved nothing, its goal could
// not start, or a person cancelled it.
func TestRetryOnAnUntouchedFailedDraftWritesInPlace(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(sw *startWorld) string
	}{
		{"the run saved nothing", func(sw *startWorld) string {
			id, _ := sw.failedByItsRun()
			return id
		}},
		{"the goal could not start", func(sw *startWorld) string {
			sw.starter.fail = errors.New(rawCause)
			nodes, err := sw.w.handleComposeEmail(importCtx(), composeArgs(), 0)
			if err != nil {
				t.Fatal(err)
			}
			_, out := startAnswer(t, nodes)
			sw.starter.fail = nil
			return str(out, "templateId")
		}},
		{"a person cancelled it", func(sw *startWorld) string {
			id, _ := sw.mustCompose(composeArgs())
			if _, err := sw.cancel(id); err != nil {
				t.Fatal(err)
			}
			return id
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sw := newStartWorld(t)
			sw.withKit()
			id := tc.fail(sw)
			if c := sw.compositionOf(id); c.State != compositionFailed {
				t.Fatalf("composition = %s", c.State)
			}
			sw.w.now = func() time.Time { return templateNow.Add(time.Hour) }
			out, err := sw.retry(id)
			if err != nil {
				t.Fatal(err)
			}
			if c := sw.compositionOf(id); !c.BaseRevision.Equal(templateTime(sw.engine.row(id)["createdAt"])) {
				t.Errorf("the retried composition is not in step with the template it did not change: base %v", c.BaseRevision)
			}
			sw.ai.session = sw.submitting(articleComposition(""))
			if _, err := sw.run(runContext(str(out, "goalId")), id); err != nil {
				t.Fatalf("the retried run: %v", err)
			}
			row := sw.engine.row(id)
			if done := sw.compositionOf(id); done.State != compositionDone || len(sw.engine.rowsNow()) != 1 || str(row, "subject") != "Spring is here" {
				t.Errorf("after the retried run: %s, %d templates, subject %q", done.State, len(sw.engine.rowsNow()), str(row, "subject"))
			}
		})
	}
}

// A retry never writes over a person's work. When the template changed since
// the composition began -- a person saved and published it while MemQL wrote,
// or another writer changed it after the start failed -- the retry keeps the
// revision the composition began from, so the retried run's email is saved
// beside the template as "<name> (MemQL's version)", and the template keeps the
// person's content and status.
func TestRetryAfterAChangeToTheTemplateSavesACopy(t *testing.T) {
	ready := readyConfirmation(t)
	for _, tc := range []struct {
		name   string
		change func(sw *startWorld) string
		status string
	}{
		{"a person published their own email while MemQL wrote", func(sw *startWorld) string {
			id, goal := sw.mustCompose(composeArgs())
			for _, action := range []string{"save", "publish"} {
				if _, err := sw.personSaves(importCtx(), id, ready, action); err != nil {
					t.Fatalf("%s: %v", action, err)
				}
			}
			if c := sw.compositionOf(id); c.State != compositionWriting {
				t.Fatalf("a save while MemQL writes changed the composition: %s", c.State)
			}
			// The run then saves nothing, and the composition fails.
			if _, err := sw.run(runContext(goal), id); err == nil {
				t.Fatal("the run succeeded")
			}
			return id
		}, "ready"},
		{"another writer changed it after the goal could not start", func(sw *startWorld) string {
			sw.starter.fail = errors.New(rawCause)
			nodes, err := sw.w.handleComposeEmail(importCtx(), composeArgs(), 0)
			if err != nil {
				t.Fatal(err)
			}
			_, out := startAnswer(t, nodes)
			sw.starter.fail = nil
			id := str(out, "templateId")
			sw.engine.edit(id, func(row map[string]any) {
				row["subject"], row["createdAt"] = "Edited by hand", templateNow.Add(30*time.Minute).Format(time.RFC3339Nano)
			})
			return id
		}, "draft"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sw := newStartWorld(t)
			sw.withKit()
			id := tc.change(sw)
			if c := sw.compositionOf(id); c.State != compositionFailed {
				t.Fatalf("composition = %s %q", c.State, c.Error)
			}
			theirs := sw.engine.row(id)

			sw.w.now = func() time.Time { return templateNow.Add(time.Hour) }
			out, err := sw.retry(id)
			if err != nil {
				t.Fatal(err)
			}
			if c := sw.compositionOf(id); c.State != compositionWriting || c.BaseRevision.Equal(templateTime(sw.engine.row(id)["createdAt"])) {
				t.Fatalf("the retry took the changed template for the one it began from: %s base %v", c.State, c.BaseRevision)
			}
			sw.ai.session = sw.submitting(articleComposition(""))
			if _, err := sw.run(runContext(str(out, "goalId")), id); err != nil {
				t.Fatalf("the retried run: %v", err)
			}

			rows := sw.engine.rowsNow()
			if len(rows) != 2 {
				t.Fatalf("%d templates, want the person's and MemQL's version", len(rows))
			}
			original := sw.engine.row(id)
			for _, field := range []string{"subject", "status", "htmlBody", "textBody", "layoutId"} {
				if str(original, field) != str(theirs, field) {
					t.Errorf("the retry changed the template's %s: %q, was %q", field, str(original, field), str(theirs, field))
				}
			}
			if !sameJSON(t, original["document"], theirs["document"]) || str(original, "status") != tc.status {
				t.Errorf("the template lost the person's email: status %q", str(original, "status"))
			}
			var copied map[string]any
			for _, row := range rows {
				if bare(str(row, "id")) != id {
					copied = row
				}
			}
			if str(copied, "name") != composeCopyName || str(copied, "subject") != "Spring is here" || str(copied, "status") != "draft" {
				t.Errorf("MemQL's version: name %q subject %q status %q", str(copied, "name"), str(copied, "subject"), str(copied, "status"))
			}
			if c := sw.compositionOf(id); c.State != compositionFailed || c.Error != composeChanged {
				t.Errorf("the template's composition = %s %q", c.State, c.Error)
			}
		})
	}
}

// A person's save or publish of a template whose composition failed ends the
// failure: the email is theirs now, so neither client marks it Failed or offers
// Retry. The composition is done from that moment, and keeps what was asked,
// why it failed and the files it left out, as the record of what happened.
func TestAPersonsSaveOrPublishEndsAFailedComposition(t *testing.T) {
	t.Run("a save of the person's own changes", func(t *testing.T) {
		sw := newStartWorld(t)
		sw.withKit()
		id, failed := sw.failedByItsRun()
		later := templateNow.Add(time.Hour)
		sw.w.now = func() time.Time { return later }
		edited := sw.storedDocument(id)
		edited["subject"] = "Finished by hand"
		saved, err := sw.personSaves(importCtx(), id, edited, "save")
		if err != nil {
			t.Fatal(err)
		}
		row := sw.engine.row(id)
		if str(row, "subject") != "Finished by hand" || str(row, "status") != "draft" || saved.Revision != templateTime(row["createdAt"]).UTC().Format(time.RFC3339Nano) {
			t.Errorf("the save: subject %q status %q, receipt %+v", str(row, "subject"), str(row, "status"), saved)
		}
		c := sw.compositionOf(id)
		if c.State != compositionDone || !c.FinishedAt.Equal(later) {
			t.Errorf("composition = %s finished %v, want done at %v", c.State, c.FinishedAt, later)
		}
		if c.Error != failed.Error || !slices.Equal(c.Notes, failed.Notes) || !sameJSON(t, c.Request, failed.Request) || c.GoalID != failed.GoalID || !c.StartedAt.Equal(failed.StartedAt) {
			t.Errorf("the record of what happened changed: %+v, was %+v", c, failed)
		}
		// Nothing failed is left to retry, and a retry writes and starts nothing.
		writes, goals := len(sw.engine.writes()), len(sw.starter.snapshot())
		if _, err := sw.retry(id); err == nil || err.Error() != composeNotFailed {
			t.Errorf("a retry after the save: err = %v", err)
		}
		if len(sw.engine.writes()) != writes || len(sw.starter.snapshot()) != goals {
			t.Error("the refused retry wrote or started something")
		}
	})

	t.Run("a save of the draft as it is", func(t *testing.T) {
		sw := newStartWorld(t)
		sw.withKit()
		id, _ := sw.failedByItsRun()
		if _, err := sw.personSaves(importCtx(), id, sw.storedDocument(id), "save"); err != nil {
			t.Fatal(err)
		}
		if c := sw.compositionOf(id); c.State != compositionDone {
			t.Errorf("composition = %s", c.State)
		}
		// Saved again as it is, nothing more is written.
		writes := len(sw.engine.writes())
		if _, err := sw.personSaves(importCtx(), id, sw.storedDocument(id), "save"); err != nil {
			t.Fatal(err)
		}
		if n := len(sw.engine.writes()); n != writes {
			t.Errorf("an unchanged save wrote %d versions", n-writes)
		}
	})

	t.Run("a publish", func(t *testing.T) {
		sw := newStartWorld(t)
		sw.withKit()
		id := sw.start("builtin:article", nil, "")
		ready := readyConfirmation(t)
		if _, err := sw.personSaves(importCtx(), id, ready, "save"); err != nil {
			t.Fatal(err)
		}
		ended := sw.compositionOf(id)
		ended.State, ended.Error, ended.FinishedAt = compositionFailed, "MemQL finished without saving the email.", templateNow
		if _, err := sw.w.writeComposition(importCtx(), templateOrg, id, ended); err != nil {
			t.Fatal(err)
		}
		later := templateNow.Add(time.Hour)
		sw.w.now = func() time.Time { return later }
		published, err := sw.personSaves(importCtx(), id, ready, "publish")
		if err != nil {
			t.Fatal(err)
		}
		if published.Status != "ready" || str(sw.engine.row(id), "status") != "ready" {
			t.Errorf("publish answered %+v, row %q", published, str(sw.engine.row(id), "status"))
		}
		if c := sw.compositionOf(id); c.State != compositionDone || !c.FinishedAt.Equal(later) || c.Error != ended.Error {
			t.Errorf("composition = %s %q finished %v", c.State, c.Error, c.FinishedAt)
		}
	})

	t.Run("a refused save leaves it failed", func(t *testing.T) {
		sw := newStartWorld(t)
		sw.withKit()
		id, failed := sw.failedByItsRun()
		edited := sw.storedDocument(id)
		edited["subject"] = "Finished by hand"
		args := map[string]any{"templateId": id, "accountId": templateOrg, "name": "Spring news", "content": edited,
			"expectedRevision": templateNow.Add(-time.Hour).Format(time.RFC3339Nano), "action": "save"}
		if _, err := sw.w.handleSaveTemplate(importCtx(), args, 0); err == nil || err.Error() != templateChanged {
			t.Fatalf("a save of an old revision: err = %v", err)
		}
		if c := sw.compositionOf(id); c.State != compositionFailed || c.Error != failed.Error {
			t.Errorf("composition = %s %q", c.State, c.Error)
		}
	})

	t.Run("archive and restore are not edits", func(t *testing.T) {
		sw := newStartWorld(t)
		sw.withKit()
		id, _ := sw.failedByItsRun()
		for _, action := range []string{"archive", "restore"} {
			row := sw.engine.row(id)
			if _, err := sw.w.handleSaveTemplate(importCtx(), map[string]any{"templateId": id, "accountId": templateOrg,
				"expectedRevision": templateTime(row["createdAt"]).UTC().Format(time.RFC3339Nano), "action": action}, 0); err != nil {
				t.Fatalf("%s: %v", action, err)
			}
		}
		if c := sw.compositionOf(id); c.State != compositionFailed {
			t.Errorf("composition = %s", c.State)
		}
	})
}

// Only the person who asked MemQL to write an email can retry it: a retry reads
// the files the request names as whoever runs it, so a colleague's retry would
// read the colleague's own Library into a template the author can read, and
// cannot read the author's files anyway. A colleague's retry changes nothing
// and says why; anyone who may write the template can still cancel it.
func TestRetryCompositionIsTheRequestersAlone(t *testing.T) {
	sw := newStartWorld(t)
	sw.withKit()
	id, _ := sw.mustCompose(composeArgs())
	if got := sw.compositionOf(id).Request.RequestedBy; got != testOwner {
		t.Errorf("requested by %q, want the caller %q", got, testOwner)
	}

	// A colleague cancels it.
	if _, err := sw.w.handleCancelComposition(colleagueCtx(), map[string]any{"templateId": id}, 0); err != nil {
		t.Fatalf("a colleague's cancel: %v", err)
	}
	if c := sw.compositionOf(id); c.State != compositionFailed || c.Error != compositionCancelled {
		t.Fatalf("composition = %s %q", c.State, c.Error)
	}

	// A colleague's retry is refused, before anything is asked or written.
	writes, goals, asked := len(sw.engine.writes()), len(sw.starter.snapshot()), len(sw.apps.askedAbout())
	if _, err := sw.w.handleRetryComposition(colleagueCtx(), map[string]any{"templateId": id}, 0); err == nil || err.Error() != composeNotYours {
		t.Fatalf("a colleague's retry: err = %v", err)
	}
	if len(sw.engine.writes()) != writes || len(sw.starter.snapshot()) != goals || len(sw.apps.askedAbout()) != asked {
		t.Error("a colleague's retry wrote, started or asked something")
	}
	if c := sw.compositionOf(id); c.State != compositionFailed {
		t.Errorf("composition = %s", c.State)
	}

	// The person who asked retries it, however their id is written, and it is
	// still theirs.
	asker := auth.ContextWithUserActor(context.Background(), "v1:identity:user:"+testOwner)
	nodes, err := sw.w.handleRetryComposition(asker, map[string]any{"templateId": id}, 0)
	if err != nil {
		t.Fatalf("the requester's retry: %v", err)
	}
	if kind, out := startAnswer(t, nodes); kind != "campaignCompositionStarted" || str(out, "state") != compositionWriting {
		t.Errorf("answer = %s %v", kind, out)
	}
	if got := sw.compositionOf(id).Request.RequestedBy; got != testOwner {
		t.Errorf("after the retry, requested by %q", got)
	}

	// A composition that does not say who asked is nobody's to retry.
	unsaid := sw.compositionOf(id)
	unsaid.State, unsaid.Error, unsaid.Request.RequestedBy = compositionFailed, compositionCancelled, ""
	if _, err := sw.w.writeComposition(importCtx(), templateOrg, id, unsaid); err != nil {
		t.Fatal(err)
	}
	if _, err := sw.retry(id); err == nil || err.Error() != composeNotYours {
		t.Errorf("a retry of a composition nobody is named for: err = %v", err)
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
		sw.starter.fail = errors.New(rawCause)
		nodes, err := sw.w.handleRetryComposition(importCtx(), map[string]any{"templateId": id}, 0)
		if err != nil {
			t.Fatalf("a retry whose goal cannot start is answered, not refused: %v", err)
		}
		if kind, out := startAnswer(t, nodes); kind != "campaignCompositionFailedToStart" || str(out, "templateId") != id ||
			str(out, "state") != compositionFailed || str(out, "error") != composeStartFailed {
			t.Errorf("answer = %s %v", kind, out)
		}
		if c := sw.compositionOf(id); c.State != compositionFailed || c.Error != composeStartFailed {
			t.Errorf("composition = %s %q", c.State, c.Error)
		}
		if n := len(sw.engine.rowsNow()); n != 1 {
			t.Errorf("%d templates", n)
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

// TestAskCompositionToolsLoadWithTheirBuiltins: Ask's assistant reaches the
// layout catalogue and the start of a composition through two tools of its
// own, each the assistant's alone. Each declares exactly the fields its builtin
// takes -- a field the builtin does not take is refused on every call, and one
// it requires that the tool does not declare could never be sent -- and stamps
// none: a composition started from Ask is the signed-in person's, as Write it
// is.
func TestAskCompositionToolsLoadWithTheirBuiltins(t *testing.T) {
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
	for _, tc := range []struct{ tool, builtin, executor string }{
		{"emailLayoutCatalogue", "campaignLayoutCatalogue", "integration.campaigns.layoutCatalogue"},
		{"startEmailComposition", "campaignComposeEmail", "integration.campaigns.composeEmail"},
	} {
		got, err := eng.Tools().Get(tc.tool)
		if err != nil {
			t.Fatalf("tool %s: %v", tc.tool, err)
		}
		if got.Handler == nil || got.Handler.Type != "function" || got.Handler.FunctionName != tc.builtin {
			t.Errorf("%s handler = %+v, want function %s", tc.tool, got.Handler, tc.builtin)
		}
		if !slices.Equal(got.RequiresAgentRole, []string{"assistant"}) || len(got.AutoInjectedFields) != 0 {
			t.Errorf("%s: roles %v, stamped %v; want the assistant's, stamping nothing", tc.tool, got.RequiresAgentRole, got.AutoInjectedFields)
		}
		fn, err := eng.Functions().Get(tc.builtin)
		if err != nil || fn == nil || !fn.IsBuiltin() || fn.Executor != tc.executor {
			t.Fatalf("builtin %s: %+v %v", tc.builtin, fn, err)
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(got.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		declared := slices.Sorted(maps.Keys(schema.Properties))
		var taken []string
		if fn.BuiltinArgs != nil {
			taken = slices.Sorted(maps.Keys(fn.BuiltinArgs.Properties))
		}
		if !slices.Equal(declared, taken) {
			t.Errorf("%s declares %v; %s takes %v", tc.tool, declared, tc.builtin, taken)
		}
		var required []string
		if fn.BuiltinArgs != nil {
			required = slices.Clone(fn.BuiltinArgs.Required)
		}
		slices.Sort(required)
		slices.Sort(schema.Required)
		if !slices.Equal(schema.Required, required) {
			t.Errorf("%s requires %v; %s requires %v", tc.tool, schema.Required, tc.builtin, required)
		}
	}
}
