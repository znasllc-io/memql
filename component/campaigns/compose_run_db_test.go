package campaigns

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// compose_run_db_test.go -- a composition run against the real engine, on a
// replica other than the one that started the composition (and whose clock
// lags it): the run claims a composition whose goal is not recorded yet
// through the real updateTemplate, and its session's submit then saves over the
// template rather than beside it; a run whose composer saves nothing fails the
// composition through the same write and leaves the email as it was.

// runDBEngine is the real engine with the tool loop a run calls stood in for:
// session is what the composer does.
type runDBEngine struct {
	deliveryDBEngine
	session func(ctx context.Context) (string, error)
}

func (e runDBEngine) InvokeAIChatWithFilteredTools(ctx context.Context, _ string, _ map[string]any, _ []string) (string, error) {
	return e.session(ctx)
}

func TestRunCompositionThroughTheRealTemplateWrites(t *testing.T) {
	w := newLayoutWorld(t)
	account := w.suffix + "-org"
	w.createOrganization(t, account, "Composed client")
	runner := w.workers[1]
	runner.SetMaterialFiler(&recordingFiler{})
	fresh := memql.ContextWithFreshRead(w.owner)

	read := func(id string) map[string]any {
		t.Helper()
		rows, err := runner.store.rows(fresh, call("query", "templateById", arg{"templateId", id}))
		if err != nil || len(rows) != 1 {
			t.Fatalf("templateById(%s): %v %v", id, rows, err)
		}
		return rows[0]
	}
	// start makes a template whose composition is writing and has no goal
	// recorded yet, on the other replica.
	start := func(name string) string {
		t.Helper()
		c := composition{State: compositionWriting, StartedAt: time.Now().UTC().Truncate(time.Millisecond),
			Request: compositionRequest{LayoutID: "builtin:article", Name: name, Description: "Spring news"}}
		id, _, _, err := w.workers[0].createTemplateFromLayout(w.owner, account, name, "builtin:article", &c)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	run := func(id, goal string, session func(context.Context) (string, error)) error {
		t.Helper()
		runner.store = NewStore(runDBEngine{deliveryDBEngine{w.engines[1]}, session})
		ctx := common.ContextWithRun(w.owner, common.RunContext{
			RunId: "v1:work:run:" + w.suffix, GoalId: goal, OwnerUserId: w.ownerID, StepKey: "composeEmail", Mode: common.RunModeLive,
			Routing: common.RouteChoice{Source: "route:apps", Level: "strong", By: w.ownerID},
		})
		nodes, err := runner.handleRunComposition(ctx, map[string]any{"templateId": id}, 0)
		if err == nil && (len(nodes) != 1 || !json.Valid(nodes[0].Payload)) {
			t.Fatalf("answer %+v", nodes)
		}
		return err
	}

	// The session submits with the stamps the run gave it: the composition is
	// this run's, and the email is saved over the template.
	goal := "v1:work:goal:" + w.suffix + "-1"
	id := start("Spring")
	err := run(id, goal, func(ctx context.Context) (string, error) {
		args := map[string]any{"composition": map[string]any{
			"subject": "Spring is here", "preheader": "Three things to plant.",
			"content": []any{map[string]any{"id": "intro", "text": "The days are longer."}}, "drop": []any{"highlight"},
		}}
		maps.Copy(args, common.DelegatedToolDefaultsFromContext(ctx)["emailComposeSubmit"])
		if _, err := runner.handleComposeSubmit(ctx, args, 0); err != nil {
			return "", err
		}
		return "Saved the draft.", nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	row := read(id)
	done, _ := compositionFromRow(row)
	if done.State != compositionDone || !sameGoal(done.GoalID, goal) || str(row, "subject") != "Spring is here" || str(row, "status") != "draft" {
		t.Errorf("composition %s (goal %q), subject %q, status %q", done.State, done.GoalID, str(row, "subject"), str(row, "status"))
	}

	// The composer saves nothing: the composition fails, the email stays.
	id = start("Autumn")
	before := read(id)
	err = run(id, "v1:work:goal:"+w.suffix+"-2", func(context.Context) (string, error) { return "I could not finish.", nil })
	if err == nil || !strings.Contains(err.Error(), compositionFailedCode) {
		t.Fatalf("run error = %v", err)
	}
	row = read(id)
	failed, _ := compositionFromRow(row)
	if failed.State != compositionFailed || failed.Error != "MemQL finished without saving the email." {
		t.Errorf("composition = %s %q", failed.State, failed.Error)
	}
	if !sameJSON(t, row["document"], before["document"]) || str(row, "subject") != str(before, "subject") {
		t.Error("failing the composition changed the email")
	}
}
