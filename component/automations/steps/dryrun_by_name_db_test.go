package steps

// dryrun_by_name_db_test.go -- who a dry run acts as.
//
// A dry run is asked for in one of two ways. BY NAME, with no text, it runs the
// tree loader's own copy of the automation -- the copy a live run executes --
// and holds a principal exactly when a live run would: the automation's own
// when the request carries no caller's identity, the caller's otherwise. WITH A
// TEXT it compiles that text and runs it as its caller; a text never runs with
// the engine's authority, whatever it names its automation.
//
// And the report says who ran. When the engine's own automation of the name
// acts under the cluster's maintenance principal and the run did not hold it,
// the report states that, so a run that found nothing to do because of who it
// ran as does not read as a clean run with nothing to do.
//
// Postgres-gated: skips cleanly when no database is reachable.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// dryRunSweep is one of the engine's listed maintenance automations
// (dsl/worker/automations.memql): it reads every owner's open model pulls and
// closes the ones no replica is driving.
const dryRunSweep = "workerModelPullStaleSweep"

const dryRunSweepConcept = "v1:worker:modelPull"

// dryRunSweepRows is the pulls one test seeds: one past the claim grace, which
// the sweep closes, and one asked for just now, which it leaves.
type dryRunSweepRows struct {
	t      *testing.T
	db     *bun.DB
	prefix string
	stale  string
	fresh  string
}

func seedDryRunSweepRows(t *testing.T, db *bun.DB) dryRunSweepRows {
	t.Helper()
	prefix := fmt.Sprintf("zzdryrunbyname%d", time.Now().UnixNano())
	rows := dryRunSweepRows{t: t, db: db, prefix: prefix,
		stale: dryRunSweepConcept + ":" + prefix + "-stale",
		fresh: dryRunSweepConcept + ":" + prefix + "-fresh"}
	now := time.Now().UTC()
	var nodes []memoryNodes.MemoryNode
	for id, age := range map[string]time.Duration{rows.stale: 10 * time.Minute, rows.fresh: 0} {
		at := now.Add(-age).Format(time.RFC3339)
		payload, err := json.Marshal(map[string]any{
			"ownerUserId": "v1:identity:user:" + prefix + "-owner", "workerId": prefix + "-worker", "model": "fixture/model:1",
			"status": "requested", "statusLine": "", "layer": "", "completedBytes": 0, "totalBytes": 0,
			"readvertised": false, "errorMessage": "", "targetNodeId": "", "requestedAt": at, "updatedAt": at, "endedAt": "",
		})
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, memoryNodes.MemoryNode{ID: id, CreatedAt: now.Add(-age), CreatedBy: "system:test", Concept: dryRunSweepConcept,
			Type: memoryNodes.NodeTypeObject, Schema: json.RawMessage(`{}`), Payload: payload, Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)})
	}
	if _, err := db.NewInsert().Model(&nodes).Exec(context.Background()); err != nil {
		t.Fatalf("seed the model pulls: %v", err)
	}
	// Every version of the two pulls, and whatever a live run journaled about
	// them: the prefix is this test's own.
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memoryNodes.MemoryNode)(nil)).Where("concept = ?", dryRunSweepConcept).
			Where("id IN (?)", bun.In([]string{rows.stale, rows.fresh})).Exec(context.Background())
		_, _ = db.NewDelete().Model((*memoryNodes.MemoryNode)(nil)).Where("payload::text LIKE ?", "%"+prefix+"%").Exec(context.Background())
	})
	return rows
}

// stored is every version this test's rows hold, as stored: what "a dry run
// changes no row" is read against.
func (r dryRunSweepRows) stored() string {
	r.t.Helper()
	var nodes []memoryNodes.MemoryNode
	if err := r.db.NewSelect().Model(&nodes).Where("concept = ?", dryRunSweepConcept).
		Where("id IN (?)", bun.In([]string{r.stale, r.fresh})).OrderExpr(`id ASC, "createdAt" ASC`).Scan(context.Background()); err != nil {
		r.t.Fatalf("read the seeded model pulls: %v", err)
	}
	var b strings.Builder
	for _, n := range nodes {
		fmt.Fprintf(&b, "%s %s %s\n", n.ID, n.CreatedAt.UTC().Format(time.RFC3339Nano), n.Payload)
	}
	return b.String()
}

// status is a seeded row's latest status.
func (r dryRunSweepRows) status(id string) string {
	r.t.Helper()
	var nodes []memoryNodes.MemoryNode
	if err := r.db.NewSelect().Model(&nodes).Where("concept = ?", dryRunSweepConcept).Where("id = ?", id).
		OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background()); err != nil || len(nodes) != 1 {
		r.t.Fatalf("read %s: %d rows, %v", id, len(nodes), err)
	}
	var payload map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
		r.t.Fatal(err)
	}
	status, _ := payload["status"].(string)
	return status
}

// closedBy lists, sorted, this test's rows the report's manifest holds a
// closing write for.
func (r dryRunSweepRows) closedBy(report memql.BundleDryRunReport) []string {
	var out []string
	for _, m := range report.SideEffectManifest.Mutations {
		if id, _ := m.Payload["pullId"].(string); strings.Contains(id, r.prefix) {
			out = append(out, id[strings.Index(id, r.prefix):])
		}
	}
	sort.Strings(out)
	return out
}

func dryRunByNameEngine(t *testing.T) (*memql.MemQLEngine, *bun.DB, *Registry) {
	t.Helper()
	eng, db := dryRunTrustTestEngine(t)
	reg := NewRegistry()
	eng.SetLogicRunner(automations.NewLogicRunner(eng, reg, eng.Logger))
	return eng, db, reg
}

// By name, with no caller's identity, the dry run is the engine's own
// automation under the principal a live run of it holds: its manifest holds
// exactly the writes a live run makes, and no row changes.
func TestADryRunByNameInterceptsTheWritesOfALiveRun(t *testing.T) {
	eng, db, reg := dryRunByNameEngine(t)
	rows := seedDryRunSweepRows(t, db)
	before := rows.stored()

	report, err := runBundleDryRun(context.Background(), eng, memql.DryRunRequest{AutomationName: dryRunSweep})
	if err != nil {
		t.Fatalf("a dry run by name of %s: %v", dryRunSweep, err)
	}
	if !report.OK {
		t.Fatalf("a dry run by name of %s did not complete: %s", dryRunSweep, report.FailureReason)
	}
	if want := "system:maintenance:" + dryRunSweep; report.RanAs != want {
		t.Errorf("the dry run by name ran as %q, want %q: with no caller's identity it holds the principal a live run of the engine's own automation holds", report.RanAs, want)
	}
	if report.Authority != "" {
		t.Errorf("the run held its automation's principal, and the report says otherwise: %s", report.Authority)
	}
	dry := rows.closedBy(report)
	if after := rows.stored(); after != before {
		t.Errorf("the dry run changed a stored row:\nbefore\n%s\nafter\n%s", before, after)
	}

	// The live run, of the copy the loader hands every runner.
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := automations.NewLoader(automations.LoaderOptions{Logger: quiet, Registry: eng.Concepts()}).LoadByName(dryRunSweep)
	if err != nil {
		t.Fatalf("load %s: %v", dryRunSweep, err)
	}
	ex := automations.NewExecutor(automations.ExecutorOptions{Logger: quiet, Engine: eng, EventBus: eng.EventBus(), StepRegistry: reg})
	defer ex.Close()
	exec, err := ex.Execute(context.Background(), a, "manual")
	if err != nil || exec == nil || exec.Status != "completed" {
		t.Fatalf("the live run of %s: %+v, %v", dryRunSweep, exec, err)
	}
	var live []string
	for _, id := range []string{rows.stale, rows.fresh} {
		if rows.status(id) != "requested" {
			live = append(live, id[strings.Index(id, rows.prefix):])
		}
	}
	sort.Strings(live)
	if want := []string{rows.prefix + "-stale"}; strings.Join(live, ",") != strings.Join(want, ",") {
		t.Fatalf("the live run closed %v of this test's pulls, want %v: the comparison below would prove nothing", live, want)
	}
	if strings.Join(dry, ",") != strings.Join(live, ",") {
		t.Errorf("the dry run by name intercepted writes for %v, and a live run writes %v", dry, live)
	}
}

// The same automation supplied as a text does not run as the engine: with no
// caller's identity it runs as the ordinary automation actor, the read that
// spans owners answers it nothing, and the report says who ran and why that is
// not the engine's own run.
func TestADryRunOfASuppliedTextIsNotTheEnginesRun(t *testing.T) {
	eng, db, _ := dryRunByNameEngine(t)
	rows := seedDryRunSweepRows(t, db)
	before := rows.stored()

	// The control: by name, the engine's own automation finds the pull.
	byName, err := runBundleDryRun(context.Background(), eng, memql.DryRunRequest{AutomationName: dryRunSweep})
	if err != nil || !byName.OK {
		t.Fatalf("the dry run by name: %+v, %v", byName, err)
	}
	if got := rows.closedBy(byName); len(got) != 1 {
		t.Fatalf("the dry run by name found %v of this test's pulls, want the stale one: the empty answers below would prove nothing", got)
	}

	shipped, ok := memql.DSLConstructSource(slog.New(slog.NewTextHandler(io.Discard, nil)), "automation", dryRunSweep)
	if !ok {
		t.Fatalf("the tree holds no text for %s", dryRunSweep)
	}
	// A body of the caller's own under the same name, making the sweep's read.
	own := "use worker.queries.{ openModelPulls }\nuse worker.mutations.{ finishModelPull }\n\n" +
		"@trigger(schedule=\"0 */2 * * * *\")\n" +
		"automation " + dryRunSweep + " {\n" +
		"  pulls := query openModelPulls(requestedBefore: \"9999-01-01T00:00:00Z\", updatedBefore: \"9999-01-01T00:00:00Z\")\n" +
		"  for item in pulls {\n" +
		"    mutation finishModelPull(pullId: item.id, status: \"failed\", endedAt: now)\n" +
		"  }\n" +
		"}\n"
	writer := &auth.AccessContext{UserId: "v1:identity:user:" + rows.prefix + "-caller", Role: auth.RoleWriter}
	for _, tc := range []struct {
		why    string
		ctx    context.Context
		source string
		ranAs  string
	}{
		{"the shipped text, no caller's identity", context.Background(), shipped, "system:automation:" + dryRunSweep},
		{"a body of the caller's own, no caller's identity", context.Background(), own, "system:automation:" + dryRunSweep},
		{"the shipped text, as a caller", auth.ContextWithAccess(context.Background(), writer), shipped, writer.UserId},
		{"a body of the caller's own, as a caller", auth.ContextWithAccess(context.Background(), writer), own, writer.UserId},
	} {
		t.Run(tc.why, func(t *testing.T) {
			report, err := runBundleDryRun(tc.ctx, eng, memql.DryRunRequest{AutomationName: dryRunSweep, AutomationSource: tc.source})
			if err != nil {
				t.Fatalf("runBundleDryRun: %v", err)
			}
			if !report.OK {
				t.Fatalf("the run did not complete: %s", report.FailureReason)
			}
			if report.RanAs != tc.ranAs {
				t.Errorf("the supplied text ran as %q, want %q", report.RanAs, tc.ranAs)
			}
			if got := rows.closedBy(report); len(got) != 0 {
				t.Errorf("the supplied text was answered %v of the pulls the engine's own run finds", got)
			}
			for _, want := range []string{dryRunSweep, tc.ranAs, "maintenance principal", "supplied text"} {
				if !strings.Contains(report.Authority, want) {
					t.Errorf("the report does not say %q: the run found nothing to do because of who it ran as, and reads as a clean run. authority=%q", want, report.Authority)
				}
			}
		})
	}
	if after := rows.stored(); after != before {
		t.Errorf("a dry run changed a stored row:\nbefore\n%s\nafter\n%s", before, after)
	}
}

// By name with a caller's identity the run is the engine's own automation as
// that caller, as a live run started by that caller is; the report says so.
func TestADryRunByNameAsACallerRunsAsTheCaller(t *testing.T) {
	eng, db, _ := dryRunByNameEngine(t)
	rows := seedDryRunSweepRows(t, db)
	writer := &auth.AccessContext{UserId: "v1:identity:user:" + rows.prefix + "-caller", Role: auth.RoleWriter}

	report, err := runBundleDryRun(auth.ContextWithAccess(context.Background(), writer), eng, memql.DryRunRequest{AutomationName: dryRunSweep})
	if err != nil {
		t.Fatalf("runBundleDryRun: %v", err)
	}
	if !report.OK {
		t.Fatalf("the run did not complete: %s", report.FailureReason)
	}
	if report.RanAs != writer.UserId {
		t.Errorf("the dry run by name ran as %q, want the caller %q", report.RanAs, writer.UserId)
	}
	if got := rows.closedBy(report); len(got) != 0 {
		t.Errorf("a caller who is not a cluster owner was answered %v", got)
	}
	for _, want := range []string{dryRunSweep, writer.UserId, "maintenance principal", "engine's own automation"} {
		if !strings.Contains(report.Authority, want) {
			t.Errorf("the report does not say %q. authority=%q", want, report.Authority)
		}
	}
}

// dryRunOriginProbe is a tree automation whose decide logic reaches a
// @serverOnly query (dsl/identity/automations.memql).
const dryRunOriginProbe = "accountDeletionSweep"

// The loader's copy is Trusted, and a dry run by name still dispatches its
// steps with client origin: the trigger payload is the caller's, so the run is
// driven through ExecuteWithClientEvent as the live run by name is. A
// @serverOnly construct therefore refuses the preview as it refuses that live
// run, and the preview is no more permissive than the run it predicts.
//
// And the refused preview leaves no run behind: a sandboxed executor holds no
// journal, so there is nothing a later resume could pick up.
func TestADryRunByNameRunsAtClientOrigin(t *testing.T) {
	eng, db, _ := dryRunByNameEngine(t)
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := automations.NewLoader(automations.LoaderOptions{Logger: quiet, Registry: eng.Concepts()}).LoadByName(dryRunOriginProbe)
	if err != nil || a == nil {
		t.Fatalf("LoadByName(%s) = %v, %v. Repoint dryRunOriginProbe at a tree automation whose body reaches a @serverOnly construct; do not skip", dryRunOriginProbe, a, err)
	}
	if !a.Trusted {
		t.Fatalf("%s came back Trusted=false, so a refusal below would not show the origin was downgraded", dryRunOriginProbe)
	}
	// The run's trigger payload carries a value of this test's own, so a run
	// row it wrote is told from any other on a shared database.
	sentinel := fmt.Sprintf("zzdryrunbyname-origin-%d", time.Now().UnixNano())
	var mark string
	if err := db.NewSelect().ColumnExpr("now()::text").Scan(ctx, &mark); err != nil {
		t.Fatalf("clock read: %v", err)
	}
	report, err := runBundleDryRun(ctx, eng, memql.DryRunRequest{
		AutomationName: dryRunOriginProbe,
		TriggerEvent:   &memql.DryRunTriggerEvent{Topic: "mcp.run." + dryRunOriginProbe, Kind: "manual", Payload: map[string]any{"from": sentinel}},
	})
	if err != nil {
		t.Fatalf("runBundleDryRun: %v", err)
	}
	if report.OK || !strings.Contains(strings.ToLower(report.FailureReason), "server-only") {
		t.Fatalf("a dry run by name of %s was not refused at the @serverOnly gate, so its steps did not run with client origin. ok=%v reason=%q",
			dryRunOriginProbe, report.OK, report.FailureReason)
	}
	var runs int
	if err := db.NewSelect().Table("MemoryNodes").ColumnExpr("count(*)").
		Where("concept = ?", "v1:work:run").
		Where("\"createdAt\" >= ?::timestamptz", mark).
		Where("payload::text LIKE ?", "%"+sentinel+"%").
		Scan(ctx, &runs); err != nil {
		t.Fatalf("count the run rows: %v", err)
	}
	if runs != 0 {
		t.Fatalf("a failing dry run by name wrote %d v1:work:run row(s) carrying its own payload: a preview has nothing to resume, and the sandboxed executor must hold no journal", runs)
	}
}
