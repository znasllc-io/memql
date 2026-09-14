package steps

// dryrun_v1_db_test.go -- a dry run of a statement body (epic memql#5370, task
// memql#5372): the same preview a legacy body gets, over the statement forms.
//
// The statements hold lists of their own -- a `for`'s body, an `if`'s arms
// flattened onto conditions, a parallel's branches -- and a logic called as a
// statement runs its own statements. Every one of them must re-enter the
// sandbox, or a write nested in one escapes the preview: the property
// memql#2943 won for forEach, parallel and switch, held here for the forms
// that replace them. And a statement-body logic run in the preview journals
// nothing, whatever it writes.
//
// Postgres-gated (the engine needs one): skips cleanly when no DB is
// reachable, FAILS under MEMQL_REQUIRE_DB=1.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/language/compiler"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// compiledLogicBody parses and compiles one statement-form logic the way the
// function loader does.
func compiledLogicBody(t *testing.T, src string) []map[string]any {
	t.Helper()
	norm, err := languageParser.NormaliseAll(src)
	if err != nil {
		t.Fatal(err)
	}
	file, err := languageParser.ParseFile(norm)
	if err != nil {
		t.Fatal(err)
	}
	fn := file.Definitions[0].(*languageParser.FunctionDef)
	steps, problems := compiler.CompileBody("logic", fn.Name, nil, fn.Body.(*languageParser.AutomationDef).Body)
	if len(problems) > 0 {
		t.Fatalf("compile %s: %v", fn.Name, problems)
	}
	return steps
}

func TestDryRunV1_DB_AStatementBodyWritesAndPublishesNothing(t *testing.T) {
	eng, db := dryRunTrustTestEngine(t)
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	// The test engine has no bus, and a publish to no bus reaches nobody
	// whether or not the sandbox stopped it: give it one to listen on.
	if eng.EventBus() == nil {
		eng.SetEventBus(events.NewBus())
	}

	// A statement-body logic that writes, called from the body.
	logicName := fmt.Sprintf("dryRunSaves%d", stamp)
	if err := eng.Functions().Upsert(&memql.Function{Name: logicName, FunctionKind: "logic", Enabled: true,
		LogicBody: compiledLogicBody(t, fmt.Sprintf(`logic %s {
  mutation saveThing(id: "from-logic")
  return 1
}`, logicName))}); err != nil {
		t.Fatalf("register %s: %v", logicName, err)
	}

	topic := fmt.Sprintf("dryrun.statements.%d", stamp)
	var (
		mu       sync.Mutex
		received []string
	)
	sentinel := make(chan struct{}, 1)
	unsubscribe := eng.EventBus().Subscribe(topic, func(ev events.Event) {
		mu.Lock()
		received = append(received, fmt.Sprint(ev.Payload["from"]))
		mu.Unlock()
		if ev.Payload["from"] == "sentinel" {
			sentinel <- struct{}{}
		}
	})
	defer unsubscribe()

	var mark string
	if err := db.NewSelect().ColumnExpr("now()::text").Scan(ctx, &mark); err != nil {
		t.Fatalf("clock read: %v", err)
	}

	name := fmt.Sprintf("dryRunStatements%d", stamp)
	report, err := runBundleDryRun(ctx, eng, memql.DryRunRequest{
		AutomationName: name,
		AutomationSource: fmt.Sprintf(`@trigger(event="dryrun.probe")
automation %s {
  items := [{ id: "v1:x:thing:a", payload: { n: 1 } }, { id: "v1:x:thing:b", payload: { n: 2 } }]
  for it in items {
    if it.n > 1 {
      mutation saveThing(id: it.id)
    }
  }
  parallel {
    branch left {
      mutation saveThing(id: "from-branch")
    }
    branch right {
      checked := items.count()
    }
  }
  publish "%s" { from: "dry-run" }
  v := logic %s()
  return v
}`, name, topic, logicName),
		Mode: memql.DryRunModeIsolated,
	})
	if err != nil {
		t.Fatalf("runBundleDryRun: %v", err)
	}
	if !report.OK {
		t.Fatalf("dry run failed: %s", report.FailureReason)
	}

	// Every statement of the body is reported, and none was left unrun.
	var reported []string
	for _, s := range report.Trace {
		reported = append(reported, s.StepId+":"+s.Status)
	}
	if got, want := strings.Join(reported, " "), "items:success for_it:success parallel:success publish:success v:success return:success"; got != want {
		t.Fatalf("trace %q, want %q", got, want)
	}

	// Each would-be write is in the manifest: the `for`'s one (it.n > 1 holds
	// for b only), the branch's, the logic's, and the publish.
	var recorded []string
	for _, m := range report.SideEffectManifest.Mutations {
		recorded = append(recorded, fmt.Sprintf("%s=%v", m.Concept, m.Payload["id"]))
	}
	for _, want := range []string{"=v1:x:thing:b", "=from-branch", "=from-logic", "event="} {
		found := false
		for _, r := range recorded {
			if strings.HasSuffix(r, want) || (want == "event=" && strings.HasPrefix(r, "event=")) {
				found = true
			}
		}
		if !found {
			t.Errorf("no manifest entry %q in %v", want, recorded)
		}
	}
	if len(recorded) != 4 {
		t.Errorf("manifest %v, want four entries: a statement ran twice, or one escaped it", recorded)
	}

	// Nothing landed: no row -- the logic's journal included -- carries this
	// run's names.
	var landed int
	if err := db.NewSelect().Table("MemoryNodes").ColumnExpr("count(*)").
		Where("\"createdAt\" >= ?::timestamptz", mark).
		Where("(payload::text LIKE ? OR payload::text LIKE ?)", "%"+logicName+"%", "%"+name+"%").
		Scan(ctx, &landed); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if landed != 0 {
		t.Fatalf("a dry run wrote %d row(s) naming its automation or its logic", landed)
	}

	// And nothing was published. The subscription is live -- the sentinel
	// arrives -- and a leaked publish, delivered asynchronously, is given as
	// long to arrive.
	eng.EventBus().Publish(events.Event{Topic: topic, Kind: events.KindMessage, Payload: map[string]any{"from": "sentinel"}})
	select {
	case <-sentinel:
	case <-time.After(5 * time.Second):
		t.Fatal("the sentinel never arrived: the subscription sees nothing, so its silence proves nothing")
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("the topic received %v, want only the sentinel", received)
	}
}
