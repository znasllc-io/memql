//go:build bff

package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity/githubconnect"
	"github.com/znasllc-io/memql/component/inbound"
	"github.com/znasllc-io/memql/component/packages"
)

// THREE SPELLINGS OF ONE NAME, in three modules that cannot import each other
// downward: the URL the cluster's GitHub App is told to post its webhook to
// (githubconnect.WebhookPath, composed into the manifest), the route the
// inbound receiver serves (inbound.RoutePrefix + source), and the source the
// packages pipeline reads a staged delivery under (its default). A drift
// between any two is silent: GitHub posts, the receiver answers 404 or stages a
// row nothing reads, and the update cue falls back to the ten-minute poll with
// nothing anywhere saying why. app/ is the one place that can see all three.
func TestTheGitHubWebhookIsOneNameInThreeModules(t *testing.T) {
	if got, want := githubconnect.WebhookPath, inbound.RoutePrefix+packages.DefaultWebhookSource; got != want {
		t.Errorf("the app's manifest posts to %q and the pipeline listens on %q", got, want)
	}
	if githubWebhookSource != packages.DefaultWebhookSource {
		t.Errorf("the registered inbound policy answers for %q and the pipeline reads %q", githubWebhookSource, packages.DefaultWebhookSource)
	}
}

// The registered GitHub source stages the event on the row. GitHub names the
// event only in X-GitHub-Event, so before this list existed the row carried
// `headersJson: "{}"` and nothing on it said whether a push or a ping had
// arrived (memql#5707 residual). Driven through the real receiver, so what is
// asserted is the staged statement rather than the policy struct.
func TestARegisteredGitHubDeliveryStagesItsEvent(t *testing.T) {
	const secret, body = "gh-secret", `{"ref":"refs/heads/main"}`
	eng := &stagingEngine{}
	h := inbound.NewHandler(inbound.Config{Enabled: true, MaxBodyBytes: 1024, Tolerance: time.Minute}, eng, nil)
	h.SetRegisteredSource(func(_ context.Context, name string) (inbound.SourceConfig, bool) {
		return githubWebhookPolicy(secret), name == githubWebhookSource
	})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	r := httptest.NewRequest(http.MethodPost, inbound.RoutePrefix+githubWebhookSource, strings.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	r.Header.Set("X-GitHub-Event", "push")
	r.Header.Set("X-GitHub-Delivery", "d-1")
	r.Header.Set("X-GitHub-Hook-ID", "42")
	r.Header.Set("User-Agent", "GitHub-Hookshot/abc")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusAccepted || len(eng.calls) != 1 {
		t.Fatalf("status %d, %d staged: %s", rec.Code, len(eng.calls), rec.Body.String())
	}
	staged := eng.calls[0]
	want := `{"x-github-delivery":"d-1","x-github-event":"push","x-github-hook-id":"42"}`
	if !strings.Contains(staged, "headersJson: "+strconv.Quote(want)) {
		t.Fatalf("the staged row does not carry exactly the GitHub delivery headers %s:\n%s", want, staged)
	}
}

// The env-configured source stages the same list only when an operator sets
// FORWARD_HEADERS, and github-connect.md is where they copy it from. A list
// that drifted from the registered one would stage two different rows for one
// webhook depending on how it was set up.
func TestTheGitHubDeliveryHeadersAreOneListInTwoPaths(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "docs", "public", "operate", "github-connect.md"))
	if err != nil {
		t.Fatal(err)
	}
	const key = "MEMQL_INBOUND_SOURCE_GITHUB_FORWARD_HEADERS="
	var documented []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, key) {
			documented = strings.Split(strings.TrimPrefix(line, key), ",")
		}
	}
	if !reflect.DeepEqual(documented, githubDeliveryHeaders) {
		t.Errorf("github-connect.md tells an operator to forward %q; a registered app forwards %q", documented, githubDeliveryHeaders)
	}
}

// stagingEngine records the staging statements the receiver builds.
type stagingEngine struct{ calls []string }

func (e *stagingEngine) Execute(_ context.Context, q string) (any, error) {
	e.calls = append(e.calls, q)
	return nil, nil
}
