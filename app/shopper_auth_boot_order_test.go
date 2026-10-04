package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity/verifier"
	"github.com/znasllc-io/memql/component/memql"
)

// A clean subprocess keeps previously anchored packs from hiding the actual
// config-before-database order of the first production boot.
func TestShopperAuthDeclarationsAreCapturedAfterPackAnchoring(t *testing.T) {
	const childEnv = "MEMQL_TEST_SHOPPER_AUTH_BOOT"
	if os.Getenv(childEnv) != "1" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestShopperAuthDeclarationsAreCapturedAfterPackAnchoring$")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fresh auth boot: %v\n%s", err, out)
		}
		return
	}
	t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
	// Core Campaigns declares its signup at package initialization. Packs
	// must still be absent until their later bootstrap phase.
	hasReviewPack := func() bool {
		for _, entry := range memql.ShopperSurface() {
			if entry.Pack == "reviews" {
				return true
			}
		}
		return false
	}
	if hasReviewPack() {
		t.Fatal("test must start before packs are anchored")
	}
	app := &App{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), identityVerifier: &verifier.Verifier{}}
	middleware := app.identityHTTPMiddleware()
	app.anchorStorefrontPacks()
	if !hasReviewPack() {
		t.Fatal("packs declared no shopper routes")
	}
	reached := false
	h := middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(http.StatusNoContent) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reads/reviews/published", nil))
	if !reached || rec.Code != http.StatusNoContent {
		t.Fatalf("snapshot preceded pack anchoring: status=%d", rec.Code)
	}
	reached = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/forms/campaigns/subscribe", nil))
	if !reached || rec.Code != http.StatusNoContent {
		t.Fatalf("core signup declaration escaped the shared carrier: status=%d", rec.Code)
	}
	reached = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/memql/query", nil))
	if reached || rec.Code != http.StatusUnauthorized {
		t.Fatalf("normal API escaped auth: status=%d", rec.Code)
	}
}
