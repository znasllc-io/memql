package memql

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// WHO WRITES A CONFIGURATION VARIABLE'S ROW, against a real engine and a real
// Postgres.
//
// v1:platform:globalVariable and v1:platform:partitionVariable hold the
// deployment's plaintext configuration, and the resolver reads a row by NAME
// ahead of the environment. So a row is the deployment's to write: a cluster
// owner writes one (which is what the operator credential admits), and so does
// server code that has authorized its caller and stamps internal origin for the
// one write -- a settings save. Nobody else does, by the named mutation or by
// the raw insert literal, on a fresh id or over a stored row.
//
// READS ARE DELIBERATELY NOT PART OF THIS RULE. The rows are plaintext and
// "visible to every tenant" by the concept's own description; MemQL OS, DSL
// logic and Go readers with no caller identity all read them. The last test
// here pins that, so the write rule cannot be "simplified" into a row tier that
// would answer those readers with zero rows -- which reads as a setting nobody
// made.
//
// Postgres-gated like its neighbours; CI's db-tests lane sets
// MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.

// variableWriteRefusal is the phrase the write rule's refusal carries. Held
// here so a write refused for any other reason -- a payload that failed
// validation, say -- does not read as the rule working.
const variableWriteRefusal = "configuration variable"

// variableWriteRow is one seeded variable row.
type variableWriteRow struct {
	// label is "global" or "partition": what ids, names and subtests are built
	// from, since a concept id carries colons and a row id may not.
	label             string
	concept, mutation string
	id, name, value   string
}

// stored is the row's id as the table keys it.
func (r variableWriteRow) stored() string { return r.concept + ":" + r.id }

// variableWriteFixture is one row in each of the two concepts, written by a
// cluster owner through the named mutations the operator CLI calls.
type variableWriteFixture struct {
	suffix string
	owner  context.Context
	rows   []variableWriteRow
}

func newVariableWriteFixture(t *testing.T, eng *MemQLEngine, tag string) variableWriteFixture {
	t.Helper()
	suffix := uniqueSuffix("variable-write-" + tag)
	f := variableWriteFixture{
		suffix: suffix,
		owner:  clusterOwnerCtx("variable-write-owner-" + suffix),
	}
	for _, spec := range []struct{ concept, mutation, label string }{
		{"v1:platform:globalVariable", "setGlobalVariable", "global"},
		{"v1:platform:partitionVariable", "setPartitionVariable", "partition"},
	} {
		row := variableWriteRow{
			label:    spec.label,
			concept:  spec.concept,
			mutation: spec.mutation,
			id:       "variable-write-" + spec.label + "-" + suffix,
			name:     strings.ToUpper(strings.ReplaceAll("VARIABLE_WRITE_"+spec.label+"_"+suffix, "-", "_")),
			value:    "set-by-the-owner-for-" + spec.label,
		}
		_, err := runSiteMutation(t, f.owner, eng, row.mutation, map[string]any{
			"id": row.id, "name": row.name, "value": row.value,
		})
		require.NoError(t, err, "a cluster owner writes a %s row: the operator path", row.concept)
		f.rows = append(f.rows, row)
	}
	return f
}

// below are the roles under the cluster owner. Developer ranks above admin and
// is here for that reason: a developer's settings saves go through server code
// that authorizes them, not through a write of their own.
func (f variableWriteFixture) below() []struct {
	name string
	ctx  context.Context
} {
	who := func(role auth.Role) context.Context {
		return rankActorCtx("variable-write-"+string(role)+"-"+f.suffix, role)
	}
	return []struct {
		name string
		ctx  context.Context
	}{
		{"a viewer", who(auth.RoleReader)},
		{"a user", who(auth.RoleWriter)},
		{"an admin", who(auth.RoleAdmin)},
		{"a developer", who(auth.RoleDeveloper)},
	}
}

// latestVariableValue reads the newest stored version of a row straight off the
// table, under no actor at all.
func latestVariableValue(t *testing.T, db *bun.DB, row variableWriteRow) string {
	t.Helper()
	value, _ := latestPayload(t, context.Background(), db, row.concept, row.stored())["value"].(string)
	return value
}

func TestVariableRowsAreWrittenByTheClusterOwnerOrStampedServerCode(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	f := newVariableWriteFixture(t, eng, "write")

	refused := func(t *testing.T, err error, who, what string) {
		t.Helper()
		require.Error(t, err, "%s's %s was admitted", who, what)
		require.Contains(t, err.Error(), variableWriteRefusal, "%s's %s was refused, but not by the variable write rule", who, what)
	}

	for _, row := range f.rows {
		for _, caller := range f.below() {
			t.Run(row.label+"/"+caller.name, func(t *testing.T) {
				tag := strings.ReplaceAll(caller.name, " ", "-")
				fresh := func(kind string) (id string, args map[string]any) {
					id = fmt.Sprintf("variable-write-%s-%s-%s-%s", row.label, kind, tag, f.suffix)
					return id, map[string]any{
						"name":  strings.ToUpper(strings.ReplaceAll(id, "-", "_")),
						"value": "written by " + caller.name,
					}
				}

				// A FRESH ID, by the named mutation and by the raw literal.
				namedId, namedArgs := fresh("named")
				namedArgs["id"] = namedId
				_, err := runSiteMutation(t, caller.ctx, eng, row.mutation, namedArgs)
				refused(t, err, caller.name, row.mutation+" on a fresh id")
				assertRowCount(t, db, row.concept, namedId, 0)

				rawId, rawArgs := fresh("raw")
				rawArgs["active"] = true
				refused(t, rawInsertFor(t, caller.ctx, eng, row.concept, rawId, rawArgs), caller.name, "raw insert on a fresh id")
				assertRowCount(t, db, row.concept, rawId, 0)

				// OVER THE STORED ROW, the same two ways. A refusal is evidence
				// only beside proof that the stored value did not move.
				_, err = runSiteMutation(t, caller.ctx, eng, row.mutation, map[string]any{
					"id": row.id, "name": row.name, "value": "overwritten by " + caller.name,
				})
				refused(t, err, caller.name, row.mutation+" over a stored row")
				refused(t, rawInsertFor(t, caller.ctx, eng, row.concept, row.id, map[string]any{
					"name": row.name, "value": "overwritten by " + caller.name, "active": true,
				}), caller.name, "raw insert over a stored row")
				// The switch a resolver honours, flipped on its own: the delta a
				// read-merge would carry onto the stored row.
				refused(t, rawInsertFor(t, caller.ctx, eng, row.concept, row.id, map[string]any{"active": false}),
					caller.name, "raw insert switching a stored row off")
				require.Equal(t, row.value, latestVariableValue(t, db, row),
					"%s's refused write changed the stored %s row", caller.name, row.concept)
				require.Equal(t, true, latestPayload(t, context.Background(), db, row.concept, row.stored())["active"],
					"%s's refused write switched the stored %s row off", caller.name, row.concept)
			})
		}
	}

	// THE WRITERS THAT MUST STILL LAND, each under the context it really uses.
	for _, row := range f.rows {
		t.Run(row.label+"/the writers that land", func(t *testing.T) {
			writers := []struct {
				name string
				ctx  context.Context
			}{
				// scripts/secrets over the operator credential, an owner in MemQL
				// OS, and an owner's own GitHub App setup.
				{"a cluster owner", f.owner},
				// A settings save that has already authorized its caller: the
				// caller's own actor, internal origin stamped for the one write.
				{"server code for a developer", auth.ContextWithInternalOrigin(rankActorCtx("variable-write-dev-writer-"+f.suffix, auth.RoleDeveloper))},
				// The boot injector's and the seed materializer's actor.
				{"the boot seeder", systemActorContext(context.Background())},
				// The deployment's own named actor: an automation of the
				// registered tree, a connector's configuration writes.
				{"a system actor", auth.ContextWithSystemActor(context.Background(), "variable-write-test")},
			}
			for i, w := range writers {
				value := fmt.Sprintf("set-%d-by-%s", i, w.name)
				freshId := fmt.Sprintf("variable-write-%s-writer-%d-%s", row.label, i, f.suffix)
				_, err := runSiteMutation(t, w.ctx, eng, row.mutation, map[string]any{
					"id": freshId, "name": strings.ToUpper(strings.ReplaceAll(freshId, "-", "_")), "value": value,
				})
				require.NoError(t, err, "%s could not write a fresh %s row", w.name, row.concept)
				assertRowCount(t, db, row.concept, freshId, 1)

				// And a change of the stored row: the same id, a new value.
				_, err = runSiteMutation(t, w.ctx, eng, row.mutation, map[string]any{
					"id": row.id, "name": row.name, "value": value,
				})
				require.NoError(t, err, "%s could not change a stored %s row", w.name, row.concept)
				require.Equal(t, value, latestVariableValue(t, db, row), "%s's change did not land", w.name)
			}

			// The raw literal is the cluster owner's too.
			rawId := fmt.Sprintf("variable-write-%s-owner-raw-%s", row.label, f.suffix)
			require.NoError(t, rawInsertFor(t, f.owner, eng, row.concept, rawId, map[string]any{
				"name": strings.ToUpper(strings.ReplaceAll(rawId, "-", "_")), "value": "raw, by the owner", "active": true,
			}), "a cluster owner's raw insert of a %s row", row.concept)
			assertRowCount(t, db, row.concept, rawId, 1)
		})
	}
}

// A NAME RESOLVES ONLY TO A ROW THE DEPLOYMENT'S AUDIENCE WROTE. The resolver
// reads the first row carrying a name, ahead of the environment, so who may
// write a row is who decides what a name means.
func TestAVariableNameResolvesOnlyToARowTheDeploymentsAudienceWrote(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	f := newVariableWriteFixture(t, eng, "names")
	for _, caller := range f.below() {
		t.Run(caller.name, func(t *testing.T) {
			tag := strings.ReplaceAll(caller.name, " ", "-")
			for _, row := range f.rows {
				id := fmt.Sprintf("variable-write-placed-%s-%s-%s", row.label, tag, f.suffix)
				name := strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
				_, err := runSiteMutation(t, caller.ctx, eng, row.mutation, map[string]any{
					"id": id, "name": name, "value": "placed by " + caller.name,
				})
				require.Error(t, err, "%s placed a %s row under a name", caller.name, row.concept)
				require.Contains(t, err.Error(), variableWriteRefusal, "%s was refused, but not by the variable write rule", caller.name)
				_, err = eng.ResolveVariable(context.Background(), name)
				require.True(t, IsVariableNotFound(err), "the name %s tried to place resolved (%v)", caller.name, err)
			}
		})
	}
}

// READS ARE AS THEY WERE. Every role, and a call with no identity, reads a
// variable row by the named query MemQL OS and DSL logic use, by the raw form a
// concept browser sends, and through the resolvers.
//
// This is a pin and not a regression test: it passed before the write rule and
// must keep passing. It is here because the obvious way to state the write rule
// -- a cluster-owner row tier -- would also decide reads, and would answer a
// reader with no identity, and an account-scoped role, with zero rows.
func TestVariableRowsAreStillReadByEveryCaller(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	f := newVariableWriteFixture(t, eng, "read")
	global, partition := f.rows[0], f.rows[1]

	callers := append(f.below(),
		struct {
			name string
			ctx  context.Context
		}{"the cluster owner", f.owner},
		struct {
			name string
			ctx  context.Context
		}{"a call with no identity", context.Background()},
	)
	for _, caller := range callers {
		t.Run(caller.name, func(t *testing.T) {
			res, err := eng.Execute(caller.ctx, fmt.Sprintf(`query globalVariable(name: %s)`, langparser.QuoteString(global.name)))
			require.NoError(t, err, "the named query under the context of %s", caller.name)
			require.Contains(t, resultBlob(t, res), global.value, "%s did not read the row through the named query", caller.name)

			for _, row := range f.rows {
				res, err := eng.Execute(caller.ctx, fmt.Sprintf(`concept==%s && payload.name==%s`, row.concept, langparser.QuoteString(row.name)))
				require.NoError(t, err, "the raw read under the context of %s", caller.name)
				require.Contains(t, resultBlob(t, res), row.value, "%s did not read the %s row through the raw form", caller.name, row.concept)
			}

			got, err := eng.ResolveSystemVariable(caller.ctx, global.name)
			require.NoError(t, err, "ResolveSystemVariable under the context of %s", caller.name)
			require.Equal(t, global.value, got)

			got, err = eng.ResolveVariable(caller.ctx, partition.name)
			require.NoError(t, err, "ResolveVariable (the partition row) under the context of %s", caller.name)
			require.Equal(t, partition.value, got)
		})
	}
}
