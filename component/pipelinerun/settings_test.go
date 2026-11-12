package pipelinerun

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/pipelines"
)

// settings_test.go -- the two installation-wide switches of Pipelines >
// Settings, saved through this package's capabilities (settings.go), against
// the production store over a RECORDING engine. What the engine does with the
// statements is settings_db_test.go's question; this holds what goes out, under
// whom, and that nothing goes out for a caller without the settings part.

func settingsCtx(role auth.Role) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "settings-" + string(role), Role: role})
}

func settingsIntegration(engine Engine) *Integration {
	return New(Deps{Store: NewDSLStore(engine)})
}

// storedSetting answers the settings read with one active row.
func storedSetting(engine *recordingEngine, id, name, value string) {
	engine.answers["globalVariable"] = []any{map[string]any{"id": id, "name": name, "value": value}}
}

// THE SETTINGS PART DECIDES, IN THE SAVE ITSELF, BEFORE ANYTHING IS READ. The
// builtin declares @requiresCapability and the engine enforces it on the call;
// saveSetting asks again because the write it reaches is stamped, and a stamp
// is earned by the order of its own function, never by an annotation one layer
// up. The handlers are called directly here, so the engine's gate is not in
// the way and what refuses is the save's own check.
func TestSavingAPipelineSettingNeedsTheSettingsPart(t *testing.T) {
	saves := map[string]func(*Integration, context.Context) error{
		"ai": func(i *Integration, ctx context.Context) error {
			_, err := i.handleAISettingsSave(ctx, map[string]any{"review": true, "diagnosis": true}, 0)
			return err
		},
		"security": func(i *Integration, ctx context.Context) error {
			_, err := i.handleSecuritySettingsSave(ctx, map[string]any{"enabled": true}, 0)
			return err
		},
	}
	for name, save := range saves {
		for _, role := range []auth.Role{auth.RoleReader, auth.RoleWriter, auth.RoleAdmin} {
			engine := newRecordingEngine()
			err := save(settingsIntegration(engine), settingsCtx(role))
			if err == nil || !strings.Contains(err.Error(), "capability_not_held") {
				t.Errorf("%s: a %s's save answered %v; want the capability refusal", name, role, err)
			}
			if calls := engine.recorded(); len(calls) != 0 {
				t.Errorf("%s: a refused %s reached the engine: %s", name, role, calls[0].query)
			}
		}
		// A call with no person behind it holds no part.
		engine := newRecordingEngine()
		if err := save(settingsIntegration(engine), context.Background()); err == nil {
			t.Errorf("%s: a call with no identity saved a setting", name)
		}
		if len(engine.recorded()) != 0 {
			t.Errorf("%s: a call with no identity reached the engine", name)
		}
		for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper} {
			engine := newRecordingEngine()
			if err := save(settingsIntegration(engine), settingsCtx(role)); err != nil {
				t.Errorf("%s: a %s's save was refused: %v", name, role, err)
			}
		}
	}
}

// ONE READ AS THE CALLER, ONE WRITE STAMPED, AND THE ROW IS NEVER THE CALLER'S
// TO NAME. The arguments are switches; the variable's name, its row id, its
// description and the JSON the engine parses all come from this package.
func TestAPipelineSettingIsSavedToItsOwnRow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		save      func(*Integration, context.Context) error
		variable  string
		seederId  string
		wantValue string
	}{
		{
			name: "ai",
			save: func(i *Integration, ctx context.Context) error {
				_, err := i.handleAISettingsSave(ctx, map[string]any{"review": true, "diagnosis": false}, 0)
				return err
			},
			variable: pipelines.AISettingsVariable, seederId: "var-global-memql-pipelines-ai",
			wantValue: `{"review":true,"diagnosis":false}`,
		},
		{
			name: "security",
			save: func(i *Integration, ctx context.Context) error {
				_, err := i.handleSecuritySettingsSave(ctx, map[string]any{"enabled": true}, 0)
				return err
			},
			variable: pipelines.SecuritySettingsVariable, seederId: "var-global-memql-pipelines-security",
			wantValue: `{"enabled":true}`,
		},
	} {
		for _, existing := range []string{"", "v1:platform:globalVariable:row-an-operator-seeded"} {
			engine := newRecordingEngine()
			wantId := tc.seederId
			if existing != "" {
				storedSetting(engine, existing, tc.variable, "{}")
				wantId = "row-an-operator-seeded"
			}
			ctx := settingsCtx(auth.RoleDeveloper)
			if err := tc.save(settingsIntegration(engine), ctx); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			calls := engine.recorded()
			if len(calls) != 2 {
				t.Fatalf("%s: want the read and the write, got %d calls: %+v", tc.name, len(calls), calls)
			}

			read, write := calls[0], calls[1]
			if read.query != "query globalVariable(name: "+langparser.QuoteString(tc.variable)+")" {
				t.Errorf("%s: the read is %s", tc.name, read.query)
			}
			if read.origin.IsInternal() || !read.fresh || read.actor == nil || read.actor.UserId != "settings-developer" {
				t.Errorf("%s: the read must be the caller's own, unstamped and fresh: %+v", tc.name, read)
			}

			if !strings.HasPrefix(write.query, "mutation setGlobalVariable(") {
				t.Fatalf("%s: the write is %s", tc.name, write.query)
			}
			if _, err := langparser.ParseExpression(strings.TrimPrefix(write.query, "mutation ")); err != nil {
				t.Errorf("%s: the write does not parse: %v\n  %s", tc.name, err, write.query)
			}
			if !write.origin.IsInternal() {
				t.Errorf("%s: the row write is not stamped, so the engine refuses a developer's save", tc.name)
			}
			if write.actor == nil || write.actor.UserId != "settings-developer" || write.actor.Role != auth.RoleDeveloper {
				t.Errorf("%s: the write must keep the caller's actor, so the row records who saved it: %+v", tc.name, write.actor)
			}
			for what, want := range map[string]string{
				"id": "id: " + langparser.QuoteString(wantId), "name": "name: " + langparser.QuoteString(tc.variable),
				"value": "value: " + langparser.QuoteString(tc.wantValue), "active": "active: true",
			} {
				if !strings.Contains(write.query, want) {
					t.Errorf("%s (existing row %q): the write's %s is not %s:\n  %s", tc.name, existing, what, want, write.query)
				}
			}
			// The value is what the engine's own parser reads back.
			if tc.name == "ai" {
				if got := pipelines.ParseAISettings(tc.wantValue); !got.Review || got.Diagnosis {
					t.Errorf("the saved AI value does not read back as the switches that were sent: %+v", got)
				}
			} else if !pipelines.ParseSecuritySettings(tc.wantValue).Enabled {
				t.Errorf("the saved security value does not read back as enabled")
			}
		}
	}
}

// A SWITCH THAT WAS NOT SENT IS NOT "OFF". Both switches of the AI setting are
// written together, so a call missing one would turn it off without anybody
// having chosen that.
func TestAPipelineSettingRefusesAMissingSwitch(t *testing.T) {
	cases := map[string]func(*Integration, context.Context) error{
		"ai without diagnosis": func(i *Integration, ctx context.Context) error {
			_, err := i.handleAISettingsSave(ctx, map[string]any{"review": true}, 0)
			return err
		},
		"ai with a switch that is not a boolean": func(i *Integration, ctx context.Context) error {
			_, err := i.handleAISettingsSave(ctx, map[string]any{"review": true, "diagnosis": "maybe"}, 0)
			return err
		},
		"security with nothing": func(i *Integration, ctx context.Context) error {
			_, err := i.handleSecuritySettingsSave(ctx, map[string]any{}, 0)
			return err
		},
	}
	for name, save := range cases {
		engine := newRecordingEngine()
		if err := save(settingsIntegration(engine), settingsCtx(auth.RoleOwner)); err == nil {
			t.Errorf("%s: saved", name)
		}
		if len(engine.recorded()) != 0 {
			t.Errorf("%s: reached the engine", name)
		}
	}
}

// TWO ACTIVE ROWS UNDER ONE NAME IS NOT A SETTING. The engine's reader refuses
// the pair as ambiguous consent, so a save that updated one of them would
// report success while the other still decided.
func TestAPipelineSettingRefusesAnAmbiguousRow(t *testing.T) {
	engine := newRecordingEngine()
	engine.answers["globalVariable"] = []any{
		map[string]any{"id": "row-one", "name": pipelines.SecuritySettingsVariable, "value": `{"enabled":false}`},
		map[string]any{"id": "row-two", "name": pipelines.SecuritySettingsVariable, "value": `{"enabled":true}`},
	}
	_, err := settingsIntegration(engine).handleSecuritySettingsSave(settingsCtx(auth.RoleOwner), map[string]any{"enabled": true}, 0)
	if err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("an ambiguous setting answered %v; want the refusal that names it", err)
	}
	for _, call := range engine.recorded() {
		if strings.HasPrefix(call.query, "mutation ") {
			t.Errorf("an ambiguous setting was written: %s", call.query)
		}
	}
}
