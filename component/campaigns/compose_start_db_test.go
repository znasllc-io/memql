package campaigns

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// compose_start_db_test.go -- starting, cancelling and retrying a composition
// against real engines: the three builtins called through the engine (the
// declaration, the executor name and the handler), the composition written
// through the real createTemplate and updateTemplate, and the goal's run on the
// other replica, whose clock lags, writing the email over the template rather
// than beside it. And a caller who goes away while the goal is being opened:
// the real gate and engine refuse a cancelled context, so only here does it
// show that the goal is recorded all the same.

// dbGoals stands in for the work runtime and the app doors: goals named after
// the run's suffix, so the rows that mention them are the run's to clean up.
type dbGoals struct {
	suffix    string
	mu        sync.Mutex
	opened    int
	cancelled []string
	before    func()
	onCancel  func()
}

func (g *dbGoals) StartComposition(context.Context, CompositionGoal) (string, error) {
	g.mu.Lock()
	g.opened++
	id := fmt.Sprintf("v1:work:goal:%s-%d", g.suffix, g.opened)
	before := g.before
	g.mu.Unlock()
	if before != nil {
		before()
	}
	return id, nil
}

func (g *dbGoals) CancelGoal(_ context.Context, goalID, _ string) error {
	g.mu.Lock()
	g.cancelled = append(g.cancelled, goalID)
	onCancel := g.onCancel
	g.mu.Unlock()
	if onCancel != nil {
		onCancel()
	}
	return nil
}

func (g *dbGoals) HasReachableApp(context.Context, string) (bool, error) { return true, nil }

func TestComposeEmailThroughTheRealEngine(t *testing.T) {
	w := newLayoutWorld(t)
	account := w.suffix + "-org"
	w.createOrganization(t, account, "Composing client")
	starter, runner := w.workers[0], w.workers[1]
	runner.SetMaterialFiler(&recordingFiler{})
	goals := &dbGoals{suffix: w.suffix}
	starter.SetCompositionGoals(goals, goals, goals)
	fresh := memql.ContextWithFreshRead(w.owner)

	builtin := func(name string, args map[string]any) map[string]any {
		t.Helper()
		call, err := langparser.RenderCall(name, args)
		if err != nil {
			t.Fatal(err)
		}
		rows := memql.MaterializeRows(w.run(t, w.owner, "builtin "+call))
		if len(rows) != 1 {
			t.Fatalf("%s answered %v", name, rows)
		}
		return rows[0]
	}
	read := func(id string) (map[string]any, composition) {
		t.Helper()
		rows, err := runner.store.rows(fresh, call("query", "templateById", arg{"templateId", id}))
		if err != nil || len(rows) != 1 {
			t.Fatalf("templateById(%s): %v %v", id, rows, err)
		}
		c, ok := compositionFromRow(rows[0])
		if !ok {
			t.Fatalf("template %s has no composition", id)
		}
		return rows[0], c
	}
	// inStep fails the test unless the composition is writing under goal and
	// starts from the template as it is: its run's email lands in place.
	inStep := func(id, goal string) {
		t.Helper()
		row, c := read(id)
		if c.State != compositionWriting || !sameGoal(c.GoalID, goal) || !c.BaseRevision.Equal(templateTime(row["createdAt"])) {
			t.Fatalf("composition %s goal %q base %v, row %v", c.State, c.GoalID, c.BaseRevision, row["createdAt"])
		}
	}
	// runSubmits runs the goal's step on the other replica, its session
	// submitting with the stamps the run gave it.
	runSubmits := func(id, goal, subject string) {
		t.Helper()
		runner.store = NewStore(runDBEngine{deliveryDBEngine{w.engines[1]}, func(ctx context.Context) (string, error) {
			args := map[string]any{"composition": map[string]any{
				"subject": subject, "preheader": "Three things to plant.",
				"content": []any{map[string]any{"id": "intro", "text": "The days are longer."}}, "drop": []any{"highlight"},
			}}
			maps.Copy(args, common.DelegatedToolDefaultsFromContext(ctx)["emailComposeSubmit"])
			if _, err := runner.handleComposeSubmit(ctx, args, 0); err != nil {
				return "", err
			}
			return "Saved the draft.", nil
		}})
		ctx := common.ContextWithRun(w.owner, common.RunContext{
			RunId: "v1:work:run:" + w.suffix, GoalId: goal, OwnerUserId: w.ownerID, StepKey: "composeEmail", Mode: common.RunModeLive,
			Routing: common.RouteChoice{Source: "route:apps", Level: "strong", By: w.ownerID},
		})
		if _, err := runner.handleRunComposition(ctx, map[string]any{"templateId": id}, 0); err != nil {
			t.Fatalf("run: %v", err)
		}
		row, c := read(id)
		if c.State != compositionDone || str(row, "subject") != subject {
			t.Fatalf("after the run: %s %q, subject %q", c.State, c.Error, str(row, "subject"))
		}
	}

	// Without a brand kit nothing is started, through the engine as anywhere.
	call, err := langparser.RenderCall("campaignComposeEmail", map[string]any{
		"accountId": account, "name": "Spring", "layoutId": "builtin:article", "description": "Tell them about spring.", "fileIds": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.engines[0].Execute(w.owner, "builtin "+call); err == nil || !strings.Contains(err.Error(), composeNeedsBrand) {
		t.Fatalf("without a kit: %v", err)
	}
	if _, err := starter.handleSaveBrandKit(w.owner, map[string]any{"accountId": account, "kit": testBrandKit(t), "expectedRevision": ""}, 0); err != nil {
		t.Fatal(err)
	}

	// Started: writing under its goal and in step with the template, and its
	// run's email lands in place.
	started := builtin("campaignComposeEmail", map[string]any{
		"accountId": account, "name": "Spring", "layoutId": "builtin:article", "description": "Tell them about spring.", "fileIds": []any{},
	})
	id, goal := str(started, "templateId"), str(started, "goalId")
	inStep(id, goal)
	if row, _ := read(id); !templateTime(started["revision"]).Equal(templateTime(row["createdAt"])) {
		t.Errorf("answered revision %v, template %v", started["revision"], row["createdAt"])
	}
	runSubmits(id, goal, "Spring is here")

	// Cancelled, then retried: the goal was asked to stop, the composition
	// failed with Cancelled, and the retry writes it again under a new goal,
	// in step, and its run's email lands in place.
	started = builtin("campaignComposeEmail", map[string]any{
		"accountId": account, "name": "Autumn", "layoutId": "builtin:article", "description": "Tell them about autumn.", "fileIds": []any{},
	})
	id, goal = str(started, "templateId"), str(started, "goalId")
	cancelled := builtin("campaignCancelComposition", map[string]any{"templateId": id})
	if _, c := read(id); c.State != compositionFailed || c.Error != compositionCancelled || str(cancelled, "state") != compositionFailed {
		t.Fatalf("after cancel: %s %q, answered %v", c.State, c.Error, cancelled)
	}
	if len(goals.cancelled) != 1 || !sameGoal(goals.cancelled[0], goal) {
		t.Errorf("goals asked to stop: %v", goals.cancelled)
	}
	retried := builtin("campaignRetryComposition", map[string]any{"templateId": id})
	second := str(retried, "goalId")
	if sameGoal(second, goal) {
		t.Fatalf("the retry answered the old goal %q", second)
	}
	inStep(id, second)
	runSubmits(id, second, "Autumn is here")

	// The caller goes away while the goal is being opened: the goal is
	// recorded all the same, so a cancel can find it.
	ctx, leave := context.WithCancel(w.owner)
	goals.before = leave
	nodes, err := starter.handleComposeEmail(ctx, map[string]any{
		"accountId": account, "name": "Winter", "layoutId": "builtin:article", "description": "Tell them about winter.", "fileIds": []any{},
	}, 0)
	goals.before = nil
	if err != nil {
		t.Fatalf("a start whose caller left: %v", err)
	}
	out := decodeResult(t, nodes)
	id = str(out, "templateId")
	inStep(id, str(out, "goalId"))

	// The caller goes away once the goal is asked to stop: the composition is
	// cancelled all the same, so it does not wait for a run that never comes.
	ctx, leave = context.WithCancel(w.owner)
	goals.onCancel = leave
	_, err = starter.handleCancelComposition(ctx, map[string]any{"templateId": id}, 0)
	goals.onCancel = nil
	if err != nil {
		t.Fatalf("a cancel whose caller left: %v", err)
	}
	if _, c := read(id); c.State != compositionFailed || c.Error != compositionCancelled {
		t.Errorf("after a cancel whose caller left: %s %q", c.State, c.Error)
	}
}
