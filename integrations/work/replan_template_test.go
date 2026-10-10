package work

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

func TestReplanReadsTheSealedSourceAcrossReplicas(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		i, engine := newTestIntegration(t)
		run := remedyWaitRow(waitKindReplan)
		run["automationName"] = "original"
		run["templateConstructId"] = "original-id"
		source := []memql.SandboxConstruct{
			{Kind: "automation", Name: "original", Source: "@template\nautomation original { draft := logic privateDraft() }\n"},
			{Kind: "logic", Name: "privateDraft", Source: "\nlogic privateDraft { return 42 }\n"},
		}
		run["templateVersion"] = memql.WorkBundleVersion(source)
		if mismatch {
			source[1].Source = "logic privateDraft { return 99 }"
		}
		engine.reply("workRunForOwner", run)
		engine.reply("authoringConstructById", map[string]any{"kind": "automation", "name": "original", "bundleId": "bundle-id"})
		var rows []map[string]any
		for _, construct := range source {
			rows = append(rows, map[string]any{"kind": construct.Kind, "name": construct.Name, "source": construct.Source})
		}
		engine.reply("authoringConstructsForBundle", rows...)
		// No planner registry or local template is present on this receiver.
		rc, err := i.LoadReplanContext(context.Background(), "u-alice", remedyRunId, "draft")
		if mismatch {
			if err == nil || !strings.Contains(err.Error(), "changed since compilation") {
				t.Fatalf("changed source was not refused: %v", err)
			}
			continue
		}
		if err != nil || len(rc.Template) != 2 || rc.TemplateName != "original" || rc.Template[1].Source != source[1].Source {
			t.Fatalf("lost sealed source: %+v, %v", rc, err)
		}
	}
}

func installedReplanRun(t *testing.T) map[string]any {
	t.Helper()
	// Use the actual tree loader that admits runs, not the source recompilation
	// under test. A receiving integration has no originating node's registry.
	auto, err := automations.NewLoader(automations.LoaderOptions{}).LoadByName("reviseLibraryDocument")
	if err != nil {
		t.Fatal(err)
	}
	run := remedyWaitRow(waitKindReplan)
	run["automationName"] = auto.Name
	run["templateFingerprint"] = auto.DefinitionFingerprint(id.NewUntracked())
	return run
}

func TestReplanReadsInstalledTemplateAcrossReplicas(t *testing.T) {
	i, receiver := newTestIntegration(t)
	run := installedReplanRun(t)
	receiver.reply("workRunForOwner", run)
	rc, err := i.LoadReplanContext(context.Background(), "u-alice", remedyRunId, "supplement#2")
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Template) != 1 || rc.Template[0].Name != rc.TemplateName || !strings.Contains(rc.Template[0].Source, "libraryPrepareRevisionImage") {
		t.Fatalf("installed repair lost the original DSL: %+v", rc.Template)
	}
	auto, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(rc.Template[0].Source, "untrusted-repair.memql")
	if err != nil || auto.Trusted {
		t.Fatalf("source recovery must not grant the later draft installed trust: %v, %v", auto, err)
	}
}

func TestReplanRefusesMissingOrChangedInstalledTemplate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{"changed recipe", func(run map[string]any) { run["templateFingerprint"] = "old-definition" }, "changed since admission"},
		{"missing seal", func(run map[string]any) { delete(run, "templateFingerprint") }, "no recorded identity"},
		{"missing source", func(run map[string]any) { run["automationName"] = "unavailableInstalledRecipe" }, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, receiver := newTestIntegration(t)
			run := installedReplanRun(t)
			tc.edit(run)
			receiver.reply("workRunForOwner", run)
			if _, err := i.LoadReplanContext(context.Background(), "u-alice", remedyRunId, "supplement#2"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unsafe installed source was not refused: %v", err)
			}
		})
	}
}
