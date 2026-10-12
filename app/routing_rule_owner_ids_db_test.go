package app

// routing_rule_owner_ids_db_test.go -- a person's routing-rule bundle cannot be
// taken or blocked by anyone else (memql#6022, Task 3b fix round 4), against a
// REAL engine and a REAL Postgres, through the routing-rule engine adapter the
// App binds.
//
// A routing rule's bundle id is derived, and it used to be derived from the
// rule's name alone: the first person to arm a name -- or to create a bundle at
// its id -- owned that id, and everybody else's rule of that name was refused
// at the bundle write.
//
// WHAT THIS CHECKS IS THE BUNDLE WRITE, deliberately. Gate 1 refuses every
// generated routing rule today -- the bundle splitter it runs classifies no
// `rule` region, and createAuthoringConstruct's kind enum has no `rule` -- so
// no activation here reaches active. That defect predates this change and is
// reported with it. The bundle write comes BEFORE the gate (a refused gate is
// recorded on the bundle), so it is where another person's row blocked the
// owner, and it is what these assertions read; they hold unchanged once the
// gate accepts a rule.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/routingrules"
	"github.com/znasllc-io/memql/core/id"
)

// routingRuleAuthor is a person who may author: their envelope, and their
// user id in the spelling the routing-rule builtin hands the activator.
//
// A developer: they may author, and they are not a cluster owner, whose
// composite tier would let their write take over a row someone else created
// -- the case where another person's row blocks them.
type routingRuleAuthor struct {
	ctx   context.Context
	owner string
}

func newRoutingRuleAuthor() routingRuleAuthor {
	canonical := "v1:identity:user:" + id.NewShortId()
	ctx := auth.ContextWithToken(auth.ContextWithAccess(context.Background(),
		&auth.AccessContext{UserId: canonical, Role: auth.RoleDeveloper}), &auth.TokenInfo{Subject: canonical})
	// The builtin hands the activator the caller's BARE id
	// (integrations/router actingOwner).
	return routingRuleAuthor{ctx: ctx, owner: memql.BareShortId(canonical)}
}

func routingRuleForm(name string) routingrules.Form {
	return routingrules.Form{
		Name:          name,
		Description:   "Route my own agent replies.",
		When:          map[string]string{"prompt": "agentReply", "role": "operator"},
		Policy:        "localOnly",
		Level:         "reasoning",
		Precedence:    500,
		OnUnavailable: "park",
	}
}

// uniqueRuleName is a rule name no other run has used: rule names are bare
// identifiers, and the old ids were derived from the name alone.
func uniqueRuleName(label string) string {
	return label + strings.ReplaceAll(strconv.FormatInt(time.Now().UnixNano(), 36), "-", "")
}

// storedRoutingBundle reads a bundle's latest version straight off the
// table; nil when there is none.
func storedRoutingBundle(t *testing.T, db *bun.DB, bundleID string) map[string]any {
	t.Helper()
	var raw []byte
	err := db.QueryRowContext(context.Background(), `
		SELECT payload FROM "MemoryNodes"
		 WHERE concept = 'v1:authoring:bundle' AND (id = ? OR id = ?)
		 ORDER BY "createdAt" DESC LIMIT 1`,
		bundleID, "v1:authoring:bundle:"+memql.BareShortId(bundleID)).Scan(&raw)
	if err != nil {
		return nil
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode bundle %s: %v", bundleID, err)
	}
	return p
}

// TestASecondUserCannotTakeOrBlockTheFirstUsersRoutingRule: two people arm a
// routing rule of the same name, the second user first. Each one's bundle is
// written at an id of their own and is theirs -- the first user's write is
// not refused at the second user's row, and does not take that row over
// either, which under the name-only id the internal-origin gate recorder did.
// Before the first user arms another rule, the second user cannot create a
// bundle at the id it will have -- through the mutation or a raw insert --
// and that rule's bundle is then written there, the first user's.
func TestASecondUserCannotTakeOrBlockTheFirstUsersRoutingRule(t *testing.T) {
	engine, db := workTemplateDBEngineAndDB(t)
	registry := memql.NewAuthoredRuntimeRegistry()
	activator := routingrules.NewActivator(routingRuleEngine{engine: engine},
		func() memql.AuthoredRuntimeDeps { return memql.AuthoredRuntimeDeps{Registry: registry} },
		routingrules.EngineShippedNames{Engine: engine})
	first, second := newRoutingRuleAuthor(), newRoutingRuleAuthor()

	// written requires the rule's bundle to be at the id derived for its
	// owner, and to be theirs. The activation's own error is not the claim
	// (see the file header) unless it is the bundle write refused.
	written := func(who routingRuleAuthor, name string, res routingrules.Result, err error) {
		t.Helper()
		want := routingrules.BundleIdFor(who.owner, name)
		if err != nil && strings.Contains(err.Error(), "createAuthoringBundle") {
			t.Fatalf("%s's bundle write for %s was refused: %v", who.owner, name, err)
		}
		if memql.BareShortId(res.BundleId) != memql.BareShortId(want) {
			t.Fatalf("%s's rule %s wrote bundle %q, want the id derived for them, %q", who.owner, name, res.BundleId, want)
		}
		row := storedRoutingBundle(t, db, want)
		if row == nil || memql.BareShortId(fmt.Sprint(row["ownerUserId"])) != who.owner {
			t.Fatalf("%s's rule %s: bundle %s reads %v, want it written and owned by them", who.owner, name, want, row)
		}
	}

	// Retired however the test ends, once a rule can arm: a bundle left active
	// is armed by every later reconcile against this shared database. Through
	// the mutation as the author -- the activator's Retire hands the engine the
	// bare owner the builtin passes, which the reported defect makes refuse.
	retireWhenDone := func(who routingRuleAuthor, name string) {
		t.Cleanup(func() {
			id := routingrules.BundleIdFor(who.owner, name)
			if _, err := engine.Execute(who.ctx, fmt.Sprintf(`mutation retireAuthoringBundle(bundleId: %s)`, langparser.QuoteString(id))); err != nil {
				t.Logf("retire %s after the test: %v", id, err)
			}
		})
	}

	shared := uniqueRuleName("sharedRule")
	retireWhenDone(second, shared)
	retireWhenDone(first, shared)
	res, err := activator.Activate(second.ctx, second.owner, routingRuleForm(shared))
	written(second, shared, res, err)
	theirs := res
	res, err = activator.Activate(first.ctx, first.owner, routingRuleForm(shared))
	written(first, shared, res, err)
	written(second, shared, theirs, nil) // still theirs once the first user has armed the name
	if routingrules.BundleIdFor(first.owner, shared) == routingrules.BundleIdFor(second.owner, shared) {
		t.Fatalf("both users' rules %s derive one bundle id", shared)
	}

	// The id the first user's next rule will have is known before it is
	// written; the second user cannot create a bundle there.
	next := uniqueRuleName("nextRule")
	retireWhenDone(first, next)
	nextId := routingrules.BundleIdFor(first.owner, next)
	for _, c := range []struct{ how, call string }{
		{"through createAuthoringBundle", fmt.Sprintf(`mutation createAuthoringBundle(bundleId: %s, title: "Squatting")`, langparser.QuoteString(nextId))},
		{"through a raw insert", fmt.Sprintf(`insert("v1:authoring:bundle", id=%s, payload={"title":"Squatting","status":"draft"})`, langparser.QuoteString(nextId))},
	} {
		if _, err := engine.Execute(second.ctx, c.call); err == nil {
			t.Errorf("the second user created a bundle at the first user's next id %s, %s", nextId, c.how)
		}
	}
	if row := storedRoutingBundle(t, db, nextId); row != nil {
		t.Fatalf("the second user's refused writes left a bundle at the first user's next id: %v", row)
	}
	res, err = activator.Activate(first.ctx, first.owner, routingRuleForm(next))
	written(first, next, res, err)
}
