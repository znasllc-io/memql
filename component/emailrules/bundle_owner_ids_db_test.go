package emailrules

// bundle_owner_ids_db_test.go -- a person's next email-rule bundle cannot be
// taken by anyone else, and a rule armed before bundle ids carried their
// owner still turns off and on (memql#6022, Task 3b fix round 4), against a
// REAL engine and a REAL Postgres: the @serverOnly gate, the bundle create
// guard, row authz and the gated activation.
//
// A rule's bundle id is derived, so the id an owner's next activation writes
// is known before it is written. When anyone could create a bundle, a second
// user could create one at that id first -- it was then theirs, and the
// owner's activation, writing to a row it did not own, was refused.
//
// Postgres-gated like the rest of this package's db tests: the ids are unique
// per run, and nothing is truncated.

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
)

// nextRuleBundleID is the id an owner's activation of a rule at a version
// writes.
func nextRuleBundleID(owner, ruleID string, version int) string {
	return bundleIDFor(owner, ruleID, version)
}

// oldRuleBundleID is the id the activation wrote before ids carried the
// owner: the rule and the version alone.
func oldRuleBundleID(ruleID string, version int) string {
	return legacyBundleIDFor(ruleID, version)
}

// ruleAuthor is one person with an email rule ready to arm.
type ruleAuthor struct {
	ctx   context.Context // their own envelope, role developer
	id    string          // their canonical user id
	rule  string          // the rule's id, as the store reads it back
	short string          // the rule's bare id
}

// seedRuleAuthor writes a person and, in an organization of theirs, a
// template and a draft operational rule on todos.
//
// The person is a developer: they may author, and they are not a cluster
// owner, whose composite tier would let their write take over a row someone
// else created -- the case where another person's row blocks them.
func seedRuleAuthor(t *testing.T, eng *memql.MemQLEngine, label string) ruleAuthor {
	t.Helper()
	ctx := context.Background()
	suffix := label + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	short, org, template, rule := "oid-user-"+suffix, "oid-org-"+suffix, "oid-tpl-"+suffix, "oid-rule-"+suffix
	q := langparser.QuoteString
	execute := func(ctx context.Context, query string) {
		t.Helper()
		if _, err := eng.Execute(ctx, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	system := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, auth.SystemActor("owner-ids-test")))
	system = auth.ContextWithToken(system, &auth.TokenInfo{Subject: "owner-ids-test"})
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"displayName":"Rule author","primaryEmail":"%s@example.test","role":"developer","active":true})`,
		q(short), short))

	id := "v1:identity:user:" + short
	owner := auth.ContextWithToken(auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: id, Role: auth.RoleDeveloper}), &auth.TokenInfo{Subject: id})
	execute(owner, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Owner ids organization")`, q(org)))
	execute(auth.ContextWithInternalOrigin(owner), fmt.Sprintf(`mutation createTemplate(versionTime: "2026-10-01T00:00:00Z", templateId: %s, name: "Owner ids template", subject: "A todo", textBody: "A todo was added.", accountId: %s)`,
		q(template), q(org)))
	execute(owner, fmt.Sprintf(`mutation createEmailRule(emailRuleId: %s, name: "Tell me about new todos", triggerConcept: "v1:todos:todo", eventKind: "created", templateId: %s, recipientMode: "cluster_roles", accountId: %s)`,
		q(rule), q(template), q(org)))

	read, found, err := NewStore(dbEngine{eng}).RuleByID(owner, rule)
	if err != nil || !found {
		t.Fatalf("read the seeded rule back (found %v): %v", found, err)
	}
	return ruleAuthor{ctx: owner, id: id, rule: read.ID, short: rule}
}

// registryDeps is a registry-only authored runtime: what an activation arms
// is read off the registry.
func registryDeps() (*memql.AuthoredRuntimeRegistry, func() memql.AuthoredRuntimeDeps) {
	registry := memql.NewAuthoredRuntimeRegistry()
	return registry, func() memql.AuthoredRuntimeDeps { return memql.AuthoredRuntimeDeps{Registry: registry} }
}

// storedBundle reads a bundle's latest version straight off the table, under
// either spelling of its id; nil when there is none.
func storedBundle(t *testing.T, db *bun.DB, bundleID string) map[string]any {
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

// armedFrom names the bundle the registry's entry for the rule's automation
// was armed from, "" when the registry holds none.
func armedFrom(registry *memql.AuthoredRuntimeRegistry, owner, ruleID string) string {
	if c, ok := registry.Lookup(owner, "automation", ConstructNameFor(ruleID)); ok {
		return memql.BareShortId(c.BundleId)
	}
	return ""
}

// TestASecondUserCannotTakeTheFirstUsersNextEmailRuleBundle: a second user
// knows the first user's rule and so the id their next activation writes.
// Creating a bundle at that id -- through the mutation or a raw insert -- is
// refused, before the first activation and before an edit's, and both of the
// first user's activations land at the ids derived for them. The second
// user's own rule arms at an id of their own, beside the first's.
func TestASecondUserCannotTakeTheFirstUsersNextEmailRuleBundle(t *testing.T) {
	r := openReplica(t)
	if r.eng == nil {
		return
	}
	first, second := seedRuleAuthor(t, r.eng, "first"), seedRuleAuthor(t, r.eng, "second")
	registry, deps := registryDeps()
	activator := NewActivator(EngineAdapter{Engine: r.eng}, deps)
	// Turned off however the test ends: every replica's reconcile arms every
	// active bundle in the shared database, so a rule left on fires in other
	// tests too.
	t.Cleanup(func() {
		_, _ = activator.Retire(first.ctx, first.id, first.short)
		_, _ = activator.Retire(second.ctx, second.id, second.short)
	})

	squat := func(bundleID string) {
		t.Helper()
		for _, c := range []struct{ how, call string }{
			{"through createAuthoringBundle", fmt.Sprintf(`mutation createAuthoringBundle(bundleId: %s, title: "Squatting")`, langparser.QuoteString(bundleID))},
			{"through a raw insert", fmt.Sprintf(`insert("v1:authoring:bundle", id=%s, payload={"title":"Squatting","status":"draft"})`, langparser.QuoteString(bundleID))},
		} {
			if _, err := r.eng.Execute(second.ctx, c.call); err == nil {
				t.Errorf("the second user created a bundle at the first user's next id %s, %s", bundleID, c.how)
			}
		}
		if row := storedBundle(t, r.db, bundleID); row != nil && memql.BareShortId(fmt.Sprint(row["ownerUserId"])) != memql.BareShortId(first.id) {
			t.Errorf("the first user's next bundle %s exists, owned by %v", bundleID, row["ownerUserId"])
		}
	}

	next := nextRuleBundleID(first.id, first.rule, 1)
	squat(next)
	armed, err := activator.Activate(first.ctx, first.id, first.short)
	if err != nil || armed.Status != "active" {
		t.Fatalf("the first user's activation after the second user's attempts: %+v, %v", armed, err)
	}
	if memql.BareShortId(armed.BundleID) != memql.BareShortId(next) {
		t.Fatalf("the first user's rule armed at %s, want the id derived for them, %s", armed.BundleID, next)
	}

	// An edit writes the next version's id, which is just as knowable.
	edit := nextRuleBundleID(first.id, first.rule, 2)
	squat(edit)
	edited, err := activator.Activate(first.ctx, first.id, first.short)
	if err != nil || edited.Status != "active" || memql.BareShortId(edited.BundleID) != memql.BareShortId(edit) {
		t.Fatalf("the first user's edit after the second user's attempts: %+v, %v (want active at %s)", edited, err, edit)
	}
	if got := armedFrom(registry, first.id, first.rule); got != memql.BareShortId(edit) {
		t.Fatalf("the registry runs the first user's rule from %q, want the edit %s", got, edit)
	}

	// The second user's own rule arms at an id of their own, and the two run
	// side by side.
	theirs, err := activator.Activate(second.ctx, second.id, second.short)
	if err != nil || theirs.Status != "active" {
		t.Fatalf("the second user's own rule: %+v, %v", theirs, err)
	}
	for _, mine := range []string{next, edit} {
		if memql.BareShortId(theirs.BundleID) == memql.BareShortId(mine) {
			t.Fatalf("the second user's rule armed at the first user's id %s", mine)
		}
	}
	if armedFrom(registry, first.id, first.rule) == "" || armedFrom(registry, second.id, second.rule) == "" {
		t.Fatal("the two rules do not both run")
	}
}

// armTheOldWay arms the rule as the activation did before ids carried their
// owner: its bundle at the rule-and-version id, through the same gated
// writes, recorded on the rule row.
func armTheOldWay(t *testing.T, eng *memql.MemQLEngine, deps func() memql.AuthoredRuntimeDeps, a ruleAuthor) string {
	t.Helper()
	store := NewStore(dbEngine{eng})
	rule, _, err := store.RuleByID(a.ctx, a.short)
	if err != nil {
		t.Fatal(err)
	}
	source, err := GenerateAutomation(rule)
	if err != nil {
		t.Fatal(err)
	}
	old := oldRuleBundleID(rule.ID, 1)
	internal := auth.ContextWithInternalOrigin(a.ctx)
	if err := store.CreateBundle(internal, old, BundleTitleFor(rule), summaryFor(rule), "", 1); err != nil {
		t.Fatalf("the old bundle: %v", err)
	}
	if err := store.CreateConstruct(internal, constructIDFor(old), old, ConstructNameFor(rule.ID), "campaigns", source); err != nil {
		t.Fatalf("the old construct: %v", err)
	}
	if err := store.RecordValidation(internal, old, true, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDryRun(internal, old, rule.ID, LaneFor(rule.RecipientMode)); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ActivateApprovedBundle(a.ctx, a.id, old, deps()); err != nil {
		t.Fatalf("the old activation: %v", err)
	}
	if err := store.RecordGeneration(internal, rule.ID, "active", old, ConstructNameFor(rule.ID), ""); err != nil {
		t.Fatal(err)
	}
	return old
}

// TestAnEmailRuleArmedUnderTheOldIdsTurnsOffAndOnAgain: a rule armed before
// bundle ids carried their owner keeps its bundle's id. Turning it off
// retires the bundle its row names; turning it on again arms the id derived
// for its owner. A rule edited while the old bundle runs supersedes it.
func TestAnEmailRuleArmedUnderTheOldIdsTurnsOffAndOnAgain(t *testing.T) {
	r := openReplica(t)
	if r.eng == nil {
		return
	}
	registry, deps := registryDeps()
	activator := NewActivator(EngineAdapter{Engine: r.eng}, deps)

	t.Run("off and on", func(t *testing.T) {
		a := seedRuleAuthor(t, r.eng, "offon")
		t.Cleanup(func() { _, _ = activator.Retire(a.ctx, a.id, a.short) }) // off however it ends: a rule left on is armed everywhere
		old := armTheOldWay(t, r.eng, deps, a)
		if got := armedFrom(registry, a.id, a.rule); got != memql.BareShortId(old) {
			t.Fatalf("setup: the registry runs the rule from %q, want the old bundle %s", got, old)
		}

		off, err := activator.Retire(a.ctx, a.id, a.short)
		if err != nil || off.Status != "paused" {
			t.Fatalf("turn off the rule armed under the old id: %+v, %v", off, err)
		}
		if st := storedBundle(t, r.db, old)["status"]; st != "retired" {
			t.Fatalf("turning it off left the old bundle %v, want retired", st)
		}
		if got := armedFrom(registry, a.id, a.rule); got != "" {
			t.Fatalf("turning it off left the rule running from %s", got)
		}

		on, err := activator.Activate(a.ctx, a.id, a.short)
		if err != nil || on.Status != "active" {
			t.Fatalf("turn it on again: %+v, %v", on, err)
		}
		want := nextRuleBundleID(a.id, a.rule, 1)
		if memql.BareShortId(on.BundleID) != memql.BareShortId(want) || memql.BareShortId(on.BundleID) == memql.BareShortId(old) {
			t.Fatalf("turned on again at %s, want the id derived for its owner, %s (the old one was %s)", on.BundleID, want, old)
		}
		if got := armedFrom(registry, a.id, a.rule); got != memql.BareShortId(want) {
			t.Fatalf("the registry runs the rule from %q, want %s", got, want)
		}
		if st := storedBundle(t, r.db, old)["status"]; st != "retired" {
			t.Fatalf("the old bundle reads %v after the rule came back, want retired", st)
		}
		if again, err := activator.Retire(a.ctx, a.id, a.short); err != nil || again.Status != "paused" {
			t.Fatalf("turn it off again: %+v, %v", again, err)
		}
	})

	t.Run("an edit supersedes the old bundle", func(t *testing.T) {
		a := seedRuleAuthor(t, r.eng, "edit")
		t.Cleanup(func() { _, _ = activator.Retire(a.ctx, a.id, a.short) }) // off however it ends: a rule left on is armed everywhere
		old := armTheOldWay(t, r.eng, deps, a)
		edited, err := activator.Activate(a.ctx, a.id, a.short)
		if err != nil || edited.Status != "active" {
			t.Fatalf("edit the rule armed under the old id: %+v, %v", edited, err)
		}
		want := nextRuleBundleID(a.id, a.rule, 2)
		if memql.BareShortId(edited.BundleID) != memql.BareShortId(want) {
			t.Fatalf("the edit armed at %s, want %s", edited.BundleID, want)
		}
		if st := storedBundle(t, r.db, old)["status"]; st != "retired" {
			t.Fatalf("the edit left the old bundle %v, want retired", st)
		}
		if got := armedFrom(registry, a.id, a.rule); got != memql.BareShortId(want) {
			t.Fatalf("the registry runs the rule from %q, want the edit %s", got, want)
		}
		if off, err := activator.Retire(a.ctx, a.id, a.short); err != nil || off.Status != "paused" {
			t.Fatalf("turn the edited rule off: %+v, %v", off, err)
		}
		if strings.TrimSpace(armedFrom(registry, a.id, a.rule)) != "" {
			t.Fatal("turning the edited rule off left it running")
		}
	})
}
