package steps

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/automations"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// Replace only the external release effect. The shipped DSL, expression
// binding, FunctionExecutor and builtin argument validation are all real.
type releaseArgumentProbe struct{ calls []map[string]any }

func (p *releaseArgumentProbe) IntegrationName() string { return "release" }

func (p *releaseArgumentProbe) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{{Name: "releaseCut", Handler: func(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		p.calls = append(p.calls, args)
		return nil, nil
	}}}
}

func TestShippedReleaseAutomationAllowsOmittedNotes(t *testing.T) {
	source, err := os.ReadFile("../../../dsl/cluster/automations.memql")
	if err != nil {
		t.Fatal(err)
	}
	var auto *automations.Automation
	for _, slice := range memql.ExtractAutomationSlices(string(source)) {
		if slice.Name == "publishEngineRelease" {
			auto, err = automations.NewLoader(automations.LoaderOptions{}).CompileSource(slice.Source, "cluster/automations.memql")
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if auto == nil || len(auto.Steps) != 1 {
		t.Fatal("manual release template must contain its publication step")
	}
	engine := bootEmbeddedEngine(t)
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN("postgres://unused:unused@127.0.0.1:1/unused?sslmode=disable"))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	engine.SetDatabaseGetter(func() *bun.DB { return db })
	probe := &releaseArgumentProbe{}
	if err := engine.RegisterIntegration(probe); err != nil {
		t.Fatal(err)
	}
	for _, withNotes := range []bool{false, true} {
		args := map[string]any{"bump": "minor", "expectedRepository": "acme/engine", "expectedSha": "reviewed-sha", "expectedVersion": "v1.1.0"}
		if withNotes {
			args["notes"] = "Approved engine changes"
		}
		eval := automations.NewEvaluator()
		eval.SetCustom("args", args)
		result, err := (&FunctionExecutor{}).Execute(context.Background(), auto.Steps[0], &Context{Engine: engine, Evaluator: eval})
		if err != nil || result.Status != "success" {
			t.Fatalf("withNotes=%v: publication rejected before integration dispatch: result=%+v err=%v", withNotes, result, err)
		}
		want := map[string]any{"bump": "minor", "expectedRepository": "acme/engine", "expectedSha": "reviewed-sha", "expectedVersion": "v1.1.0", "dryRun": false}
		if withNotes {
			want["notes"] = args["notes"]
		}
		if len(probe.calls) == 0 || !reflect.DeepEqual(probe.calls[len(probe.calls)-1], want) {
			t.Fatalf("withNotes=%v: integration calls=%v, want final call=%v", withNotes, probe.calls, want)
		}
	}
	if len(probe.calls) != 2 {
		t.Fatalf("publication calls=%d, want one per invocation", len(probe.calls))
	}
}
