//go:build bff

package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/inbound"
)

// GitHub sends a webhook payload of up to 25 MB and never a larger one, so that
// is the one cap for its source that cannot refuse a real delivery. At the
// receiver's 256 KiB default a push of a long branch was answered 413, staged
// nothing, and opened no run.
func TestTheGitHubSourceTakesGitHubsLargestPayload(t *testing.T) {
	if githubMaxPayloadBytes < 25_000_000 {
		t.Fatalf("githubMaxPayloadBytes is %d, under GitHub's 25 MB", githubMaxPayloadBytes)
	}
	if got := githubWebhookPolicy("s").MaxBodyBytes; got != githubMaxPayloadBytes {
		t.Errorf("a registered app's source caps bodies at %d, want GitHub's %d", got, githubMaxPayloadBytes)
	}

	env := inbound.Config{MaxBodyBytes: 262144, Sources: map[string]inbound.SourceConfig{
		githubWebhookSource: {Name: githubWebhookSource},
		"acme":              {Name: "acme"},
	}}
	got := withGitHubPayloadCap(env)
	if cap := got.Sources[githubWebhookSource].MaxBodyBytes; cap != githubMaxPayloadBytes {
		t.Errorf("the env-configured %q source caps bodies at %d, want GitHub's %d", githubWebhookSource, cap, githubMaxPayloadBytes)
	}
	if cap := got.Sources["acme"].MaxBodyBytes; cap != 0 {
		t.Errorf("another source was given a cap of its own (%d); it keeps the receiver's default", cap)
	}
	if env.Sources[githubWebhookSource].MaxBodyBytes != 0 {
		t.Errorf("withGitHubPayloadCap changed the config it was handed instead of returning a new one")
	}

	pinned := inbound.Config{Sources: map[string]inbound.SourceConfig{
		githubWebhookSource: {Name: githubWebhookSource, MaxBodyBytes: 1 << 20},
	}}
	if cap := withGitHubPayloadCap(pinned).Sources[githubWebhookSource].MaxBodyBytes; cap != 1<<20 {
		t.Errorf("an operator's MEMQL_INBOUND_SOURCE_GITHUB_MAX_BODY_BYTES was overridden: cap %d, want %d", cap, 1<<20)
	}
}

// Driven through the real receiver at its real default: a registered app's push
// the size of a long branch's -- far over 256 KiB -- is staged, not refused.
func TestARegisteredGitHubPushOverTheReceiverDefaultIsStaged(t *testing.T) {
	const secret = "gh-secret"
	body := `{"ref":"refs/heads/main","commits":"` + strings.Repeat("c", 400<<10) + `"}`
	eng := &stagingEngine{}
	h := inbound.NewHandler(inbound.Config{Enabled: true, MaxBodyBytes: 262144, Tolerance: time.Minute}, eng, nil)
	h.SetRegisteredSource(func(_ context.Context, name string) (inbound.SourceConfig, bool) {
		return githubWebhookPolicy(secret), name == githubWebhookSource
	})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	r := httptest.NewRequest(http.MethodPost, inbound.RoutePrefix+githubWebhookSource, strings.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	r.Header.Set("X-GitHub-Event", "push")
	r.Header.Set("X-GitHub-Delivery", "d-big")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusAccepted || len(eng.calls) != 1 {
		t.Fatalf("a %d-byte push was answered %d with %d staged: %s", len(body), rec.Code, len(eng.calls), rec.Body.String())
	}
}
