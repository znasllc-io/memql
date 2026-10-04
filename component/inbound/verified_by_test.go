package inbound

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// The staged row records WHICH TIER verified the delivery (memql#5795). The
// dispatcher routes a row by its source name, and the connector a name routes
// to reads the row as signed by its OWN secret; the receiver refuses an env
// pin on a claimed name, but only at receipt. A store connected between
// staging and dispatch would otherwise receive an env-verified row as its
// own. With the tier on the row, the dispatcher refuses it too.
//
// The wire values are spelled as literals, so a renamed constant cannot move
// them and this test with it: they are what the concept's enum admits.
func TestTheStagedRowRecordsWhichTierVerifiedIt(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		eng := &fakeEngine{}
		rec := httptest.NewRecorder()
		testHandler(t, eng, hexSource()).ServeHTTP(rec, signedRequest(t, `{"id":1}`))
		requireStagedTier(t, rec, eng, "env")
	})
	t.Run("registered", func(t *testing.T) {
		eng := &fakeEngine{}
		h := connectorHandler(t, eng)
		h.SetRegisteredSource(githubPolicy(registeredSecret))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, githubSigned(t, registeredSecret, `{"ref":"refs/heads/main"}`))
		requireStagedTier(t, rec, eng, "registered")
	})
	t.Run("connector", func(t *testing.T) {
		withConnector(t, &fakeConnector{name: "shopify", sources: map[string]memqlsync.InboundSource{
			"shopify-acme": {Scheme: SchemeHMACSHA256Base64, SignatureHeader: "X-Shopify-Hmac-Sha256", Secret: "acme-secret"},
		}})
		eng := &fakeEngine{}
		rec := httptest.NewRecorder()
		connectorHandler(t, eng).ServeHTTP(rec, shopifySigned(t, "/inbound/shopify-acme", "acme-secret", `{"id":1}`))
		requireStagedTier(t, rec, eng, "connector")
	})
}

func requireStagedTier(t *testing.T, rec *httptest.ResponseRecorder, eng *fakeEngine, want string) {
	t.Helper()
	if rec.Code != http.StatusAccepted || len(eng.calls) != 1 {
		t.Fatalf("delivery answered %d with %d staging calls: %s", rec.Code, len(eng.calls), rec.Body)
	}
	if !strings.Contains(eng.calls[0], `verifiedBy: "`+want+`"`) {
		t.Errorf("the staging call does not record verifiedBy %q:\n%s", want, eng.calls[0])
	}
}
