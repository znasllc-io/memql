package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	pure "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

type sectionEngine struct {
	*materializeEngine
	steps    map[string]map[string]any
	receipts map[string][]map[string]any
}

func (e *sectionEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	name, args := materializeCall(e.t, query)
	switch name {
	case "workStepForOwnerRun":
		e.calls = append(e.calls, name)
		if row := e.steps[stringOf(args["stepKey"])]; row != nil {
			return memql.NewResultWithOutput([]map[string]any{row}), nil
		}
		return memql.NewResultWithOutput([]map[string]any{}), nil
	case "workModelCallsForOwnerStep":
		e.calls = append(e.calls, name)
		return memql.NewResultWithOutput(e.receipts[stringOf(args["stepKey"])]), nil
	}
	return e.materializeEngine.Execute(ctx, query)
}
func sectionRow(key, body string) map[string]any {
	return map[string]any{"id": "v1:work:step:" + key, "key": key, "runId": "v1:work:run:nexus", "ownerUserId": "u-alice", "status": "done", "finishedAt": "2026-10-10T15:00:00Z", "result": map[string]any{"value": map[string]any{"reply": body}}}
}
func sectionFixture(t *testing.T) (*Integration, *sectionEngine, *materializeUploader) {
	e := &sectionEngine{materializeEngine: newMaterializeEngine(t), steps: map[string]map[string]any{"one": sectionRow("one", "# First\n\nExact **prose**."), "two": sectionRow("two", "## Second\n\n- Final item")}, receipts: map[string][]map[string]any{"one": {{"ownerUserId": "u-alice", "runId": "nexus", "stepKey": "one", "provider": "local", "model": "model-a", "inputTokens": 12, "outputTokens": 7}}}}
	i := New(e, nil)
	u := &materializeUploader{}
	i.SetUploader(u, "files")
	i.SetComposer(materializeComposerFunc(func(context.Context, ComposeRequest) (ComposeReply, error) {
		t.Error("completed text was regenerated")
		return ComposeReply{}, fmt.Errorf("unexpected generation")
	}))
	return i, e, u
}
func sectionArgs() materializeArgs {
	return materializeArgs{Name: "report", Format: pure.FormatMarkdown, SectionKeys: []string{"one", "two"}}
}

func TestSectionsRenderExactOrderedTextAndRecoverOnAnotherReplica(t *testing.T) {
	i, e, u := sectionFixture(t)
	e.failReady = true
	if _, err := i.materialize(nestedMaterializeContext(), "u-alice", "", sectionArgs()); err == nil {
		t.Fatal("expected final receipt interruption")
	}
	if !strings.Contains(string(u.bytes), "# First\n\nExact **prose**.\n\n## Second\n\n- Final item") {
		t.Fatalf("render changed ordered source: %s", u.bytes)
	}
	// Sources are gone on the recovering replica. It must use the durable private snapshot.
	e.steps = nil
	e.receipts = nil
	e.failReady = false
	sibling := New(e, nil)
	sibling.SetUploader(u, "files")
	sibling.SetComposer(i.composerRef())
	out, err := sibling.materialize(nestedMaterializeContext(), "u-alice", "", sectionArgs())
	if err != nil || out["status"] != "ready" || u.calls != 1 {
		t.Fatalf("recovery duplicated or lost delivery: %v %v uploads=%d", out, err, u.calls)
	}
	for _, input := range e.inputs {
		request := input["request"].(map[string]any)
		sections := request["sections"].(map[string]any)
		models := sections["models"].([]any)
		if len(models) != 1 || models[0].(map[string]any)["model"] != "model-a" || models[0].(map[string]any)["tokens"] != float64(19) {
			t.Fatalf("missing recorded model contribution: %v", models)
		}
		if len(sections["sources"].([]any)) != 2 {
			t.Fatal("missing captured section identities")
		}
	}
}
func TestSectionsRejectInvalidInputBeforeAnyEffects(t *testing.T) {
	for _, name := range []string{"missing", "unfinished", "foreign owner", "other run", "nontext", "blank", "duplicate", "self", "empty key", "no run", "another draft", "tabular", "foreign model", "too large"} {
		t.Run(name, func(t *testing.T) {
			i, e, u := sectionFixture(t)
			a := sectionArgs()
			ctx := nestedMaterializeContext()
			switch name {
			case "missing":
				delete(e.steps, "two")
			case "unfinished":
				e.steps["two"]["status"] = "running"
			case "foreign owner":
				e.steps["two"]["ownerUserId"] = "stranger"
			case "other run":
				e.steps["two"]["runId"] = "other"
			case "nontext":
				e.steps["two"]["result"] = map[string]any{"value": map[string]any{"status": "done"}}
			case "blank":
				e.steps["two"] = sectionRow("two", " ")
			case "duplicate":
				a.SectionKeys = []string{"one", "one"}
			case "self":
				a.SectionKeys = []string{"file"}
			case "empty key":
				a.SectionKeys = []string{""}
			case "no run":
				ctx = materializeContext()
			case "another draft":
				a.Draft = "Ambiguous"
			case "tabular":
				a.Format = pure.FormatCSV
			case "foreign model":
				e.receipts["one"][0]["ownerUserId"] = "stranger"
			case "too large":
				e.steps["two"] = sectionRow("two", strings.Repeat("x", 8<<20))
			}
			if _, err := i.materialize(ctx, "u-alice", "", a); err == nil {
				t.Fatal("invalid sections accepted")
			}
			if len(e.inputs) != 0 || len(e.compositions) != 0 || u.calls != 0 {
				t.Fatal("invalid selection had effects")
			}
		})
	}
	for _, value := range []any{"one", []any{3}, []string{}, []any{nil}} {
		if a, err := parseMaterializeArgs(map[string]any{"name": "report", "format": "markdown", "sectionKeys": value}); err == nil {
			if err = validateSectionAssembly(a, common.RunContext{OwnerUserId: "u-alice"}, true, "u-alice"); err == nil {
				t.Fatalf("invalid argument accepted: %v", value)
			}
		}
	}
}

func TestSectionOptionPreservesExistingMaterializationIdentity(t *testing.T) {
	// Persisted executions hash the existing argument object. Adding an unused
	// capability argument must not create a second file on resume/replay.
	raw, err := json.Marshal(materializeDraft())
	if err != nil {
		t.Fatal(err)
	}
	const before = `{"Name":"report","Statement":"","Format":"markdown","OutputKind":"","Sources":null,"Draft":"September report","TemplateId":"","FolderId":"","AccountIds":null,"DeployableKind":"","RecipeId":"","Ceilings":null}`
	if string(raw) != before {
		t.Fatalf("existing idempotency input changed: %s", raw)
	}
}
