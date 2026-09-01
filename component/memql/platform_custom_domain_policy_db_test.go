package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// platform_custom_domain_policy_db_test.go -- epic memql#4805, the half that has
// to run against a real engine.
//
// The unit tests beside this file drive validateCustomDomainHostname directly.
// That is necessary and not sufficient: every one of them would keep passing if
// executeWrite stopped calling the guard, and the failure would be silent --
// bindings landing on the cluster's own front-door hosts, past the per-site cap,
// or duplicating a hostname another row already answers on, with a suite
// reporting green. So the properties that matter are proven HERE, through
// `eng.Execute` on the real mutation path.
//
// The EDGE ALIAS is proven here too, and it is the reason this file exists at
// all: `liveCustomDomainByHostname`'s `status=="live"` conjunct is the whole
// security property of custom domains -- it is what stops a hostname somebody
// merely typed into a form from being served -- and it lives in a DSL filter
// that no Go unit test can reach. component/edge's resolver tests stub the
// query out entirely, so without this the filter is exercised nowhere.
//
// Postgres-gated like its neighbours. CI's db-tests lane runs this package with
// MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.

const customDomainTestDomain = "cd-policy-test.example"

// cdSuffix keys the fixtures on the TEST's own name.
//
// uniqueSuffix is "the string you pass + the pid", so it is unique per NAME
// rather than per call -- passing a constant here made every test in this file
// share one deployable, and the per-site cap tests then counted each other's
// bindings. The shared engine is a package-level fixture (memql#4032's own
// note about sync.Once fixtures applies), so isolation has to come from the
// ids.
func cdSuffix(t *testing.T) string {
	t.Helper()
	return uniqueSuffix("cd-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")))
}

// seedCustomDomainSite creates the deployable a binding needs to name, as the
// operator: v1:platform:customDomain is clusterOwner tier, so every caller who
// reaches these writes is one.
func seedCustomDomainSite(t *testing.T, eng *MemQLEngine, suffix string) string {
	t.Helper()
	ctx := systemSiteCtx()
	id := "site-cd-" + suffix
	if _, err := createSiteRaw(t, ctx, eng, map[string]any{
		"siteId":    id,
		"hostname":  "cd" + suffix + "." + customDomainTestDomain,
		"bundleRef": "blob://sites/" + id + "/v1/",
		"status":    "live",
	}); err != nil {
		t.Fatalf("could not seed the deployable a binding needs: %v", err)
	}
	return id
}

func createCustomDomainRaw(t *testing.T, eng *MemQLEngine, domainId, siteId, hostname string) error {
	t.Helper()
	_, err := runSiteMutation(t, ownerCustomDomainCtx(), eng, "createCustomDomain", map[string]any{
		"domainId": domainId,
		"siteId":   siteId,
		"hostname": hostname,
		"token":    "tok-" + domainId,
	})
	return err
}

// ===========================================================================
// The three guards, through the real write path
// ===========================================================================

// GUARD 1 + 2, and the point of running them here is that the guard is REACHED.
func TestCustomDomainCreateRefusesTheClustersOwnTerritoryThroughTheMutationPath(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, customDomainTestDomain)

	suffix := cdSuffix(t)
	siteId := seedCustomDomainSite(t, eng, suffix)

	for name, hostname := range map[string]string{
		"a slug under our own domain": "shop." + customDomainTestDomain,
		"our own apex":                customDomainTestDomain,
		"a front-door host":           "identity." + customDomainTestDomain,
	} {
		err := createCustomDomainRaw(t, eng, "cd-refuse-"+suffix+"-"+strings.ReplaceAll(name, " ", "-"), siteId, hostname)
		if err == nil {
			t.Errorf("%s (%q) was admitted as a custom domain", name, hostname)
			continue
		}
		if !strings.Contains(err.Error(), "v1:platform:customDomain") {
			t.Errorf("%s was refused by something other than the custom-domain guard: %v", name, err)
		}
	}

	// A WILDCARD IS REFUSED A LAYER EARLIER, by the mutation's own @pattern,
	// and that is worth asserting separately rather than folding into the loop
	// above: the guard's wildcard branch is still what covers the raw insert()
	// surface, which bypasses argument validation entirely. Two layers, and
	// this measures the outer one.
	if err := createCustomDomainRaw(t, eng, "cd-refuse-wildcard-"+suffix, siteId, "*.acme-"+suffix+".com"); err == nil {
		t.Error("a wildcard hostname was admitted as a custom domain")
	}
}

// GUARD 2's read half: a hostname a SITE already answers on collides, even
// though the two rows live in different concepts. The edge asks siteByHostname
// first, so a binding duplicating one would be dead on arrival.
func TestCustomDomainCreateRefusesAHostnameASiteAlreadyServes(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, customDomainTestDomain)

	suffix := cdSuffix(t)
	siteId := seedCustomDomainSite(t, eng, suffix)

	taken := "taken-" + suffix + ".acme.com"
	if _, err := createSiteRaw(t, systemSiteCtx(), eng, map[string]any{
		"siteId":    "site-taken-" + suffix,
		"hostname":  taken,
		"bundleRef": "blob://sites/taken/v1/",
	}); err != nil {
		t.Fatalf("could not seed the colliding site: %v", err)
	}

	err := createCustomDomainRaw(t, eng, "cd-collide-"+suffix, siteId, taken)
	if err == nil {
		t.Fatal("a hostname an existing deployable already serves was admitted as a custom domain")
	}
	if !strings.Contains(err.Error(), "already served by deployable") {
		t.Errorf("the refusal does not name the collision: %v", err)
	}
}

// A SECOND BINDING on one hostname collides too, and the message says which
// row holds it.
func TestCustomDomainCreateRefusesADuplicateBinding(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, customDomainTestDomain)

	suffix := cdSuffix(t)
	siteId := seedCustomDomainSite(t, eng, suffix)
	host := "dup-" + suffix + ".acme.com"

	if err := createCustomDomainRaw(t, eng, "cd-dup-a-"+suffix, siteId, host); err != nil {
		t.Fatalf("the first binding was refused: %v", err)
	}
	err := createCustomDomainRaw(t, eng, "cd-dup-b-"+suffix, siteId, host)
	if err == nil {
		t.Fatal("a second binding on one hostname was admitted")
	}
	if !strings.Contains(err.Error(), "already bound by") {
		t.Errorf("the refusal does not name the holder: %v", err)
	}
}

// GUARD 3: the per-site cap, read from the env-tunable. Set to 2 so the test
// does not have to create five bindings to prove a limit exists.
func TestCustomDomainCreateRefusesPastThePerSiteMaximum(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, customDomainTestDomain)
	t.Setenv(customDomainMaxPerSiteEnv, "2")

	suffix := cdSuffix(t)
	siteId := seedCustomDomainSite(t, eng, suffix)

	for i, host := range []string{"cap1-" + suffix + ".acme.com", "cap2-" + suffix + ".acme.com"} {
		if err := createCustomDomainRaw(t, eng, "cd-cap-"+suffix+"-"+host[:5], siteId, host); err != nil {
			t.Fatalf("binding %d of the allowed 2 was refused: %v", i+1, err)
		}
	}
	err := createCustomDomainRaw(t, eng, "cd-cap-over-"+suffix, siteId, "cap3-"+suffix+".acme.com")
	if err == nil {
		t.Fatal("a third binding was admitted with the per-site maximum set to 2")
	}
	if !strings.Contains(err.Error(), customDomainMaxPerSiteEnv) {
		t.Errorf("the refusal does not name the tunable an operator would raise: %v", err)
	}
}

// A REMOVED BINDING DOES NOT COUNT against the cap. Its row survives as
// history, and a cap that counted it would turn an audit trail into a quota.
func TestCustomDomainRemovedBindingsDoNotCountAgainstTheCap(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, customDomainTestDomain)
	t.Setenv(customDomainMaxPerSiteEnv, "1")

	suffix := cdSuffix(t)
	siteId := seedCustomDomainSite(t, eng, suffix)

	first := "gone-" + suffix + ".acme.com"
	if err := createCustomDomainRaw(t, eng, "cd-gone-"+suffix, siteId, first); err != nil {
		t.Fatalf("the first binding was refused: %v", err)
	}
	if _, err := runSiteMutation(t, ownerCustomDomainCtx(), eng, "markCustomDomainRemoved", map[string]any{
		"domainId":      "cd-gone-" + suffix,
		"removedAt":     "2026-09-01T12:00:00Z",
		"lastCheckedAt": "2026-09-01T12:00:00Z",
	}); err != nil {
		t.Fatalf("could not close the first binding's walk: %v", err)
	}
	if err := createCustomDomainRaw(t, eng, "cd-fresh-"+suffix, siteId, "fresh-"+suffix+".acme.com"); err != nil {
		t.Fatalf("a removed binding counted against the per-site cap: %v", err)
	}
}

// ===========================================================================
// The edge alias -- the `status=="live"` filter
// ===========================================================================

// THE SECURITY PROPERTY OF CUSTOM DOMAINS, measured where it lives. A binding
// resolves for the edge only once it is `live`, which it reaches only after
// both DNS checks passed and its certificate came back Ready. Every other
// status must resolve to nothing -- including `removing`, which is why a
// removal takes effect at the speed of a row write rather than at the speed of
// an Ingress deletion.
func TestLiveCustomDomainByHostnameAnswersOnlyForALiveBinding(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, customDomainTestDomain)

	suffix := cdSuffix(t)
	siteId := seedCustomDomainSite(t, eng, suffix)
	host := "alias-" + suffix + ".acme.com"
	domainId := "cd-alias-" + suffix

	if err := createCustomDomainRaw(t, eng, domainId, siteId, host); err != nil {
		t.Fatalf("could not create the binding: %v", err)
	}

	// pending_dns: a hostname somebody typed into a form. Resolving it would
	// let anyone serve a site at any name they could spell.
	if n := countLiveCustomDomain(t, eng, host); n != 0 {
		t.Fatalf("a `pending_dns` binding resolved (%d rows) -- the edge would serve a hostname nobody has proved they own", n)
	}

	mustRun(t, eng, "markCustomDomainVerified", map[string]any{
		"domainId": domainId, "verifiedAt": "2026-09-01T12:00:00Z", "lastCheckedAt": "2026-09-01T12:00:00Z",
	})
	if n := countLiveCustomDomain(t, eng, host); n != 0 {
		t.Fatalf("an `issuing` binding resolved (%d rows) -- there is no certificate for it yet", n)
	}

	mustRun(t, eng, "markCustomDomainLive", map[string]any{
		"domainId": domainId, "issuedAt": "2026-09-01T12:05:00Z", "lastCheckedAt": "2026-09-01T12:05:00Z",
	})
	if n := countLiveCustomDomain(t, eng, host); n != 1 {
		t.Fatalf("a `live` binding resolved %d rows, want 1 -- the edge cannot serve the domain at all", n)
	}

	// A REMOVAL STOPS SERVING AT THE ROW WRITE, before the Ingress is anywhere
	// near gone. That ordering is the point: an operator unbinding a domain
	// because it is being abused does not have to wait for kubectl.
	mustRun(t, eng, "removeCustomDomain", map[string]any{"domainId": domainId})
	if n := countLiveCustomDomain(t, eng, host); n != 0 {
		t.Fatalf("a `removing` binding still resolved (%d rows) -- serving would continue until the objects were deleted", n)
	}

	mustRun(t, eng, "markCustomDomainRemoved", map[string]any{
		"domainId": domainId, "removedAt": "2026-09-01T12:10:00Z", "lastCheckedAt": "2026-09-01T12:10:00Z",
	})
	if n := countLiveCustomDomain(t, eng, host); n != 0 {
		t.Fatalf("a `removed` binding resolved (%d rows)", n)
	}
}

// The sweep's read excludes the two settled statuses in SQL, which is what
// makes a pass over a quiet cluster nearly free -- and what makes the
// unpaginated read defensible.
func TestCustomDomainsToReconcileExcludesTheSettledStatuses(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, customDomainTestDomain)

	suffix := cdSuffix(t)
	siteId := seedCustomDomainSite(t, eng, suffix)
	host := "sweep-" + suffix + ".acme.com"
	domainId := "cd-sweep-" + suffix

	if err := createCustomDomainRaw(t, eng, domainId, siteId, host); err != nil {
		t.Fatalf("could not create the binding: %v", err)
	}
	if !inReconcileSet(t, eng, domainId) {
		t.Fatal("a `pending_dns` binding is not in the sweep's set, so nothing would ever check its DNS")
	}

	mustRun(t, eng, "markCustomDomainVerified", map[string]any{
		"domainId": domainId, "verifiedAt": "2026-09-01T12:00:00Z", "lastCheckedAt": "2026-09-01T12:00:00Z",
	})
	if !inReconcileSet(t, eng, domainId) {
		t.Fatal("an `issuing` binding is not in the sweep's set, so nothing would ever provision it")
	}

	mustRun(t, eng, "markCustomDomainLive", map[string]any{
		"domainId": domainId, "issuedAt": "2026-09-01T12:05:00Z", "lastCheckedAt": "2026-09-01T12:05:00Z",
	})
	if inReconcileSet(t, eng, domainId) {
		t.Fatal("a `live` binding is still in the sweep's set -- every pass would do two DNS lookups for a settled domain")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// ownerCustomDomainCtx is a cluster owner: the only caller the concept's tier
// admits, and therefore the only shape these tests need.
func ownerCustomDomainCtx() context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "owner-custom-domain-test",
		Role:   auth.RoleOwner,
	})
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "owner-custom-domain-test"})
	// The six sweep writers are @serverOnly, so a caller identity alone is not
	// enough -- call origin defaults to CLIENT and the function validator
	// refuses. Every legitimate writer stamps this; so does the test.
	return auth.ContextWithInternalOrigin(ctx)
}

func mustRun(t *testing.T, eng *MemQLEngine, name string, args map[string]any) {
	t.Helper()
	if _, err := runSiteMutation(t, ownerCustomDomainCtx(), eng, name, args); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// countLiveCustomDomain asks the EDGE's own query -- the one whose
// `status=="live"` conjunct is the property being measured.
func countLiveCustomDomain(t *testing.T, eng *MemQLEngine, hostname string) int {
	t.Helper()
	res, err := eng.Execute(ownerCustomDomainCtx(),
		`query liveCustomDomainByHostname(hostname: "`+hostname+`")`)
	if err != nil {
		t.Fatalf("liveCustomDomainByHostname: %v", err)
	}
	return len(MaterializeRows(res))
}

func inReconcileSet(t *testing.T, eng *MemQLEngine, domainId string) bool {
	t.Helper()
	res, err := eng.Execute(ownerCustomDomainCtx(), "query customDomainsToReconcile()")
	if err != nil {
		t.Fatalf("customDomainsToReconcile: %v", err)
	}
	want := canonicalCustomDomainStorageId(domainId)
	for _, r := range MaterializeRows(res) {
		if canonicalCustomDomainStorageId(stringFromAny(r["id"])) == want {
			return true
		}
	}
	return false
}
