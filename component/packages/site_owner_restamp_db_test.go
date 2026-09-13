package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// site_owner_restamp_db_test.go -- the cloud repair for the owner erasure
// (memql#5292), run against a real engine over real rows.
//
// Every developer deploy before PR #5284 left a v1:platform:site row whose
// LATEST version carries no ownerUserId and a non-empty packageId: the
// pipeline's recordSitePackageOrigin write, stamped internal on the
// developer's own context, tripped the privileged self-match undo and DELETED
// the key the developer's createSite had just stamped. Such a row is
// cluster-owned, so the developer who deployed it cannot read it through
// sitesAll, cannot flip it live, and cannot delete it. #5284 stops the erasure
// going forward; the migration under test re-stamps the rows it already left.
//
// Why a database test and not a unit one: the claim is "the developer can then
// read the site through sitesAll and write updateSiteStatus(live)", which is a
// property of the row gate over real rows. A fake engine has no gates.
//
// The blanked shape is built the way the erasure built it -- the row is written
// THROUGH the engine by the pipeline's exact writes, and then a new version is
// appended with the key removed -- so the packageId and the deployment's
// deployables[].siteId carry the spelling the engine really stores, and the
// migration's SQL is measured against that spelling rather than a guess.
//
// Postgres-gated like its neighbours; MEMQL_REQUIRE_DB=1 turns the skip into a
// failure.

// restampSiteDomain mirrors component/memql's siteHostnamePolicyDomain: the
// hostname policy admits a user-owned site only at <slug>.<domain>, and the
// domain is MEMQL_DOMAIN or the local default.
func restampSiteDomain() string {
	if d := strings.TrimSpace(os.Getenv("MEMQL_DOMAIN")); d != "" {
		return strings.ToLower(d)
	}
	return "memql.localhost"
}

// restampActor is the request context the gRPC layer gives a signed-in
// developer: AccessContext for actor.* and the row gate, TokenInfo for the
// mutation actor. The same pair the #5284 database test uses.
func restampActor(userId string) context.Context {
	return auth.ContextWithToken(
		auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userId, Role: auth.RoleDeveloper}),
		&auth.TokenInfo{Subject: userId},
	)
}

func restampCleanup(t *testing.T, db *bun.DB, ids ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ?", id).Exec(context.Background())
		}
	})
}

// restampLatest reads the newest version of one id: owner, status, and how
// many versions the id carries.
func restampLatest(t *testing.T, db *bun.DB, id string) (owner, status string, versions int) {
	t.Helper()
	row := db.QueryRowContext(context.Background(),
		`SELECT COALESCE(payload->>'ownerUserId', ''), COALESCE(payload->>'status', ''),
		        (SELECT count(*) FROM "MemoryNodes" WHERE id = ?)
		   FROM "MemoryNodes" WHERE id = ? ORDER BY "createdAt" DESC LIMIT 1`, id, id)
	if err := row.Scan(&owner, &status, &versions); err != nil {
		t.Fatalf("reading the latest version of %s: %v", id, err)
	}
	return owner, status, versions
}

// restampErase appends the version the pre-#5284 undo wrote: the newest
// payload with ownerUserId DELETED, nothing else changed. Strictly later than
// the version it copies, since (id, createdAt) is the primary key.
func restampErase(t *testing.T, db *bun.DB, siteId string) {
	t.Helper()
	res, err := db.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", concept, type, schema, payload, metadata, provenance)
		 SELECT id, GREATEST(now(), "createdAt" + interval '1 millisecond'), "createdBy", concept, type, schema,
		        payload - 'ownerUserId', metadata, provenance
		   FROM "MemoryNodes" WHERE id = ? ORDER BY "createdAt" DESC LIMIT 1`, siteId)
	if err != nil {
		t.Fatalf("erasing the owner on %s: %v", siteId, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("erasing the owner on %s appended %d versions, want 1", siteId, n)
	}
}

// restampSeed writes ONE version directly, for rows the migration must leave
// alone and for the cluster-owned package no engine write in this test can
// produce (createPackage stamps the actor).
func restampSeed(t *testing.T, db *bun.DB, concept, id string, at time.Time, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshalling %s: %v", id, err)
	}
	_, err = db.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", concept, type, schema, payload, metadata, provenance)
		 VALUES (?, ?, 'site-owner-restamp-test', ?, 'object', '{}'::jsonb, ?::jsonb, '{}'::jsonb,
		         '{"kind":"direct","name":"site-owner-restamp-test"}'::jsonb)`,
		id, at.UTC(), concept, string(raw))
	if err != nil {
		t.Fatalf("seeding %s: %v", id, err)
	}
}

func restampIds(rows []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		out[rowString(r, "id")] = true
	}
	return out
}

func restampFind(report database.SiteOwnerRestampReport, siteId string) (restamped *database.SiteOwnerRestamp, unresolved bool) {
	for i := range report.Restamped {
		if report.Restamped[i].SiteId == siteId {
			return &report.Restamped[i], false
		}
	}
	for _, u := range report.Unresolved {
		if u.SiteId == siteId {
			return nil, true
		}
	}
	return nil, false
}

// TestSiteOwnerRestampRepairsTheErasedDeveloperSite is the acceptance run:
// the pipeline's writes, the erasure, the refusal, the repair, and the
// developer's Go-live click landing.
func TestSiteOwnerRestampRepairsTheErasedDeveloperSite(t *testing.T) {
	eng, db := dbEngine(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	developer := "v1:identity:user:restamp-dev-" + suffix
	packageId := "v1:platform:package:restamp-" + suffix
	siteId := "v1:platform:site:restamp-" + suffix
	deploymentId := "v1:platform:packageDeployment:restamp-" + suffix
	hostname := "restamp-" + suffix + "." + restampSiteDomain()
	restampCleanup(t, db, packageId, siteId, deploymentId)

	devCtx := restampActor(developer)
	// store.writeInternal's exact shape: the developer's context with internal
	// origin stamped inline.
	pipelineCtx := auth.ContextWithInternalOrigin(devCtx)
	now := time.Now().UTC().Format(time.RFC3339)

	// ---- the deploy, as the pipeline writes it ----
	mustExecute(t, eng, devCtx, fmt.Sprintf(
		`mutation createPackage(packageId: %s, name: "restamp", sourceKind: "artifact")`,
		langparser.QuoteString(packageId)))
	mustExecute(t, eng, devCtx, fmt.Sprintf(
		`mutation createSite(siteId: %s, hostname: %s, kind: "static", bundleRef: "", status: "draft")`,
		langparser.QuoteString(siteId), langparser.QuoteString(hostname)))
	mustExecute(t, eng, pipelineCtx, fmt.Sprintf(
		`mutation recordSitePackageOrigin(siteId: %s, packageId: %s, packageDeployableName: "web")`,
		langparser.QuoteString(siteId), langparser.QuoteString(packageId)))
	mustExecute(t, eng, pipelineCtx, fmt.Sprintf(
		`mutation openPackageDeployment(deploymentId: %s, packageId: %s, sourceVersion: "abc123", requestedBy: %s, automatic: false, nodeId: "test", scopedTo: [], fromDeploymentId: "", startedAt: %s)`,
		langparser.QuoteString(deploymentId), langparser.QuoteString(packageId), langparser.QuoteString(developer), langparser.QuoteString(now)))
	mustExecute(t, eng, pipelineCtx, fmt.Sprintf(
		`mutation closePackageDeployment(deploymentId: %s, status: "succeeded", deployables: [{"name": "web", "siteId": %s, "hostname": %s, "bundleRef": "blob://sites/x/1/", "version": "abc123"}], dslVersion: "", buildLogTail: "", builtOn: {"surface": "prebuilt", "nodeId": ""}, finishedAt: %s)`,
		langparser.QuoteString(deploymentId), langparser.QuoteString(siteId), langparser.QuoteString(hostname), langparser.QuoteString(now)))

	if owner, _, _ := restampLatest(t, db, siteId); owner != developer {
		t.Fatalf("after the pipeline's writes ownerUserId = %q, want %q (PR #5284 is the fix for that; this test is for the rows it left behind)", owner, developer)
	}

	// ---- the erasure, as it was left in the cloud ----
	restampErase(t, db, siteId)
	if owner, _, _ := restampLatest(t, db, siteId); owner != "" {
		t.Fatalf("the erased version still carries ownerUserId=%q", owner)
	}

	// ---- the negative control: the developer is locked out ----
	if restampIds(mustExecute(t, eng, devCtx, "query sitesAll()"))[siteId] {
		t.Fatalf("the developer can read %s through sitesAll while its owner is blank -- the blanked shape did not reproduce the lockout", siteId)
	}
	if _, err := eng.Execute(devCtx, fmt.Sprintf(`mutation updateSiteStatus(siteId: %s, status: "live")`, langparser.QuoteString(siteId))); err == nil {
		t.Fatalf("updateSiteStatus(live) by the developer succeeded on the blanked row; the lockout did not reproduce")
	}

	// ---- the repair ----
	_, _, before := restampLatest(t, db, siteId)
	report, err := database.RestampSiteOwners(ctx, db, discardLogger())
	if err != nil {
		t.Fatalf("RestampSiteOwners: %v", err)
	}
	// Pure reads are result-cached by default (result_cache_policy.go), keyed
	// by query text, and the negative control above cached `sitesAll()` EMPTY
	// for the developer. At boot the migration runs before any read, so no
	// engine has cached anything it could repair past; here the read came
	// first, so evict the concept the way executeWrite does after its own
	// write. Without this the assertion below reads the pre-repair cache.
	eng.InvalidateCacheForConcept("v1:platform:site")
	got, unresolved := restampFind(report, siteId)
	if unresolved || got == nil {
		t.Fatalf("the migration did not restamp %s (unresolved=%v); report: %+v", siteId, unresolved, report)
	}
	if got.OwnerUserId != developer || got.Source != database.SiteOwnerRestampSourceDeployment || got.PackageId != packageId {
		t.Fatalf("restamped %+v, want owner %q from %q for package %q", *got, developer, database.SiteOwnerRestampSourceDeployment, packageId)
	}
	owner, status, after := restampLatest(t, db, siteId)
	if owner != developer || status != "draft" || after != before+1 {
		t.Fatalf("after the repair: owner=%q status=%q versions=%d, want %q / draft / %d (one NEW version, nothing else changed)", owner, status, after, developer, before+1)
	}

	// ---- the developer is back in ----
	if !restampIds(mustExecute(t, eng, devCtx, "query sitesAll()"))[siteId] {
		t.Fatalf("after the repair the developer still cannot read %s through sitesAll", siteId)
	}
	mustExecute(t, eng, devCtx, fmt.Sprintf(`mutation updateSiteStatus(siteId: %s, status: "live")`, langparser.QuoteString(siteId)))
	if owner, status, _ := restampLatest(t, db, siteId); owner != developer || status != "live" {
		t.Fatalf("after Go live: owner=%q status=%q, want %q / live", owner, status, developer)
	}

	// ---- idempotent: a second run selects nothing ----
	_, _, beforeAgain := restampLatest(t, db, siteId)
	again, err := database.RestampSiteOwners(ctx, db, discardLogger())
	if err != nil {
		t.Fatalf("second RestampSiteOwners: %v", err)
	}
	if r, u := restampFind(again, siteId); r != nil || u {
		t.Fatalf("the second run touched %s again: restamped=%v unresolved=%v", siteId, r, u)
	}
	if _, _, afterAgain := restampLatest(t, db, siteId); afterAgain != beforeAgain {
		t.Fatalf("the second run appended a version to %s (%d -> %d)", siteId, beforeAgain, afterAgain)
	}
}

// TestSiteOwnerRestampFallsBackToThePackageOwnerAndLeavesTheRestAlone pins
// the fallback order and the selector's edges: a site no deployment names
// takes the package row's owner; a site whose package has no owner either is
// logged and left alone; and a cluster-owned hand-made site, a systemOwned
// row, a deleted row and a row whose LATEST version already carries an owner
// are not touched.
func TestSiteOwnerRestampFallsBackToThePackageOwnerAndLeavesTheRestAlone(t *testing.T) {
	eng, db := dbEngine(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	domain := restampSiteDomain()
	owner := "v1:identity:user:restamp-owner-" + suffix
	packageId := "v1:platform:package:restamp-fb-" + suffix
	orphanPackageId := "v1:platform:package:restamp-orphan-" + suffix
	siteByPackage := "v1:platform:site:restamp-pkg-" + suffix
	siteUnresolved := "v1:platform:site:restamp-none-" + suffix
	siteHandMade := "v1:platform:site:restamp-hand-" + suffix
	siteSystem := "v1:platform:site:restamp-sys-" + suffix
	siteDeleted := "v1:platform:site:restamp-del-" + suffix
	siteRepairedLater := "v1:platform:site:restamp-later-" + suffix
	deploymentId := "v1:platform:packageDeployment:restamp-fb-" + suffix
	restampCleanup(t, db, packageId, orphanPackageId, siteByPackage, siteUnresolved, siteHandMade, siteSystem, siteDeleted, siteRepairedLater, deploymentId)

	ownerCtx := restampActor(owner)
	pipelineCtx := auth.ContextWithInternalOrigin(ownerCtx)
	now := time.Now().UTC().Format(time.RFC3339)

	// The package-owner fallback: a deployment exists for the package, but
	// its deployables[] names a DIFFERENT site, so the deployment answer is
	// empty and the package row's owner is the one to take.
	mustExecute(t, eng, ownerCtx, fmt.Sprintf(
		`mutation createPackage(packageId: %s, name: "restamp-fb", sourceKind: "artifact")`,
		langparser.QuoteString(packageId)))
	mustExecute(t, eng, ownerCtx, fmt.Sprintf(
		`mutation createSite(siteId: %s, hostname: %s, kind: "static", bundleRef: "", status: "draft")`,
		langparser.QuoteString(siteByPackage), langparser.QuoteString("restamp-pkg-"+suffix+"."+domain)))
	mustExecute(t, eng, pipelineCtx, fmt.Sprintf(
		`mutation recordSitePackageOrigin(siteId: %s, packageId: %s, packageDeployableName: "web")`,
		langparser.QuoteString(siteByPackage), langparser.QuoteString(packageId)))
	mustExecute(t, eng, pipelineCtx, fmt.Sprintf(
		`mutation openPackageDeployment(deploymentId: %s, packageId: %s, sourceVersion: "abc123", requestedBy: %s, automatic: false, nodeId: "test", scopedTo: [], fromDeploymentId: "", startedAt: %s)`,
		langparser.QuoteString(deploymentId), langparser.QuoteString(packageId), langparser.QuoteString("v1:identity:user:restamp-stranger-"+suffix), langparser.QuoteString(now)))
	mustExecute(t, eng, pipelineCtx, fmt.Sprintf(
		`mutation closePackageDeployment(deploymentId: %s, status: "succeeded", deployables: [{"name": "other", "siteId": %s}], dslVersion: "", buildLogTail: "", builtOn: {"surface": "prebuilt", "nodeId": ""}, finishedAt: %s)`,
		langparser.QuoteString(deploymentId), langparser.QuoteString("v1:platform:site:restamp-other-"+suffix), langparser.QuoteString(now)))
	restampErase(t, db, siteByPackage)

	// Seeded rows: a cluster-owned package (no engine write can produce one
	// here, since createPackage stamps the actor) and the bystanders.
	at := time.Now().UTC()
	restampSeed(t, db, "v1:platform:package", orphanPackageId, at, map[string]any{
		"name": "orphan", "sourceKind": "artifact", "status": "active", "updateAvailable": false})
	site := func(hostname string, extra map[string]any) map[string]any {
		p := map[string]any{"hostname": hostname + "." + domain, "kind": "static", "bundleRef": "", "status": "draft", "apiProxy": false, "systemOwned": false, "title": hostname}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	restampSeed(t, db, "v1:platform:site", siteUnresolved, at, site("restamp-none-"+suffix,
		map[string]any{"packageId": orphanPackageId, "packageDeployableName": "web"}))
	restampSeed(t, db, "v1:platform:site", siteHandMade, at, site("restamp-hand-"+suffix, nil))
	restampSeed(t, db, "v1:platform:site", siteSystem, at, site("restamp-sys-"+suffix,
		map[string]any{"packageId": packageId, "packageDeployableName": "sys", "systemOwned": true}))
	restampSeed(t, db, "v1:platform:site", siteDeleted, at, site("restamp-del-"+suffix,
		map[string]any{"packageId": packageId, "packageDeployableName": "del", "deleted": true}))
	// An OLDER version blanked, the LATEST version owned: the selector reads
	// the latest version only, so this row is already fine.
	restampSeed(t, db, "v1:platform:site", siteRepairedLater, at, site("restamp-later-"+suffix,
		map[string]any{"packageId": packageId, "packageDeployableName": "later"}))
	restampSeed(t, db, "v1:platform:site", siteRepairedLater, at.Add(time.Second), site("restamp-later-"+suffix,
		map[string]any{"packageId": packageId, "packageDeployableName": "later", "ownerUserId": owner}))

	untouched := map[string]int{}
	for _, id := range []string{siteUnresolved, siteHandMade, siteSystem, siteDeleted, siteRepairedLater} {
		_, _, untouched[id] = restampLatest(t, db, id)
	}

	report, err := database.RestampSiteOwners(ctx, db, discardLogger())
	if err != nil {
		t.Fatalf("RestampSiteOwners: %v", err)
	}

	got, unresolved := restampFind(report, siteByPackage)
	if unresolved || got == nil || got.OwnerUserId != owner || got.Source != database.SiteOwnerRestampSourcePackage {
		t.Fatalf("the package-owner fallback: got %+v (unresolved=%v), want owner %q from %q", got, unresolved, owner, database.SiteOwnerRestampSourcePackage)
	}
	if o, _, _ := restampLatest(t, db, siteByPackage); o != owner {
		t.Fatalf("after the fallback repair %s carries owner %q, want %q", siteByPackage, o, owner)
	}

	if r, u := restampFind(report, siteUnresolved); r != nil || !u {
		t.Fatalf("a site whose package has no owner must be reported unresolved and left alone; got restamped=%v unresolved=%v", r, u)
	}
	for _, id := range []string{siteHandMade, siteSystem, siteDeleted, siteRepairedLater} {
		if r, u := restampFind(report, id); r != nil || u {
			t.Errorf("%s must not appear in the report at all (restamped=%v unresolved=%v)", id, r, u)
		}
	}
	for id, before := range untouched {
		if _, _, after := restampLatest(t, db, id); after != before {
			t.Errorf("%s gained a version (%d -> %d); the migration must leave it alone", id, before, after)
		}
	}
}
