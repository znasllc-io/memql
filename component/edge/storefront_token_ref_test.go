package edge

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The edge publishes a store's Storefront token to every visitor, which is
// right for THAT token and wrong for every other secret on the cluster. Until
// memql#5626 it published whatever secret the store row's storefrontTokenRef
// named. These tests pin the defence in depth at the point of publication: the
// ref is resolved only when it names the bound store's OWN Storefront token,
// and anything else is refused and logged, with no lookup made at all.

// tokenRefStoreID is the store every case binds: "acme", so its own
// Storefront token is sealed as SHOPIFY_ACME_STOREFRONT_TOKEN.
const tokenRefStoreID = "acme"

// clusterSecrets is what one secret store holds for the cases below: the
// store's own public token, the same store's ADMIN token, another store's
// public token, and an unrelated cluster credential. Every one of them is a
// single lookup away from the serve path, which is what makes "none of them
// but the first reached the document" an assertion rather than an accident.
var clusterSecrets = map[string]string{
	"SHOPIFY_ACME_STOREFRONT_TOKEN": "public-acme-storefront-token",
	"SHOPIFY_ACME_ADMIN_TOKEN":      "shpat_ACME_ADMIN_TOKEN_MUST_NEVER_BE_SERVED",
	"SHOPIFY_BETA_STOREFRONT_TOKEN": "public-beta-storefront-token",
	"MEMQL_SMTP_PASSWORD":           "smtp-password-must-never-be-served",
}

// recordingSecrets answers from clusterSecrets and records every name it was
// asked for, so a case can prove a refused ref was never even looked up.
type recordingSecrets struct {
	mu    sync.Mutex
	asked []string
}

func (r *recordingSecrets) resolve(_ context.Context, name string) (string, error) {
	r.mu.Lock()
	r.asked = append(r.asked, name)
	r.mu.Unlock()
	v, ok := clusterSecrets[name]
	if !ok {
		return "", fmt.Errorf("no secret named %q", name)
	}
	return v, nil
}

func (r *recordingSecrets) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
}

func storefrontNaming(ref string) *Site {
	return &Site{
		ID:       "s-token-ref",
		Hostname: "shop.acme.example.com",
		Kind:     storefrontKind,
		Status:   "live",
		Binding:  map[string]any{"storeId": tokenRefStoreID},
		Store: &BoundStore{
			ID:                 tokenRefStoreID,
			Domain:             "acme.myshopify.com",
			StorefrontTokenRef: ref,
		},
	}
}

// serveConfigWithLog serves /runtime-config.json for site through the real
// handler and returns the served body; the caller holds the handler's log.
func serveConfigWithLog(t *testing.T, h *Handler, site *Site) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, runtimeConfigPath, nil)
	req.Host = site.Hostname
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", runtimeConfigPath, rec.Code, rec.Body.String())
	}
	return sourceHTML(rec.Body.String())
}

func TestTheEdgePublishesOnlyTheBoundStoresOwnStorefrontToken(t *testing.T) {
	refused := map[string]string{
		"the same store's admin token": "SHOPIFY_ACME_ADMIN_TOKEN",
		"an unrelated cluster secret":  "MEMQL_SMTP_PASSWORD",
		"another store's token":        "SHOPIFY_BETA_STOREFRONT_TOKEN",
		"a near miss in case":          "shopify_acme_storefront_token",
	}
	for name, ref := range refused {
		t.Run(name, func(t *testing.T) {
			secrets := &recordingSecrets{}
			site := storefrontNaming(ref)
			h := NewHandler(Options{Resolver: staticResolver{site: site}, SecretResolver: secrets.resolve})

			body := serveConfigWithLog(t, h, site)

			for secretName, value := range clusterSecrets {
				if strings.Contains(body, value) {
					t.Errorf("the served document carries the value of %s while the store names %s: %s",
						secretName, ref, body)
				}
			}
			if asked := secrets.names(); len(asked) != 0 {
				t.Errorf("the secret store was asked for %v; a ref that is not the store's own "+
					"Storefront token must be refused before any lookup", asked)
			}
			if !strings.Contains(body, `"connectionState":"unavailable"`) {
				t.Errorf("a refused token must read as an unavailable connection, never as connected "+
					"or as an unbound design preview: %s", body)
			}
		})
	}

	// THE REACHABLE POSITIVE: the same handler shape publishes the store's own
	// token, so every refusal above is the gate and not a broken resolver.
	t.Run("the store's own token", func(t *testing.T) {
		secrets := &recordingSecrets{}
		site := storefrontNaming(StorefrontTokenSecretName(tokenRefStoreID))
		h := NewHandler(Options{Resolver: staticResolver{site: site}, SecretResolver: secrets.resolve})
		body := serveConfigWithLog(t, h, site)
		if !strings.Contains(body, clusterSecrets["SHOPIFY_ACME_STOREFRONT_TOKEN"]) {
			t.Fatalf("the store's own Storefront token was not published: %s", body)
		}
		if !strings.Contains(body, `"connectionState":"connected"`) {
			t.Errorf("the store's own token did not connect: %s", body)
		}
		if asked := secrets.names(); len(asked) != 1 || asked[0] != "SHOPIFY_ACME_STOREFRONT_TOKEN" {
			t.Errorf("the secret store was asked for %v, want exactly the store's own token", asked)
		}
	})
}

// THE OPERATOR IS TOLD WHY, AT RESOLVE TIME, ONCE. The served document says
// "unavailable" to every visitor and must not say which secret was named, so
// the reason -- and the act that clears it -- goes to the log when the edge
// resolves the store. Once an hour per site, store and reference: the cache
// refills every thirty seconds on every replica, and a line per refill would
// bury itself.
func TestTheResolverWarnsOnceWithTheRemedyWhenAStoreNamesAnotherSecret(t *testing.T) {
	site := &Site{
		ID: "s-legacy", Hostname: "shop.acme.example.com", Kind: storefrontKind, Status: "live",
		Binding: map[string]any{"storeId": "acme"}, PreviewBinding: map[string]any{"storeId": "acme-dev"},
	}
	ex := &destinationExec{
		stubExec: &stubExec{rows: map[string]*Site{site.Hostname: site}},
		stores: map[string]*BoundStore{
			// Registered by hand before the write guard, under a free-text name.
			"acme":     {ID: "acme", Domain: "acme.myshopify.com", StorefrontTokenRef: "ACME_STOREFRONT_TOKEN"},
			"acme-dev": {ID: "acme-dev", Domain: "acme-dev.myshopify.com", StorefrontTokenRef: StorefrontTokenSecretName("acme-dev")},
		},
	}
	var logs bytes.Buffer
	r := NewResolver(ex, time.Minute).(*resolver)
	r.logger = slog.New(slog.NewTextHandler(&logs, nil))

	if _, err := r.Resolve(context.Background(), site.Hostname); err != nil {
		t.Fatal(err)
	}
	got := logs.String()
	for _, says := range []string{"level=WARN", "s-legacy", "acme", "ACME_STOREFRONT_TOKEN", "SHOPIFY_ACME_STOREFRONT_TOKEN", "Connect Shopify"} {
		if !strings.Contains(got, says) {
			t.Errorf("the resolve-time warning does not carry %q:\n%s", says, got)
		}
	}
	if strings.Contains(got, "acme-dev.myshopify.com") || strings.Count(got, "level=WARN") != 1 {
		t.Errorf("want exactly one warning, about the misnamed store only:\n%s", got)
	}

	r.Invalidate(site.Hostname)
	if _, err := r.Resolve(context.Background(), site.Hostname); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("a cache refill logged the same warning again (%d lines)", n)
	}
}

// A STORE WHOSE ID CANNOT NAME A TOKEN gets the remedy for that, not "name
// SHOPIFY__STOREFRONT_TOKEN".
func TestTheResolverWarnsAboutAStoreIdThatNamesNoToken(t *testing.T) {
	site := &Site{ID: "s-upper", Hostname: "shop.upper.example.com", Kind: storefrontKind, Status: "live", Binding: map[string]any{"storeId": "Acme"}}
	ex := &destinationExec{
		stubExec: &stubExec{rows: map[string]*Site{site.Hostname: site}},
		stores:   map[string]*BoundStore{"Acme": {ID: "Acme", Domain: "acme.myshopify.com", StorefrontTokenRef: "SHOPIFY_ACME_STOREFRONT_TOKEN"}},
	}
	var logs bytes.Buffer
	r := NewResolver(ex, time.Minute).(*resolver)
	r.logger = slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := r.Resolve(context.Background(), site.Hostname); err != nil {
		t.Fatal(err)
	}
	if got := logs.String(); !strings.Contains(got, "store id") || strings.Contains(got, "SHOPIFY__STOREFRONT_TOKEN") {
		t.Errorf("the warning does not explain the store id:\n%s", got)
	}
}

// THE PREVIEW STORE IS JUDGED AS ITSELF. Under a Testing grant the edge serves
// the Testing store's token, so that token must be the Testing store's own --
// a Testing binding naming the live store's token would otherwise put one
// store's credential under another store's catalog.
func TestTheTestingStoreIsHeldToItsOwnStorefrontToken(t *testing.T) {
	resolve := func(_ context.Context, name string) (string, error) { return "token-for-" + name, nil }
	site := &Site{
		ID:                "s",
		Kind:              storefrontKind,
		Status:            "live",
		StorefrontTesting: true,
		Binding:           map[string]any{"storeId": "acme"},
		Store:             &BoundStore{ID: "acme", Domain: "acme.myshopify.com", StorefrontTokenRef: StorefrontTokenSecretName("acme")},
		PreviewBinding:    map[string]any{"storeId": "acme-dev"},
		// The Testing store's row names the LIVE store's token.
		PreviewStore: &BoundStore{ID: "acme-dev", Domain: "acme-dev.myshopify.com", StorefrontTokenRef: StorefrontTokenSecretName("acme")},
	}
	grant := &PreviewGrant{ID: "g", SiteID: "s"}

	if got := storefrontForSite(context.Background(), previewSite(site, grant), resolve); got.StorefrontToken != "" {
		t.Fatalf("the Testing store published another store's token: %+v", got)
	}

	site.PreviewStore.StorefrontTokenRef = StorefrontTokenSecretName("acme-dev")
	got := storefrontForSite(context.Background(), previewSite(site, grant), resolve)
	if got.StorefrontToken != "token-for-SHOPIFY_ACME-DEV_STOREFRONT_TOKEN" || got.StoreDomain != "acme-dev.myshopify.com" {
		t.Fatalf("the Testing store's own token was not published: %+v", got)
	}
}
