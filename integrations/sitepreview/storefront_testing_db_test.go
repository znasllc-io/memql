package sitepreview

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

func TestStorefrontTestingOpensSharedBuildWithoutCandidateOrStore(t *testing.T) {
	eng, db := parityBoot(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	siteID, host := "testing-"+suffix, "shop-"+suffix+".example.com"
	owner := parityOwner("testing-owner-" + suffix)
	if _, err := eng.Execute(owner, fmt.Sprintf(`mutation createSite(siteId: %q, hostname: %q, kind: "shopify_storefront", status: "draft", bundleRef: "blob://sites/shared/v1/")`, siteID, host)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ? OR payload->>'siteId' = ? OR payload->>'siteId' = ?", "v1:platform:site:"+siteID, siteID, "v1:platform:site:"+siteID).Exec(context.Background())
	})
	if _, err := eng.Execute(owner, fmt.Sprintf(`mutation createSite(siteId: %q, hostname: %q, kind: "spa", status: "draft")`, "reserved-"+suffix, "test--"+host)); err == nil {
		t.Fatal("a site claimed the reserved testing hostname")
	}
	integration := NewIntegration(parityEngine{eng}, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, store := range []string{"", "live-" + suffix, "sandbox-" + suffix} {
		if store != "" {
			if _, err := eng.Execute(auth.ContextWithInternalOrigin(owner), fmt.Sprintf(`mutation createStore(storeId: %q, domain: %q, isDevelopment: %t)`, store, store+".myshopify.com", strings.HasPrefix(store, "sandbox-"))); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Execute(owner, fmt.Sprintf(`mutation updateSitePreviewBinding(siteId: %q, storeId: %q)`, siteID, store)); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ?", "v1:shopify:store:"+store).Exec(context.Background())
			})
		}
		nodes, err := integration.handleReadiness(owner, map[string]any{"siteId": siteID}, 0)
		if err != nil {
			t.Fatal(err)
		}
		var readiness map[string]any
		if err := json.Unmarshal(nodes[0].Payload, &readiness); err != nil {
			t.Fatal(err)
		}
		if readiness["canPreview"] != true || readiness["testingUrl"] != "https://test--"+host+"/" || readiness["canPromote"] != false {
			t.Fatalf("incorrect testing readiness: %v", readiness)
		}
		opened, err := integration.handleOpen(owner, map[string]any{"siteId": siteID}, 0)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(opened[0].Payload, &result); err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(result["url"].(string))
		if err != nil || u.Scheme != "https" || u.Host != "test--"+host || !memql.WellFormedPreviewToken(u.Query().Get(memql.PreviewGrantParam)) {
			t.Fatal("testing entry URL is not scoped to testing origin")
		}
		if result["candidateRef"] != "blob://sites/shared/v1/" {
			t.Fatal("testing did not use shared build")
		}
		site, ok, err := integration.store.SiteByID(owner, siteID)
		if err != nil || !ok || site.CandidateRef != "" || site.StoreID != "" || site.PreviewStoreID != store {
			t.Fatalf("opening Testing mutated deployable: %+v %v", site, err)
		}
	}
}
