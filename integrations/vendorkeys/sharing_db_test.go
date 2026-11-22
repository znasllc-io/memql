package vendorkeys

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// sharing_db_test.go -- lending a key, and what it paid for (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K6, K14 and K15, safety rule S7).

func (d *desk) share(ctx context.Context, keyId any, args map[string]any) (map[string]any, error) {
	d.t.Helper()
	args["keyId"] = keyId
	nodes, err := d.in.handleSetSharing(ctx, args, 0)
	if err != nil {
		return nil, err
	}
	return cardOf(d.t, nodes), nil
}

func (d *desk) sharingOf(keyId string) map[string]any {
	d.t.Helper()
	sharing, _ := d.latest(keyConcept, keyId)["sharing"].(map[string]any)
	return sharing
}

func bareList(v any) []string {
	out := []string{}
	list, _ := v.([]any)
	for _, item := range list {
		out = append(out, memql.BareShortId(fmt.Sprint(item)))
	}
	return out
}

// K6: a person a key is lent to pays with it on their next call.
func TestLendingToAPersonAdmitsThemOnTheNextCall(t *testing.T) {
	d := newDesk(t, "lend")
	dana := d.member("dana", auth.RoleDeveloper)
	wes := d.member("wes", auth.RoleWriter)
	ctx := session(dana, auth.RoleDeveloper)
	key := d.mustAdd(ctx, "Dana's")
	keyId := key["id"].(string)
	require.Empty(t, d.keysFor(d.resolver, wes, true).usable)

	// The owner names herself too. She is dropped: her key is hers already.
	card, err := d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": []any{wes, dana, "v1:identity:user:" + wes}})
	require.NoError(t, err)
	require.EqualValues(t, 1, card["lentTo"])
	sharing := d.sharingOf(keyId)
	require.Equal(t, "people", sharing["mode"])
	require.Equal(t, []string{wes}, bareList(sharing["userIds"]))
	require.Equal(t, dana, memql.BareShortId(fmt.Sprint(sharing["sharedBy"])))

	require.Equal(t, []string{keyId}, d.keysFor(d.resolver, wes, true).usable, "a person a key was lent to cannot pay with it")
	require.Empty(t, d.keysFor(d.resolver, wes, false).usable, "a lent key paid under Auto before its owner switched Auto on")
	require.Equal(t, []string{auditAdded, auditLent}, d.auditActions(keyId))

	// Saving the same audience again is not a second lending.
	_, err = d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": []any{wes}})
	require.NoError(t, err)
	require.Equal(t, []string{auditAdded, auditLent}, d.auditActions(keyId))
}

// K6: one person can be paused without anybody else being touched, and the
// whole list can be.
func TestPausingOnePersonStopsOnlyThem(t *testing.T) {
	d := newDesk(t, "pause")
	dana := d.member("dana", auth.RoleDeveloper)
	wes, sam := d.member("wes", auth.RoleWriter), d.member("sam", auth.RoleWriter)
	ctx := session(dana, auth.RoleDeveloper)
	keyId := d.mustAdd(ctx, "Dana's")["id"].(string)
	both := []any{wes, sam}
	_, err := d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": both})
	require.NoError(t, err)

	_, err = d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": both, "pausedUserIds": []any{wes}})
	require.NoError(t, err)
	paused := d.keysFor(d.resolver, wes, true)
	require.Empty(t, paused.usable)
	require.Equal(t, "an API key's owner has paused lending it", paused.passed[keyId])
	require.Equal(t, []string{keyId}, d.keysFor(d.resolver, sam, true).usable, "pausing one person stopped another")

	_, err = d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": both})
	require.NoError(t, err)
	require.Equal(t, []string{keyId}, d.keysFor(d.resolver, wes, true).usable)

	// Everybody at once. The list is kept, and left out of a later save the
	// pause stays as it was.
	_, err = d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": both, "paused": true})
	require.NoError(t, err)
	_, err = d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": both})
	require.NoError(t, err)
	require.Equal(t, true, d.sharingOf(keyId)["paused"])
	require.Empty(t, d.keysFor(d.resolver, sam, true).usable)
	require.Equal(t, []string{keyId}, d.keysFor(d.resolver, dana, true).usable, "pausing the people a key is lent to stopped its owner")
	_, err = d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": both, "paused": false})
	require.NoError(t, err)

	require.Equal(t, []string{auditAdded, auditLent, auditLendingPaused, auditLendingResumed, auditLendingPaused, auditLendingResumed},
		d.auditActions(keyId))

	// Somebody who is not on the list cannot be paused on it.
	_, err = d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": []any{sam}, "pausedUserIds": []any{wes}})
	require.Error(t, err)
}

// K6: taking a key back leaves no names and no pause behind.
func TestStoppingLendingClearsTheLists(t *testing.T) {
	d := newDesk(t, "stop")
	dana := d.member("dana", auth.RoleDeveloper)
	wes := d.member("wes", auth.RoleWriter)
	ctx := session(dana, auth.RoleDeveloper)
	keyId := d.mustAdd(ctx, "Dana's")["id"].(string)
	_, err := d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": []any{wes}, "pausedUserIds": []any{wes}, "paused": true})
	require.NoError(t, err)

	// The lists are sent again, as a careless client would. They are not kept.
	card, err := d.share(ctx, keyId, map[string]any{"mode": "owner", "userIds": []any{wes}, "paused": true})
	require.NoError(t, err)
	require.EqualValues(t, 0, card["lentTo"])
	sharing := d.sharingOf(keyId)
	require.Equal(t, "owner", sharing["mode"])
	require.Equal(t, false, sharing["paused"])
	for _, list := range []string{"userIds", "groupIds", "pausedUserIds", "pausedGroupIds"} {
		require.Empty(t, sharing[list], "%s survived the key being taken back", list)
	}
	got := d.keysFor(d.resolver, wes, true)
	require.Empty(t, got.usable)
	require.Empty(t, got.passed, "a key that is no longer lent to somebody is not theirs to hear about")
	actions := d.auditActions(keyId)
	require.Equal(t, auditLendingStopped, actions[len(actions)-1])

	// A key cannot be lent to everyone.
	_, err = d.share(ctx, keyId, map[string]any{"mode": "cluster"})
	require.Error(t, err)
	require.Equal(t, "owner", d.sharingOf(keyId)["mode"])
}

// K6: only somebody the owner is offered can be added, an unknown id and a
// person who is not theirs to pick are one answer, and somebody already on the
// list stays there until the owner takes them off.
func TestOnlyAnOfferedSubjectCanBeAdded(t *testing.T) {
	d := newDesk(t, "offered")
	dana := d.member("dana", auth.RoleDeveloper)
	wes := d.member("wes", auth.RoleWriter)
	gone := d.member("gone", auth.RoleWriter)
	ctx := session(dana, auth.RoleDeveloper)
	keyId := d.mustAdd(ctx, "Dana's")["id"].(string)

	_, err := d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": []any{wes, gone}})
	require.NoError(t, err)

	d.writeUser(gone, false, "Gone")
	_, unknownErr := d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": []any{wes, gone, "vk-nobody-at-all"}})
	require.Error(t, unknownErr)
	inactive := d.member("inactive", auth.RoleWriter)
	d.writeUser(inactive, false)
	_, inactiveErr := d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": []any{wes, inactive}})
	require.Error(t, inactiveErr)
	_, groupErr := d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": []any{wes}, "groupIds": []any{"vk-no-such-group"}})
	require.Error(t, groupErr)
	require.Equal(t, unknownErr.Error(), inactiveErr.Error(), "the refusal tells a made-up id from a person who cannot be picked")
	require.Equal(t, unknownErr.Error(), groupErr.Error())

	// Already on the list, and no longer offered: kept until she takes them off.
	_, err = d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": []any{wes, gone}})
	require.NoError(t, err, "a save was refused because of somebody already on the list")
	require.ElementsMatch(t, []string{wes, gone}, bareList(d.sharingOf(keyId)["userIds"]))

	// The directory names both, and says which is no longer offered.
	nodes, err := d.in.handleShareDirectory(ctx, map[string]any{"keyId": keyId}, 0)
	require.NoError(t, err)
	directory := cardOf(t, nodes)
	current := map[string]map[string]any{}
	for _, item := range directory["currentPeople"].([]any) {
		entry := item.(map[string]any)
		current[entry["id"].(string)] = entry
	}
	require.Equal(t, true, current[wes]["inDirectory"])
	require.Equal(t, false, current[gone]["inDirectory"])
	require.Equal(t, "Gone", current[gone]["name"], "somebody on the list is not named once they stop being offered")
	offered := map[string]bool{}
	for _, item := range directory["people"].([]any) {
		offered[item.(map[string]any)["id"].(string)] = true
	}
	require.True(t, offered[wes])
	require.False(t, offered[dana], "the owner is offered her own key")
	require.False(t, offered[gone])

	// Too many.
	many := make([]any, 0, memql.ShareSubjectsMax+1)
	for n := 0; n <= memql.ShareSubjectsMax; n++ {
		many = append(many, fmt.Sprintf("vk-crowd-%d", n))
	}
	_, err = d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": many})
	require.Error(t, err)
	require.Contains(t, err.Error(), fmt.Sprint(memql.ShareSubjectsMax))
}

// K5: a cluster owner can stop a key and cannot decide who spends it.
func TestAClusterOwnerCannotLendAnotherPersonsKey(t *testing.T) {
	d := newDesk(t, "operator-lend")
	dana := d.member("dana", auth.RoleDeveloper)
	omar := d.member("omar", auth.RoleOwner)
	wes := d.member("wes", auth.RoleWriter)
	keyId := d.mustAdd(session(dana, auth.RoleDeveloper), "Dana's")["id"].(string)
	operator := session(omar, auth.RoleOwner)

	_, err := d.share(operator, keyId, map[string]any{"mode": "people", "userIds": []any{wes, omar}})
	require.Error(t, err)
	_, unknown := d.share(operator, "vk-key-never-minted", map[string]any{"mode": "people", "userIds": []any{wes}})
	require.Equal(t, unknown.Error(), err.Error(), "a cluster owner can tell somebody's key from an id that was never minted")
	require.Equal(t, "owner", d.sharingOf(keyId)["mode"])

	for name, act := range map[string]func() error{
		"read who it is lent to": func() error {
			_, err := d.in.handleShareDirectory(operator, map[string]any{"keyId": keyId}, 0)
			return err
		},
		"read what it paid for": func() error {
			_, err := d.in.handleActivity(operator, map[string]any{"keyId": keyId}, 0)
			return err
		},
	} {
		require.Error(t, act(), "a cluster owner could %s", name)
	}
}

// Lending decides who spends a person's money, so it is that person's own
// session or nothing.
func TestLendingNeedsThePersonsOwnSession(t *testing.T) {
	d := newDesk(t, "lend-session")
	dana := d.member("dana", auth.RoleDeveloper)
	wes := d.member("wes", auth.RoleWriter)
	keyId := d.mustAdd(session(dana, auth.RoleDeveloper), "Dana's")["id"].(string)

	for name, ctx := range map[string]context.Context{
		"a personal access token": sessionWith(dana, auth.RoleDeveloper, map[string]any{"pat_id": "pat-1"}),
		"a service account":       sessionWith(dana, auth.RoleDeveloper, map[string]any{"class": "service_account"}),
		"a step of a run":         common.ContextWithRun(session(dana, auth.RoleDeveloper), common.RunContext{RunId: "v1:work:run:r1", StepKey: "s1"}),
		"borrowed authority":      auth.ContextWithUserActor(context.Background(), dana),
	} {
		_, err := d.share(ctx, keyId, map[string]any{"mode": "people", "userIds": []any{wes}})
		require.Error(t, err, "%s lent a key", name)
	}
	require.Equal(t, "owner", d.sharingOf(keyId)["mode"])
}

// recordCall writes one call record the way the router's ledger writer does.
func (d *desk) recordCall(by context.Context, user, keyId, owner string, cost float64, priced bool, outcome string) {
	d.t.Helper()
	call, err := langparser.RenderCall("recordRouterCall", map[string]any{
		"callId": id.NewShortId(), "requestId": "req-" + id.NewShortId(), "userId": user, "callerKind": "user",
		"vendor": "anthropic", "model": "claude-test", "providerName": "claude", "totalDurationMs": 12,
		"outcome": outcome, "totalCost": cost, "pricingConfigured": priced, "billing": "metered",
		"accountKind": "key", "accountKeyId": keyId, "accountKeyOwnerUserId": owner,
	})
	require.NoError(d.t, err)
	_, err = d.eng.Execute(by, "mutation "+call)
	require.NoError(d.t, err)
}

// asTheRouter is the context the router's ledger writer runs under.
func asTheRouter() context.Context {
	return auth.ContextWithToken(context.Background(), &auth.TokenInfo{
		Subject: "system:router", Claims: map[string]any{"sub": "system:router"},
	})
}

func (d *desk) activity(ctx context.Context, keyId string) map[string]any {
	d.t.Helper()
	nodes, err := d.in.handleActivity(ctx, map[string]any{"keyId": keyId}, 0)
	require.NoError(d.t, err)
	d.t.Cleanup(func() {
		_, _ = d.db.NewDelete().TableExpr(`"MemoryNodes"`).
			Where("concept = ?", "v1:router:call").Where("payload->>'accountKeyId' LIKE ?", "%"+keyId).Exec(context.Background())
	})
	return cardOf(d.t, nodes)
}

// K15: the owner of a key sees how much each person used it this month.
func TestActivityCountsThisMonthPerPerson(t *testing.T) {
	d := newDesk(t, "activity")
	dana := d.member("dana", auth.RoleDeveloper)
	wes, sam := d.member("wes", auth.RoleWriter), d.member("sam", auth.RoleWriter)
	ctx := session(dana, auth.RoleDeveloper)
	keyId := d.mustAdd(ctx, "Dana's")["id"].(string)
	other := d.mustAdd(ctx, "Other")["id"].(string)

	router := asTheRouter()
	d.recordCall(router, wes, keyId, dana, 0.25, true, "ok")
	d.recordCall(router, wes, keyId, dana, 0.50, true, "ok")
	d.recordCall(router, dana, keyId, dana, 0.10, true, "ok")
	d.recordCall(router, sam, keyId, dana, 0.05, true, "ok")
	// Not counted: a failed attempt, a call another key paid for, and a row a
	// person wrote themselves naming this key.
	d.recordCall(router, wes, keyId, dana, 9.00, true, "error")
	d.recordCall(router, wes, other, dana, 9.00, true, "ok")
	d.recordCall(session(sam, auth.RoleWriter), sam, keyId, dana, 99.00, true, "ok")

	got := d.activity(ctx, keyId)
	d.activity(ctx, other)
	require.EqualValues(t, 4, got["calls"])
	require.InDelta(t, 0.90, got["estimatedCost"], 1e-9, "a call that failed, another key's call, or a row a person wrote was counted")
	require.Equal(t, true, got["priced"])
	require.Equal(t, time.Now().UTC().Format("2006-01"), got["month"])

	raw, _ := json.Marshal(got["people"])
	var people []struct {
		UserId        string  `json:"userId"`
		Name          string  `json:"name"`
		Mine          bool    `json:"mine"`
		Calls         int     `json:"calls"`
		EstimatedCost float64 `json:"estimatedCost"`
	}
	require.NoError(t, json.Unmarshal(raw, &people))
	require.Len(t, people, 3)
	require.Equal(t, wes, people[0].UserId, "the person who spent the most is not first")
	require.Equal(t, 2, people[0].Calls)
	require.InDelta(t, 0.75, people[0].EstimatedCost, 1e-9)
	require.Equal(t, "Wes", people[0].Name)
	require.Equal(t, dana, people[1].UserId)
	require.True(t, people[1].Mine)
	require.Equal(t, sam, people[2].UserId)

	// Last month's calls are not this month's: seen from next month, nothing.
	d.clock = time.Now().UTC().AddDate(0, 1, 5)
	next := d.activity(ctx, keyId)
	require.EqualValues(t, 0, next["calls"])
	require.EqualValues(t, 0, next["estimatedCost"])

	// Only the owner reads it. A borrower who used the key does not.
	_, err := d.in.handleActivity(session(wes, auth.RoleWriter), map[string]any{"keyId": keyId}, 0)
	require.Error(t, err)
}

// K14: a model with no price listed here is counted and not costed, and the
// answer says the total is short.
func TestActivityOfAnUnpricedModelSaysNotPriced(t *testing.T) {
	d := newDesk(t, "unpriced")
	dana := d.member("dana", auth.RoleDeveloper)
	ctx := session(dana, auth.RoleDeveloper)
	keyId := d.mustAdd(ctx, "Dana's")["id"].(string)

	router := asTheRouter()
	d.recordCall(router, dana, keyId, dana, 0.40, true, "ok")
	d.recordCall(router, dana, keyId, dana, 0, false, "ok")

	got := d.activity(ctx, keyId)
	require.EqualValues(t, 2, got["calls"])
	require.InDelta(t, 0.40, got["estimatedCost"], 1e-9)
	require.Equal(t, false, got["priced"], "a total that leaves out an unpriced model's calls reads as complete")

	// A removed key's activity stays readable.
	_, err := d.in.handleRemove(ctx, map[string]any{"keyId": keyId}, 0)
	require.NoError(t, err)
	require.EqualValues(t, 2, d.activity(ctx, keyId)["calls"])
}
