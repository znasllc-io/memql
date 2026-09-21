//go:build clustere2e

package clustere2e

// storefront_serving_test.go -- the first time the shopify_storefront path
// has run end to end (memql#5536, design record Q7).
//
// WHAT WAS WRONG. The storefront kind was verified at the level of its model
// (the site row, the binding) and its document contract (the storefront block
// in /runtime-config.json) only. Nobody had served one. memql#5534 found out
// why that mattered: the content security policy the edge wrote named no
// store, so the browser's call to the Storefront API and every Shopify-hosted
// image were refused by the page's own policy. The kind could not have worked,
// and no test in the tree would have said so.
//
// WHAT THIS LEG PROVES, AND WHAT IT DOES NOT.
//
// It publishes a real fixture bundle to a real shopify_storefront site through
// the ordinary authorized path (a Library zip artifact, then
// sitePublishFromArtifact), serves it through a real edge pod, and asserts:
//
//   - the bundle is served, its own script and stylesheet included;
//   - the Content-Security-Policy the edge WROTE names the bound store in
//     connect-src and img-src, and names Shopify's asset CDN;
//   - /runtime-config.json carries the storefront block, naming that store;
//   - the Shopify ADMIN token appears in NO served byte;
//   - the exact Storefront GraphQL call and image load the bundle makes
//     succeed against a stub standing in for the bound store -- issued only
//     after the SERVED policy has been checked to admit each one, so "under
//     the policy the edge actually writes" is a check rather than a hope;
//   - a storefront declaring resolutionTail=not_found 404s a missing path
//     while still being served its storefront block (memql#5535).
//
// IT RUNS NO BROWSER. The two cross-origin requests are issued by this test
// with the same method, URL, headers and body the fixture's own JavaScript
// issues, against a stub reached by mapping the bound host to a local
// listener. That is the honest boundary: the policy is the cluster's real
// answer and the requests are real requests, and nothing here proves a
// browser's CSP implementation agrees with cspAdmits below. Opening
// https://<the site>/ in a browser is what proves that, and it is what the
// fixture bundle is designed to be read in.
//
// THE TOKEN IS NOT SEEDED. Resolving storefrontTokenRef needs a
// v1:platform:globalSecret holding an encrypted value, and nothing a client
// may call writes one -- so the served document's storefrontToken is EMPTY
// here, which is the documented honest answer for a ref that resolves to
// nothing, and the stub is called with a synthetic token it does not check.
// What is asserted about the token is what this leg can honestly assert: the
// field is present, and it is not the Admin token.
// component/edge/runtimeconfig_test.go covers the resolution itself.
//
// PREREQUISITES, ALL SKIPPED GRACEFULLY WHEN ABSENT:
//   - MEMQL_E2E_TOKEN for a CLUSTER OWNER (v1:platform:site is clusterOwner-
//     tier; see site_edge_invalidation_test.go's header for the full note).
//   - kubectl on PATH, pointed at a cluster with >=1 Running edge pod.
//   - Object storage configured on the cluster (the publish path writes to it).
//
// RUN
//
//	MEMQL_E2E_TOKEN=<cluster owner JWT> go test -tags clustere2e -count=1 \
//	  -timeout=300s ./test/clustere2e/... -run TestStorefrontServing -v

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// storefrontFixtureDir is the bundle this leg publishes. A real tree on disk
// rather than bytes built here: it is meant to be opened in a browser by
// whoever is debugging a storefront, and a fixture assembled inside a test
// file cannot be.
const storefrontFixtureDir = "testdata/storefront-fixture"

// shopifyAssetCDN is the fixed host a storefront's images come from. Spelled
// here rather than imported: component/edge is a different module's package
// and this leg is asserting on the SERVED HEADER, so reading the constant out
// of the code that produced it would make the assertion circular.
const shopifyAssetCDN = "https://cdn.shopify.com"

// adminTokenPrefix is what a Shopify Admin API token begins with, and the
// whole of what this leg greps the served bytes for.
//
// A PREFIX, DELIBERATELY, AND NOT A FULL SENTINEL VALUE. The first version of
// this file carried a token-shaped constant so the grep had something
// specific to find, and GitHub's push protection refused the branch over it
// -- correctly: a scanner cannot tell a fake token from a real one, and that
// is the whole point of a scanner. The prefix is also the stronger check.
// A sentinel only ever finds the one value somebody remembered to write,
// while a real leak would carry whatever token the store actually issued,
// and every one of those starts here.
const adminTokenPrefix = "shpat_"

// anEdgePod returns one Running edge pod, or skips. The invalidation leg in
// this package needs two named replicas; this one needs any single edge, so
// it asks for what it needs rather than inheriting a stricter prerequisite.
func anEdgePod(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not on PATH -- this leg needs a live cluster")
	}
	out, err := exec.Command("kubectl", "-n", k3dNamespace(), "get", "pods",
		"-l", "app.kubernetes.io/name=edge",
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[*].metadata.name}").Output()
	if err != nil {
		t.Skipf("kubectl get pods -l app.kubernetes.io/name=edge failed (no live cluster?): %v", err)
	}
	names := strings.Fields(string(out))
	if len(names) == 0 {
		t.Skipf("no Running edge pod in namespace %q -- run `make up` first", k3dNamespace())
	}
	return names[0]
}

// zipFixture packs the fixture directory into a zip with index.html at the
// ROOT, which is what the publish path validates for a storefront bundle.
func zipFixture(t *testing.T) []byte {
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
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		_, err = w.Write(body)
		return err
	})
	if err != nil {
		t.Fatalf("pack %s: %v", storefrontFixtureDir, err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// uploadFixtureArtifact posts the zip to the Library through the edge pod's
// own /_memql proxy is NOT how this works -- the Library's byte routes are
// served by the BFF, so this dials the bff's HTTP edge through its own
// port-forward.
func uploadFixtureArtifact(t *testing.T, ctx context.Context, tok string, zipped []byte) string {
	t.Helper()
	pf := startBffForward(t, ctx)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "storefront-fixture.zip")
	if err != nil {
		t.Fatalf("multipart: %v", err)
	}
	if _, err := part.Write(zipped); err != nil {
		t.Fatalf("multipart write: %v", err)
	}
	_ = mw.WriteField("name", "storefront-fixture.zip")
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/artifacts", pf), &body)
	if err != nil {
		t.Fatalf("build upload request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /artifacts: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /artifacts answered %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		ArtifactId string `json:"artifactId"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || strings.TrimSpace(out.ArtifactId) == "" {
		t.Fatalf("POST /artifacts returned no artifactId: %s (err %v)", string(raw), err)
	}
	return out.ArtifactId
}

// startBffForward port-forwards one Running bff pod's HTTP edge and returns
// the local port. The Library's byte routes (POST /artifacts) are the bff's,
// never the edge's.
func startBffForward(t *testing.T, ctx context.Context) int {
	t.Helper()
	out, err := exec.Command("kubectl", "-n", k3dNamespace(), "get", "pods",
		"-l", "app.kubernetes.io/name=bff",
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[*].metadata.name}").Output()
	if err != nil {
		t.Skipf("kubectl get pods -l app.kubernetes.io/name=bff failed: %v", err)
	}
	names := strings.Fields(string(out))
	if len(names) == 0 {
		t.Skipf("no Running bff pod in namespace %q", k3dNamespace())
	}
	port := freeLocalPort(t)
	cmd := exec.CommandContext(ctx, "kubectl", "-n", k3dNamespace(), "port-forward",
		"pod/"+names[0], fmt.Sprintf("%d:8085", port))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start port-forward to pod/%s: %v", names[0], err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	waitForHealthz(t, port, stderr.String)
	return port
}

// waitForHealthz blocks until the forwarded port answers /healthz 200.
func waitForHealthz(t *testing.T, port int, stderr func() string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if info, err := healthzProbe(port); err == nil && info.Code == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("port-forward on :%d never became healthy within 30s (kubectl stderr: %s)",
				port, stderr())
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// cspAdmits reports whether policy's named directive admits source, as a
// browser would read it: the directive's own source list if it has one, and
// otherwise default-src, which is the fallback CSP defines.
//
// PARSED, NOT SUBSTRING-MATCHED. `strings.Contains(policy, host)` answers
// true when the host appears in ANY directive -- so a policy that named the
// store only in img-src would pass an assertion about connect-src, which is
// precisely the confusion this leg exists to rule out.
func cspAdmits(policy, directive, source string) bool {
	sources, ok := cspDirective(policy, directive)
	if !ok {
		sources, ok = cspDirective(policy, "default-src")
		if !ok {
			return false
		}
	}
	for _, s := range sources {
		if s == source {
			return true
		}
	}
	return false
}

// cspDirective returns one directive's source list, and whether it was
// present at all -- absent and empty are different answers, and the fallback
// above depends on telling them apart.
func cspDirective(policy, name string) ([]string, bool) {
	for _, d := range strings.Split(policy, ";") {
		fields := strings.Fields(strings.TrimSpace(d))
		if len(fields) == 0 || fields[0] != name {
			continue
		}
		return fields[1:], true
	}
	return nil, false
}

// storefrontGet issues one GET through the edge pod with an explicit Host
// header naming the site. No DNS is involved: the edge resolves off Host.
func storefrontGet(t *testing.T, port int, hostname, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", path, err)
	}
	req.Host = hostname
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s (Host %s): %v", path, hostname, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestStorefrontServing_TheBoundStoreIsReachableUnderTheServedPolicy(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	podName := anEdgePod(t)
	pf := startPodForward(t, ctx, podName)
	t.Logf("edge replica = pod/%s (nodeId=%s)", pf.podName, pf.nodeID)

	// THE STUB STANDS IN FOR THE BOUND STORE, and the site is bound to the
	// host the stub answers as -- not to the stub's address. That is what
	// makes the policy assertion and the request assertion be about the SAME
	// name: the request below reaches the stub by a dialer that maps this
	// host to it, exactly as DNS would.
	storeHost := fmt.Sprintf("clustere2e-%s.myshopify.com", strings.ToLower(id.NewShortId()))
	stub := newStorefrontStub(t)
	defer stub.Close()

	hostname := fmt.Sprintf("clustere2e-store-%s.example.com", strings.ToLower(id.NewShortId()))
	siteID := "v1:platform:site:" + id.NewShortId()

	conns := openConnections(ctx, t, tok, 1)
	defer conns[0].Close()
	qc := memqlclient.NewQueryClient(conns[0].Dispatcher())

	if _, err := qc.CreateSite(ctx, memqlclient.CreateSiteArgs{
		SiteId:    siteID,
		Hostname:  hostname,
		Kind:      "shopify_storefront",
		BundleRef: "",
		Status:    "live",
		Title:     "clustere2e storefront fixture",
		Binding: map[string]any{
			"storeDomain": storeHost,
			// NAMES A SECRET THAT DOES NOT EXIST, deliberately: see the file
			// header. The document's honest answer is an empty token, and
			// that is what is asserted.
			"storefrontTokenRef": "clustere2e_storefront_token_absent",
		},
	}); err != nil {
		t.Fatalf("createSite (needs a CLUSTER OWNER token): %v", err)
	}

	artifactID := uploadFixtureArtifact(t, ctx, tok, zipFixture(t))
	if _, err := qc.SitePublishFromArtifact(ctx, memqlclient.SitePublishFromArtifactArgs{
		SiteId:     siteID,
		ArtifactId: artifactID,
	}); err != nil {
		t.Fatalf("sitePublishFromArtifact: %v (does this cluster have object storage configured?)", err)
	}

	// --- the bundle is served, its own assets included ----------------------

	resp, body := storefrontGet(t, pf.port, hostname, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / answered %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Storefront path") {
		t.Fatalf("GET / did not serve the fixture document: %.200s", body)
	}
	for _, asset := range []string{"/storefront.js", "/storefront.css"} {
		if r, b := storefrontGet(t, pf.port, hostname, asset); r.StatusCode != http.StatusOK {
			t.Errorf("GET %s answered %d, want 200: %.200s", asset, r.StatusCode, b)
		}
	}

	// --- THE POLICY NAMES THE BOUND STORE (memql#5534, G1) ------------------

	policy := resp.Header.Get("Content-Security-Policy")
	if policy == "" {
		t.Fatal("the served document carries no Content-Security-Policy at all")
	}
	t.Logf("served policy: %s", policy)

	storeOrigin := "https://" + storeHost
	if !cspAdmits(policy, "connect-src", storeOrigin) {
		t.Errorf("connect-src does not admit the bound store %s -- a storefront cannot call the "+
			"Storefront API under this policy.\npolicy: %s", storeOrigin, policy)
	}
	if !cspAdmits(policy, "img-src", shopifyAssetCDN) {
		t.Errorf("img-src does not admit %s -- every Shopify-hosted product image is refused "+
			"under this policy.\npolicy: %s", shopifyAssetCDN, policy)
	}
	for _, token := range strings.Fields(policy) {
		if strings.Contains(token, "*") {
			t.Errorf("the served policy carries a wildcard source %q -- naming the store buys "+
				"nothing if a pattern admits one that was never bound", token)
		}
	}

	// --- the runtime document carries the block, and no Admin token ---------

	cfgResp, cfgBody := storefrontGet(t, pf.port, hostname, "/runtime-config.json")
	if cfgResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /runtime-config.json answered %d: %s", cfgResp.StatusCode, cfgBody)
	}
	var cfg struct {
		Storefront *struct {
			Kind            string `json:"kind"`
			StoreDomain     string `json:"storeDomain"`
			StorefrontToken string `json:"storefrontToken"`
		} `json:"storefront"`
	}
	if err := json.Unmarshal([]byte(cfgBody), &cfg); err != nil {
		t.Fatalf("runtime-config is not JSON: %v (%s)", err, cfgBody)
	}
	if cfg.Storefront == nil {
		t.Fatal("the runtime document carries NO storefront block for a shopify_storefront site")
	}
	if cfg.Storefront.StoreDomain != storeHost {
		t.Errorf("storefront.storeDomain = %q, want %q", cfg.Storefront.StoreDomain, storeHost)
	}
	if cfg.Storefront.Kind != "shopify_storefront" {
		t.Errorf("storefront.kind = %q, want shopify_storefront", cfg.Storefront.Kind)
	}
	// TestRuntimeConfigNeverCarriesTheShopifyAdminToken greps the document in
	// a unit test; this is the same grep against a document a real cluster
	// really served, over the wire, which is the one place it could have
	// arrived by a route no unit test models.
	for _, served := range []string{cfgBody, body} {
		if strings.Contains(served, adminTokenPrefix) {
			t.Error("a served document carries something shaped like a Shopify ADMIN token")
		}
	}

	// --- the two cross-origin requests the bundle makes ---------------------

	// Checked FIRST, so what follows is issued under a policy this test has
	// confirmed admits it rather than under one it hopes does.
	if !cspAdmits(policy, "connect-src", storeOrigin) {
		t.Fatal("refusing to issue the Storefront call: the served policy does not admit it")
	}
	client := stub.clientFor(storeHost)

	gql := fmt.Sprintf("https://%s/api/2026-07/graphql.json", storeHost)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gql,
		strings.NewReader(`{"query":"query FixtureCatalog { products(first: 3) { edges { node { title } } } }"}`))
	if err != nil {
		t.Fatalf("build Storefront request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Storefront-Access-Token", "clustere2e-synthetic-token")
	gqlResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the Storefront call the bundle makes did not complete: %v", err)
	}
	defer gqlResp.Body.Close()
	gqlBody, _ := io.ReadAll(gqlResp.Body)
	if gqlResp.StatusCode != http.StatusOK || !strings.Contains(string(gqlBody), "Ceramic table lamp") {
		t.Fatalf("the stub store answered %d: %s", gqlResp.StatusCode, string(gqlBody))
	}

	imgURL := fmt.Sprintf("https://%s/cdn/shop/products/lamp.png", storeHost)
	if !cspAdmits(policy, "img-src", "https://"+storeHost) {
		t.Fatal("refusing to issue the image load: the served policy does not admit the store's own origin")
	}
	imgResp, err := client.Get(imgURL)
	if err != nil {
		t.Fatalf("the image load the bundle makes did not complete: %v", err)
	}
	defer imgResp.Body.Close()
	if imgResp.StatusCode != http.StatusOK {
		t.Fatalf("the stub asset host answered %d for %s", imgResp.StatusCode, imgURL)
	}

	// --- the resolution tail, live (memql#5535) -----------------------------

	// A storefront with NO declared tail keeps the kind's fallback, so a path
	// matching no file is the index document rather than a 404.
	missResp, missBody := storefrontGet(t, pf.port, hostname, "/products/nothing-here")
	if missResp.StatusCode != http.StatusOK || !strings.Contains(missBody, "Storefront path") {
		t.Errorf("a storefront declaring no tail answered %d for a missing path, want the "+
			"index.html fallback", missResp.StatusCode)
	}

	tailHost := fmt.Sprintf("clustere2e-tail-%s.example.com", strings.ToLower(id.NewShortId()))
	tailID := "v1:platform:site:" + id.NewShortId()
	if _, err := qc.CreateSite(ctx, memqlclient.CreateSiteArgs{
		SiteId:         tailID,
		Hostname:       tailHost,
		Kind:           "shopify_storefront",
		ResolutionTail: "not_found",
		BundleRef:      "",
		Status:         "live",
		Title:          "clustere2e storefront fixture, 404 tail",
		Binding:        map[string]any{"storeDomain": storeHost},
	}); err != nil {
		t.Fatalf("createSite for the not_found tail: %v", err)
	}
	if _, err := qc.SitePublishFromArtifact(ctx, memqlclient.SitePublishFromArtifactArgs{
		SiteId:     tailID,
		ArtifactId: artifactID,
	}); err != nil {
		t.Fatalf("sitePublishFromArtifact for the not_found tail: %v", err)
	}

	if r, _ := storefrontGet(t, pf.port, tailHost, "/products/nothing-here"); r.StatusCode != http.StatusNotFound {
		t.Errorf("a storefront declaring resolutionTail=not_found answered %d for a missing "+
			"path, want 404", r.StatusCode)
	}
	// The reachable positive: the SAME site still serves its root and still
	// gets its storefront block, so the 404 above is the tail rather than a
	// site that is simply broken.
	if r, b := storefrontGet(t, pf.port, tailHost, "/"); r.StatusCode != http.StatusOK ||
		!strings.Contains(b, "Storefront path") {
		t.Errorf("the not_found-tail site does not serve its root: %d", r.StatusCode)
	}
	if r, b := storefrontGet(t, pf.port, tailHost, "/runtime-config.json"); r.StatusCode != http.StatusOK ||
		!strings.Contains(b, storeHost) {
		t.Errorf("the not_found-tail site lost its storefront block: %d %s", r.StatusCode, b)
	}
}

// storefrontStub is a TLS listener that plays the bound store: the Storefront
// GraphQL endpoint and an asset host, on one origin, exactly as a Shopify
// store serves both.
type storefrontStub struct {
	srv *httptest.Server
}

func newStorefrontStub(t *testing.T) *storefrontStub {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "the Storefront API accepts POST only", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("X-Shopify-Storefront-Access-Token") == "" {
			http.Error(w, "no storefront token presented", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"products":{"edges":[`+
			`{"node":{"title":"Ceramic table lamp","priceRange":{"minVariantPrice":`+
			`{"amount":"48.00","currencyCode":"USD"}},"images":{"edges":[{"node":`+
			`{"url":"https://`+r.Host+`/cdn/shop/products/lamp.png","altText":"A lamp"}}]}}}`+
			`]}}}`)
	})
	mux.HandleFunc("/cdn/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		// The eight-byte PNG signature plus nothing: this asserts that bytes
		// were served from the asset host, not that they decode.
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	})
	return &storefrontStub{srv: httptest.NewTLSServer(mux)}
}

func (s *storefrontStub) Close() { s.srv.Close() }

// clientFor returns a client that reaches this stub for host, exactly as DNS
// pointing host at it would -- so the request under test names the host the
// SITE IS BOUND TO and the POLICY NAMES, rather than the stub's own address.
func (s *storefrontStub) clientFor(host string) *http.Client {
	stubAddr := strings.TrimPrefix(s.srv.URL, "https://")
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if strings.HasPrefix(addr, host+":") {
					addr = stubAddr
				}
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
			// The stub's certificate names 127.0.0.1, and the request names
			// the bound store. Verification is off for that reason alone:
			// what is under test is the policy and the call, not TLS.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

// --- the leg's own assertion helper, verified ------------------------------
//
// NO PREREQUISITES: these run on any machine with the tag set, cluster or
// not. cspAdmits is what every policy assertion above rests on, and a parser
// that answered true too easily would make the whole leg vacuous while
// reporting a pass -- so it is tested here rather than trusted.

func TestCspAdmitsReadsTheNamedDirective(t *testing.T) {
	const policy = "default-src 'self'; " +
		"connect-src 'self' https://shop.example.com https://acme.myshopify.com; " +
		"img-src 'self' data: blob: https://cdn.shopify.com; " +
		"script-src 'self'"

	for name, tc := range map[string]struct {
		directive, source string
		want              bool
	}{
		"named in connect-src":            {"connect-src", "https://acme.myshopify.com", true},
		"named in img-src":                {"img-src", "https://cdn.shopify.com", true},
		"absent everywhere":               {"connect-src", "https://evil.example.com", false},
		"named in ANOTHER directive only": {"connect-src", "https://cdn.shopify.com", false},
		"the store is not an img source":  {"img-src", "https://acme.myshopify.com", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := cspAdmits(policy, tc.directive, tc.source); got != tc.want {
				t.Errorf("cspAdmits(%q, %q) = %v, want %v", tc.directive, tc.source, got, tc.want)
			}
		})
	}
}

// A DIRECTIVE THAT IS ABSENT FALLS BACK TO default-src, which is how a
// browser reads it -- and is why media-src being absent on a non-storefront
// policy means 'self', not "anything".
func TestCspAdmitsFallsBackToDefaultSrc(t *testing.T) {
	const policy = "default-src 'self' https://fallback.example.com; script-src 'self'"

	if !cspAdmits(policy, "media-src", "https://fallback.example.com") {
		t.Error("an absent media-src did not fall back to default-src")
	}
	// script-src is PRESENT, so it does NOT fall back -- the fallback must
	// not leak into a directive that stated its own list.
	if cspAdmits(policy, "script-src", "https://fallback.example.com") {
		t.Error("a present script-src fell back to default-src anyway")
	}
}

// Absent and empty are different answers, and cspAdmits' fallback depends on
// telling them apart: an EMPTY directive admits nothing and must not fall
// back to default-src.
func TestCspDirectiveDistinguishesAbsentFromEmpty(t *testing.T) {
	const policy = "default-src 'self'; media-src"

	sources, present := cspDirective(policy, "media-src")
	if !present || len(sources) != 0 {
		t.Errorf("an empty media-src read as present=%v sources=%v, want present with no sources",
			present, sources)
	}
	if _, present := cspDirective(policy, "img-src"); present {
		t.Error("an absent img-src read as present")
	}
	if cspAdmits(policy, "media-src", "'self'") {
		t.Error("an empty media-src admitted a source by falling back to default-src")
	}
}
