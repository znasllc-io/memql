package packages

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/edge"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

func TestPublisherPersistsOrganizationAndAuthenticatedOwnerTogether(t *testing.T) {
	eng, db := dbEngine(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "publisher-owner-" + suffix
	accountID := "publisher-org-" + suffix
	seedTiePrincipal(t, eng, userID, auth.RoleOwner)
	ctx := tieActorCtx(userID, auth.RoleOwner)
	mustExecute(t, eng, ctx, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Publisher organization")`, langparser.QuoteString(accountID)))
	pub := &enginePublisher{engine: eng, store: &store{engine: eng}}
	siteID, _, created, err := pub.EnsureSite(ctx, EnsureSiteRequest{
		PackageId: "no-existing-package-" + suffix, DeployableName: "web", Kind: KindSPA,
		Hostname: "publisher-" + suffix + ".example.test", AccountId: accountID,
		OwnerUserId: "must-not-override-authenticated-caller",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("site wasn't created")
	}
	var owner, org, creator string
	err = db.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr(`payload->>'ownerUserId'`).ColumnExpr(`payload->>'accountId'`).ColumnExpr(`"createdBy"`).Where("concept = ?", "v1:platform:site").Where("id = ?", siteID).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx, &owner, &org, &creator)
	if err != nil {
		t.Fatal(err)
	}
	if memql.BareShortId(owner) != userID || memql.BareShortId(org) != accountID {
		t.Fatalf("persisted owner/org = %q/%q, want %q/%q", owner, org, userID, accountID)
	}
	if memql.BareShortId(creator) != userID {
		t.Fatalf("persisted creator = %q, want %q", creator, userID)
	}
	var versions int
	if err := db.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("COUNT(*)").Where("concept = ?", "v1:platform:site").Where("id = ?", siteID).Scan(ctx, &versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("ownership needed %d writes instead of one", versions)
	}
}

// Object storage is the only replacement: rows, current memberships, group
// grants, actor resolution and the final write use the real engine.
type organizationRecordingBlobs struct{ puts int }

func (b *organizationRecordingBlobs) Put(context.Context, string, []byte) error { b.puts++; return nil }

func TestPackagePlacementRequiresTargetOrganizationActionBeforeSideEffects(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "example.test")
	eng, db := dbEngine(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, user := "placement-owner-"+suffix, "placement-member-"+suffix
	a, b := "placement-a-"+suffix, "placement-b-"+suffix
	seedTiePrincipal(t, eng, owner, auth.RoleOwner)
	seedTiePrincipal(t, eng, user, auth.RoleWriter)
	ownerCtx, memberCtx := tieActorCtx(owner, auth.RoleOwner), tieActorCtx(user, auth.RoleWriter)
	for _, org := range []string{a, b} {
		mustExecute(t, eng, ownerCtx, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Placement org")`, langparser.QuoteString(org)))
		seedTieGroup(t, eng, "placement-group-"+org, org)
		seedTieMembership(t, eng, "placement-group-"+org, user)
	}
	grant := func(org, verb, resource, effect string) {
		t.Helper()
		group := "placement-group-" + org
		id := auth.GrantRowID(auth.SubjectKindGroup, group, verb, resource)
		mustExecute(t, eng, tieSeederCtx(), fmt.Sprintf(`mutation writeGrant(grantId: %s, subjectKind: "group", subjectId: %s, verb: %s, resourceType: %s, effect: %s, grantedBy: %s)`, langparser.QuoteString(id), langparser.QuoteString(group), langparser.QuoteString(verb), langparser.QuoteString(resource), langparser.QuoteString(effect), langparser.QuoteString(owner)))
	}
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	eng.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })
	for _, org := range []string{a, b} {
		grant(org, auth.VerbRead, "app:deployables", auth.GrantAllow)
		grant(org, auth.VerbCreate, auth.ResourceData, auth.GrantAllow)
		grant(org, auth.VerbUpdate, auth.ResourceData, auth.GrantAllow)
	}
	for _, action := range []string{"deploy", "publish", "retire"} {
		grant(a, auth.VerbExecute, "app:deployables/"+action, auth.GrantAllow)
	}
	packageID := "placement-package-" + suffix
	createTiedPackage(t, eng, ownerCtx, packageID, "Placement source "+suffix, a)
	if !eng.OrganizationCapable(memberCtx, a, auth.VerbExecute, "app:deployables/deploy") || !eng.OrganizationCapable(memberCtx, b, auth.VerbUpdate, auth.ResourceData) {
		t.Fatal("fixture lacks source action or target data authority")
	}
	blobs := &organizationRecordingBlobs{}
	pub := &enginePublisher{engine: eng, store: &store{engine: eng}, blobs: blobs}
	req := EnsureSiteRequest{PackageId: packageID, DeployableName: "web", Kind: KindSPA, Hostname: "placement-" + suffix + ".example.test", AccountId: b}
	if _, _, _, err := pub.EnsureSite(memberCtx, req); err == nil || !strings.Contains(err.Error(), "organization_forbidden") {
		t.Fatalf("source A action created target B: %v", err)
	}
	var count int
	if err := db.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("COUNT(*)").Where("concept = ?", "v1:platform:site").Where("payload->>'hostname' = ?", req.Hostname).Scan(memberCtx, &count); err != nil || count != 0 {
		t.Fatalf("denied placement persisted %d rows: %v", count, err)
	}
	// Seed an existing B target through its authorized owner; the member may
	// edit B data but must still be unable to upload, roll back or retire it.
	siteID, _, _, err := pub.EnsureSite(ownerCtx, req)
	if err != nil {
		t.Fatal(err)
	}
	mustExecute(t, eng, auth.ContextWithInternalOrigin(ownerCtx), fmt.Sprintf(`mutation recordSitePackageOrigin(siteId: %s, packageId: %s, packageDeployableName: "web")`, langparser.QuoteString(siteID), langparser.QuoteString(packageID)))
	bundle := edge.Bundle{"index.html": []byte("<!doctype html><title>Organization</title>")}
	if _, err := pub.PublishBundle(memberCtx, siteID, bundle); err == nil {
		t.Fatal("A deploy grant uploaded to B")
	}
	if blobs.puts != 0 {
		t.Fatal("denied publish reached object storage")
	}
	if err := pub.RepointSite(memberCtx, siteID, "blob://forbidden"); err == nil {
		t.Fatal("A publish grant rolled back B")
	}
	row := mustExecute(t, eng, memberCtx, fmt.Sprintf(`query siteById(siteId: %s)`, langparser.QuoteString(siteID)))[0]
	if err := requireSiteRowAction(memberCtx, pub.store, row, "retire"); err == nil {
		t.Fatal("A retire grant authorized B")
	}
	for _, action := range []string{"deploy", "publish", "retire"} {
		grant(b, auth.VerbExecute, "app:deployables/"+action, auth.GrantAllow)
	}
	// Holding the action is insufficient when the target's data authority
	// was revoked: refusal must precede even the first uploaded byte.
	grant(b, auth.VerbUpdate, auth.ResourceData, auth.GrantDeny)
	if _, err := pub.PublishBundle(memberCtx, siteID, bundle); err == nil {
		t.Fatal("execute grant bypassed denied target data permission")
	}
	if blobs.puts != 0 {
		t.Fatal("denied target data write uploaded bytes")
	}
	if err := pub.RepointSite(memberCtx, siteID, "blob://data-forbidden"); err == nil {
		t.Fatal("rollback bypassed denied target data permission")
	}
	if err := requireSiteRowAction(memberCtx, pub.store, row, "retire"); err == nil {
		t.Fatal("retirement bypassed denied target data permission")
	}
	grant(b, auth.VerbUpdate, auth.ResourceData, auth.GrantAllow)
	if _, _, _, err := pub.EnsureSite(memberCtx, req); err != nil {
		t.Fatalf("both-org authority refused existing target: %v", err)
	}
	req.DeployableName = "new-web"
	req.Hostname = "new-placement-" + suffix + ".example.test"
	if _, _, _, err := pub.EnsureSite(memberCtx, req); err != nil {
		t.Fatalf("both-org authority refused explicit new B placement: %v", err)
	}
	if _, err := pub.PublishBundle(memberCtx, siteID, bundle); err != nil {
		t.Fatalf("both-org authority refused publish: %v", err)
	}
	if blobs.puts != 1 {
		t.Fatalf("authorized publish wrote %d files", blobs.puts)
	}
	if err := pub.RepointSite(memberCtx, siteID, "blob://permitted"); err != nil {
		t.Fatal(err)
	}
	var persistedOwner, creator, org string
	if err := db.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr(`payload->>'ownerUserId'`).ColumnExpr(`"createdBy"`).ColumnExpr(`payload->>'accountId'`).Where("concept = ?", "v1:platform:site").Where("id = ?", siteID).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(memberCtx, &persistedOwner, &creator, &org); err != nil {
		t.Fatal(err)
	}
	if memql.BareShortId(persistedOwner) != owner || memql.BareShortId(creator) != user || memql.BareShortId(org) != b {
		t.Fatalf("publication lost owner/actor/org: %q/%q/%q", persistedOwner, creator, org)
	}
}
