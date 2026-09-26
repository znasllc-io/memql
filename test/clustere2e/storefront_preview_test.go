//go:build clustere2e

package clustere2e

// Storefront destination integration: one build, two stable origins and
// independent stores, through real authorized mutations and a real edge pod.
// No Shopify endpoint is contacted. MEMQL_E2E_TOKEN and a cluster with object
// storage are required. Run with -tags clustere2e -run TestStorefrontPreview.

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

func TestStorefrontPreview_TwoDestinationsShareOneBuild(t *testing.T) {
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

	servingRef := publishVersion(t, ctx, tok, qc, siteID, servingMarker)
	testingHost := "test--" + hostname
	opened, err := qc.SitePreviewOpen(ctx, memqlclient.SitePreviewOpenArgs{SiteId: siteID})
	if err != nil {
		t.Fatal(err)
	}
	grantID, accessToken := grantTokenFrom(t, opened)
	entry, _ := url.Parse(memqlclient.RowString(opened.Single(), "url"))
	if entry.Host != testingHost {
		t.Fatalf("wrong testing origin: %s", entry.Host)
	}
	if response, _ := previewGet(t, pf.port, hostname, "/", ""); response.StatusCode != http.StatusNotFound {
		t.Fatal("draft Production is public")
	}
	if response, _ := previewGet(t, pf.port, testingHost, "/runtime-config.json", ""); response.StatusCode != http.StatusUnauthorized {
		t.Fatal("Testing exposed without access")
	}
	if response, body := previewGet(t, pf.port, testingHost, "/", accessToken); response.StatusCode != http.StatusOK || !strings.Contains(body, servingMarker) {
		t.Fatalf("Testing did not show shared design: %d", response.StatusCode)
	}
	if _, err := qc.UpdateSiteStatus(ctx, memqlclient.UpdateSiteStatusArgs{SiteId: siteID, Status: "live"}); err != nil {
		t.Fatal(err)
	}
	check := func(marker string) {
		t.Helper()
		for _, host := range []string{hostname, testingHost} {
			deadline := time.Now().Add(15 * time.Second)
			for {
				response, body := previewGet(t, pf.port, host, "/", accessToken)
				if response.StatusCode == http.StatusOK && strings.Contains(body, marker) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("shared build did not reach %s: %d", host, response.StatusCode)
				}
				time.Sleep(200 * time.Millisecond)
			}
			response, cfg := previewGet(t, pf.port, host, "/runtime-config.json", accessToken)
			want, absent := liveStoreHost, devStoreHost
			if host == testingHost {
				want, absent = devStoreHost, liveStoreHost
			}
			if !strings.Contains(cfg, want) || strings.Contains(cfg, absent) || strings.Contains(cfg, adminTokenPrefix) {
				t.Fatalf("wrong catalog or leaked credential on %s", host)
			}
			if !cspAdmits(response.Header.Get("Content-Security-Policy"), "connect-src", "https://"+want) {
				t.Fatalf("wrong store policy on %s", host)
			}
			if host == testingHost && (!strings.Contains(response.Header.Get("Cache-Control"), "no-store") || !strings.Contains(response.Header.Get("X-Robots-Tag"), "noindex")) {
				t.Fatal("Testing is cacheable/indexable")
			}
		}
	}
	check(servingMarker)
	publishVersion(t, ctx, tok, qc, siteID, candidateMarker)
	check(candidateMarker)
	if _, err := qc.UpdateSiteBundle(ctx, memqlclient.UpdateSiteBundleArgs{SiteId: siteID, BundleRef: servingRef}); err != nil {
		t.Fatal(err)
	}
	check(servingMarker)
	enter := fmt.Sprintf("%s?%s=%s", memqlengine.PreviewEnterPath, memqlengine.PreviewGrantParam, accessToken)
	response, _ := previewGet(t, pf.port, testingHost, enter, "")
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/" {
		t.Fatal("Testing entry did not land on stable URL")
	}
	if _, err := qc.RevokeSitePreviewGrant(ctx, memqlclient.RevokeSitePreviewGrantArgs{GrantId: grantID, RevokedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(25 * time.Second)
	for {
		response, _ := previewGet(t, pf.port, testingHost, "/runtime-config.json", accessToken)
		if response.StatusCode == http.StatusUnauthorized {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("revoked Testing access still works")
		}
		time.Sleep(time.Second)
	}
}

func TestStorefrontPreview_BothDestinationsAcceptTheSameSandbox(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	connections := openConnections(ctx, t, tok, 1)
	defer connections[0].Close()
	qc := memqlclient.NewQueryClient(connections[0].Dispatcher())
	suffix := strings.ToLower(id.NewShortId())
	storeID, siteID := "sandbox-"+suffix, "v1:platform:site:"+id.NewShortId()
	if _, err := qc.CreateStore(ctx, memqlclient.CreateStoreArgs{StoreId: storeID, Domain: storeID + ".myshopify.com", IsDevelopment: true, IsDevelopmentSet: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := qc.CreateSite(ctx, memqlclient.CreateSiteArgs{SiteId: siteID, Hostname: "clustere2e-" + suffix + ".example.com", Kind: "shopify_storefront", Status: "draft", BundleRef: "blob://sites/shared/v1/", Binding: map[string]any{"storeId": storeID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := qc.UpdateSitePreviewBinding(ctx, memqlclient.UpdateSitePreviewBindingArgs{SiteId: siteID, StoreId: storeID}); err != nil {
		t.Fatal(err)
	}
	if _, err := qc.UpdateSiteStatus(ctx, memqlclient.UpdateSiteStatusArgs{SiteId: siteID, Status: "live"}); err != nil {
		t.Fatal(err)
	}
	result, err := qc.SitePreviewReadiness(ctx, memqlclient.SitePreviewReadinessArgs{SiteId: siteID})
	if err != nil {
		t.Fatal(err)
	}
	row := result.Single()
	if !memqlclient.RowBool(row, "canPreview") || !memqlclient.RowBool(row, "canGoLive") || memqlclient.RowBool(row, "canPromote") {
		t.Fatalf("incorrect destination readiness: %v", row)
	}
	if _, err := qc.UpdateSitePreviewBinding(ctx, memqlclient.UpdateSitePreviewBindingArgs{SiteId: siteID, StoreId: "missing-" + suffix}); err == nil {
		t.Fatal("unreadable store accepted")
	}
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
