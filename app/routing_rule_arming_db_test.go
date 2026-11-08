package app

// routing_rule_arming_db_test.go -- a routing rule a person fills in is ARMED,
// and turned off again, through the path production runs, against a REAL
// engine and a REAL Postgres: the routingRuleActivate builtin, the router
// integration's handler, the app adapter, component/routingrules, the bundle
// and construct rows, Gate 1, the dry-run verdict and the activation; then
// routingRuleRetire.
//
// No generated routing rule had ever reached active (memql#6022 follow-up).
// Three defects stood in the way, each behind the one before it, so each
// showed only once the one in front of it was gone:
//
//   - Gate 1's bundle splitter (SplitBundleSource) sliced no `rule` region, so
//     every generated rule was refused as an unrecognized construct;
//   - createAuthoringConstruct's kind enum had no `rule`, so the construct row
//     could not be written with its true kind;
//   - the activation and retirement owner checks compared the bundle's stored
//     CANONICAL ownerUserId with the BARE id the builtin hands over
//     (integrations/router actingOwner), so a person was refused their own
//     bundle.
//
// The builtins are called through engine.Execute with the router integration
// registered and its rule activator installed, as app wiring does
// (engine_authored.go), so the owner reaches the gates in the spelling
// production uses rather than one a test chose.
//
// "Armed" here is what activation does: the bundle and its construct read
// active and the construct is registered in the authored runtime. Whether a
// call is then ROUTED by that rule is a different question, and this file does
// not answer it.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/routingrules"
	integrationsrouter "github.com/znasllc-io/memql/integrations/router"
)

// storedRoutingConstruct reads a construct's latest version straight off the
// table; nil when there is none.
func storedRoutingConstruct(t *testing.T, db *bun.DB, constructID string) map[string]any {
	t.Helper()
	var raw []byte
	err := db.QueryRowContext(context.Background(), `
		SELECT payload FROM "MemoryNodes"
		 WHERE concept = 'v1:authoring:construct' AND (id = ? OR id = ?)
		 ORDER BY "createdAt" DESC LIMIT 1`,
		constructID, "v1:authoring:construct:"+memql.BareShortId(constructID)).Scan(&raw)
	if err != nil {
		return nil
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode construct %s: %v", constructID, err)
	}
	return p
}

// storedBundleAudit is the latest audit event recording action on a bundle;
// nil when there is none.
func storedBundleAudit(t *testing.T, db *bun.DB, action, bundleID string) map[string]any {
	t.Helper()
	var raw []byte
	err := db.QueryRowContext(context.Background(), `
		SELECT payload FROM "MemoryNodes"
		 WHERE concept = 'v1:identity:auditEvent' AND payload->>'action' = ?
		   AND (payload->>'targetId' = ? OR payload->>'targetId' = ?)
		 ORDER BY "createdAt" DESC LIMIT 1`,
		action, bundleID, memql.BareShortId(bundleID)).Scan(&raw)
	if err != nil {
		return nil
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode audit event for %s: %v", bundleID, err)
	}
	return p
}

// armedRoutingRule is the authored-runtime entry for a rule name, or nil.
func armedRoutingRule(registry *memql.AuthoredRuntimeRegistry, name string) *memql.AuthoredConstruct {
	for _, c := range registry.ListAll() {
		if c.Kind == "rule" && c.Name == name && c.Status == memql.AuthoredActive {
			return c
		}
	}
	return nil
}

// TestARoutingRuleIsArmedAndRetiredThroughItsBuiltins: a developer arms a
// routing rule with routingRuleActivate and it reads active everywhere
// activation writes -- the bundle, its one construct, the authored runtime --
// owned by them. Somebody else cannot turn it off by naming it. The owner can:
// routingRuleRetire retires the bundle and takes the rule out of the runtime.
func TestARoutingRuleIsArmedAndRetiredThroughItsBuiltins(t *testing.T) {
	engine, db := workTemplateDBEngineAndDB(t)
	registry := memql.NewAuthoredRuntimeRegistry()
	// The deps production assembles (catalog promoter, audit sink), less the
	// scheduler hooks, which only an automation reaches.
	wiring := &App{engine: engine, Logger: engine.Logger, authoredRuntime: registry}
	routingrules.Bind(routingRuleEngine{engine: engine}, wiring.AuthoredRuntimeDeps,
		routingrules.EngineShippedNames{Engine: engine})
	t.Cleanup(func() { routingrules.Bind(nil, nil, nil) })
	router := integrationsrouter.New(engine, engine.Providers(), engine.Policies(), engine.Logger)
	router.SetRuleActivator(routingRuleActivatorAdapter{})
	if err := engine.RegisterIntegration(router); err != nil {
		t.Fatalf("register the router integration: %v", err)
	}

	author, stranger := newRoutingRuleAuthor(), newRoutingRuleAuthor()
	name := uniqueRuleName("armedRule")
	bundleID := routingrules.BundleIdFor(author.owner, name)
	constructID := "v1:authoring:construct:" + strings.TrimPrefix(bundleID, "v1:authoring:bundle:")

	// Not left active however the test ends: every later reconcile against
	// this shared database would arm it.
	t.Cleanup(func() {
		if row := storedRoutingBundle(t, db, bundleID); row != nil && row["status"] == "active" {
			if _, err := engine.Execute(author.ctx, fmt.Sprintf(`mutation retireAuthoringBundle(bundleId: %s)`, langparser.QuoteString(bundleID))); err != nil {
				t.Logf("retire %s after the test: %v", bundleID, err)
			}
		}
	})

	// No `when`: it is a keyword, so call text cannot name it, and arming does
	// not read the conditions. The rule this renders matches every call.
	activate := fmt.Sprintf(`builtin routingRuleActivate(name: %s, description: "Route my own agent replies.", `+
		`policy: "localOnly", level: "reasoning", precedence: 500, onUnavailable: "park")`, langparser.QuoteString(name))
	if _, err := engine.Execute(author.ctx, activate); err != nil {
		t.Fatalf("routingRuleActivate(%s) was refused: %v", name, err)
	}

	bundle := storedRoutingBundle(t, db, bundleID)
	if bundle == nil || bundle["status"] != "active" {
		t.Fatalf("after routingRuleActivate the bundle %s reads %v, want active", bundleID, bundle)
	}
	if memql.BareShortId(fmt.Sprint(bundle["ownerUserId"])) != author.owner {
		t.Fatalf("the bundle %s is owned by %v, want the author %s", bundleID, bundle["ownerUserId"], author.owner)
	}
	construct := storedRoutingConstruct(t, db, constructID)
	if construct == nil || construct["kind"] != "rule" || construct["name"] != name || construct["status"] != "active" {
		t.Fatalf("after routingRuleActivate the construct %s reads %v, want kind rule, name %s, status active", constructID, construct, name)
	}
	armed := armedRoutingRule(registry, name)
	if armed == nil {
		t.Fatalf("rule %s is not armed in the authored runtime: %+v", name, registry.ListAll())
	}
	if memql.BareShortId(armed.OwnerUserId) != author.owner || memql.BareShortId(armed.BundleId) != memql.BareShortId(bundleID) {
		t.Fatalf("rule %s is armed for owner %q from bundle %q, want the author %s and bundle %s",
			name, armed.OwnerUserId, armed.BundleId, author.owner, bundleID)
	}
	if ev := storedBundleAudit(t, db, memql.AuditActionBundleActivated, bundleID); ev == nil ||
		memql.BareShortId(fmt.Sprint(ev["actorUserId"])) != author.owner {
		t.Fatalf("the activation of %s was not audited as the author's: %v", bundleID, ev)
	}

	// Somebody else naming the rule reaches a bundle id derived for THEM, not
	// the author's: nothing of the author's is retired.
	if _, err := engine.Execute(stranger.ctx, fmt.Sprintf(`builtin routingRuleRetire(name: %s)`, langparser.QuoteString(name))); err == nil {
		t.Errorf("a second developer retired a rule named %s that they never armed", name)
	}
	if row := storedRoutingBundle(t, db, bundleID); row == nil || row["status"] != "active" {
		t.Fatalf("after a second developer's retire the author's bundle %s reads %v, want still active", bundleID, row)
	}
	if armedRoutingRule(registry, name) == nil {
		t.Fatalf("a second developer's retire took the author's rule %s out of the authored runtime", name)
	}

	if _, err := engine.Execute(author.ctx, fmt.Sprintf(`builtin routingRuleRetire(name: %s)`, langparser.QuoteString(name))); err != nil {
		t.Fatalf("routingRuleRetire(%s) by its author was refused: %v", name, err)
	}
	if row := storedRoutingBundle(t, db, bundleID); row == nil || row["status"] != "retired" {
		t.Fatalf("after routingRuleRetire the bundle %s reads %v, want retired", bundleID, row)
	}
	if armedRoutingRule(registry, name) != nil {
		t.Fatalf("after routingRuleRetire rule %s is still armed in the authored runtime", name)
	}
	if ev := storedBundleAudit(t, db, memql.AuditActionBundleRetired, bundleID); ev == nil ||
		memql.BareShortId(fmt.Sprint(ev["actorUserId"])) != author.owner {
		t.Fatalf("the retirement of %s was not audited as the author's: %v", bundleID, ev)
	}
}
