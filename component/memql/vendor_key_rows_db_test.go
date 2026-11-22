package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
)

// vendor_key_rows_db_test.go -- the two rows of a person's API key (design
// record docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K1, K2 and K16, safety rules S2 and S3).
//
// A key is a credential that spends its owner's money. Three things about its
// rows are asserted here against the real tree and a real database:
//
//   - NO CLIENT WRITES ONE. Every named write is @serverOnly and both concepts
//     are @serverWritten, so a raw insert or an inline update is refused for
//     the row's owner and for a cluster owner alike. The integration is the
//     only writer, with internal origin.
//   - NO CLIENT READS ONE, by any path. The row tier would admit the owner and
//     a cluster owner; the clause in site_access_guards.go refuses them anyway,
//     because the only client path to a key is a builtin that decides what a
//     caller may see of it (its owner: everything but the value; a person it
//     is lent to: a short card).
//   - NO EVENT CARRIES ONE. A write publishes no graph event, so nothing can
//     subscribe to a key or trigger on one, and the sealed value rides no
//     topic.

// vendorKeyRowIDs are one test's people and its key.
type vendorKeyRowIDs struct {
	owner, borrower, operator, stranger string
	key                                 string
	// sealed stands in for what component/secret.Encrypt returns. The DSL
	// stores what the integration sealed and never looks inside.
	sealed, label, fingerprint string
}

func newVendorKeyRowIDs(name string) vendorKeyRowIDs {
	suffix := fmt.Sprintf("%s-%d-%d", name, os.Getpid(), time.Now().UnixNano())
	return vendorKeyRowIDs{
		owner:       "vkey-owner-" + suffix,
		borrower:    "vkey-borrower-" + suffix,
		operator:    "vkey-operator-" + suffix,
		stranger:    "vkey-stranger-" + suffix,
		key:         "vkey-" + suffix,
		sealed:      "c2VhbGVkLXN0YW5kLWluLQ" + strings.ReplaceAll(suffix, "-", ""),
		label:       "Label " + suffix,
		fingerprint: "...w" + suffix[len(suffix)-3:],
	}
}

func (ids vendorKeyRowIDs) createArgs() map[string]any {
	return map[string]any{
		"keyId": ids.key, "vendor": "anthropic", "label": ids.label,
		"encryptedValue": ids.sealed, "fingerprint": ids.fingerprint, "checkedBy": "list",
	}
}

func (ids vendorKeyRowIDs) stateArgs(at time.Time) map[string]any {
	stamp := at.UTC().Format(time.RFC3339)
	return map[string]any{
		"keyId": ids.key, "vendor": "anthropic", "state": "good",
		"evidenceAt": stamp, "checkedAt": stamp, "goodAt": stamp,
	}
}

// vendorKeyCallers are the people the design names: the key's owner, a person
// it is lent to, a cluster owner and somebody with no tie to it.
func (ids vendorKeyRowIDs) callers() []struct {
	name, userID string
	role         auth.Role
} {
	return []struct {
		name, userID string
		role         auth.Role
	}{
		{"the key's owner", ids.owner, auth.RoleWriter},
		{"a person the key is lent to", ids.borrower, auth.RoleWriter},
		{"a cluster owner", ids.operator, auth.RoleOwner},
		{"a stranger", ids.stranger, auth.RoleWriter},
	}
}

// asKeyOwner is how integrations/vendorkeys writes: the owner's actor, with
// internal origin stamped.
func (ids vendorKeyRowIDs) asKeyOwner() context.Context {
	return auth.ContextWithInternalOrigin(rankActorCtx(ids.owner, auth.RoleWriter))
}

// seedVendorKey writes one key and its state the way the integration does and
// returns the two stored ids.
func seedVendorKey(t *testing.T, eng *MemQLEngine, ids vendorKeyRowIDs) (keyStored, stateStored string) {
	t.Helper()
	for _, c := range ids.callers() {
		seedPrincipal(t, eng, c.userID, c.role)
	}
	res, err := eng.Execute(ids.asKeyOwner(), siteAccessCall(t, "mutation", "createVendorKey", ids.createArgs()))
	require.NoError(t, err)
	require.NotEmpty(t, res.Bundle.Nodes)
	keyStored = res.Bundle.Nodes[0].Id
	res, err = eng.Execute(ids.asKeyOwner(), siteAccessCall(t, "mutation", "createVendorKeyState", ids.stateArgs(time.Now())))
	require.NoError(t, err)
	require.NotEmpty(t, res.Bundle.Nodes)
	return keyStored, res.Bundle.Nodes[0].Id
}

func TestVendorKeyRowsRefuseAWriteWithoutInternalOrigin(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	ids := newVendorKeyRowIDs("write")
	for _, c := range ids.callers() {
		seedPrincipal(t, eng, c.userID, c.role)
	}
	now := time.Now().UTC().Truncate(time.Second)

	// Every construct, with VALID arguments, so a refusal can only be the gate.
	calls := []struct{ construct, call string }{
		{"createVendorKey", siteAccessCall(t, "mutation", "createVendorKey", ids.createArgs())},
		{"createVendorKeyState", siteAccessCall(t, "mutation", "createVendorKeyState", ids.stateArgs(now))},
		{"updateVendorKey", siteAccessCall(t, "mutation", "updateVendorKey", map[string]any{"keyId": ids.key, "label": ids.label + " renamed"})},
		{"setVendorKeySharing", siteAccessCall(t, "mutation", "setVendorKeySharing", map[string]any{
			"keyId": ids.key, "mode": "people", "userIds": []string{ids.borrower},
		})},
		{"updateVendorKeyState", siteAccessCall(t, "mutation", "updateVendorKeyState", map[string]any{
			"keyId": ids.key, "state": "refused", "evidenceAt": now.Add(time.Minute).Format(time.RFC3339), "detail": "the vendor refused it",
		})},
		{"removeVendorKey", siteAccessCall(t, "mutation", "removeVendorKey", map[string]any{"keyId": ids.key, "removedBy": ids.operator})},
		{"vendorKeyById", siteAccessCall(t, "query", "vendorKeyById", map[string]any{"keyId": ids.key})},
		{"vendorKeySealedById", siteAccessCall(t, "query", "vendorKeySealedById", map[string]any{"keyId": ids.key})},
		{"vendorKeysForOwner", siteAccessCall(t, "query", "vendorKeysForOwner", map[string]any{"ownerUserId": ids.owner})},
		{"vendorKeysActiveAtVendor", siteAccessCall(t, "query", "vendorKeysActiveAtVendor", map[string]any{"vendor": "anthropic"})},
		{"vendorKeyStateById", siteAccessCall(t, "query", "vendorKeyStateById", map[string]any{"keyId": ids.key})},
		{"vendorKeyStatesForKeys", siteAccessCall(t, "query", "vendorKeyStatesForKeys", map[string]any{"keyIds": []string{ids.key}})},
	}

	// The key's owner and a cluster owner are the two people the row tier
	// admits, so they are the two the refusal is asserted for.
	for _, caller := range []struct {
		name, userID string
		role         auth.Role
	}{{"the key's owner", ids.owner, auth.RoleWriter}, {"a cluster owner", ids.operator, auth.RoleOwner}} {
		client := auth.ContextWithClientOrigin(rankActorCtx(caller.userID, caller.role))
		for _, tc := range calls {
			t.Run(caller.name+": a client-origin "+tc.construct+" is refused", func(t *testing.T) {
				_, err := eng.Execute(client, tc.call)
				require.Error(t, err, "%s answered a client-origin call", tc.construct)
				require.Contains(t, err.Error(), "server-only", "%s was refused, but not by the server-only gate", tc.construct)
			})
		}
		for _, kind := range []string{vendorKeyConcept, vendorKeyStateConcept} {
			for _, verb := range []string{"insert", "update"} {
				t.Run(fmt.Sprintf("%s: a raw %s of %s is refused", caller.name, verb, BareShortId(kind)), func(t *testing.T) {
					id := fmt.Sprintf("%s:forged-%d", kind, time.Now().UnixNano())
					payload := fmt.Sprintf(`{"ownerUserId":"v1:identity:user:%s","vendor":"anthropic","label":"forged","status":"active","state":"good","keyId":"x"}`, caller.userID)
					err := attemptServerRecordWrite(eng, rankActorCtx(caller.userID, caller.role), verb, kind, id, payload)
					require.Error(t, err)
					require.Contains(t, err.Error(), "@serverWritten requires internal origin")
					n, err := db.NewSelect().Model((*concept.MemoryNode)(nil)).Where("concept = ? AND id = ?", kind, id).Count(ctx)
					require.NoError(t, err)
					require.Zero(t, n, "a refused write stored a row")
				})
			}
		}
	}

	// With internal origin the same writes pass, in the order a key lives.
	owner := ids.asKeyOwner()
	stored := map[string]string{}
	for _, tc := range calls {
		if strings.HasPrefix(tc.call, "query ") {
			continue
		}
		res, err := eng.Execute(owner, tc.call)
		require.NoError(t, err, "%s refused the integration's own write", tc.construct)
		if len(res.Bundle.Nodes) > 0 {
			stored[tc.construct] = res.Bundle.Nodes[0].Id
		}
		switch tc.construct {
		case "createVendorKey":
			row := latestPayload(t, ctx, db, vendorKeyConcept, stored[tc.construct])
			require.Equal(t, ids.owner, BareShortId(fmt.Sprint(row["ownerUserId"])), "the key belongs to the actor the write ran as, never to an argument")
			require.Equal(t, "active", row["status"])
			require.NotEmpty(t, row["addedAt"])
			require.Equal(t, false, row["autoUse"], "Auto is off for a new key until its owner switches it on")
			require.Equal(t, false, row["isDefault"])
			sharing, _ := row["sharing"].(map[string]any)
			require.Equal(t, "owner", sharing["mode"], "a new key is lent to nobody")
		case "createVendorKeyState":
			row := latestPayload(t, ctx, db, vendorKeyStateConcept, stored[tc.construct])
			require.Equal(t, ids.owner, BareShortId(fmt.Sprint(row["ownerUserId"])))
			require.Equal(t, ids.key, BareShortId(fmt.Sprint(row["keyId"])))
			require.Equal(t, "good", row["state"])
		case "updateVendorKey":
			row := latestPayload(t, ctx, db, vendorKeyConcept, stored["createVendorKey"])
			require.Equal(t, ids.label+" renamed", row["label"])
			require.Equal(t, false, row["isDefault"], "a rename left the other settings alone")
			require.Equal(t, ids.sealed, row["encryptedValue"], "a rename does not touch the sealed value")
		case "setVendorKeySharing":
			row := latestPayload(t, ctx, db, vendorKeyConcept, stored["createVendorKey"])
			sharing, _ := row["sharing"].(map[string]any)
			require.Equal(t, "people", sharing["mode"])
			require.Equal(t, ids.owner, BareShortId(fmt.Sprint(sharing["sharedBy"])), "who lent a key is stamped from the actor")
			require.NotEmpty(t, sharing["sharedAt"])
			require.Equal(t, ids.label+" renamed", row["label"], "lending left the owner's other settings alone")
		case "updateVendorKeyState":
			row := latestPayload(t, ctx, db, vendorKeyStateConcept, stored["createVendorKeyState"])
			require.Equal(t, "refused", row["state"])
			require.Equal(t, "the vendor refused it", row["detail"])
			require.NotEmpty(t, row["goodAt"], "an observation keeps the stamps it did not write")
			key := latestPayload(t, ctx, db, vendorKeyConcept, stored["createVendorKey"])
			sharing, _ := key["sharing"].(map[string]any)
			require.Equal(t, "people", sharing["mode"], "what the engine observed is a row of its own and cannot undo what the owner decided")
		case "removeVendorKey":
			row := latestPayload(t, ctx, db, vendorKeyConcept, stored["createVendorKey"])
			require.Equal(t, "removed", row["status"])
			require.NotEmpty(t, row["removedAt"])
			require.Equal(t, ids.operator, BareShortId(fmt.Sprint(row["removedBy"])), "who removed a key is the person who asked")
			require.Equal(t, false, row["autoUse"])
			require.Equal(t, false, row["isDefault"])
			sharing, _ := row["sharing"].(map[string]any)
			require.Equal(t, "owner", sharing["mode"], "a removed key is lent to nobody")
			require.Empty(t, sharing["userIds"])
			require.Equal(t, ids.owner, BareShortId(fmt.Sprint(row["ownerUserId"])), "removal never moves the key to whoever removed it")
		}
	}

	// A removed key leaves its owner's list and stays readable by id (K16).
	res, err := eng.Execute(owner, siteAccessCall(t, "query", "vendorKeysForOwner", map[string]any{"ownerUserId": ids.owner}))
	require.NoError(t, err)
	require.NotContains(t, resultBlob(t, res), ids.key, "a removed key is still listed")
	res, err = eng.Execute(owner, siteAccessCall(t, "query", "vendorKeyById", map[string]any{"keyId": ids.key}))
	require.NoError(t, err)
	require.Contains(t, resultBlob(t, res), `"removed"`, "the removed key's row is gone, and a deleted row answers no question after an incident")
}

func TestVendorKeyRowsAreUnreadableByAClient(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	ids := newVendorKeyRowIDs("read")
	keyStored, stateStored := seedVendorKey(t, eng, ids)
	_, err := eng.Execute(ids.asKeyOwner(), siteAccessCall(t, "mutation", "setVendorKeySharing", map[string]any{
		"keyId": ids.key, "mode": "people", "userIds": []string{ids.borrower},
	}))
	require.NoError(t, err)
	at := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)

	leaks := []string{ids.key, ids.sealed, ids.label, ids.fingerprint}
	forms := []struct{ name, query string }{
		{"a raw read by id", fmt.Sprintf(`concept==%s && id==%q`, vendorKeyConcept, keyStored)},
		{"a raw read by label", fmt.Sprintf(`concept==%s && label==%q`, vendorKeyConcept, ids.label)},
		{"a raw read by sealed value", fmt.Sprintf(`concept==%s && encryptedValue==%q`, vendorKeyConcept, ids.sealed)},
		{"sort(paginate(...))", fmt.Sprintf(`sort(paginate(concept==%s && label==%q, 10), "createdAt", "desc")`, vendorKeyConcept, ids.label)},
		{"asOf", fmt.Sprintf(`asOf(concept==%s && label==%q, %q)`, vendorKeyConcept, ids.label, at)},
		{"a raw read of the state by id", fmt.Sprintf(`concept==%s && id==%q`, vendorKeyStateConcept, stateStored)},
		{"parentOf the state", fmt.Sprintf(`parentOf(concept==%s && id==%q)`, vendorKeyStateConcept, stateStored)},
	}
	reply := func(ctx context.Context, query string) (string, int, error) {
		res, err := eng.Execute(ctx, query)
		if err != nil {
			return "", 0, err
		}
		return resultBlob(t, res), len(MaterializeRows(res)), nil
	}
	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			// THE CONTROL: the integration's own reader gets the row through
			// this very query, so a refusal below is not an empty fixture.
			blob, rows, err := reply(ids.asKeyOwner(), form.query)
			require.NoError(t, err, "the control read failed, so the form proves nothing")
			require.NotZero(t, rows, "the control read returned no rows: %s", blob)
			require.Contains(t, blob, ids.key, "the control read returned rows that are not this key's")

			for _, caller := range ids.callers() {
				blob, rows, err := reply(rankActorCtx(caller.userID, caller.role), form.query)
				if err != nil {
					continue // a refusal is a refusal
				}
				require.Zero(t, rows, "%s read %d row(s) through %s: %s", caller.name, rows, form.name, blob)
				for _, leak := range leaks {
					require.NotContains(t, blob, leak, "%s was handed %q through %s", caller.name, leak, form.name)
				}
			}
		})
	}

	// The named reads under them, as the integration calls them.
	t.Run("the integration's reads", func(t *testing.T) {
		owner := ids.asKeyOwner()
		card, rows, err := reply(owner, siteAccessCall(t, "query", "vendorKeyById", map[string]any{"keyId": ids.key}))
		require.NoError(t, err)
		require.Equal(t, 1, rows)
		require.Contains(t, card, ids.label)
		require.NotContains(t, card, ids.sealed, "the card carries the sealed value; only the one sealed read may")

		sealed, rows, err := reply(owner, siteAccessCall(t, "query", "vendorKeySealedById", map[string]any{"keyId": ids.key}))
		require.NoError(t, err)
		require.Equal(t, 1, rows)
		require.Contains(t, sealed, ids.sealed)

		for name, call := range map[string]string{
			"vendorKeysForOwner":       siteAccessCall(t, "query", "vendorKeysForOwner", map[string]any{"ownerUserId": ids.owner}),
			"vendorKeysActiveAtVendor": siteAccessCall(t, "query", "vendorKeysActiveAtVendor", map[string]any{"vendor": "anthropic"}),
		} {
			blob, _, err := reply(owner, call)
			require.NoError(t, err, name)
			require.Contains(t, blob, ids.key, "%s did not return the key", name)
			require.NotContains(t, blob, ids.sealed, "%s carries the sealed value", name)
		}
		// The other vendor's list does not carry an Anthropic key.
		blob, _, err := reply(owner, siteAccessCall(t, "query", "vendorKeysActiveAtVendor", map[string]any{"vendor": "openai"}))
		require.NoError(t, err)
		require.NotContains(t, blob, ids.key)

		for name, call := range map[string]string{
			"vendorKeyStateById":     siteAccessCall(t, "query", "vendorKeyStateById", map[string]any{"keyId": ids.key}),
			"vendorKeyStatesForKeys": siteAccessCall(t, "query", "vendorKeyStatesForKeys", map[string]any{"keyIds": []string{ids.key, "vkey-that-does-not-exist"}}),
		} {
			blob, rows, err := reply(owner, call)
			require.NoError(t, err, name)
			require.Equal(t, 1, rows, "%s returned %d rows: %s", name, rows, blob)
			require.Contains(t, blob, `"good"`)
		}

		// The cluster acting on its own reads every key: the daily check and
		// the resolver of lent keys have no person to read as.
		blob, _, err = reply(groupSeedCtx(), siteAccessCall(t, "query", "vendorKeysActiveAtVendor", map[string]any{"vendor": "anthropic"}))
		require.NoError(t, err)
		require.Contains(t, blob, ids.key, "the cluster's own reader cannot see a key it must check")
		// And a stranger, even with internal origin, is still decided by the
		// tier: the clause grants nothing.
		blob, _, err = reply(auth.ContextWithInternalOrigin(rankActorCtx(ids.stranger, auth.RoleWriter)),
			siteAccessCall(t, "query", "vendorKeyById", map[string]any{"keyId": ids.key}))
		require.NoError(t, err)
		require.NotContains(t, blob, ids.label, "internal origin alone handed a stranger somebody else's key")
	})
}

// S3: nothing subscribes to a key. A write publishes no graph event, and the
// per-row admission a stream's subscription passes through denies both
// concepts to everyone.
func TestVendorKeyEventsReachNoStream(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	previous := eng.EventBus()
	bus := events.NewBus()
	eng.SetEventBus(bus)
	t.Cleanup(func() { eng.SetEventBus(previous) })
	recorder := &busRecorder{}
	t.Cleanup(bus.Subscribe("#", recorder.record))

	ids := newVendorKeyRowIDs("events")
	keyStored, stateStored := seedVendorKey(t, eng, ids)
	owner := ids.asKeyOwner()
	for _, call := range []string{
		siteAccessCall(t, "mutation", "updateVendorKey", map[string]any{"keyId": ids.key, "autoUse": true}),
		siteAccessCall(t, "mutation", "setVendorKeySharing", map[string]any{"keyId": ids.key, "mode": "people", "userIds": []string{ids.borrower}}),
		siteAccessCall(t, "mutation", "updateVendorKeyState", map[string]any{
			"keyId": ids.key, "state": "credit", "evidenceAt": time.Now().UTC().Format(time.RFC3339), "detail": "out of credit",
		}),
		siteAccessCall(t, "mutation", "removeVendorKey", map[string]any{"keyId": ids.key, "removedBy": ids.owner}),
	} {
		_, err := eng.Execute(owner, call)
		require.NoError(t, err, call)
	}

	// THE CONTROL: a write to an ordinary concept publishes, so a silent bus
	// would mean a harness that hears nothing. It is written last, and its
	// arrival bounds how long the earlier events could still be in flight.
	seedGroup(t, eng, "g-"+ids.key, "Events", "account", "acct-"+ids.key)
	require.Eventually(t, func() bool { return recorder.seen("graph.node.created.v1:identity:group") },
		5*time.Second, 20*time.Millisecond, "the control write published no graph event, so this test cannot hear the bus")
	time.Sleep(300 * time.Millisecond)

	topics, bodies := recorder.snapshot()
	for _, topic := range topics {
		require.False(t, strings.HasPrefix(topic, "graph.") && strings.Contains(topic, "vendorKey"),
			"a write to a key published %s", topic)
	}
	for i, body := range bodies {
		for _, private := range []string{ids.sealed, ids.label, ids.fingerprint} {
			require.NotContains(t, body, private, "the event on %s carries a value of a key", topics[i])
		}
	}
	// The result cache and the other replicas still hear of the write, on the
	// channel that carries the concept's name and nothing of its rows.
	for _, name := range []string{vendorKeyConcept, vendorKeyStateConcept} {
		require.True(t, recorder.seen(events.TopicCacheInvalidateForConcept(name)),
			"no cache invalidation was published for %s, so a cached answer would outlive a removal", name)
	}

	// A subscription is a read: the admission every graph event passes on its
	// way to a stream denies both concepts to everyone, the owner included.
	ownerCanonical := "v1:identity:user:" + ids.owner
	rows := []struct {
		concept, id string
		payload     map[string]any
	}{
		{vendorKeyConcept, keyStored, map[string]any{"ownerUserId": ownerCanonical, "vendor": "anthropic", "label": ids.label}},
		{vendorKeyStateConcept, stateStored, map[string]any{"ownerUserId": ownerCanonical, "keyId": ids.key, "state": "good"}},
	}
	for _, row := range rows {
		raw, err := json.Marshal(row.payload)
		require.NoError(t, err)
		for _, caller := range ids.callers() {
			got := AdmitSubscriptionRow(eng.SubscriptionRankContext(context.Background()),
				&auth.AccessContext{UserId: caller.userID, Role: caller.role}, row.concept, row.id, raw)
			require.Equal(t, SubscriptionDeny, got, "%s would be sent a live %s event", caller.name, row.concept)
		}
		require.Equal(t, SubscriptionDeny,
			AdmitSubscriptionRow(eng.SubscriptionRankContext(context.Background()), nil, row.concept, row.id, raw))
	}
}
