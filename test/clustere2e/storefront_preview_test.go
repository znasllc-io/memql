//go:build clustere2e

package clustere2e

// storefront_preview_test.go -- the whole of storefront preview, end to end
// against a real cluster (epic memql#5531, issue memql#5548).
//
// WHAT THIS LEG PROVES, AND WHY IT HAD TO BE A CLUSTER.
//
// Every property below is one no unit test can reach, because each of them is
// the agreement between two things that only exist together in a running
// deployment: a row written through the ordinary authorized path, a grant
// minted by the engine under a real caller, and an edge pod resolving both at
// serve time. It asserts, in the order an operator does them:
//
//   - an unauthenticated request to a DRAFT deployable is still 404, candidate
//     or no candidate;
//   - an unauthenticated request to a LIVE deployable carrying a candidate is
//     still served the SERVING version;
//   - a request carrying a valid grant is served the CANDIDATE, on the site's
//     own origin, for a draft deployable AND for a live one;
//   - the SERVED runtime-config document carries the preview binding's store
//     ONLY under the grant -- grepped out of the bytes a real pod really sent,
//     the way TestRuntimeConfigNeverCarriesTheShopifyAdminToken greps for the
//     Admin token;
//   - promotion flips the serving version and clears the candidate, and every
//     open preview of that candidate stops resolving as a consequence;
//   - rollback is ONE updateSiteBundle write, unchanged by this epic;
//   - each guard case refuses with its typed code.
//
// WHAT IT DOES NOT PROVE. No browser runs here, and no payment is made: the
// payment walk happens on Shopify's hosted checkout and is the product's test
// (znasllc-io/memql-fylo#28), not the engine's. The Storefront probe is not
// driven either -- it would create a cart on a real development store, which is
// not something a CI leg should do to somebody's Shopify account.
//
// THE CLUSTER-FREE HALVES ARE THE ONES THAT ACTUALLY GATE THE FEATURE. This
// file skips on every machine and every CI lane with no cluster, and a gate
// skipped by default cannot be what stands between a feature and the bug it
// prevents (CLAUDE.md, on the worker forward hop). So the serving half is also
// asserted in component/edge/preview_serve_test.go, the guard's wiring in
// component/memql/platform_site_preview_guard_test.go, and the rules in
// component/memql/site_preview_rules_test.go -- all three in the ordinary lane.
// This leg is what covers the seams BETWEEN them.
//
// THE STORES ARE ROWS, NOT SHOPIFY ACCOUNTS. Nothing here dials Shopify. Two
// v1:shopify:store rows are created -- one ordinary, one isDevelopment -- and
// what is under test is which of them the edge resolves into a served document
// and which the guard refuses. storefront_serving_test.go beside this one is
// the leg that exercises a (stubbed) Storefront endpoint.
//
// PREREQUISITES, ALL SKIPPED GRACEFULLY WHEN ABSENT:
//   - MEMQL_E2E_TOKEN for a CLUSTER OWNER (v1:platform:site and
//     v1:shopify:store are both cluster-owner work).
//   - kubectl on PATH, pointed at a cluster with >=1 Running edge pod.
//   - Object storage configured on the cluster (the publish path writes to it).
//
// RUN
//
//	MEMQL_E2E_TOKEN=<cluster owner JWT> go test -tags clustere2e -count=1 \
//	  -timeout=420s ./test/clustere2e/... -run TestStorefrontPreview -v

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// The marker each published version carries. The two bundles must differ in
// their BYTES, or every assertion about which one was served is vacuous while
// reporting a pass -- the same trap refOpener exists to avoid in the in-process
// half.
const (
	servingMarker   = "clustere2e-SERVING-VERSION"
	candidateMarker = "clustere2e-CANDIDATE-VERSION"
)

// zipFixtureWithMarker packs the storefront fixture with `marker` appended to
// its index document, so a served body says which version it is.
func zipFixtureWithMarker(t *testing.T, marker string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := filepath.Walk(storefrontFixtureDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(storefrontFixtureDir, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if filepath.ToSlash(rel) == "index.html" {
			body = append(body, []byte("\n<!-- "+marker+" -->\n")...)
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		_, err = w.Write(body)
		return err
	})
	if err != nil {
		t.Fatalf("pack %s with marker %s: %v", storefrontFixtureDir, marker, err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// previewGet issues one GET through the edge pod with an explicit Host header,
// carrying the preview cookie when token is non-empty.
//
// THE COOKIE IS SET DIRECTLY rather than by following the preview link, because
// this test dials the pod over http with a Host header while the link the mint
// composes is https on the real hostname -- and the Secure cookie the entry
// point sets would not survive the difference. The entry point's own behaviour
// is asserted separately below, and in full in the in-process half.
func previewGet(t *testing.T, port int, hostname, path, token string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", path, err)
	}
	req.Host = hostname
	if token != "" {
		req.AddCookie(&http.Cookie{Name: memqlengine.PreviewCookieName, Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s (Host %s): %v", path, hostname, err)
	}
	defer resp.Body.Close()
	body := readAllString(t, resp)
	return resp, body
}

func readAllString(t *testing.T, resp *http.Response) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// siteRefs reads the serving and candidate versions back off the row -- the one
// place this test learns what a publish actually wrote, rather than guessing a
// ref format it does not own.
func siteRefs(t *testing.T, ctx context.Context, qc *memqlclient.QueryClient, siteID string) (bundleRef, candidate string) {
	t.Helper()
	res, err := qc.SiteById(ctx, memqlclient.SiteByIdArgs{SiteId: siteID})
	if err != nil {
		t.Fatalf("siteById(%s): %v", siteID, err)
	}
	row := res.Single()
	if row == nil {
		t.Fatalf("siteById(%s) resolved no row", siteID)
	}
	return memqlclient.RowString(row, "bundleRef"), memqlclient.RowString(row, "candidateRef")
}

// publishVersion publishes one marked bundle and returns the ref it landed at.
func publishVersion(t *testing.T, ctx context.Context, tok string, qc *memqlclient.QueryClient, siteID, marker string) string {
	t.Helper()
	artifactID := uploadFixtureArtifact(t, ctx, tok, zipFixtureWithMarker(t, marker))
	if _, err := qc.SitePublishFromArtifact(ctx, memqlclient.SitePublishFromArtifactArgs{
		SiteId:     siteID,
		ArtifactId: artifactID,
	}); err != nil {
		t.Fatalf("sitePublishFromArtifact (%s): %v (does this cluster have object storage configured?)",
			marker, err)
	}
	ref, _ := siteRefs(t, ctx, qc, siteID)
	if strings.TrimSpace(ref) == "" {
		t.Fatalf("publishing %s left the site with no bundleRef", marker)
	}
	return ref
}

// grantTokenFrom pulls the plain token out of the URL sitePreviewOpen returns.
// It is the ONE time the token is readable; there is no row read that could
// recover it afterwards, which is the property being relied on here rather than
// worked around.
func grantTokenFrom(t *testing.T, res *memqlclient.Result) (grantID, token string) {
	t.Helper()
	row := res.Single()
	if row == nil {
		t.Fatal("sitePreviewOpen returned no result row")
	}
	raw := memqlclient.RowString(row, "url")
	if raw == "" {
		t.Fatalf("sitePreviewOpen returned no url: %v", row)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("sitePreviewOpen returned an unparseable url %q: %v", raw, err)
	}
	if u.Scheme != "https" {
		t.Errorf("the preview link is %s, not https -- a bearer credential must not travel in the clear", u.Scheme)
	}
	if u.Path != memqlengine.PreviewEnterPath {
		t.Errorf("the preview link lands on %q, want %q", u.Path, memqlengine.PreviewEnterPath)
	}
	tok := u.Query().Get(memqlengine.PreviewGrantParam)
	if !memqlengine.WellFormedPreviewToken(tok) {
		t.Fatalf("the preview link carries %q, which is not a well-formed preview token", tok)
	}
	return memqlclient.RowString(row, "grantId"), tok
}

func TestStorefrontPreview_ACandidateIsServedToItsGrantAndToNobodyElse(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 360*time.Second)
	defer cancel()

	podName := anEdgePod(t)
	pf := startPodForward(t, ctx, podName)
	t.Logf("edge replica = pod/%s (nodeId=%s)", pf.podName, pf.nodeID)

	conns := openConnections(ctx, t, tok, 1)
	defer conns[0].Close()
	qc := memqlclient.NewQueryClient(conns[0].Dispatcher())

	suffix := strings.ToLower(id.NewShortId())
	liveStoreID := "live-" + suffix
	devStoreID := "dev-" + suffix
	liveStoreHost := fmt.Sprintf("clustere2e-%s.myshopify.com", suffix)
	devStoreHost := fmt.Sprintf("clustere2e-dev-%s.myshopify.com", suffix)

	// TWO STORE ROWS, AND THE FLAG IS THE WHOLE DIFFERENCE. `isDevelopment` is a
	// fact about somebody else's system, recorded here when the store is
	// attached, and every guard on this path asks it of the store a site NAMES.
	if _, err := qc.CreateStore(ctx, memqlclient.CreateStoreArgs{
		StoreId:            liveStoreID,
		Domain:             liveStoreHost,
		Name:               "clustere2e preview, the store shoppers reach",
		StorefrontTokenRef: "clustere2e_storefront_token_absent",
	}); err != nil {
		t.Fatalf("createStore (needs a CLUSTER OWNER token): %v", err)
	}
	if _, err := qc.CreateStore(ctx, memqlclient.CreateStoreArgs{
		StoreId:              devStoreID,
		Domain:               devStoreHost,
		Name:                 "clustere2e preview, the development store",
		StorefrontTokenRef:   "clustere2e_dev_storefront_token_absent",
		IsDevelopment:        true,
		IsDevelopmentSet:     true,
		DevelopmentOfStoreId: liveStoreID,
	}); err != nil {
		t.Fatalf("createStore for the development store: %v", err)
	}

	hostname := fmt.Sprintf("clustere2e-preview-%s.example.com", suffix)
	siteID := "v1:platform:site:" + id.NewShortId()
	if _, err := qc.CreateSite(ctx, memqlclient.CreateSiteArgs{
		SiteId:   siteID,
		Hostname: hostname,
		Kind:     "shopify_storefront",
		Status:   "draft",
		Title:    "clustere2e storefront preview",
		Binding:  map[string]any{"storeId": liveStoreID},
	}); err != nil {
		t.Fatalf("createSite: %v", err)
	}
	if _, err := qc.UpdateSitePreviewBinding(ctx, memqlclient.UpdateSitePreviewBindingArgs{
		SiteId:  siteID,
		StoreId: devStoreID,
	}); err != nil {
		t.Fatalf("updateSitePreviewBinding to a development store was refused: %v", err)
	}

	// TWO VERSIONS, PUBLISHED IN ORDER, THEN THE OLDER ONE PUT BACK IN FRONT.
	// That last step is rollback, used here as setup -- which is exactly the
	// point being made: it is one updateSiteBundle write and needed nothing new
	// from this epic.
	servingRef := publishVersion(t, ctx, tok, qc, siteID, servingMarker)
	candidateRef := publishVersion(t, ctx, tok, qc, siteID, candidateMarker)
	if servingRef == candidateRef {
		t.Fatalf("two publishes landed at the same ref %q -- the two versions are "+
			"indistinguishable and every assertion below would be vacuous", servingRef)
	}
	if _, err := qc.UpdateSiteBundle(ctx, memqlclient.UpdateSiteBundleArgs{
		SiteId:    siteID,
		BundleRef: servingRef,
	}); err != nil {
		t.Fatalf("rolling back to the first version was refused: %v", err)
	}
	if _, err := qc.SetSiteCandidate(ctx, memqlclient.SetSiteCandidateArgs{
		SiteId:       siteID,
		CandidateRef: candidateRef,
	}); err != nil {
		t.Fatalf("setSiteCandidate: %v", err)
	}
	if got, cand := siteRefs(t, ctx, qc, siteID); got != servingRef || cand != candidateRef {
		t.Fatalf("after setup the row reads bundleRef=%q candidateRef=%q, want %q / %q",
			got, cand, servingRef, candidateRef)
	}

	// --- a draft deployable is still 404 for everybody ----------------------

	for _, path := range []string{"/", "/runtime-config.json"} {
		if r, b := previewGet(t, pf.port, hostname, path, ""); r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s on a DRAFT deployable answered %d, want 404 -- publishing a "+
				"candidate must not make a draft deployable visible: %.200s", path, r.StatusCode, b)
		}
	}

	// --- a grant is served the candidate, on the draft deployable -----------

	opened, err := qc.SitePreviewOpen(ctx, memqlclient.SitePreviewOpenArgs{SiteId: siteID})
	if err != nil {
		t.Fatalf("sitePreviewOpen: %v", err)
	}
	grantID, previewToken := grantTokenFrom(t, opened)
	t.Logf("preview grant = %s", grantID)

	resp, body := previewGet(t, pf.port, hostname, "/", previewToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a valid grant against a DRAFT deployable answered %d, want 200: %.200s",
			resp.StatusCode, body)
	}
	if !strings.Contains(body, candidateMarker) {
		t.Fatalf("a valid grant was not served the candidate version: %.400s", body)
	}
	if strings.Contains(body, servingMarker) {
		t.Fatalf("a valid grant was served the serving version: %.400s", body)
	}
	// ON THE SITE'S OWN ORIGIN. A storefront's cookies, its cart and Shopify's
	// checkout return all assume it; a redirect to anywhere else would break
	// the walk in ways that say nothing about the candidate.
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("the preview redirected to %q -- it must be served on the site's own origin", loc)
	}
	if rt := resp.Header.Get("X-Robots-Tag"); !strings.Contains(rt, "noindex") {
		t.Errorf("a previewed response carries X-Robots-Tag %q, want noindex", rt)
	}

	// --- the preview binding reaches a served document ONLY under a grant ---
	//
	// GREPPED OUT OF THE BYTES A REAL POD SENT. A struct assertion would miss
	// the development store's domain arriving through a route nobody modelled,
	// which is the whole reason the Admin token's absence is checked this way.

	_, publicCfg := previewGet(t, pf.port, hostname, "/runtime-config.json", "")
	_, previewCfg := previewGet(t, pf.port, hostname, "/runtime-config.json", previewToken)
	if strings.Contains(publicCfg, devStoreHost) {
		t.Errorf("the document served WITHOUT a grant names the development store %s:\n%s",
			devStoreHost, publicCfg)
	}
	if !strings.Contains(previewCfg, devStoreHost) {
		t.Errorf("the document served UNDER a grant does not name the development store %s:\n%s",
			devStoreHost, previewCfg)
	}
	if strings.Contains(previewCfg, liveStoreHost) {
		t.Errorf("the document served under a grant still names the store shoppers reach (%s) -- "+
			"the preview store must REPLACE it, never sit beside it:\n%s", liveStoreHost, previewCfg)
	}
	for _, served := range []string{publicCfg, previewCfg, body} {
		if strings.Contains(served, adminTokenPrefix) {
			t.Error("a served document carries something shaped like a Shopify ADMIN token")
		}
	}

	// THE POLICY FOLLOWS THE BINDING TOO. A previewed page whose policy still
	// named the live store could not call the development store's Storefront
	// API at all: the browser would refuse it under the page's own policy.
	previewPolicy := resp.Header.Get("Content-Security-Policy")
	if !cspAdmits(previewPolicy, "connect-src", "https://"+devStoreHost) {
		t.Errorf("the previewed policy does not admit the development store %s:\n%s",
			devStoreHost, previewPolicy)
	}
	if cspAdmits(previewPolicy, "connect-src", "https://"+liveStoreHost) {
		t.Errorf("the previewed policy still admits the store shoppers reach:\n%s", previewPolicy)
	}

	// --- go live, and the third row of the table ----------------------------
	//
	// THE REQUIREMENT THAT SHAPES THE WHOLE DESIGN: after publishing, the owner
	// keeps working and exercises version N+1 WHILE N goes on serving the
	// public. A design that previewed only a draft deployable would deliver the
	// sandbox once and never again.

	if _, err := qc.UpdateSiteStatus(ctx, memqlclient.UpdateSiteStatusArgs{
		SiteId: siteID,
		Status: "live",
	}); err != nil {
		t.Fatalf("going live bound to the store shoppers reach was refused: %v", err)
	}

	if r, b := previewGet(t, pf.port, hostname, "/", ""); r.StatusCode != http.StatusOK ||
		!strings.Contains(b, servingMarker) {
		t.Fatalf("a LIVE deployable carrying a candidate answered %d and served %.200s to an "+
			"unauthenticated request, want the serving version", r.StatusCode, b)
	}
	if _, b := previewGet(t, pf.port, hostname, "/", previewToken); !strings.Contains(b, candidateMarker) {
		t.Fatalf("a valid grant against a LIVE deployable was not served the candidate: %.400s", b)
	}
	// And the public answer is still the serving version AFTER a preview has
	// been served from the same replica -- the mutation bug, which a single
	// request per state cannot see.
	if _, b := previewGet(t, pf.port, hostname, "/", ""); !strings.Contains(b, servingMarker) {
		t.Fatalf("after a preview, an unauthenticated request was served %.200s -- the preview "+
			"reached the resolver's CACHED site instead of a copy of it", b)
	}

	// --- the entry point ----------------------------------------------------
	//
	// Asserted for its REDIRECT and its refusal to be an oracle. The cookie it
	// sets is Secure, so it cannot be followed over this http port-forward;
	// that half is covered in the in-process leg.

	enter := fmt.Sprintf("%s?%s=%s", memqlengine.PreviewEnterPath, memqlengine.PreviewGrantParam, previewToken)
	good, _ := previewGet(t, pf.port, hostname, enter, "")
	bad, _ := previewGet(t, pf.port, hostname,
		memqlengine.PreviewEnterPath+"?"+memqlengine.PreviewGrantParam+"=nonsense", "")
	if good.StatusCode != http.StatusSeeOther {
		t.Errorf("the preview entry point answered %d, want 303", good.StatusCode)
	}
	if good.StatusCode != bad.StatusCode ||
		good.Header.Get("Location") != bad.Header.Get("Location") {
		t.Errorf("a good token answered %d/%q and a bad one %d/%q -- the entry point is an "+
			"oracle for which tokens exist", good.StatusCode, good.Header.Get("Location"),
			bad.StatusCode, bad.Header.Get("Location"))
	}

	// --- promote, and what promotion does to the open preview ---------------

	if _, err := qc.PromoteSiteCandidate(ctx, memqlclient.PromoteSiteCandidateArgs{
		SiteId:       siteID,
		CandidateRef: candidateRef,
	}); err != nil {
		t.Fatalf("promoting the candidate was refused: %v", err)
	}
	afterBundle, afterCandidate := siteRefs(t, ctx, qc, siteID)
	if afterBundle != candidateRef {
		t.Errorf("after promotion bundleRef = %q, want the candidate %q", afterBundle, candidateRef)
	}
	if strings.TrimSpace(afterCandidate) != "" {
		t.Errorf("after promotion candidateRef = %q, want empty -- promotion is the flip, not a copy",
			afterCandidate)
	}
	if _, b := previewGet(t, pf.port, hostname, "/", ""); !strings.Contains(b, candidateMarker) {
		t.Errorf("after promotion the public is not served the promoted version: %.200s", b)
	}
	// THE GRANT NAMES THE VERSION IT WAS ISSUED FOR, so a promotion ends the
	// preview of it. Nobody is ever silently shown a version they did not ask
	// to see -- and the person is served the public site, which now happens to
	// be the same bytes, so the check that matters is that the candidate is
	// gone rather than that the body changed.
	if _, cand := siteRefs(t, ctx, qc, siteID); cand != "" {
		t.Errorf("the candidate survived promotion: %q", cand)
	}

	// --- rollback is ONE updateSiteBundle write -----------------------------

	if _, err := qc.UpdateSiteBundle(ctx, memqlclient.UpdateSiteBundleArgs{
		SiteId:    siteID,
		BundleRef: servingRef,
	}); err != nil {
		t.Fatalf("rolling back after a promotion was refused: %v", err)
	}
	rolledBundle, rolledCandidate := siteRefs(t, ctx, qc, siteID)
	if rolledBundle != servingRef {
		t.Errorf("after rollback bundleRef = %q, want %q", rolledBundle, servingRef)
	}
	if rolledCandidate != "" {
		t.Errorf("rollback wrote a candidate (%q) -- it is one row write and touches nothing else",
			rolledCandidate)
	}
	if _, b := previewGet(t, pf.port, hostname, "/", ""); !strings.Contains(b, servingMarker) {
		t.Errorf("after rollback the public is not served the previous version: %.200s", b)
	}

	// --- ending a preview ---------------------------------------------------

	if _, err := qc.RevokeSitePreviewGrant(ctx, memqlclient.RevokeSitePreviewGrantArgs{
		GrantId:   grantID,
		RevokedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("revokeSitePreviewGrant: %v", err)
	}
	// A SOFT END: the row stays, which is what answers "who opened a preview of
	// this, and when" after something unexpected turns up in a development
	// store. A filtered list would answer it for the last half hour only.
	listed, err := qc.SitePreviewGrantsForSite(ctx, memqlclient.SitePreviewGrantsForSiteArgs{SiteId: siteID})
	if err != nil {
		t.Fatalf("sitePreviewGrantsForSite: %v", err)
	}
	var found bool
	for _, row := range listed.Rows() {
		if strings.Contains(memqlclient.RowString(row, "id"), grantID) {
			found = true
			if memqlclient.RowString(row, "revokedAt") == "" {
				t.Error("the revoked grant carries no revokedAt")
			}
			// THE TOKEN DIGEST IS NOT PROJECTED. It is not itself a credential,
			// but putting the one stored artifact of a bearer token on every
			// screen that renders a grant buys nothing for any reader who needs
			// it.
			if _, ok := row["tokenHash"]; ok {
				t.Error("sitePreviewGrantFull projects the token digest")
			}
		}
	}
	if !found {
		t.Errorf("the revoked grant %s is not in the deployable's list -- a revoke must be a "+
			"soft end, since a deleted row answers nobody's question afterwards", grantID)
	}
}

// EACH GUARD CASE REFUSES WITH ITS TYPED CODE, against real rows.
//
// The code is what the OS keys its copy on when a refusal reaches the engine
// anyway -- because the readiness answer was stale, or because the caller never
// asked -- so a refusal that stopped naming it would leave a person with a
// generic error where a remedy used to be.
func TestStorefrontPreview_TheGuardRefusesEachCaseByName(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, 1)
	defer conns[0].Close()
	qc := memqlclient.NewQueryClient(conns[0].Dispatcher())

	suffix := strings.ToLower(id.NewShortId())
	liveStoreID := "guard-live-" + suffix
	devStoreID := "guard-dev-" + suffix
	if _, err := qc.CreateStore(ctx, memqlclient.CreateStoreArgs{
		StoreId: liveStoreID,
		Domain:  fmt.Sprintf("clustere2e-guard-%s.myshopify.com", suffix),
		Name:    "clustere2e guard, the store shoppers reach",
	}); err != nil {
		t.Fatalf("createStore (needs a CLUSTER OWNER token): %v", err)
	}
	if _, err := qc.CreateStore(ctx, memqlclient.CreateStoreArgs{
		StoreId:              devStoreID,
		Domain:               fmt.Sprintf("clustere2e-guard-dev-%s.myshopify.com", suffix),
		Name:                 "clustere2e guard, the development store",
		IsDevelopment:        true,
		IsDevelopmentSet:     true,
		DevelopmentOfStoreId: liveStoreID,
	}); err != nil {
		t.Fatalf("createStore for the development store: %v", err)
	}

	newSite := func(t *testing.T, storeID string) string {
		t.Helper()
		siteID := "v1:platform:site:" + id.NewShortId()
		if _, err := qc.CreateSite(ctx, memqlclient.CreateSiteArgs{
			SiteId:    siteID,
			Hostname:  fmt.Sprintf("clustere2e-guard-%s.example.com", strings.ToLower(id.NewShortId())),
			Kind:      "shopify_storefront",
			Status:    "draft",
			BundleRef: "blob://clustere2e/guard/v/1/",
			Title:     "clustere2e guard fixture",
			Binding:   map[string]any{"storeId": storeID},
		}); err != nil {
			t.Fatalf("createSite: %v", err)
		}
		return siteID
	}

	// A PREVIEW BINDING NAMING THE STORE SHOPPERS REACH. The failure it
	// prevents is silent: a preview against the live store looks exactly like a
	// preview until a test payment lands in the merchant's real orders.
	t.Run("preview_binding_is_not_development_store", func(t *testing.T) {
		siteID := newSite(t, liveStoreID)
		_, err := qc.UpdateSitePreviewBinding(ctx, memqlclient.UpdateSitePreviewBindingArgs{
			SiteId:  siteID,
			StoreId: liveStoreID,
		})
		requireRefusal(t, err, memqlengine.PreviewRefusalBindingIsNotDevelopment)

		// The reachable positive: the DEVELOPMENT store is accepted on the same
		// call, so the refusal above is the flag rather than a closed path.
		if _, err := qc.UpdateSitePreviewBinding(ctx, memqlclient.UpdateSitePreviewBindingArgs{
			SiteId:  siteID,
			StoreId: devStoreID,
		}); err != nil {
			t.Fatalf("pointing the preview binding at a development store was refused: %v", err)
		}
	})

	// A CANDIDATE THAT IS THE VERSION ALREADY SERVING is not a candidate: there
	// would be nothing to exercise, and promoting it would be a write that
	// changes nothing while reading like a release.
	t.Run("candidate_is_serving_version", func(t *testing.T) {
		siteID := newSite(t, liveStoreID)
		serving, _ := siteRefs(t, ctx, qc, siteID)
		_, err := qc.SetSiteCandidate(ctx, memqlclient.SetSiteCandidateArgs{
			SiteId:       siteID,
			CandidateRef: serving,
		})
		requireRefusal(t, err, memqlengine.PreviewRefusalCandidateIsServing)

		if _, err := qc.SetSiteCandidate(ctx, memqlclient.SetSiteCandidateArgs{
			SiteId:       siteID,
			CandidateRef: "blob://clustere2e/guard/v/2/",
		}); err != nil {
			t.Fatalf("setting a genuine candidate was refused: %v", err)
		}
	})

	// A PROMOTION MUST NAME THE CANDIDATE IT IS PROMOTING, or a candidate
	// republished between the reading and the click would be promoted by
	// surprise -- the rule the work spine's approvals use their artifact hash
	// for, and for the same reason.
	t.Run("candidate_moved", func(t *testing.T) {
		siteID := newSite(t, liveStoreID)
		if _, err := qc.SetSiteCandidate(ctx, memqlclient.SetSiteCandidateArgs{
			SiteId:       siteID,
			CandidateRef: "blob://clustere2e/guard/v/2/",
		}); err != nil {
			t.Fatalf("setSiteCandidate: %v", err)
		}
		_, err := qc.PromoteSiteCandidate(ctx, memqlclient.PromoteSiteCandidateArgs{
			SiteId:       siteID,
			CandidateRef: "blob://clustere2e/guard/v/9/",
		})
		requireRefusal(t, err, memqlengine.PreviewRefusalCandidateMoved)

		if _, err := qc.PromoteSiteCandidate(ctx, memqlclient.PromoteSiteCandidateArgs{
			SiteId:       siteID,
			CandidateRef: "blob://clustere2e/guard/v/2/",
		}); err != nil {
			t.Fatalf("promoting the stored candidate was refused: %v", err)
		}
	})

	// PROMOTING WITH NOTHING TO PROMOTE.
	t.Run("no_candidate_version", func(t *testing.T) {
		siteID := newSite(t, liveStoreID)
		_, err := qc.PromoteSiteCandidate(ctx, memqlclient.PromoteSiteCandidateArgs{
			SiteId:       siteID,
			CandidateRef: "blob://clustere2e/guard/v/2/",
		})
		requireRefusal(t, err, memqlengine.PreviewRefusalNoCandidate)
	})

	// GOING LIVE BOUND TO A DEVELOPMENT STORE. It would serve a catalog nobody
	// can buy from and take orders into a store that is not the merchant's --
	// and it would look like a successful launch while doing it.
	t.Run("serving_binding_is_development_store", func(t *testing.T) {
		siteID := newSite(t, devStoreID)
		_, err := qc.UpdateSiteStatus(ctx, memqlclient.UpdateSiteStatusArgs{
			SiteId: siteID,
			Status: "live",
		})
		requireRefusal(t, err, memqlengine.PreviewRefusalServingBindingIsDevelopment)

		// The reachable positive: re-bound to the store shoppers reach, the
		// SAME site goes live. Without it this subtest would pass against a
		// go-live path that had simply stopped working.
		if _, err := qc.UpdateSiteStoreBinding(ctx, memqlclient.UpdateSiteStoreBindingArgs{
			SiteId:  siteID,
			StoreId: liveStoreID,
		}); err != nil {
			t.Fatalf("re-binding to the store shoppers reach was refused: %v", err)
		}
		if _, err := qc.UpdateSiteStatus(ctx, memqlclient.UpdateSiteStatusArgs{
			SiteId: siteID,
			Status: "live",
		}); err != nil {
			t.Fatalf("going live bound to the store shoppers reach was refused: %v", err)
		}
	})

	// PROMOTING IS REFUSED BY THE SAME RULE, because it is the other act that
	// puts a version in front of shoppers. One question asked twice.
	t.Run("promotion is refused while the serving binding is a development store", func(t *testing.T) {
		siteID := newSite(t, devStoreID)
		if _, err := qc.SetSiteCandidate(ctx, memqlclient.SetSiteCandidateArgs{
			SiteId:       siteID,
			CandidateRef: "blob://clustere2e/guard/v/2/",
		}); err != nil {
			t.Fatalf("setSiteCandidate on a storefront bound to a development store was refused "+
				"-- preparing a version is not the act this guard is about: %v", err)
		}
		_, err := qc.PromoteSiteCandidate(ctx, memqlclient.PromoteSiteCandidateArgs{
			SiteId:       siteID,
			CandidateRef: "blob://clustere2e/guard/v/2/",
		})
		requireRefusal(t, err, memqlengine.PreviewRefusalServingBindingIsDevelopment)
	})

	// OPENING A PREVIEW IS REFUSED WHEN THERE IS NOTHING TO PREVIEW, and when
	// there is nothing to preview it AGAINST. Both are gates that run before
	// anything is minted.
	t.Run("sitePreviewOpen refuses before it mints", func(t *testing.T) {
		siteID := newSite(t, liveStoreID)
		if _, err := qc.SitePreviewOpen(ctx, memqlclient.SitePreviewOpenArgs{SiteId: siteID}); err == nil {
			t.Error("a deployable with no candidate opened a preview")
		} else if !strings.Contains(err.Error(), "no candidate version") {
			t.Errorf("the refusal does not say there is no candidate: %v", err)
		}

		if _, err := qc.SetSiteCandidate(ctx, memqlclient.SetSiteCandidateArgs{
			SiteId:       siteID,
			CandidateRef: "blob://clustere2e/guard/v/2/",
		}); err != nil {
			t.Fatalf("setSiteCandidate: %v", err)
		}
		if _, err := qc.SitePreviewOpen(ctx, memqlclient.SitePreviewOpenArgs{SiteId: siteID}); err == nil {
			t.Error("a storefront with no development store attached opened a preview")
		} else if !strings.Contains(err.Error(), "development store") {
			t.Errorf("the refusal does not name the development store: %v", err)
		}
	})

	// THE READINESS READ ANSWERS IN THE GUARD'S OWN WORDS, which is what lets
	// the OS make an illegal act ABSENT rather than drawing it and having it
	// fail. It DECIDES nothing -- every act it describes is independently
	// refused above -- so what is asserted is that the two agree.
	t.Run("readiness reports the same refusal the guard raises", func(t *testing.T) {
		siteID := newSite(t, devStoreID)
		res, err := qc.SitePreviewReadiness(ctx, memqlclient.SitePreviewReadinessArgs{SiteId: siteID})
		if err != nil {
			t.Fatalf("sitePreviewReadiness: %v", err)
		}
		row := res.Single()
		if row == nil {
			t.Fatal("sitePreviewReadiness returned no row")
		}
		if memqlclient.RowBool(row, "canGoLive") {
			t.Error("readiness says a storefront bound to a development store may go live, " +
				"which the guard refuses -- the OS would draw a control that cannot work")
		}
		goLive := memqlclient.RowObject(row, "goLiveRefusal")
		if code := memqlclient.RowString(goLive, "code"); code != memqlengine.PreviewRefusalServingBindingIsDevelopment {
			t.Errorf("readiness reports goLiveRefusal.code = %q, want %q", code,
				memqlengine.PreviewRefusalServingBindingIsDevelopment)
		}
		if memqlclient.RowString(goLive, "remedy") == "" {
			t.Error("readiness reports a refusal with no remedy -- a refusal naming only what is " +
				"wrong leaves an operator with nothing to do")
		}
		if memqlclient.RowBool(row, "canPreview") {
			t.Error("readiness says a storefront with no candidate and no development store may " +
				"be previewed")
		}
	})
}

// requireRefusal asserts that err is a refusal naming code.
//
// THE CODE IS THE ASSERTION, not the sentence. The wording is allowed to
// improve; the code is what the OS matches on, and a refusal that stopped
// carrying it would leave a person with a generic error where a remedy used to
// be.
func requireRefusal(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the write was allowed; it should have been refused %s", code)
	}
	if !strings.Contains(err.Error(), code) {
		t.Errorf("the refusal does not name %q: %v", code, err)
	}
}
