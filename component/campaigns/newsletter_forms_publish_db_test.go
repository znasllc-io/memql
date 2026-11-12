package campaigns

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// TestASignupFormThatTurnsTheFormsOnAsksPublishInWords: connecting a signup
// form turns the deployable's public forms on when they are off, and changing
// that switch on an existing deployable asks the deployables publish part
// (siteFieldParts, component/memql/organization_action_capabilities.go). A
// member of the organization who may send its email and read its deployables,
// but not publish them, was refused by the engine in its own words --
// "mutation updateSiteShopperForms: capability_not_held: ..." -- which is what
// the Email app's Connect a signup form shows (refusalSentence takes the frames
// off and leaves the rest). The save asks first, says why, and writes nothing.
//
// The member is a real one: a writer tied to the organization by an account
// group, resolved by the engine's own grant resolution, holding update on data
// and read on app:deployables and, like every role below developer, no
// app:deployables/publish (dsl/rbac/seeds.memql).
func TestASignupFormThatTurnsTheFormsOnAsksPublishInWords(t *testing.T) {
	f := newDigestFixture(t)
	d := f.newDigestTenant(t, "forms-publish")
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	d.engine.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	member, group := d.suffix+"-member", d.suffix+"-group"
	d.execute(t, d.system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Member","role":%s,"active":true})`,
		langparser.QuoteString(member), langparser.QuoteString(member+"@example.test"), langparser.QuoteString(string(auth.RoleWriter))))
	d.execute(t, d.system, fmt.Sprintf(`mutation writeGroup(groupId: %s, name: %s, kind: "account", accountId: %s, status: "active")`,
		langparser.QuoteString(group), langparser.QuoteString(group), langparser.QuoteString(d.org)))
	d.execute(t, d.system, fmt.Sprintf(`mutation writeGroupMembership(membershipId: %s, groupId: %s, userId: %s, origin: "added", status: "active", accountId: %s)`,
		langparser.QuoteString(group+"-"+member), langparser.QuoteString(group), langparser.QuoteString(member), langparser.QuoteString(d.org)))
	memberCtx := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: member, Role: auth.RoleWriter}), &auth.TokenInfo{Subject: member})

	worker := newGatedWorker(t, d.engine, d.db, &recordingSender{})
	site := func(name string) string {
		id := d.suffix + "-" + name
		d.execute(t, d.owner, call("mutation", "createSite", arg{"siteId", id}, arg{"accountId", d.org}, arg{"hostname", id + ".example.test"}, arg{"bundleRef", "blob://sites/" + id + "/v1/"}, arg{"kind", "static"}))
		return id
	}
	connect := func(siteID string) error {
		_, err := worker.handleConfigureNewsletter(memberCtx, map[string]any{"siteId": siteID, "audienceId": d.audience, "consentText": "Yes, email me news and offers. I can unsubscribe anytime.", "enabled": true}, 0)
		return err
	}
	stored := func(query, siteID string) map[string]any {
		row, err := worker.newsletterRow(digestOwner(d.user), query, "siteId", siteID)
		if err != nil {
			t.Fatalf("%s %s: %v", query, siteID, err)
		}
		return row
	}

	off := site("forms-off")
	err := connect(off)
	if err == nil {
		t.Fatal("a member who cannot publish the deployable turned its public forms on")
	}
	t.Logf("the refusal a person reads: %v", err)
	if !strings.Contains(err.Error(), "permission to publish") || strings.Contains(err.Error(), memql.CodeCapabilityNotHeld) || strings.Contains(err.Error(), "mutation") {
		t.Fatalf("refused, but not in words a person can act on: %v", err)
	}
	if booleanOr(stored("siteById", off), "shopperForms", false) {
		t.Fatal("the refused save still turned the forms on")
	}
	if stored("newsletterForSite", off) != nil {
		t.Fatal("the refused save still wrote the newsletter")
	}

	// Control: with the forms already on, connecting changes nothing a visitor
	// gets, so the same member needs no publish part.
	on := site("forms-on")
	d.execute(t, d.owner, call("mutation", "updateSiteShopperForms", arg{"siteId", on}, arg{"shopperForms", true}))
	if err := connect(on); err != nil {
		t.Fatalf("a member was refused a signup form on a deployable whose forms are already on: %v", err)
	}
	if stored("newsletterForSite", on) == nil {
		t.Fatal("the admitted save wrote no newsletter")
	}
}
