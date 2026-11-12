package pipelinerun

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
)

// THE PIPELINES SETTINGS SAVES, through the builtins a client calls, against
// two real engines over one real Postgres.
//
// settings_test.go holds what the saves send. This holds what the engine does
// with it, on the path MemQL OS takes: `builtin pipelinesAISettingsSave(...)`
// and `builtin pipelinesSecuritySettingsSave(...)` by name, so the capability
// the builtin declares, its argument types and the dispatch to this package are
// all the real ones. The switches are v1:platform:globalVariable rows, which a
// cluster owner or stamped server code writes, so:
//
//   - an owner's AND a developer's save land, and read back through the
//     readers a run uses;
//   - a role without the settings part is refused by the engine and changes
//     nothing;
//   - the developer's own setGlobalVariable is still refused;
//   - each setting stays ONE row however many saves are made and whichever
//     replica takes them. A second active row under the same name is what the
//     security reader refuses as ambiguous consent.
//
// TWO ENGINES, each with its own pool, stand in for two replicas: a save on one
// is read on the other, and alternate saves land on the same row. Nothing of a
// setting lives in a node's memory, and this is what holds that.
//
// The settings are installation-wide, so the test leaves both OFF and their
// rows inactive when it ends: the database is shared by every suite in the
// lane, and a switch left on would change what another package's run does.
func TestPipelineSettingsAreSavedThroughTheirBuiltinsOnEveryReplica(t *testing.T) {
	base := dbEngine(t) // loads the shipped registry and requires the database
	suffix := strings.ReplaceAll(id.NewShortId(), "-", "")[:12]
	quote := langparser.QuoteString

	replicas := make([]*memqlengine.MemQLEngine, 2)
	readers := make([]Deps, 2)
	for n := range replicas {
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
		t.Cleanup(func() { _ = db.Close() })
		eng, err := memqlengine.New(db)
		if err != nil {
			t.Fatalf("replica %d: %v", n, err)
		}
		eng.Logger = base.Logger
		if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatalf("replica %d init: %v", n, err)
		}
		if err := eng.RegisterIntegration(New(Deps{Store: NewDSLStore(eng)})); err != nil {
			t.Fatalf("replica %d register: %v", n, err)
		}
		replicas[n] = eng
		// What a run reads on this replica: the readers the plug-in wires.
		readers[n] = Deps{SystemVariable: pipelineSystemVariableReader(eng, eng.ResolveSystemVariable)}
	}

	activeRows := func(name string) []map[string]any {
		t.Helper()
		res, err := base.Execute(memqlengine.ContextWithFreshRead(context.Background()), "query globalVariable(name: "+quote(name)+")")
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return rowsOf(res)
	}
	t.Cleanup(func() {
		for name, off := range map[string]string{
			pipelines.AISettingsVariable:       pipelines.AISettings{}.Encode(),
			pipelines.SecuritySettingsVariable: pipelines.SecuritySettings{}.Encode(),
		} {
			for _, row := range activeRows(name) {
				_, err := base.Execute(seederCtx(), fmt.Sprintf(`mutation setGlobalVariable(id: %s, name: %s, value: %s, active: false)`,
					quote(bareID(rowString(row, "id"))), quote(name), quote(off)))
				if err != nil {
					t.Errorf("could not leave %s off: %v", name, err)
				}
			}
		}
	})

	save := func(replica int, ctx context.Context, builtin string, args map[string]any) error {
		call, err := langparser.RenderCall(builtin, args)
		if err != nil {
			t.Fatalf("render %s: %v", builtin, err)
		}
		_, err = replicas[replica].Execute(ctx, "builtin "+call)
		return err
	}

	saves := 0
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper} {
		t.Run(string(role), func(t *testing.T) {
			ctx := signedInAs("settings-db-"+string(role)+"-"+suffix, role)

			for _, enabled := range []bool{true, false, true} {
				on, other := saves%2, (saves+1)%2
				saves++
				if err := save(on, ctx, "pipelinesSecuritySettingsSave", map[string]any{"enabled": enabled}); err != nil {
					t.Fatalf("a %s could not save weekly security scans = %v on replica %d: %v", role, enabled, on, err)
				}
				for n, reader := range []Deps{readers[on], readers[other]} {
					if got := reader.securitySettings(context.Background()).Enabled; got != enabled {
						t.Fatalf("a %s saved weekly security scans = %v on replica %d, and a run on replica %d reads %v", role, enabled, on, []int{on, other}[n], got)
					}
				}
			}
			for _, want := range []pipelines.AISettings{{Review: true}, {Review: true, Diagnosis: true}, {Diagnosis: true}} {
				on, other := saves%2, (saves+1)%2
				saves++
				if err := save(on, ctx, "pipelinesAISettingsSave", map[string]any{"review": want.Review, "diagnosis": want.Diagnosis}); err != nil {
					t.Fatalf("a %s could not save the AI switches %+v on replica %d: %v", role, want, on, err)
				}
				// Read fresh on both: the AI reader is the general resolver, which
				// this node's result cache may answer until the mesh's eviction
				// arrives -- these two engines are not meshed, so the read that
				// stands in for "after the eviction" is a fresh one.
				for n, reader := range []Deps{readers[on], readers[other]} {
					if got := reader.aiSettings(memqlengine.ContextWithFreshRead(context.Background())); got != want {
						t.Fatalf("a %s saved the AI switches %+v on replica %d, and a run on replica %d reads %+v", role, want, on, []int{on, other}[n], got)
					}
				}
			}

			// However many saves, whoever made them and wherever they landed,
			// each setting is one row.
			for _, name := range []string{pipelines.AISettingsVariable, pipelines.SecuritySettingsVariable} {
				if rows := activeRows(name); len(rows) != 1 {
					t.Fatalf("after a %s's saves %s is carried by %d active rows, want the one", role, name, len(rows))
				}
			}
		})
	}

	// A role without the part is refused by the engine, on the builtin's own
	// declaration, and changes nothing.
	fresh := memqlengine.ContextWithFreshRead(context.Background())
	before := readers[0].aiSettings(fresh)
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleWriter, auth.RoleReader} {
		ctx := signedInAs("settings-db-"+string(role)+"-"+suffix, role)
		err := save(0, ctx, "pipelinesAISettingsSave", map[string]any{"review": !before.Review, "diagnosis": !before.Diagnosis})
		if err == nil || !strings.Contains(err.Error(), memqlengine.CodeCapabilityNotHeld) {
			t.Errorf("a %s's save answered %v; want the capability refusal", role, err)
		}
		err = save(0, ctx, "pipelinesSecuritySettingsSave", map[string]any{"enabled": false})
		if err == nil || !strings.Contains(err.Error(), memqlengine.CodeCapabilityNotHeld) {
			t.Errorf("a %s's security save answered %v; want the capability refusal", role, err)
		}
	}
	if got := readers[0].aiSettings(fresh); got != before {
		t.Errorf("a refused save changed the AI switches from %+v to %+v", before, got)
	}
	if !readers[0].securitySettings(context.Background()).Enabled {
		t.Errorf("a refused save turned weekly security scans off")
	}

	// A switch that was not sent is refused before anything is written.
	owner := signedInAs("settings-db-owner-"+suffix, auth.RoleOwner)
	if err := save(0, owner, "pipelinesAISettingsSave", map[string]any{"review": true}); err == nil {
		t.Errorf("an AI save with one switch missing was admitted")
	}
	if got := readers[0].aiSettings(fresh); got != before {
		t.Errorf("a refused save with a missing switch changed the AI switches to %+v", got)
	}

	// The stamp is the save's, for the one row it composed. The same row, sent
	// by the developer themself, is refused by the variable's own write rule.
	developer := signedInAs("settings-db-direct-"+suffix, auth.RoleDeveloper)
	_, err := replicas[0].Execute(developer, fmt.Sprintf(`mutation setGlobalVariable(id: %s, name: %s, value: %s, active: true)`,
		quote(seededVariableID(pipelines.AISettingsVariable)), quote(pipelines.AISettingsVariable),
		quote(pipelines.AISettings{Review: true, Diagnosis: true}.Encode())))
	if err == nil || !strings.Contains(err.Error(), "configuration variable") {
		t.Errorf("a developer's own setGlobalVariable answered %v; want the variable write rule's refusal", err)
	}
	if got := readers[0].aiSettings(fresh); got != before {
		t.Errorf("a developer's refused write changed the AI switches from %+v to %+v", before, got)
	}
}
