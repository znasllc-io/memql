package vendorkeys

import (
	"context"
	"sort"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// activity.go -- what a key paid for (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K14 and K15):
//
//	integration.vendorkeys.activity  this month's calls and cost (dsl builtin vendorKeyActivity)
//
// A key's owner lends it and is billed for what the borrowers do, so they are
// shown how much each person used it this month. The figures come from the
// call records (v1:router:call), which name the account that paid each call.
//
// AN ESTIMATE, AND IT SAYS SO. The cost is this cluster's own arithmetic, from
// each model's listed price and a token count that is itself often estimated.
// It is not the vendor's invoice. When a model that was called has no price
// listed here, its calls are counted and not costed, and `priced` is false so
// the screen can say the total is short.
//
// ONLY THE ROUTER'S ROWS COUNT. A call record can be written by a signed-in
// client, so the read keeps only the rows the router itself wrote
// (routerCallsPaidByKey). Nobody can put calls on another person's key.
//
// WHO THE CALLS BELONG TO is the record's `userId`: the person whose call it
// was. A borrower is named when they are still somebody the owner's directory
// names, and is "someone" otherwise, by id.

// activityPerson is one person's use of a key in the window.
type activityPerson struct {
	userId string
	calls  int
	cost   float64
}

// handleActivity sums what one of the caller's keys paid for this month.
func (i *Integration) handleActivity(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	me, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	key, found, err := i.store.Key(ctx, asString(args["keyId"]))
	if err != nil {
		return nil, i.failed("activity", memql.BareShortId(asString(args["keyId"])), err)
	}
	// The owner's alone, and a removed key's activity stays readable: what it
	// paid for is the question asked after it is gone.
	if !found || key.OwnerUserId != me.userId {
		return nil, errNoSuchKey()
	}

	now := i.store.now().UTC()
	since := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := i.store.callsPaidBy(ctx, key.Id, since)
	if err != nil {
		return nil, i.failed("activity", key.Id, err)
	}

	byPerson := map[string]*activityPerson{}
	var order []string
	total := activityPerson{}
	priced := true
	for _, row := range rows {
		user := memql.BareShortId(rowString(row, "userId"))
		p, seen := byPerson[user]
		if !seen {
			p = &activityPerson{userId: user}
			byPerson[user] = p
			order = append(order, user)
		}
		p.calls++
		total.calls++
		if !rowBool(row, "pricingConfigured") {
			priced = false
			continue
		}
		cost := rowFloat(row, "totalCost")
		p.cost += cost
		total.cost += cost
	}
	sort.SliceStable(order, func(a, b int) bool {
		pa, pb := byPerson[order[a]], byPerson[order[b]]
		if pa.cost != pb.cost {
			return pa.cost > pb.cost
		}
		if pa.calls != pb.calls {
			return pa.calls > pb.calls
		}
		return pa.userId < pb.userId
	})

	people := make([]any, 0, len(order))
	if len(order) > 0 {
		directory, derr := i.directory(ctx)
		for _, user := range order {
			p := byPerson[user]
			name := ""
			switch {
			case user == me.userId:
				name = i.store.UserName(ctx, user)
			case derr == nil:
				if entry, known := directory.Person(user); known {
					name = entry.Name
				}
			}
			people = append(people, map[string]any{
				"userId": p.userId, "name": name, "mine": user == me.userId,
				"calls": p.calls, "estimatedCost": roundCost(p.cost),
			})
		}
	}
	return i.node("activity:"+key.Id, map[string]any{
		"keyId":         key.Id,
		"month":         since.Format("2006-01"),
		"since":         since.Format(time.RFC3339),
		"calls":         total.calls,
		"estimatedCost": roundCost(total.cost),
		"priced":        priced,
		"people":        people,
	})
}

// roundCost keeps a sum of estimates from reading as more exact than it is:
// six decimal places of a dollar is finer than any price list.
func roundCost(v float64) float64 {
	const scale = 1e6
	if v < 0 {
		return 0
	}
	return float64(int64(v*scale+0.5)) / scale
}

func rowFloat(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}
