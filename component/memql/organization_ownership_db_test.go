package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func organizationInsert(t *testing.T, e *MemQLEngine, ctx context.Context, concept, id string, payload map[string]any) error {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Execute(ctx, fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, langparser.QuoteString(concept), langparser.QuoteString(id), encoded))
	return err
}

func TestOrganizationPersistedAttributionAndCrossReplicaMembership(t *testing.T) {
	engine, db, _ := sharedReadMergeEngine(t)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	engine.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })
	suffix := uniqueSuffix("org")
	acme, beta := "acme-"+suffix, "beta-"+suffix
	member, multi, operator := "member-"+suffix, "multi-"+suffix, "operator-"+suffix
	seedPrincipal(t, engine, member, auth.RoleWriter)
	seedPrincipal(t, engine, multi, auth.RoleWriter)
	seedPrincipal(t, engine, operator, auth.RoleOwner)
	for _, account := range []string{acme, beta, "self"} {
		if err := organizationInsert(t, engine, groupSeedCtx(), conceptAccountsAccount, account, map[string]any{"name": account, "status": "active", "domainStatus": "unverified"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, account := range []string{acme, beta} {
		seedGroup(t, engine, "g-"+account, account, "account", account)
	}
	seedMembership(t, engine, "g-"+acme, member, "active")
	seedMembership(t, engine, "g-"+acme, multi, "active")
	seedMembership(t, engine, "g-"+beta, multi, "active")
	for _, part := range []string{"deploy", "publish"} {
		writeGrant(t, engine, auth.SubjectKindGroup, "g-"+acme, auth.VerbExecute, "app:deployables/"+part, auth.GrantAllow)
	}
	actor := rankActorCtx(member, auth.RoleWriter)
	// Actual raw writes exercise defaulting, owner stamping, schema validation
	// and persistence, not just a helper returning a guessed account id.
	cases := []struct {
		concept string
		payload map[string]any
	}{
		{"v1:campaigns:audience", map[string]any{"name": "Audience", "status": "active"}},
		{"v1:campaigns:template", map[string]any{"name": "Template", "subject": "Subject", "textBody": "Hello", "status": "ready"}},
		{"v1:campaigns:senderIdentity", map[string]any{"address": "news@example.test", "fromName": "News", "status": "active"}},
		{"v1:platform:package", map[string]any{"name": "Package", "sourceKind": "repo", "status": "active"}},
		{"v1:platform:site", map[string]any{"hostname": "site-" + suffix + "." + siteHostnamePolicyDomain(), "kind": "spa", "bundleRef": "blob://site/test", "status": "live"}},
	}
	for _, tc := range cases {
		id := strings.ReplaceAll(tc.concept, ":", "-") + "-" + suffix
		tc.payload["ownerUserId"] = "forged-other-user"
		if err := organizationInsert(t, engine, actor, tc.concept, id, tc.payload); err != nil {
			t.Fatalf("%s: %v", tc.concept, err)
		}
		var rows []memorynodes.MemoryNode
		if err := db.NewSelect().Model(&rows).Where("concept = ?", tc.concept).Where("id = ?", tc.concept+":"+id).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s was not persisted", tc.concept)
		}
		p := accountRowPayload(rows[0])
		if BareShortId(stringFromAny(p["accountId"])) != acme || BareShortId(stringFromAny(p["ownerUserId"])) != member || BareShortId(rows[0].CreatedBy) != member {
			t.Fatalf("%s incorrect persisted attribution: %v", tc.concept, p)
		}
	}
	audience := "v1-campaigns-audience-" + suffix
	template := "v1-campaigns-template-" + suffix
	campaign := "campaign-" + suffix
	// Derived rows persist the same account and original person while rejecting
	// a supplied organization that disagrees with their authoritative parent.
	childRows := []struct {
		concept, id string
		payload     map[string]any
	}{
		{"v1:campaigns:emailRule", "rule-" + suffix, map[string]any{"name": "Rule", "triggerConcept": "v1:campaigns:recipient", "eventKind": "created", "templateId": template, "audienceId": audience, "recipientMode": "audience", "status": "draft"}},
		{"v1:campaigns:recipient", "recipient-" + suffix, map[string]any{"audienceId": audience, "email": "recipient@example.test", "subscriptionStatus": "subscribed", "source": "manual"}},
		{"v1:platform:packageDeployment", "run-" + suffix, map[string]any{"packageId": "v1-platform-package-" + suffix, "status": "analyzing"}},
	}
	for _, child := range childRows {
		if err := organizationInsert(t, engine, actor, child.concept, child.id, child.payload); err != nil {
			t.Fatalf("child %s: %v", child.concept, err)
		}
		var rows []memorynodes.MemoryNode
		if err := db.NewSelect().Model(&rows).Where("concept = ?", child.concept).Where("id = ?", child.concept+":"+child.id).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || BareShortId(stringFromAny(accountRowPayload(rows[0])["accountId"])) != acme || BareShortId(rows[0].CreatedBy) != member {
			t.Fatalf("child attribution not persisted: %s", child.concept)
		}
		if decl := rowAuthzDeclFor(child.concept); decl != nil && decl.Owner != "" && BareShortId(stringFromAny(accountRowPayload(rows[0])[decl.Owner])) != member {
			t.Fatalf("child owning user not persisted: %s", child.concept)
		}
	}

	if _, err := engine.Execute(actor, fmt.Sprintf(`mutation createCampaign(campaignId: %s, name: "Campaign", audienceId: %s, templateId: %s)`, langparser.QuoteString(campaign), langparser.QuoteString(audience), langparser.QuoteString(template))); err != nil {
		t.Fatal(err)
	}
	namedSite := "named-site-" + suffix
	if _, err := engine.Execute(actor, fmt.Sprintf(`mutation createSite(siteId: %s, hostname: %s, bundleRef: "blob://named/test", accountId: %s)`, langparser.QuoteString(namedSite), langparser.QuoteString(namedSite+"."+siteHostnamePolicyDomain()), langparser.QuoteString(acme))); err != nil {
		t.Fatal(err)
	}
	for concept, id := range map[string]string{"v1:campaigns:campaign": campaign, "v1:platform:site": namedSite} {
		var rows []memorynodes.MemoryNode
		if err := db.NewSelect().Model(&rows).Where("concept = ?", concept).Where("id = ?", concept+":"+id).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("named %s not persisted", concept)
		}
		p := accountRowPayload(rows[0])
		if BareShortId(stringFromAny(p["accountId"])) != acme || BareShortId(stringFromAny(p["ownerUserId"])) != member || BareShortId(rows[0].CreatedBy) != member {
			t.Fatalf("named %s attribution failed: %v", concept, p)
		}
	}
	if err := organizationInsert(t, engine, rankActorCtx(operator, auth.RoleOwner), "v1:campaigns:audience", "operator-audience-"+suffix, map[string]any{"name": "Default", "ownerUserId": operator}); err != nil {
		t.Fatal(err)
	}
	var own []memorynodes.MemoryNode
	if err := db.NewSelect().Model(&own).Where("concept = ?", "v1:campaigns:audience").Where("id = ?", "v1:campaigns:audience:operator-audience-"+suffix).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 || BareShortId(stringFromAny(accountRowPayload(own[0])["accountId"])) != "self" {
		t.Fatal("operator default not persisted as self")
	}
	if err := organizationInsert(t, engine, rankActorCtx(multi, auth.RoleWriter), "v1:campaigns:audience", "ambiguous-"+suffix, map[string]any{"name": "Ambiguous"}); err == nil {
		t.Fatal("multiple memberships silently defaulted")
	}
	if err := organizationInsert(t, engine, rankActorCtx(multi, auth.RoleWriter), "v1:campaigns:audience", "explicit-"+suffix, map[string]any{"name": "Explicit", "accountId": beta}); err != nil {
		t.Fatal(err)
	}
	if err := organizationInsert(t, engine, actor, "v1:campaigns:audience", "forged-"+suffix, map[string]any{"name": "Forbidden", "accountId": beta}); err == nil {
		t.Fatal("foreign account create succeeded")
	}
	// A fresh receiver gets only a verified forwarded identity and re-reads
	// membership rows. It has none of the originating request's account memo.
	receiver, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Logger = engine.Logger
	if err := receiver.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	ac, _ := auth.AccessFromContext(rankActorCtx(multi, auth.RoleWriter))
	wire, err := auth.ForwardedAuthorityForUser(ac, auth.ForwardedClassUser, "", time.Time{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	remote, err := auth.VerifyForwardedAuthority(wire, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	read := func() int {
		ctx := auth.ContextWithAccess(context.Background(), remote)
		result, err := receiver.Execute(ctx, fmt.Sprintf(`query campaignById(campaignId: %s)`, langparser.QuoteString(campaign)))
		if err != nil {
			t.Fatal(err)
		}
		return len(MaterializeRows(result))
	}
	if read() != 1 {
		t.Fatal("authorized same-org colleague cannot read creator's campaign after hop")
	}
	// Another authorized person may edit organization work without becoming
	// its owner. Test both a named actor-stamping mutation and a raw upsert;
	// createdBy still attributes this new version to its actual writer.
	peer := rankActorCtx(multi, auth.RoleWriter)
	if _, err := receiver.Execute(peer, fmt.Sprintf(`mutation updateTemplate(templateId: %s, name: "Peer edit", subject: "Edited", textBody: "Edited")`, langparser.QuoteString(template))); err != nil {
		t.Fatal(err)
	}
	if err := organizationInsert(t, receiver, peer, "v1:campaigns:audience", audience, map[string]any{"name": "Peer raw edit", "ownerUserId": multi}); err != nil {
		t.Fatal(err)
	}
	for concept, id := range map[string]string{"v1:campaigns:template": template, "v1:campaigns:audience": audience} {
		var rows []memorynodes.MemoryNode
		if err := db.NewSelect().Model(&rows).Where("concept = ?", concept).Where("id = ?", concept+":"+id).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || BareShortId(stringFromAny(accountRowPayload(rows[0])["ownerUserId"])) != member || BareShortId(rows[0].CreatedBy) != multi {
			t.Fatalf("peer edit changed original owner or lost editor attribution: %s", concept)
		}
	}
	streamCtx := receiver.SubscriptionRankContext(context.Background())
	eventPayload, _ := json.Marshal(map[string]any{"ownerUserId": member, "accountId": acme})
	if AdmitSubscriptionRow(streamCtx, remote, "v1:campaigns:campaign", campaign, eventPayload) != SubscriptionAdmit {
		t.Fatal("authorized receiver stream denied organization event")
	}
	seedMembership(t, engine, "g-"+acme, multi, "removed")
	if read() != 0 {
		t.Fatal("removed member retained campaign access on other replica")
	}
	if AdmitSubscriptionRow(streamCtx, remote, "v1:campaigns:campaign", campaign, eventPayload) != SubscriptionDeny {
		t.Fatal("open receiver stream retained revoked organization membership")
	}

	// A queued worker borrows the real user's identity, not their synthetic
	// RoleWriter's permissive app baseline. A later grant denial must be visible
	// on the receiver before it can resume the work.
	receiver.InstallGrantResolution()
	// A reader in both organizations has write grants in Acme only. The
	// global transport capability may admit the request, but the target's
	// organization must independently authorize both raw creates and updates.
	delegated := "delegated-" + suffix
	seedPrincipal(t, engine, delegated, auth.RoleReader)
	seedMembership(t, engine, "g-"+acme, delegated, "active")
	seedMembership(t, engine, "g-"+beta, delegated, "active")
	for _, verb := range []string{auth.VerbCreate, auth.VerbUpdate} {
		writeGrant(t, engine, auth.SubjectKindGroup, "g-"+acme, verb, auth.ResourceData, auth.GrantAllow)
	}
	delegatedCtx := rankActorCtx(delegated, auth.RoleReader)
	if !receiver.OrganizationCapable(delegatedCtx, acme, auth.VerbUpdate, auth.ResourceData) || receiver.OrganizationCapable(delegatedCtx, beta, auth.VerbUpdate, auth.ResourceData) {
		t.Fatal("organization data grant escaped its group's account")
	}
	if err := organizationInsert(t, receiver, delegatedCtx, "v1:campaigns:audience", "delegated-"+suffix, map[string]any{"name": "Delegated", "accountId": acme}); err != nil {
		t.Fatalf("delegated Acme create: %v", err)
	}
	if err := organizationInsert(t, receiver, delegatedCtx, "v1:campaigns:audience", "delegated-beta-"+suffix, map[string]any{"name": "Denied", "accountId": beta}); err == nil {
		t.Fatal("Acme data grant permitted a Beta create")
	}
	if err := organizationInsert(t, engine, rankActorCtx(operator, auth.RoleOwner), "v1:campaigns:audience", "beta-source-"+suffix, map[string]any{"name": "Beta source", "accountId": beta, "ownerUserId": operator}); err != nil {
		t.Fatal(err)
	}
	if err := organizationInsert(t, receiver, delegatedCtx, "v1:campaigns:audience", "beta-source-"+suffix, map[string]any{"accountId": acme}); err == nil {
		t.Fatal("Acme write grant moved a Beta row without source write authority")
	}
	writeGrant(t, engine, auth.SubjectKindGroup, "g-"+acme, auth.VerbRead, "app:campaigns", auth.GrantAllow)
	writeGrant(t, engine, auth.SubjectKindGroup, "g-"+beta, auth.VerbRead, "app:campaigns", auth.GrantDeny)
	subject, _ := auth.SubjectFromContext(delegatedCtx)
	if auth.CapableFor(delegatedCtx, subject, auth.VerbRead, "app:campaigns") {
		t.Fatal("control: global deny-wins answer should be denied")
	}
	discovered := receiver.ResolveOrganizationCapabilities(delegatedCtx, auth.EffectiveCapabilities(delegatedCtx, subject))
	byAccount := map[string]string{}
	for _, entry := range discovered {
		if entry.Verb == auth.VerbRead && entry.Resource == "app:campaigns" {
			byAccount[entry.AccountID] = entry.Effect
		}
	}
	if byAccount[acme] != auth.GrantAllow || byAccount[beta] != auth.GrantDeny || len(byAccount) != 2 {
		t.Fatalf("discovery lost target-specific permissions: %v", byAccount)
	}
	// A deny on Beta's publish grant cannot block an allowed Acme target, or
	// become an unscoped allow that reaches Beta. Use the real named mutation.
	writeGrant(t, engine, auth.SubjectKindUser, delegated, auth.VerbUpdate, auth.ResourceData, auth.GrantAllow)
	writeGrant(t, engine, auth.SubjectKindGroup, "g-"+acme, auth.VerbExecute, "app:deployables/publish", auth.GrantAllow)
	writeGrant(t, engine, auth.SubjectKindGroup, "g-"+beta, auth.VerbExecute, "app:deployables/publish", auth.GrantDeny)
	betaSite := "beta-action-" + suffix
	if err := organizationInsert(t, engine, rankActorCtx(operator, auth.RoleOwner), "v1:platform:site", betaSite, map[string]any{"accountId": beta, "ownerUserId": operator, "hostname": betaSite + "." + siteHostnamePolicyDomain(), "kind": "spa", "status": "live", "bundleRef": "blob://beta/test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Execute(delegatedCtx, fmt.Sprintf(`mutation updateSiteStatus(siteId: %s, status: "disabled")`, langparser.QuoteString(namedSite))); err != nil {
		t.Fatalf("Acme target refused by Beta's flattened publish denial: %v", err)
	}
	if _, err := receiver.Execute(delegatedCtx, fmt.Sprintf(`mutation updateSiteStatus(siteId: %s, status: "disabled")`, langparser.QuoteString(betaSite))); err == nil || !strings.Contains(err.Error(), CodeCapabilityNotHeld) {
		t.Fatalf("Beta target did not refuse publish capability: %v", err)
	}
	if err := organizationInsert(t, receiver, delegatedCtx, "v1:platform:site", betaSite, map[string]any{"status": "disabled"}); err == nil || !strings.Contains(err.Error(), CodeCapabilityNotHeld) {
		t.Fatalf("raw lifecycle bypass: %v", err)
	}
	writeGrant(t, engine, auth.SubjectKindUser, delegated, auth.VerbCreate, auth.ResourceData, auth.GrantAllow)
	if err := organizationInsert(t, receiver, delegatedCtx, "v1:platform:site", "beta-live-create-"+suffix, map[string]any{"accountId": beta, "hostname": "beta-live-create-" + suffix + "." + siteHostnamePolicyDomain(), "kind": "spa", "status": "live"}); err == nil || !strings.Contains(err.Error(), CodeCapabilityNotHeld) {
		t.Fatalf("initial live creation bypass: %v", err)
	}
	if err := organizationInsert(t, receiver, delegatedCtx, "v1:platform:site", "beta-draft-create-"+suffix, map[string]any{"accountId": beta, "hostname": "beta-draft-create-" + suffix + "." + siteHostnamePolicyDomain(), "kind": "spa", "status": "draft", "bundleRef": ""}); err != nil {
		t.Fatalf("empty draft creation should remain ordinary data: %v", err)
	}
	// The actual loaded builtin must pass its evaluated target into the same
	// gate. Only the external deploy executor is replaced to avoid a build.
	writeGrant(t, engine, auth.SubjectKindGroup, "g-"+acme, auth.VerbExecute, "app:deployables/deploy", auth.GrantAllow)
	writeGrant(t, engine, auth.SubjectKindGroup, "g-"+beta, auth.VerbExecute, "app:deployables/deploy", auth.GrantDeny)
	betaPackage := "beta-package-" + suffix
	if err := organizationInsert(t, engine, rankActorCtx(operator, auth.RoleOwner), "v1:platform:package", betaPackage, map[string]any{"name": "Beta", "sourceKind": "repo", "status": "active", "accountId": beta, "ownerUserId": operator}); err != nil {
		t.Fatal(err)
	}
	if err := organizationInsert(t, engine, groupSeedCtx(), "v1:platform:packageDeployment", "beta-run-"+suffix, map[string]any{"packageId": betaPackage, "accountId": beta, "status": "awaiting_confirm"}); err != nil {
		t.Fatal(err)
	}
	deploys := 0
	receiver.builtinExecutorHandlers["integration.packages.deploy"] = func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
		deploys++
		return nil, nil
	}
	packageID := "v1-platform-package-" + suffix
	for _, extra := range []string{"", ", confirm: true, deploymentId: " + langparser.QuoteString("run-"+suffix), ", fromDeploymentId: " + langparser.QuoteString("run-"+suffix)} {
		if _, err := receiver.Execute(delegatedCtx, fmt.Sprintf(`builtin packageDeploy(packageId: %s%s)`, langparser.QuoteString(packageID), extra)); err != nil {
			t.Fatalf("allowed package deploy/confirm/retry refused: %v", err)
		}
	}
	for _, query := range []string{
		fmt.Sprintf(`builtin packageDeploy(packageId: %s)`, langparser.QuoteString(betaPackage)),
		fmt.Sprintf(`builtin packageDeploy(packageId: %s, confirm: true, deploymentId: %s)`, langparser.QuoteString(packageID), langparser.QuoteString("beta-run-"+suffix)),
		fmt.Sprintf(`builtin packageDeploy(packageId: %s, fromDeploymentId: %s)`, langparser.QuoteString(packageID), langparser.QuoteString("beta-run-"+suffix)),
	} {
		if _, err := receiver.Execute(delegatedCtx, query); err == nil || !strings.Contains(err.Error(), CodeCapabilityNotHeld) {
			t.Fatalf("foreign package/run action not denied: %v", err)
		}
	}
	if deploys != 3 {
		t.Fatalf("denied action reached deploy executor: %d", deploys)
	}
	borrowed := auth.ContextWithUserActor(context.Background(), member)
	if !receiver.organizationAppAllows(borrowed, acme, "app:campaigns") {
		t.Fatal("borrowed owner's initial app permission missing")
	}
	campaignQuery := fmt.Sprintf(`query campaignById(campaignId: %s)`, langparser.QuoteString(campaign))
	beforeDeny, err := receiver.Execute(auth.ContextWithUserActor(context.Background(), member), campaignQuery)
	if err != nil || len(MaterializeRows(beforeDeny)) != 1 {
		t.Fatalf("before denial: %v", err)
	}
	writeGrant(t, engine, auth.SubjectKindUser, member, auth.VerbRead, "app:campaigns", auth.GrantDeny)
	afterDeny, err := receiver.Execute(auth.ContextWithUserActor(context.Background(), member), campaignQuery)
	if err != nil || len(MaterializeRows(afterDeny)) != 0 {
		t.Fatalf("cached read survived app denial: %v", err)
	}
	if receiver.organizationAppAllows(auth.ContextWithUserActor(context.Background(), member), acme, "app:campaigns") {
		t.Fatal("borrowed worker ignored persisted app denial")
	}
	// Neither an internal stamp nor owning the old row bypasses that denial.
	if err := organizationInsert(t, receiver, auth.ContextWithInternalOrigin(rankActorCtx(member, auth.RoleWriter)), "v1:campaigns:audience", "internal-denied-"+suffix, map[string]any{"name": "Denied", "accountId": acme}); err == nil {
		t.Fatal("internal origin bypassed real caller app denial")
	}
	// Transferring a parent alone would leave its already-saved children in a
	// different account, so the existing graph makes the operation refuse.
	if _, err := engine.Execute(rankActorCtx(operator, auth.RoleOwner), fmt.Sprintf(`mutation updateAudience(audienceId: %s, accountId: %s)`, langparser.QuoteString(audience), langparser.QuoteString(beta))); err == nil {
		t.Fatal("retagged an audience without moving its campaign")
	}
	foreign := rankActorCtx(multi, auth.RoleWriter)
	for concept, id := range map[string]string{"v1:campaigns:campaign": campaign, "v1:platform:site": namedSite, "v1:platform:package": "v1-platform-package-" + suffix} {
		res, err := receiver.Execute(foreign, fmt.Sprintf(`concept==%s && row.id==%s`, langparser.QuoteString(concept), langparser.QuoteString(concept+":"+id)))
		if err != nil {
			t.Fatal(err)
		}
		if len(MaterializeRows(res)) != 0 {
			t.Fatalf("foreign read leaked %s", concept)
		}
		if err := organizationInsert(t, receiver, foreign, concept, id, map[string]any{"accountId": beta}); err == nil {
			t.Fatalf("foreign update succeeded for %s", concept)
		}
	}

}
