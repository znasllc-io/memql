package pipelinerun

import (
	"context"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

func TestSecuritySettingMatchesActiveRowsShownInSettings(t *testing.T) {
	engine := dbEngine(t)
	ctx := seederCtx()
	reader := pipelineSystemVariableReader(engine, func(context.Context, string) (string, error) { return "legacy-value", nil })
	d := Deps{SystemVariable: reader}
	write := func(active bool) {
		t.Helper()
		_, err := engine.Execute(ctx, fmt.Sprintf(`mutation setGlobalVariable(id: "security-settings-test", name: "MEMQL_PIPELINES_SECURITY", value: "{\"enabled\":true}", active: %t)`, active))
		if err != nil {
			t.Fatal(err)
		}
	}
	write(true)
	t.Cleanup(func() { write(false) })
	if !d.securitySettings(ctx).Enabled {
		t.Fatal("active enabled setting was not admitted")
	}
	write(false)
	if d.securitySettings(ctx).Enabled {
		t.Fatal("inactive enabled setting disagrees with the off UI")
	}
	if value, err := reader(ctx, pipelines.AISettingsVariable); err != nil || value != "legacy-value" {
		t.Fatal("unrelated system variables changed", value, err)
	}
	// Duplicate current values cannot become implicit consent.
	fake := newRecordingEngine()
	fake.answers["globalVariable"] = []any{map[string]any{"value": `{"enabled":true}`}, map[string]any{"value": `{"enabled":true}`}}
	if (Deps{SystemVariable: pipelineSystemVariableReader(fake, nil)}).securitySettings(ctx).Enabled {
		t.Fatal("duplicate settings enabled scanning")
	}
}
