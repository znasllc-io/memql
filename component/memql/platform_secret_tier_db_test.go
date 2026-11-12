package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/secret"
)

// THE TWO SECRET CONCEPTS ON THE CLUSTER-OWNER TIER, against a real engine and
// a real Postgres.
//
// v1:platform:globalSecret and v1:platform:partitionSecret hold the
// deployment's own credentials, sealed. They are administrative rows and
// declare @rowAuthz(clusterOwner): the rows are the cluster owner's to read and
// to write, and the deployment's own readers and writers reach them as the
// deployment.
//
// The declaration can be wrong in two directions, and each test here holds one:
//
//   - THE TIER HOLDS. Nobody below the cluster owner reads a row by any spelling
//     of a read, is sent one by a subscription, or writes one by a named
//     mutation or the raw literal -- on a fresh id or over a stored row.
//   - THE DEPLOYMENT STILL WORKS. Every secret in the cluster is read through
//     ResolveSecret / ResolveSystemSecret, and their callers hold whatever
//     context they were handed: a visitor's request at the edge, a webhook with
//     no identity, a send triggered by a person of any role. A tier that judged
//     the lookup by its caller would answer every one of them "not found", which
//     reads as a secret nobody stored. And the writers that seal by name -- the
//     operator CLI, the boot injector, a settings save -- still land.
//
// Postgres-gated like its neighbours; CI's db-tests lane sets
// MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.

// secretTierRow is one seeded secret row and what it was sealed from.
type secretTierRow struct {
	// label is "global" or "partition": what ids, names and subtests are built
	// from, since a concept id carries colons and a row id may not.
	label             string
	concept, mutation string
	id, name          string
	plaintext         string
	ciphertext        string
	fingerprint       string
}

// stored is the row's id as the table keys it.
func (r secretTierRow) stored() string { return r.concept + ":" + r.id }

// secretTierFixture is one sealed row in each of the two concepts, written by a
// cluster owner through the named mutations the operator CLI calls.
type secretTierFixture struct {
	suffix string
	owner  context.Context
	rows   []secretTierRow
}

func sealForSecretTier(t *testing.T, plaintext string) (ciphertext, fingerprint string) {
	t.Helper()
	ciphertext, fingerprint, err := secret.Encrypt(plaintext)
	require.NoError(t, err, "seal the fixture")
	return ciphertext, fingerprint
}

func newSecretTierFixture(t *testing.T, eng *MemQLEngine, tag string) secretTierFixture {
	t.Helper()
	t.Setenv(secret.EnvMasterKey, strings.Repeat("c3", 32))
	suffix := uniqueSuffix("secret-tier-" + tag)
	f := secretTierFixture{
		suffix: suffix,
		owner:  clusterOwnerCtx("secret-tier-owner-" + suffix),
	}
	for _, spec := range []struct{ concept, mutation, label string }{
		{"v1:platform:globalSecret", "setGlobalSecret", "global"},
		{"v1:platform:partitionSecret", "setPartitionSecret", "partition"},
	} {
		row := secretTierRow{
			label:     spec.label,
			concept:   spec.concept,
			mutation:  spec.mutation,
			id:        "secret-tier-" + spec.label + "-" + suffix,
			name:      "SECRET_TIER_" + strings.ToUpper(spec.label) + "_" + suffix,
			plaintext: "sealed-for-" + spec.label + "-" + suffix,
		}
		row.ciphertext, row.fingerprint = sealForSecretTier(t, row.plaintext)
		_, err := runSiteMutation(t, f.owner, eng, row.mutation, map[string]any{
			"id": row.id, "name": row.name, "encryptedValue": row.ciphertext, "fingerprint": row.fingerprint,
		})
		require.NoError(t, err, "a cluster owner seals a %s row: the operator path", row.concept)
		f.rows = append(f.rows, row)
	}
	return f
}

// below are the roles under the cluster owner. Developer ranks above admin and
// is here for that reason: the tier's audience is the owner role, not a rank.
func (f secretTierFixture) below() []struct {
	name string
	ctx  context.Context
} {
	who := func(role auth.Role) context.Context {
		return rankActorCtx("secret-tier-"+string(role)+"-"+f.suffix, role)
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

func TestSecretRowsAreReadByTheClusterOwnerOnly(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	f := newSecretTierFixture(t, eng, "read")
	later := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)

	reply := func(t *testing.T, ctx context.Context, query string) (string, int, error) {
		t.Helper()
		res, err := eng.Execute(ctx, query)
		if err != nil {
			return "", 0, err
		}
		return resultBlob(t, res), len(MaterializeRows(res)), nil
	}

	for _, row := range f.rows {
		byName := fmt.Sprintf(`concept==%s && payload.name==%s`, row.concept, langparser.QuoteString(row.name))
		// Every spelling of a read that reaches a row with no named construct in
		// the way. None of them has a declared binding, so each is decided by the
		// row's own concept and nothing else.
		forms := []struct{ name, query string }{
			{"by name", byName},
			{"by id", fmt.Sprintf(`concept==%s && id==%s`, row.concept, langparser.QuoteString(row.stored()))},
			{"the whole concept", `concept==` + row.concept},
			{"sort(paginate(...))", fmt.Sprintf(`sort(paginate(%s, 10), "createdAt", "desc")`, byName)},
			{"asOf", fmt.Sprintf(`asOf(%s, %s)`, byName, langparser.QuoteString(later))},
		}
		for _, form := range forms {
			t.Run(row.label+"/"+form.name, func(t *testing.T) {
				// THE CONTROL: the cluster owner reads rows through this very
				// query. A form that returned nothing here would pass every
				// refusal below for nothing.
				blob, rows, err := reply(t, f.owner, form.query)
				require.NoError(t, err, "the control read failed, so the form below proves nothing")
				require.NotZero(t, rows, "the control read returned no rows, so the form below proves nothing")
				if form.name != "the whole concept" {
					require.Contains(t, blob, row.fingerprint, "the control read did not return the seeded row")
				}

				callers := append(f.below(), struct {
					name string
					ctx  context.Context
				}{"a call with no identity", context.Background()})
				for _, caller := range callers {
					blob, rows, err := reply(t, caller.ctx, form.query)
					if err != nil {
						continue // a refusal is a refusal
					}
					require.Zero(t, rows, "%s read %d %s row(s) through %s", caller.name, rows, row.concept, form.name)
					for _, sealed := range []string{row.ciphertext, row.fingerprint} {
						require.NotContains(t, blob, sealed, "%s was handed a sealed field through %s", caller.name, form.name)
					}
				}
			})
		}
	}
}

// A SUBSCRIPTION IS A READ. Graph events are fanned out through the same per-row
// admission, so the tier decides a live feed of these rows the way it decides a
// query. Control: the cluster owner is sent the row.
func TestSecretRowsAreDeliveredToTheClusterOwnerOnly(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	f := newSecretTierFixture(t, eng, "subscription")
	admit := func(access *auth.AccessContext, row secretTierRow) SubscriptionAdmission {
		t.Helper()
		raw, err := json.Marshal(map[string]any{
			"name": row.name, "encryptedValue": row.ciphertext, "fingerprint": row.fingerprint, "active": true,
		})
		require.NoError(t, err)
		return AdmitSubscriptionRow(eng.SubscriptionRankContext(context.Background()), access, row.concept, row.stored(), raw)
	}
	for _, row := range f.rows {
		t.Run(row.label, func(t *testing.T) {
			require.Equal(t, SubscriptionAdmit, admit(&auth.AccessContext{UserId: "secret-tier-owner-" + f.suffix, Role: auth.RoleOwner}, row),
				"the cluster owner was not sent the row, so no denial below is informative")
			for _, role := range []auth.Role{auth.RoleReader, auth.RoleWriter, auth.RoleAdmin, auth.RoleDeveloper} {
				require.Equal(t, SubscriptionDeny, admit(&auth.AccessContext{UserId: "secret-tier-" + string(role) + "-" + f.suffix, Role: role}, row),
					"a %s stream would be sent a live %s event", role, row.concept)
			}
			require.Equal(t, SubscriptionDeny, admit(nil, row), "a stream with no identity would be sent a live %s event", row.concept)
		})
	}
}

func TestSecretRowsAreWrittenByTheClusterOwnerOnly(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	f := newSecretTierFixture(t, eng, "write")

	// refused holds a write to the tier's own refusal. An error of any other
	// kind -- a payload that failed validation, say -- would read as a pass on
	// an engine that judges nothing.
	refused := func(t *testing.T, err error, who, what string) {
		t.Helper()
		require.Error(t, err, "%s's %s was admitted", who, what)
		require.Contains(t, err.Error(), "row-authz", "%s's %s was refused, but not by the row tier", who, what)
	}

	for _, row := range f.rows {
		for _, caller := range f.below() {
			t.Run(row.label+"/"+caller.name, func(t *testing.T) {
				tag := strings.ReplaceAll(caller.name, " ", "-")
				ciphertext, fingerprint := sealForSecretTier(t, "written by "+caller.name)
				fresh := func(kind string) (id string, args map[string]any) {
					id = fmt.Sprintf("secret-tier-%s-%s-%s-%s", row.label, kind, tag, f.suffix)
					return id, map[string]any{
						"name":           strings.ToUpper("SECRET_TIER_" + kind + "_" + tag + "_" + f.suffix),
						"encryptedValue": ciphertext, "fingerprint": fingerprint,
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
					"id": row.id, "name": row.name, "encryptedValue": ciphertext, "fingerprint": fingerprint,
				})
				refused(t, err, caller.name, row.mutation+" over a stored row")
				refused(t, rawInsertFor(t, caller.ctx, eng, row.concept, row.id, map[string]any{
					"name": row.name, "encryptedValue": ciphertext, "fingerprint": fingerprint, "active": true,
				}), caller.name, "raw insert over a stored row")
				require.Equal(t, row.ciphertext, latestSecretCiphertext(t, db, row),
					"%s's refused write changed the stored %s row", caller.name, row.concept)
			})
		}
	}

	// THE WRITERS THAT MUST STILL LAND, each under the context it really uses.
	for _, row := range f.rows {
		t.Run(row.label+"/the tier's writers", func(t *testing.T) {
			writers := []struct {
				name string
				ctx  context.Context
			}{
				// scripts/secrets over the operator credential, and an owner acting
				// under their own actor (the GitHub App removal).
				{"a cluster owner", f.owner},
				// A settings save that has already authorized its caller: the
				// caller's own actor, internal origin stamped for the one write.
				{"server code for a developer", auth.ContextWithInternalOrigin(rankActorCtx("secret-tier-dev-writer-"+f.suffix, auth.RoleDeveloper))},
				// The boot injector's and the seed materializer's actor.
				{"the boot seeder", systemActorContext(context.Background())},
				// The deployment's own named actor, as a connector's configuration
				// writes use it.
				{"a system actor", auth.ContextWithSystemActor(context.Background(), "secret-tier-test")},
			}
			for i, w := range writers {
				plaintext := fmt.Sprintf("rotated-%d-by-%s", i, w.name)
				ciphertext, fingerprint := sealForSecretTier(t, plaintext)
				freshId := fmt.Sprintf("secret-tier-%s-writer-%d-%s", row.label, i, f.suffix)
				_, err := runSiteMutation(t, w.ctx, eng, row.mutation, map[string]any{
					"id": freshId, "name": strings.ToUpper(freshId), "encryptedValue": ciphertext, "fingerprint": fingerprint,
				})
				require.NoError(t, err, "%s could not seal a fresh %s row", w.name, row.concept)
				assertRowCount(t, db, row.concept, freshId, 1)

				// And a rotation: the same id, a new value.
				_, err = runSiteMutation(t, w.ctx, eng, row.mutation, map[string]any{
					"id": row.id, "name": row.name, "encryptedValue": ciphertext, "fingerprint": fingerprint,
				})
				require.NoError(t, err, "%s could not rotate a stored %s row", w.name, row.concept)
				require.Equal(t, ciphertext, latestSecretCiphertext(t, db, row), "%s's rotation did not land", w.name)
			}
		})
	}
}

// latestSecretCiphertext reads the newest stored version of a row straight off
// the table, under no actor at all.
func latestSecretCiphertext(t *testing.T, db *bun.DB, row secretTierRow) string {
	t.Helper()
	value, _ := latestPayload(t, context.Background(), db, row.concept, row.stored())["encryptedValue"].(string)
	return value
}

// EVERY CALLER OF THE RESOLVER STILL RESOLVES. The lookup is the deployment
// reading its own row, so the context it was handed -- any role, or none --
// does not decide it. And the lookup's result stays the deployment's: the same
// query string, issued by the person whose request triggered it, returns no
// row.
func TestSecretsResolveWhateverContextTheResolverWasHanded(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	f := newSecretTierFixture(t, eng, "resolve")
	global, partition := f.rows[0], f.rows[1]

	callers := append(f.below(), struct {
		name string
		ctx  context.Context
	}{"a call with no identity", context.Background()})
	for _, caller := range callers {
		t.Run(caller.name, func(t *testing.T) {
			got, err := eng.ResolveSystemSecret(caller.ctx, global.name)
			require.NoError(t, err, "ResolveSystemSecret under the context of %s", caller.name)
			require.Equal(t, global.plaintext, got)

			got, err = eng.ResolveSecret(caller.ctx, partition.name)
			require.NoError(t, err, "ResolveSecret (the partition row) under the context of %s", caller.name)
			require.Equal(t, partition.plaintext, got)

			// The partition-first resolver falls back to the global row on a
			// miss, and a lookup the tier emptied must still read as that miss.
			got, err = eng.ResolveSecret(caller.ctx, global.name)
			require.NoError(t, err, "ResolveSecret (the global fallback) under the context of %s", caller.name)
			require.Equal(t, global.plaintext, got)

			_, err = eng.ResolveSystemSecret(caller.ctx, "SECRET_TIER_NOBODY_"+f.suffix)
			require.True(t, IsVariableNotFound(err), "a name nobody sealed answered %v, want a not-found miss", err)

			// The resolver's own statement, byte for byte, from the caller.
			for _, row := range f.rows {
				res, err := eng.Execute(caller.ctx, fmt.Sprintf(`concept==%s&&payload.name==%s`, row.concept, langparser.QuoteString(row.name)))
				if err != nil {
					continue // a refusal is a refusal
				}
				require.Empty(t, MaterializeRows(res), "%s read the row the resolver had just read for them", caller.name)
			}
		})
	}
}

// A NAME RESOLVES ONLY TO A ROW THE TIER'S AUDIENCE WROTE. The resolver reads
// the first row carrying a name, so who may write a row is who decides what a
// name means.
func TestANameResolvesOnlyToARowTheTiersAudienceWrote(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	f := newSecretTierFixture(t, eng, "names")
	for _, caller := range f.below() {
		t.Run(caller.name, func(t *testing.T) {
			tag := strings.ReplaceAll(caller.name, " ", "-")
			ciphertext, fingerprint := sealForSecretTier(t, "placed by "+caller.name)
			for _, row := range f.rows {
				id := fmt.Sprintf("secret-tier-placed-%s-%s-%s", row.label, tag, f.suffix)
				name := strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
				_, err := runSiteMutation(t, caller.ctx, eng, row.mutation, map[string]any{
					"id": id, "name": name, "encryptedValue": ciphertext, "fingerprint": fingerprint,
				})
				require.Error(t, err, "%s placed a %s row under a name", caller.name, row.concept)
				require.Contains(t, err.Error(), "row-authz", "%s was refused, but not by the row tier", caller.name)
				_, err = eng.ResolveSecret(context.Background(), name)
				require.True(t, IsVariableNotFound(err), "the name %s tried to place resolved (%v)", caller.name, err)
			}
		})
	}
}
