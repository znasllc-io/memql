package edge

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/frontdoor"
)

type destinationExec struct {
	*stubExec
	stores map[string]*BoundStore
}

func (e *destinationExec) StoreByID(_ context.Context, id string) (*BoundStore, error) {
	return e.stores[id], nil
}

func TestStorefrontDestinationsShareBuildAndIsolateStores(t *testing.T) {
	for _, sameStore := range []bool{false, true} {
		t.Run(map[bool]string{false: "separate stores", true: "same sandbox"}[sameStore], func(t *testing.T) {
			original := previewSiteRow("live")
			original.Kind = storefrontKind
			testStore := original.PreviewStore
			if sameStore {
				original.Binding = original.PreviewBinding
				original.Store = testStore
			}
			ex := &destinationExec{stubExec: &stubExec{rows: map[string]*Site{original.Hostname: original}}, stores: map[string]*BoundStore{"acme": original.Store, "acme-dev": testStore}}
			resolver := NewResolver(ex, time.Hour)
			production, err := resolver.Resolve(context.Background(), original.Hostname)
			if err != nil {
				t.Fatal(err)
			}
			testingHost := frontdoor.StorefrontTestingHost(original.Hostname)
			testingSite, err := resolver.Resolve(context.Background(), testingHost)
			if err != nil || testingSite == nil {
				t.Fatalf("resolve testing: %v", err)
			}
			if production.StorefrontTesting || !testingSite.StorefrontTesting || testingSite.ID != production.ID || testingSite.BundleRef != production.BundleRef {
				t.Fatal("destinations do not share one site and build")
			}
			exec := newStubPreviewExec()
			h := previewHandler(production, exec)
			h.resolver = resolver
			token := exec.issue(t, PreviewGrant{ID: "g-test", SiteID: original.ID, CandidateRef: original.BundleRef})
			for _, site := range []*Site{production, testingSite} {
				page := getWithToken(h, site, "/", token)
				if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "the serving version") {
					t.Fatalf("%s did not serve shared design: %d %s", site.Hostname, page.Code, page.Body.String())
				}
				cfg := getWithToken(h, site, runtimeConfigPath, token)
				want := production.Store.Domain
				if site.StorefrontTesting {
					want = testStore.Domain
				}
				if !sameStore {
					absent := liveStoreDomain
					if !site.StorefrontTesting {
						absent = devStoreDomain
					}
					if strings.Contains(cfg.Body.String(), absent) {
						t.Fatalf("%s leaked the other store", site.Hostname)
					}
				}
				if !strings.Contains(cfg.Body.String(), want) {
					t.Fatalf("wrong %s store: %s", site.Hostname, cfg.Body.String())
				}
				if !strings.Contains(page.Header().Get("Content-Security-Policy"), "https://"+want) {
					t.Fatal("catalog origin missing from policy")
				}
				if site.StorefrontTesting && (!strings.Contains(page.Header().Get("Cache-Control"), "no-store") || page.Header().Get("X-Robots-Tag") != "noindex, nofollow") {
					t.Fatal("testing response is cacheable or indexable")
				}
			}
			// Production must ignore even a valid testing bearer cookie.
			cfg := getWithToken(h, production, runtimeConfigPath, token)
			if !strings.Contains(cfg.Body.String(), production.Store.Domain) {
				t.Fatal("testing cookie changed Production")
			}
			if response := getWithToken(h, testingSite, runtimeConfigPath, ""); response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), testStore.Domain) {
				t.Fatal("testing leaked without access")
			}

			// A design update reaches both URLs, keeping access and bindings.
			original.BundleRef = candidateRef
			resolver.Invalidate(original.Hostname)
			for _, site := range []*Site{production, testingSite} {
				page := getWithToken(h, site, "/", token)
				if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "the candidate version") {
					t.Fatalf("redeploy not visible at %s: %d", site.Hostname, page.Code)
				}
			}
		})
	}
}

func TestUnconnectedDestinationsShowDesignAndTestingAccessFailsClosed(t *testing.T) {
	for _, status := range []string{"live", "draft", "disabled", "archived"} {
		site := previewSiteRow(status)
		site.Kind = storefrontKind
		site.StorefrontTesting = true
		site.Hostname = frontdoor.StorefrontTestingHost(site.Hostname)
		site.PreviewStore = nil
		exec := newStubPreviewExec()
		h := previewHandler(site, exec)
		token := exec.issue(t, PreviewGrant{ID: "g-design", SiteID: site.ID, CandidateRef: site.BundleRef})
		response := getWithToken(h, site, runtimeConfigPath, token)
		if status == "live" || status == "draft" {
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"storeDomain":""`) || strings.Contains(response.Body.String(), liveStoreDomain) {
				t.Fatalf("unbound Testing inherited Production: %s", response.Body.String())
			}
		} else if response.Code == http.StatusOK {
			t.Fatalf("%s Testing is still served", status)
		}
		for _, grant := range []PreviewGrant{
			{ID: "other", SiteID: "other-site", ExpiresAt: time.Now().Add(time.Hour)},
			{ID: "expired", SiteID: site.ID, ExpiresAt: time.Now().Add(-time.Hour)},
			{ID: "revoked", SiteID: site.ID, RevokedAt: "now"},
		} {
			bad := exec.issue(t, grant)
			if getWithToken(h, site, runtimeConfigPath, bad).Code == http.StatusOK {
				t.Fatalf("accepted invalid testing access %s", grant.ID)
			}
		}
	}
}

func TestTestingAliasCannotResolveOrdinarySitesOrCustomDomains(t *testing.T) {
	ex := &stubExec{rows: map[string]*Site{"plain.example.com": {ID: "plain", Hostname: "plain.example.com", Kind: "spa"}}, aliases: map[string]*Site{"shop.example.org": {ID: "custom", Kind: storefrontKind}}}
	resolver := NewResolver(ex, time.Minute)
	for _, host := range []string{"test--plain.example.com", "test--shop.example.org", "test--missing.example.com"} {
		if site, err := resolver.Resolve(context.Background(), host); err != nil || site != nil {
			t.Fatalf("unexpected alias %s: %v %v", host, site, err)
		}
	}
	if ex.aliasCalls != 0 || ex.doorCalls != 0 {
		t.Fatal("testing alias used custom-domain or account lookup")
	}
}
