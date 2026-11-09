package pipelinerun

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// Only this opt-in uses the Settings UI's active-row query. The general
// system-variable resolver also returns inactive rows, which would otherwise
// let an archived enabled value run scans while the UI shows the switch off.
// Duplicate active names are ambiguous consent and fail closed.
func pipelineSystemVariableReader(engine Engine, fallback func(context.Context, string) (string, error)) func(context.Context, string) (string, error) {
	return func(ctx context.Context, name string) (string, error) {
		if name != pipelines.SecuritySettingsVariable {
			if fallback == nil {
				return "", memql.ErrVariableNotFound
			}
			return fallback(ctx, name)
		}
		if engine == nil {
			return "", fmt.Errorf("security settings reader is unavailable")
		}
		result, err := engine.Execute(memql.ContextWithFreshRead(ctx), `query globalVariable(name: "MEMQL_PIPELINES_SECURITY")`)
		if err != nil {
			return "", err
		}
		if result == nil {
			return "", fmt.Errorf("security settings returned no result")
		}
		rows := memql.MaterializeRows(result)
		if len(rows) == 0 {
			return "", nil
		}
		if len(rows) != 1 {
			return "", fmt.Errorf("security settings have multiple active values")
		}
		value, ok := rows[0]["value"].(string)
		if !ok {
			return "", fmt.Errorf("security settings have no text value")
		}
		return value, nil
	}
}

// Read at each effect, outside the immutable policy snapshot: consent can be
// withdrawn while a scan runs. Existing goals and already-started scanners are
// not cancelled, but a disabled or unreadable switch grants no new work.
func (d Deps) securitySettings(ctx context.Context) pipelines.SecuritySettings {
	if d.SystemVariable == nil {
		return pipelines.SecuritySettings{}
	}
	raw, err := d.SystemVariable(memql.ContextWithFreshRead(ctx), pipelines.SecuritySettingsVariable)
	if err != nil {
		return pipelines.SecuritySettings{}
	}
	return pipelines.ParseSecuritySettings(raw)
}
