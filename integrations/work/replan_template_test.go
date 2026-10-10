package work

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
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
