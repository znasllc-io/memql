package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// versions_db_test.go -- the epic memql#5414 acts against a REAL engine and
// Postgres (epic memql#4966's reasoning, goal_db_test.go's header): the raw
// version read's SQL, the concept type checks on every new field the acts
// write, row admission, and -- the one no fake can show -- that a head move's
// re-assertion makes every COLLAPSED read answer with the head.
//
// Rows are written the way the journal writes them: an intent (createWorkStep)
// and a receipt (updateWorkStep) per version, each a row-version of one step
// row, under the owner's borrowed authority and internal origin.

type actsDB struct {
	i     *Integration
	eng   *memqlengine.MemQLEngine
	db    *bun.DB
	owner string
}

func openActsDB(t *testing.T) actsDB {
	t.Helper()
	eng := openWorkTestEngine(t)
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	i := New(eng, testLogger())
	i.bunDB = func() *bun.DB { return db }
	i.admitRow = memqlengine.AdmitSourceRow
	owner := "dbtest-work-acts-" + strings.NewReplacer(".", "", ":", "").Replace(time.Now().UTC().Format("20060102150405.000000000"))
	return actsDB{i: i, eng: eng, db: db, owner: owner}
}

func (a actsDB) write(t *testing.T, name string, args map[string]any) {
	t.Helper()
	if err := a.i.store().writeInternal(ownerActor(context.Background(), a.owner), "mutation "+call(name, args)); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func (a actsDB) query(t *testing.T, userId, q string) []map[string]any {
	t.Helper()
	res, err := a.eng.Execute(actorCtx(userId), q)
	if err != nil {
		t.Fatalf("%s as %s: %v", q, userId, err)
	}
	return memqlengine.MaterializeRows(res)
}

// openRun writes a run as its owner and returns its id.
func (a actsDB) openRun(t *testing.T, extra map[string]any) string {
	t.Helper()
	runId := newRowId(runConcept)
	args := map[string]any{
		"runId": runId, "automationName": "weeklyReport", "templateFingerprint": "fp-weekly",
		"mode": modeLive, "status": runStatusRunning, "startedAt": rfc(time.Now()),
	}
	for k, v := range extra {
		args[k] = v
	}
	a.write(t, "createWorkRun", args)
	return runId
}

// writeVersion writes one version of one step, intent then receipt, the way
// the journal does. A version after the first names the reset fields, so it
// does not carry the previous version's answer through the read-merge.
func (a actsDB) writeVersion(t *testing.T, runId, runIdForm, key string, seq, version int, basis work.Head, result string, extra map[string]any) {
	t.Helper()
	stepId := bareRunId(runId) + "-" + key
	intent := map[string]any{
		"stepId": stepId, "runId": runIdForm, "key": key, "seq": seq, "stepType": "function",
		"kind": "reasoning", "call": map[string]any{"construct": "function", "name": key},
		"status": "running", "attempt": version, "version": version,
		"idempotencyKey": runId + ":" + key + ":" + itoa(version), "startedAt": rfc(time.Now()),
	}
	if version > 1 {
		for k, v := range map[string]any{
			"result": map[string]any{}, "resultFingerprint": "", "binding": map[string]any{},
			"errorCode": "", "errorMessage": "", "childRunId": "", "override": map[string]any{}, "authoredBy": "",
		} {
			intent[k] = v
		}
	}
	if basis != nil {
		intent["basis"] = basis.Object()
	}
	a.write(t, "createWorkStep", intent)
	receipt := map[string]any{
		"stepId": stepId, "status": "done", "result": map[string]any{"status": "done", "result": result},
		"resultFingerprint": "fp-" + result, "finishedAt": rfc(time.Now()), "durationMs": 1000 + version,
	}
	for k, v := range extra {
		receipt[k] = v
	}
	a.write(t, "updateWorkStep", receipt)
}

func (a actsDB) finishRun(t *testing.T, runId string, order []string, head work.Head) {
	t.Helper()
	args := map[string]any{"runId": runId, "status": runStatusSucceeded, "stepOrder": order, "finishedAt": rfc(time.Now())}
	if head != nil {
		args["head"] = head.Object()
	}
	a.write(t, "updateWorkRun", args)
}

func (a actsDB) runRow(t *testing.T, runId string) map[string]any {
	t.Helper()
	rows := a.query(t, a.owner, "query "+call("workRunForOwner", map[string]any{"runId": runId}))
	if len(rows) != 1 {
		t.Fatalf("run %s: %d rows for its owner", runId, len(rows))
	}
	return rows[0]
}

func TestStepVersionsReturnsEveryVersionOnceAndMarksTheHead(t *testing.T) {
	a := openActsDB(t)
	runId := a.openRun(t, nil)
	canonicalRun := runId
	// The journal writes the run id bare for a trigger run and canonical for
	// an adopted goal's; both forms must be read.
	a.writeVersion(t, runId, bareRunId(runId), "fetch", 0, 1, nil, "fetched", nil)
	a.writeVersion(t, runId, canonicalRun, "draft", 1, 1, nil, "draft one", nil)
	a.writeVersion(t, runId, canonicalRun, "draft", 1, 2, work.Head{"fetch": {Version: 1}}, "draft two", nil)
	a.finishRun(t, runId, []string{"fetch", "draft"}, work.Head{"fetch": {Version: 1}, "draft": {Version: 1}})
	// A row written before `version` was a field, stored with the run id
	// bare: its attempt is its version.
	legacy, _ := json.Marshal(map[string]any{
		"runId": bareRunId(runId), "ownerUserId": "v1:identity:user:" + a.owner, "key": "legacy", "seq": 5,
		"status": "done", "attempt": 3, "stepType": "function",
	})
	if _, err := a.db.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",type,schema,payload) VALUES (?, ?, ?, 'acts-test', 'object', '{}', ?::jsonb)`,
		"v1:work:step:"+bareRunId(runId)+"-legacy", "v1:work:step", time.Now().UTC(), string(legacy)); err != nil {
		t.Fatalf("insert the legacy row: %v", err)
	}

	rows, err := a.i.StepVersions(actorCtx(a.owner), runId)
	if err != nil {
		t.Fatalf("StepVersions: %v", err)
	}
	got := map[string]map[string]any{}
	for _, r := range rows {
		id := fmt.Sprintf("%s@v%d", rowString(r, "key"), rowVersion(r))
		if _, dup := got[id]; dup {
			t.Fatalf("%s came back twice; the intent and the receipt of one version are ONE entry", id)
		}
		got[id] = r
	}
	if len(got) != 4 {
		t.Fatalf("got versions %v, want fetch v1, draft v1, draft v2 and legacy v3", keysOf(got))
	}
	for id, want := range map[string]string{"fetch@v1": "fetched", "draft@v1": "draft one", "draft@v2": "draft two"} {
		r := got[id]
		if rowString(r, "status") != "done" || rowString(rowMap(r, "result"), "result") != want {
			t.Errorf("%s = status %v result %v; each version is its RECEIPT, with its own answer", id, r["status"], r["result"])
		}
	}
	if basis := work.ParseHead(got["draft@v2"]["basis"]); !basis.Equal(work.Head{"fetch": {Version: 1}}) {
		t.Errorf("draft v2 basis = %v", got["draft@v2"]["basis"])
	}
	if got["legacy@v3"] == nil {
		t.Error("the row stored with a bare run id and no version field was not read")
	}

	nodes, err := a.i.handleStepVersions(actorCtx(a.owner), map[string]any{"runId": runId}, 0)
	if err != nil {
		t.Fatalf("workStepVersions: %v", err)
	}
	entries := replyEntries(t, nodes)
	short := bareRunId(runId)
	for id, current := range map[string]bool{
		short + "-fetch@v1": true, short + "-draft@v1": true, short + "-draft@v2": false, short + "-legacy@v3": true,
	} {
		e, ok := entries[id]
		if !ok {
			t.Errorf("no entry %s among %v", id, keysOf(entries))
			continue
		}
		if e["current"] != current {
			t.Errorf("%s current = %v, want %v", id, e["current"], current)
		}
	}

	// Somebody else reads nothing: the handler refuses the run, and the raw
	// read's admission denies every row.
	stranger := a.owner + "-stranger"
	if _, err := a.i.handleStepVersions(actorCtx(stranger), map[string]any{"runId": runId}, 0); err == nil {
		t.Error("a stranger read the version history")
	}
	if rows, err := a.i.StepVersions(actorCtx(stranger), runId); err != nil || len(rows) != 0 {
		t.Errorf("the raw read admitted %d rows to a stranger (err %v); row admission is its whole enforcement", len(rows), err)
	}
}

// The head move's whole point, measured where only a database can show it:
// after moving back, every COLLAPSED read answers with the head, and the later
// versions are still there.
func TestAHeadMoveMakesEveryCollapsedReadAnswerWithTheHead(t *testing.T) {
	a := openActsDB(t)
	runId := a.openRun(t, nil)
	a.writeVersion(t, runId, runId, "fetch", 0, 1, nil, "fetched", nil)
	a.writeVersion(t, runId, runId, "draft", 1, 1, nil, "draft one", nil)
	a.writeVersion(t, runId, runId, "review", 2, 1, nil, "review one", nil)
	a.writeVersion(t, runId, runId, "draft", 1, 2, work.Head{"fetch": {Version: 1}}, "draft two",
		map[string]any{"override": map[string]any{"level": "reasoning", "requestedBy": a.owner}})
	a.writeVersion(t, runId, runId, "review", 2, 2, work.Head{"fetch": {Version: 1}, "draft": {Version: 2}}, "review two", nil)
	a.finishRun(t, runId, []string{"fetch", "draft", "review"}, work.Head{"fetch": {Version: 1}, "draft": {Version: 2}, "review": {Version: 2}})

	nodes, err := a.i.handleMoveRunHead(actorCtx(a.owner), map[string]any{"runId": runId, "stepKey": "draft", "version": float64(1)}, 0)
	if err != nil {
		t.Fatalf("moveRunHead: %v", err)
	}
	if stale := decodeReply(t, nodes)["staleSteps"]; stale == nil || len(stale.([]any)) != 0 {
		t.Fatalf("staleSteps = %v; every downstream version matched, so nothing runs again", stale)
	}

	collapsed := map[string]map[string]any{}
	for _, r := range a.query(t, a.owner, "query "+call("workStepsForOwnerRun", map[string]any{"runId": runId})) {
		collapsed[rowString(r, "key")] = r
	}
	for key, want := range map[string]string{"draft": "draft one", "review": "review one"} {
		r := collapsed[key]
		if rowVersion(r) != 1 || rowString(rowMap(r, "result"), "result") != want {
			t.Errorf("the collapsed read of %s answers version %d with %v; after the head moved back it must answer the head, version 1", key, rowVersion(r), r["result"])
		}
	}
	if o := rowMap(collapsed["draft"], "override"); len(o) != 0 {
		t.Errorf("draft's collapsed row still carries version 2's override %v; the re-assertion names every per-version field", o)
	}
	run := a.runRow(t, runId)
	if !work.ParseHead(run["head"]).Equal(work.Head{"fetch": {Version: 1}, "draft": {Version: 1}, "review": {Version: 1}}) {
		t.Errorf("run head = %v", run["head"])
	}
	if rowString(run, "status") != runStatusSucceeded {
		t.Errorf("run status = %v; a head move that restores everything leaves the run finished", run["status"])
	}
	rows, err := a.i.StepVersions(actorCtx(a.owner), runId)
	if err != nil {
		t.Fatalf("StepVersions: %v", err)
	}
	if len(rows) != 5 {
		t.Errorf("the version history holds %d versions after the move, want all 5 -- nothing is ever deleted", len(rows))
	}
}

// A re-run and a feedback row land on the concepts they write, through the
// real type checks: run.rerun and run.staleSteps, the reopened run's cleared
// fields, and observation.kind "feedback" -- which, with its goal signature,
// is what description guidance reads back.
func TestARerunAndAVerdictLandOnTheirRows(t *testing.T) {
	a := openActsDB(t)
	signature := "sig-dbtest-" + a.owner
	runId := a.openRun(t, map[string]any{"goalSignature": signature})
	a.writeVersion(t, runId, runId, "fetch", 0, 1, nil, "fetched", nil)
	a.writeVersion(t, runId, runId, "draft", 1, 1, nil, "draft one", nil)
	a.finishRun(t, runId, []string{"fetch", "draft"}, nil)
	a.write(t, "updateWorkRun", map[string]any{"runId": runId, "errorCode": "earlier_failure"})

	if _, err := a.i.handleRecordFeedback(actorCtx(a.owner), map[string]any{
		"runId": runId, "stepKey": "draft", "verdict": "dislike", "product": true, "reason": "the totals are missing",
	}, 0); err != nil {
		t.Fatalf("recordFeedback: %v", err)
	}
	guidance := a.query(t, a.owner, "query "+call("workDescriptionGuidance", map[string]any{"goalSignature": signature}))
	if len(guidance) != 1 || rowString(rowMap(guidance[0], "data"), "reason") != "the totals are missing" {
		t.Fatalf("description guidance for the goal = %v; the dislike must be readable by the signature it carries", guidance)
	}

	nodes, err := a.i.handleRerunStep(actorCtx(a.owner), map[string]any{"runId": runId, "stepKey": "draft", "level": "reasoning"}, 0)
	if err != nil {
		t.Fatalf("rerunStep: %v", err)
	}
	if v := decodeReply(t, nodes)["version"]; v != float64(2) {
		t.Errorf("version = %v", v)
	}
	run := a.runRow(t, runId)
	if rowString(run, "status") != runStatusRunning || rowString(run, "errorCode") != "" {
		t.Errorf("run status %v errorCode %q; the re-run reopens the run and clears its end", run["status"], rowString(run, "errorCode"))
	}
	rerun := rowMap(run, "rerun")
	override := rowMap(rerun, "override")
	if rowString(rerun, "reason") != rerunReasonRerun || rowString(rerun, "stepKey") != "draft" || rowString(override, "level") != "reasoning" {
		t.Errorf("run.rerun = %v", rerun)
	}
	if g := rowMap(override, "guidance"); rowString(g, "reason") != "the totals are missing" {
		t.Errorf("the stored override carries guidance %v", g)
	}
	if stale := rowStringSlice(run, "staleSteps"); len(stale) != 1 || stale[0] != "draft" {
		t.Errorf("run.staleSteps = %v", stale)
	}
}

// A branch's fork run type-checks with its head, its request and the goal
// signature it inherits, and belongs to the source's owner.
func TestABranchOpensAForkRunItsOwnerCanRead(t *testing.T) {
	a := openActsDB(t)
	source := a.openRun(t, map[string]any{"goalSignature": "sig-branch-" + a.owner, "variables": map[string]any{"week": "39"}})
	a.writeVersion(t, source, source, "fetch", 0, 1, nil, "fetched", nil)
	a.writeVersion(t, source, source, "draft", 1, 1, nil, "draft one", nil)
	a.finishRun(t, source, []string{"fetch", "draft"}, nil)

	nodes, err := a.i.handleBranchRun(actorCtx(a.owner), map[string]any{"runId": source, "stepKey": "draft", "effort": "high"}, 0)
	if err != nil {
		t.Fatalf("branchRun: %v", err)
	}
	forkId := rowString(decodeReply(t, nodes), "runId")
	fork := a.runRow(t, forkId)
	if rowString(fork, "mode") != modeFork || rowString(fork, "forkAtStepKey") != "draft" || rowString(fork, "goalSignature") != "sig-branch-"+a.owner {
		t.Errorf("fork = mode %v at %v signature %v", fork["mode"], fork["forkAtStepKey"], fork["goalSignature"])
	}
	head := work.ParseHead(fork["head"])
	if e := head["fetch"]; e.Version != 1 || bareRunId(e.RunId) != bareRunId(source) {
		t.Errorf("fork head = %v, want fetch v1 in the source", fork["head"])
	}
	if rerun := rowMap(fork, "rerun"); rowString(rerun, "reason") != rerunReasonBranch || rowString(rowMap(rerun, "override"), "effort") != "high" {
		t.Errorf("fork rerun = %v", rerun)
	}
	if rowString(a.runRow(t, source), "status") != runStatusSucceeded {
		t.Error("the branch touched its source")
	}
}

// dbJudge is the real engine with the model call faked: the validator's rows
// go through the real type checks, and no provider is needed.
type dbJudge struct {
	*memqlengine.MemQLEngine
	prompts *memqlengine.PromptRegistry
}

func (j *dbJudge) Prompts() *memqlengine.PromptRegistry { return j.prompts }
func (j *dbJudge) RenderPrompt(name string, data map[string]any) (string, error) {
	return (&judgeEngine{prompts: j.prompts}).RenderPrompt(name, data)
}
func (j *dbJudge) CallAIStructured(_ context.Context, req airoute.ResolveRequest, _ []common.ChatMessage, _ common.StructuredSchema) (memqlengine.StructuredAIResult, error) {
	out := memqlengine.StructuredAIResult{Text: `{"product": false, "process": false, "performance": true, "reason": "It padded the summary."}`}
	out.Resolution.Model = "chat54"
	out.Resolution.Decision.ServedLevel = req.Level
	return out, nil
}

func TestTheValidatorsRowsLand(t *testing.T) {
	a := openActsDB(t)
	a.i.engine = &dbJudge{MemQLEngine: a.eng, prompts: realPrompts(t)}
	goalId := newRowId(goalConcept)
	a.write(t, "createWorkGoal", map[string]any{"goalId": goalId, "statement": "Summarise the week", "origin": "user", "requestedVia": "nexus"})
	runId := a.openRun(t, map[string]any{"goalId": goalId})
	a.writeVersion(t, runId, runId, "draft", 0, 1, nil, "A long and padded summary.", nil)
	a.finishRun(t, runId, []string{"draft"}, nil)
	a.write(t, "updateWorkRun", map[string]any{"runId": runId, "spent": map[string]any{"modelCalls": 1}})

	nodes, err := a.i.handleValidateAnswer(validatorCaller(), map[string]any{"runId": runId, "ownerUserId": a.owner}, 0)
	if err != nil {
		t.Fatalf("workValidateAnswer: %v", err)
	}
	reply := decodeReply(t, nodes)
	if reply["verdict"] != "flag" {
		t.Fatalf("reply = %v", reply)
	}
	var decision map[string]any
	for _, o := range a.query(t, a.owner, "query "+call("workObservationsForOwnerRun", map[string]any{"runId": runId})) {
		if rowString(o, "kind") == "feedback" {
			t.Errorf("the validator wrote a feedback row: %v", o)
		}
		if rowString(o, "kind") == "decision" {
			decision = o
		}
	}
	if decision == nil || rowString(rowMap(rowMap(decision, "data"), "validator"), "verdict") != "flag" {
		t.Fatalf("no decision observation landed: %v", decision)
	}
	validation := rowMap(a.runRow(t, runId), "validation")
	if rowString(validation, "verdict") != "flag" || rowString(validation, "stepKey") != "draft" || !strings.HasSuffix(rowString(validation, "observationId"), bareShort(rowString(decision, "id"))) {
		t.Errorf("run.validation = %v", validation)
	}

	// The same answer version is never checked twice.
	again, err := a.i.handleValidateAnswer(validatorCaller(), map[string]any{"runId": runId, "ownerUserId": a.owner}, 0)
	if err != nil {
		t.Fatalf("second check: %v", err)
	}
	if !strings.Contains(rowString(decodeReply(t, again), "skipped"), "already checked") {
		t.Errorf("second check = %v", decodeReply(t, again))
	}
}

func bareShort(id string) string { return bareRunId(id) }
