package work

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/work"
)

// replyEntries decodes a multi-node builtin reply into its entries by id.
func replyEntries(t *testing.T, nodes []memorynodes.MemoryNode) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, n := range nodes {
		var payload map[string]any
		if err := json.Unmarshal(n.Payload, &payload); err != nil {
			t.Fatalf("reply node %s: %v", n.ID, err)
		}
		if _, dup := out[n.ID]; dup {
			t.Fatalf("two reply nodes share the id %s; a builtin reply is one map keyed by id, so one of them would be lost on the wire", n.ID)
		}
		out[n.ID] = payload
	}
	return out
}

// TestStepVersionsFoldsEachVersionToItsNewestRowVersion is the fold the raw
// read exists for, without a database: an intent and a receipt of one version
// are one entry carrying the receipt, a re-asserted version is still one entry,
// and a row from before versions were a field takes its attempt.
func TestStepVersionsFoldsEachVersionToItsNewestRowVersion(t *testing.T) {
	i, _, store := newActsIntegration(t)
	addPristineRun(store, actRunId, "fetch", "draft")
	addVersion(store, actRunId, "draft", 1, 2, "done", work.Head{"fetch": {Version: 1}}, nil)
	// A head move re-asserted version 1 of draft after version 2 existed.
	reasserted := map[string]any{
		"runId": actRunId, "ownerUserId": "v1:identity:user:" + actOwner, "key": "draft", "seq": 1,
		"status": "done", "attempt": 1, "version": 1, "result": map[string]any{"result": "draft version 1"},
	}
	store.add("v1:work:step:r-acts-draft", testNow.Add(24*time.Hour), reasserted)
	// A row of another run is never part of this one's history.
	addPristineRun(store, "v1:work:run:r-other", "fetch")
	// A row written before `version` existed: attempt is its version.
	store.add("v1:work:step:r-acts-legacy", testNow, map[string]any{
		"runId": "r-acts", "ownerUserId": "v1:identity:user:" + actOwner, "key": "legacy", "seq": 9,
		"status": "done", "attempt": 3,
	})

	rows, err := i.StepVersions(callerContext(actOwner), actRunId)
	if err != nil {
		t.Fatalf("StepVersions: %v", err)
	}
	type version struct {
		key     string
		version int
	}
	got := map[version]map[string]any{}
	for _, r := range rows {
		k := version{rowString(r, "key"), rowVersion(r)}
		if _, dup := got[k]; dup {
			t.Fatalf("version %d of %s appears twice; each version is ONE entry", k.version, k.key)
		}
		got[k] = r
	}
	if len(got) != 4 {
		t.Fatalf("got %d versions %v, want fetch v1, draft v1, draft v2 and legacy v3", len(got), got)
	}
	if r := got[version{"draft", 2}]; rowString(r, "status") != "done" || rowString(rowMap(r, "result"), "result") != "draft version 2" {
		t.Errorf("draft v2 = %v; the entry must be the RECEIPT, the newest row-version of the version", r)
	}
	if r := got[version{"draft", 1}]; r["createdAt"] != testNow.Add(24*time.Hour).UTC().Format(time.RFC3339Nano) {
		t.Errorf("draft v1 = %v; the re-asserted row-version is the newest of version 1", r)
	}
	if r := got[version{"legacy", 3}]; r == nil || rowInt(r, "version") != 3 {
		t.Errorf("a row with no version field must carry its attempt as its version, got %v", r)
	}
	// Sorted by seq: fetch (0), draft (1, both versions), legacy (9).
	if rowString(rows[0], "key") != "fetch" || rowString(rows[len(rows)-1], "key") != "legacy" {
		t.Errorf("rows are not in seq order: %v", rows)
	}
	queries := store.queried()
	if len(queries) != 1 || !strings.Contains(queries[0], "'r-acts'") || !strings.Contains(queries[0], "'"+actRunId+"'") {
		t.Errorf("the read must match the run id in BOTH forms the journal writes; queries: %v", queries)
	}
}

// TestStepVersionsGatesEachFoldedRow: a row the caller may not see is not
// returned, and the gate runs on the NEWEST row-version of a version, so a
// denied newest never falls through to an admitted older one.
func TestStepVersionsGatesEachFoldedRow(t *testing.T) {
	i, _, store := newActsIntegration(t)
	addPristineRun(store, actRunId, "fetch")
	older := map[string]any{"runId": actRunId, "ownerUserId": "v1:identity:user:" + actOwner, "key": "secret", "seq": 1, "status": "running", "attempt": 1}
	newer := map[string]any{"runId": actRunId, "ownerUserId": "v1:identity:user:u-mallory", "key": "secret", "seq": 1, "status": "done", "attempt": 1}
	store.add("v1:work:step:r-acts-secret", testNow, older)
	store.add("v1:work:step:r-acts-secret", testNow.Add(time.Minute), newer)
	i.admitRow = ownerAdmits(actOwner)

	rows, err := i.StepVersions(callerContext(actOwner), actRunId)
	if err != nil {
		t.Fatalf("StepVersions: %v", err)
	}
	for _, r := range rows {
		if rowString(r, "key") == "secret" {
			t.Fatalf("a version whose newest row-version the gate denied came back as %v; gating before folding hands the caller a stale row instead of none", r)
		}
	}
	if len(rows) != 1 {
		t.Errorf("got %d rows, want only fetch", len(rows))
	}
}

// TestStepVersionsRefusesWithoutAGate: a hand-rolled read with no admission
// gate is refused, never answered with every row.
func TestStepVersionsRefusesWithoutAGate(t *testing.T) {
	i, _, store := newActsIntegration(t)
	addPristineRun(store, actRunId, "fetch")
	i.admitRow = nil
	if _, err := i.StepVersions(callerContext(actOwner), actRunId); err == nil {
		t.Fatal("StepVersions answered with no row-admission gate wired")
	}
	if n := len(store.queried()); n != 0 {
		t.Errorf("the read reached the database (%d queries) before refusing", n)
	}
}

func TestStepVersionsRefusesSomebodyElsesRun(t *testing.T) {
	i, eng, store := newActsIntegration(t)
	addPristineRun(store, actRunId, "fetch", "draft")
	// workRunForOwner answers nothing: the owned read gate's answer for a run
	// that is not the caller's -- zero rows and no error.
	_, err := i.handleStepVersions(callerContext("u-mallory"), map[string]any{"runId": actRunId}, 0)
	var refusal *ActRefusal
	if !errors.As(err, &refusal) || refusal.Code != codeRunNotFound {
		t.Fatalf("err = %v, want a %s refusal", err, codeRunNotFound)
	}
	if n := len(store.queried()); n != 0 {
		t.Errorf("somebody else's version history was read (%d queries) before the refusal", n)
	}
	if c := eng.callTo(t, "workRunForOwner"); c.Actor != "u-mallory" || c.Origin.IsInternal() {
		t.Errorf("the ownership read ran as %q with origin %v; it must be the caller's own unstamped read", c.Actor, c.Origin)
	}
}

// TestStepVersionsMarksTheHead: current is the head's word for a top-level
// step, and a distinct id per version keeps every entry on the wire.
func TestStepVersionsMarksTheHead(t *testing.T) {
	i, eng, store := newActsIntegration(t)
	addPristineRun(store, actRunId, "fetch", "draft", "publish")
	addVersion(store, actRunId, "draft", 1, 2, "done", work.Head{"fetch": {Version: 1}}, nil)
	addVersion(store, actRunId, "publish", 2, 2, "done", work.Head{"fetch": {Version: 1}, "draft": {Version: 2}}, nil)
	run := actRunRow(runStatusSucceeded)
	// The person moved back to draft v1; publish v1 was restored with it.
	run["head"] = work.Head{"fetch": {Version: 1}, "draft": {Version: 1}, "publish": {Version: 1}}.Object()
	eng.reply("workRunForOwner", run)

	nodes, err := i.handleStepVersions(callerContext(actOwner), map[string]any{"runId": actRunId}, 0)
	if err != nil {
		t.Fatalf("workStepVersions: %v", err)
	}
	entries := replyEntries(t, nodes)
	want := map[string]bool{
		"r-acts-fetch@v1":   true,
		"r-acts-draft@v1":   true,
		"r-acts-draft@v2":   false,
		"r-acts-publish@v1": true,
		"r-acts-publish@v2": false,
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries %v, want %d", len(entries), keysOf(entries), len(want))
	}
	for id, current := range want {
		e, ok := entries[id]
		if !ok {
			t.Errorf("no entry %s among %v", id, keysOf(entries))
			continue
		}
		if e["current"] != current {
			t.Errorf("%s current = %v, want %v", id, e["current"], current)
		}
	}
	if got := rowString(rowMap(entries["r-acts-draft@v2"], "result"), "result"); got != "draft version 2" {
		t.Errorf("draft v2 carries result %q; every version keeps its own content", got)
	}
}

// A run written before the head existed marks each step's newest version.
func TestStepVersionsWithNoHeadMarksTheNewest(t *testing.T) {
	i, eng, store := newActsIntegration(t)
	addPristineRun(store, actRunId, "fetch", "draft")
	addVersion(store, actRunId, "draft", 1, 2, "done", nil, nil)
	eng.reply("workRunForOwner", actRunRow(runStatusSucceeded, "fetch", "draft"))

	nodes, err := i.handleStepVersions(callerContext(actOwner), map[string]any{"runId": actRunId}, 0)
	if err != nil {
		t.Fatalf("workStepVersions: %v", err)
	}
	entries := replyEntries(t, nodes)
	if entries["r-acts-draft@v2"]["current"] != true || entries["r-acts-draft@v1"]["current"] != false || entries["r-acts-fetch@v1"]["current"] != true {
		t.Errorf("with no stored head the newest version of each step is current, got %v", entries)
	}
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
