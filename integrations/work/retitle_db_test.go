package work

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// retitle_db_test.go -- work.workRetitleRun against a REAL engine and Postgres
// (goal_db_test.go's header says why a recording executor cannot stand in).
//
// Ask retitles a goal when a person's later message changes what it is about
// ("actually I'm in Phoenix"). The title is v1:work:run.title, a field of its
// own: classification.workTitle stays the intake decision, because a late
// compile that writes classification must not erase a retitle.
//
// Every case calls the builtin the way its one caller does -- rendered by the
// parser, namespace-qualified, executed on the engine under the person's
// context -- so the DSL declaration, the executor binding, the handler's gates,
// the updateWorkRun accept list and the shapes are all under test, not just the
// Go function.

// retitleRun calls work.workRetitleRun on e as ctx's caller and returns the
// reply rows.
func retitleRun(ctx context.Context, e *memqlengine.MemQLEngine, runId, title string) ([]map[string]any, error) {
	rendered, err := langparser.RenderCall("work.workRetitleRun", map[string]any{"runId": runId, "title": title})
	if err != nil {
		return nil, err
	}
	res, err := e.Execute(ctx, "builtin "+rendered)
	if err != nil {
		return nil, err
	}
	return memqlengine.MaterializeRows(res.OutputPayload()), nil
}

// openRetitleDB opens a replica whose engine reaches the work integration as a
// builtin, and seeds alice's goal and running run carrying the title intake gave
// it. alice is a.owner.
func openRetitleDB(t *testing.T) (actsDB, string) {
	t.Helper()
	a := openActsDB(t)
	require.NoError(t, a.eng.RegisterIntegration(a.i))
	runId := a.openRun(t, nil)
	a.write(t, "updateWorkRun", map[string]any{
		"runId":          runId,
		"classification": map[string]any{"workTitle": "Weather in Tucson", "workload": "lookup"},
	})
	return a, runId
}

func TestRetitleRunWritesTheTitleForTheOwner(t *testing.T) {
	a, runId := openRetitleDB(t)
	alice := actorCtx(a.owner)
	before := a.runRow(t, runId)
	require.Empty(t, rowString(before, "title"), "a run nobody retitled carries no title of its own")

	rows, err := retitleRun(alice, a.eng, runId, "Weather in Phoenix")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, memqlengine.BareShortId(runId), rows[0]["runId"], "the reply names the run by its bare id, the wire form")
	require.Equal(t, "Weather in Phoenix", rows[0]["title"])

	run := a.runRow(t, runId)
	require.Equal(t, "Weather in Phoenix", rowString(run, "title"))
	require.Equal(t, "Weather in Tucson", rowString(rowMap(run, "classification"), "workTitle"),
		"classification.workTitle is the intake decision and stays as intake wrote it")
	for _, field := range []string{"status", "goalId", "automationName", "mode", "startedAt", "ownerUserId", "classification"} {
		require.Equal(t, before[field], run[field], "retitling a run changes its title and nothing else: %s", field)
	}

	// The bare spelling of the id reaches the same run, and a later message may
	// change the title again.
	rows, err = retitleRun(alice, a.eng, memqlengine.BareShortId(runId), "Weather in Mesa")
	require.NoError(t, err)
	require.Equal(t, "Weather in Mesa", rows[0]["title"])
	require.Equal(t, "Weather in Mesa", rowString(a.runRow(t, runId), "title"))
}

func TestRetitleRunRefusesAnotherOwner(t *testing.T) {
	a, runId := openRetitleDB(t)
	var gates []string
	gate := a.i.decisionGate
	a.i.decisionGate = func(ctx context.Context, keys ...string) (func(), error) {
		gates = append(gates, keys...)
		return gate(ctx, keys...)
	}
	before := a.runRow(t, runId)

	_, err := retitleRun(actorCtx(a.owner+"-bob"), a.eng, runId, "Weather in Phoenix")
	require.ErrorContains(t, err, "no run")

	require.Equal(t, before, a.runRow(t, runId), "bob's refused call must leave alice's run exactly as it was")
	require.Empty(t, gates, "a person who cannot read the run must be refused before they can take a lock keyed on its id")
}

func TestRetitleRunRefusesInsideARun(t *testing.T) {
	a, runId := openRetitleDB(t)
	accessAs := func(synthetic, unranked bool) context.Context {
		return auth.ContextWithAccess(actorCtx(a.owner), &auth.AccessContext{UserId: a.owner, Role: auth.RoleWriter, Synthetic: synthetic, Unranked: unranked})
	}
	for name, ctx := range map[string]context.Context{
		"a running job":     common.ContextWithRun(actorCtx(a.owner), common.RunContext{RunId: runId, OwnerUserId: a.owner, StepKey: "reason", Mode: common.RunModeLive}),
		"a synthetic actor": accessAs(true, false),
		"an unranked actor": accessAs(false, true),
	} {
		_, err := retitleRun(ctx, a.eng, runId, "Weather in Phoenix")
		require.ErrorContains(t, err, "person", name)
	}
	require.Empty(t, rowString(a.runRow(t, runId), "title"), "a title change needs a person's message; nothing a job does may set one")

	// The same owner, as a person, still can.
	_, err := retitleRun(actorCtx(a.owner), a.eng, runId, "Weather in Phoenix")
	require.NoError(t, err)
}

func TestRetitleRunBoundsTheTitle(t *testing.T) {
	a, runId := openRetitleDB(t)
	alice := actorCtx(a.owner)
	_, err := retitleRun(alice, a.eng, runId, "Weather in Phoenix")
	require.NoError(t, err)

	for name, title := range map[string]string{
		"empty":                   "",
		"only whitespace":         " \t\n ",
		"81 characters":           strings.Repeat("a", 81),
		"81 multibyte characters": strings.Repeat("é", 81),
	} {
		_, err := retitleRun(alice, a.eng, runId, title)
		require.Error(t, err, name)
		require.Equal(t, "Weather in Phoenix", rowString(a.runRow(t, runId), "title"), "%s must leave the title as it was", name)
	}

	// The bound counts characters, not bytes, and counts a title without the
	// padding around it.
	for name, title := range map[string]string{
		"80 characters":           strings.Repeat("a", 80),
		"80 multibyte characters": strings.Repeat("é", 80),
		"80 characters, padded":   "  " + strings.Repeat("b", 80) + "  ",
	} {
		rows, err := retitleRun(alice, a.eng, runId, title)
		require.NoError(t, err, name)
		require.Equal(t, strings.TrimSpace(title), rows[0]["title"], name)
		require.Equal(t, strings.TrimSpace(title), rowString(a.runRow(t, runId), "title"), name)
	}
}

// A late compile writes classification. The title lives beside it, so neither
// write may take the other's value with it.
func TestRetitleRunSurvivesALaterClassificationWrite(t *testing.T) {
	a, runId := openRetitleDB(t)
	_, err := retitleRun(actorCtx(a.owner), a.eng, runId, "Weather in Phoenix")
	require.NoError(t, err)

	a.write(t, "updateWorkRun", map[string]any{
		"runId":          runId,
		"classification": map[string]any{"workTitle": "Weather", "workload": "lookup"},
	})

	run := a.runRow(t, runId)
	require.Equal(t, "Weather in Phoenix", rowString(run, "title"), "a classification write must not erase a retitle")
	require.Equal(t, "Weather", rowString(rowMap(run, "classification"), "workTitle"))
}

// Nexus reads runs through workRunSummary and everything else through
// workRunFull; a field a shape does not list is a field its readers cannot see.
func TestRetitleRunShowsInBothRunShapes(t *testing.T) {
	a, runId := openRetitleDB(t)
	summaries := func() []map[string]any {
		t.Helper()
		var rows []map[string]any
		for _, q := range []string{"query workRunSummariesForOwner()", "query " + call("workRunSummaryForOwner", map[string]any{"runId": runId})} {
			found := a.query(t, a.owner, q)
			require.Len(t, found, 1, q)
			rows = append(rows, found[0])
		}
		return rows
	}
	for _, row := range summaries() {
		require.Empty(t, rowString(row, "title"), "a run nobody retitled has no title in the summary")
		require.Equal(t, "Weather in Tucson", rowString(row, "workTitle"))
	}

	_, err := retitleRun(actorCtx(a.owner), a.eng, runId, "Weather in Phoenix")
	require.NoError(t, err)

	for _, row := range summaries() {
		require.Equal(t, "Weather in Phoenix", rowString(row, "title"))
		require.Equal(t, "Weather in Tucson", rowString(row, "workTitle"), "the summary still carries the intake title beside it")
	}
	full := a.query(t, a.owner, "query "+call("workRunForOwner", map[string]any{"runId": runId}))
	require.Len(t, full, 1)
	require.Equal(t, "Weather in Phoenix", rowString(full[0], "title"))
}

// The gate is keyed on the run, not on how the caller spelled its id: a bare id
// and a canonical one must contend for the same lock.
func TestRetitleRunTakesOneGatePerRunWhateverTheSpellingOfItsId(t *testing.T) {
	a, runId := openRetitleDB(t)
	var gates []string
	gate := a.i.decisionGate
	a.i.decisionGate = func(ctx context.Context, keys ...string) (func(), error) {
		gates = append(gates, keys...)
		return gate(ctx, keys...)
	}
	alice := actorCtx(a.owner)
	for _, spelling := range []string{runId, memqlengine.BareShortId(runId)} {
		_, err := retitleRun(alice, a.eng, spelling, "Weather in Phoenix")
		require.NoError(t, err)
	}
	require.Equal(t, []string{"run-title-" + memqlengine.BareShortId(runId), "run-title-" + memqlengine.BareShortId(runId)}, gates)
}

// A retitle is a read-merge of one run row, and the replica that writes it is
// not the replica that wrote the row last (memql#5431). A's clock runs ahead of
// B's, so the version A wrote last -- a heartbeat -- carries a createdAt B's own
// clock will not reach for minutes. B's retitle must still be the row's newest
// version: a reader sees the version with the latest createdAt, so a write
// stamped with B's own earlier clock would sort BEFORE A's heartbeat and the
// person would see the title they had just changed away from.
//
// A's lead is written into the row by A, independently of the code under test:
// it must not be something the retitle's own stamping could take away.
func TestRetitleRunFromAReplicaWhoseClockLags(t *testing.T) {
	a, b := openActsDB(t), openActsDB(t)
	b.owner = a.owner
	for _, replica := range []actsDB{a, b} {
		require.NoError(t, replica.eng.RegisterIntegration(replica.i))
	}
	runId := a.openRun(t, nil)
	alice := actorCtx(a.owner)
	ahead := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Microsecond)
	a.write(t, "updateWorkRun", map[string]any{"runId": runId, "heartbeatAt": rfc(ahead), "versionTime": rfc(ahead)})

	_, err := retitleRun(alice, b.eng, runId, "Weather in Phoenix")
	require.NoError(t, err)

	for name, replica := range map[string]actsDB{"the replica that retitled": b, "the replica whose clock leads": a} {
		run := replica.runRow(t, runId)
		require.Equal(t, "Weather in Phoenix", rowString(run, "title"),
			"%s must read the retitle: it is the later write, whatever the clocks say", name)
		beat, ok := rowTime(run, "heartbeatAt")
		require.True(t, ok)
		require.True(t, ahead.Equal(beat), "the retitle merges onto the newest version, so the heartbeat written before it survives: %s", beat)
	}
}
