//go:build bff

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	lkauth "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lkjson "github.com/livekit/protocol/utils/protojson"

	"github.com/znasllc-io/memql/component/inbound"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/calls"
)

// transport_inbound_livekit_test.go -- the calls media server's webhooks at the inbound
// receiver: the source the cluster registers for them (transport_inbound.go
// liveKitWebhookResolver), its policy, and its place beside the GitHub App's.
//
// HERE, NOT IN integrations/calls. The point of the first test is that a token LiveKit's
// own SDK mints is one the receiver's jwt-hs256-body-sha256 scheme verifies, so the SDK
// and component/inbound must both be in the module graph. integrations may not import
// component/inbound, which sits above it (docs/internal/ops/ci-design.md section D3), and
// the root module may import anything.

const (
	liveKitTestKey    = "APIlivekitwebhook"
	liveKitTestSecret = "livekit-secret-0123456789abcdefghijklmn"
)

// sealedWebhook is calls.media.webhook as the calls bootstrap seals it: a
// calls.MediaCredential's JSON.
func sealedWebhook(t *testing.T, key, secret string) string {
	t.Helper()
	raw, err := json.Marshal(calls.MediaCredential{Key: key, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// webhookSecrets resolves calls.media.webhook to plaintext, or fails with err, and has
// no other secret.
func webhookSecrets(plaintext string, err error) func(context.Context, string) (string, error) {
	return func(_ context.Context, name string) (string, error) {
		if name != calls.SecretMediaWebhook {
			return "", memql.ErrVariableNotFound
		}
		return plaintext, err
	}
}

// liveKitEventBody is a webhook body as LiveKit's notifier writes it.
func liveKitEventBody(t *testing.T) []byte {
	t.Helper()
	body, err := lkjson.Marshal(&livekit.WebhookEvent{
		Event: "participant_joined", Id: "EV_test", CreatedAt: time.Now().Unix(),
		Room:        &livekit.Room{Sid: "RM_test", Name: "room-test"},
		Participant: &livekit.ParticipantInfo{Sid: "PA_test", Identity: "seat-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// liveKitToken is the token LiveKit's notifier signs a webhook body with
// (webhook/url_notifier.go send): its own SDK's AccessToken under the server's key pair,
// valid for the time given, carrying the body's SHA-256 in standard base64.
func liveKitToken(t *testing.T, key, secret string, body []byte, validFor time.Duration) string {
	t.Helper()
	sum := sha256.Sum256(body)
	token, err := lkauth.NewAccessToken(key, secret).SetValidFor(validFor).SetSha256(base64.StdEncoding.EncodeToString(sum[:])).ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// liveKitRequest is a webhook delivery as LiveKit's notifier sends one: the bare token as
// Authorization, and its own content type.
func liveKitRequest(body []byte, authorization string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, inbound.RoutePrefix+liveKitWebhookSource, bytes.NewReader(body))
	r.Header.Set("Authorization", authorization)
	r.Header.Set("Content-Type", "application/webhook+json")
	r.Header.Set("User-Agent", "LiveKit")
	return r
}

func liveKitReceiver(eng inbound.Engine, resolve inbound.RegisteredSource) *inbound.Handler {
	h := inbound.NewHandler(inbound.Config{Enabled: true, MaxBodyBytes: 1 << 18, Tolerance: 5 * time.Minute}, eng, nil)
	h.SetRegisteredSource(resolve)
	return h
}

// TestALiveKitSignedRequestVerifies: a webhook as a LiveKit media server sends it -- a
// token LiveKit's own SDK minted with SetSha256(base64(sha256(body))), as the BARE value
// of Authorization -- is verified by the receiver under the policy the cluster registers
// for its calls media server, and staged as verified by the registered tier. The request
// is first held to LiveKit's own receiving steps (webhook/verifier.go Receive:
// ParseAPIToken on the header as it is, Verify under the secret, the digest claim
// compared), so the shape asserted is the SDK's and not this test's guess: with a
// "Bearer " in front, the SDK's own parse refuses it too. A token signed under another
// secret, one naming another key, and a body changed after signing are refused, and
// nothing is staged for them.
func TestALiveKitSignedRequestVerifies(t *testing.T) {
	body := liveKitEventBody(t)
	token := liveKitToken(t, liveKitTestKey, liveKitTestSecret, body, 5*time.Minute)

	// LiveKit's own receiving steps accept the request as built here, and only bare.
	verifier, err := lkauth.ParseAPIToken(liveKitRequest(body, token).Header.Get("Authorization"))
	if err != nil {
		t.Fatalf("LiveKit's SDK does not parse the header as the notifier sends it: %v", err)
	}
	if verifier.APIKey() != liveKitTestKey {
		t.Fatalf("the token's issuer is %q, want the server's key", verifier.APIKey())
	}
	_, grants, err := verifier.Verify(liveKitTestSecret)
	if err != nil {
		t.Fatalf("LiveKit's SDK does not verify its own token: %v", err)
	}
	sum := sha256.Sum256(body)
	if grants.Sha256 != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Fatalf("the SDK's digest claim is %q: not the standard base64 of the body's SHA-256", grants.Sha256)
	}
	if _, err := lkauth.ParseAPIToken("Bearer " + token); err == nil {
		t.Fatal("LiveKit's SDK parsed a Bearer-prefixed header: the header is not the bare token after all")
	}

	eng := &stagingEngine{}
	h := liveKitReceiver(eng, liveKitWebhookResolver(webhookSecrets(sealedWebhook(t, liveKitTestKey, liveKitTestSecret), nil), nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, liveKitRequest(body, token))
	if rec.Code != http.StatusAccepted || len(eng.calls) != 1 {
		t.Fatalf("a request LiveKit signed answered %d with %d staged: %s", rec.Code, len(eng.calls), rec.Body.String())
	}
	staged := eng.calls[0]
	for _, want := range []string{`source: "livekit"`, "signatureVerified: true", `verifiedBy: "registered"`} {
		if !strings.Contains(staged, want) {
			t.Errorf("the staged delivery does not carry %s:\n%s", want, staged)
		}
	}
	if strings.Contains(staged, token) {
		t.Error("the token reached the staged statement")
	}

	for name, refused := range map[string]*http.Request{
		"signed under another secret":  liveKitRequest(body, liveKitToken(t, liveKitTestKey, "other-secret-0123456789abcdefghijklmno", body, 5*time.Minute)),
		"naming another key":           liveKitRequest(body, liveKitToken(t, "APIsomebodyelse", liveKitTestSecret, body, 5*time.Minute)),
		"a body changed after signing": liveKitRequest(append(append([]byte(nil), body...), '\n'), token),
		"a Bearer prefix":              liveKitRequest(body, "Bearer "+token),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, refused)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("a request %s answered %d, want 401", name, rec.Code)
		}
	}
	if len(eng.calls) != 1 {
		t.Errorf("a refused request was staged: %d staged in all", len(eng.calls))
	}
}

// TestARetriedLiveKitDeliveryIsOneRow: the row's identity is the body's digest and never
// the token, so the same event sent again -- under the same token, as LiveKit's retrying
// client sends it, or under another -- lands on the row the first made.
func TestARetriedLiveKitDeliveryIsOneRow(t *testing.T) {
	body := liveKitEventBody(t)
	h := liveKitReceiver(&stagingEngine{}, liveKitWebhookResolver(webhookSecrets(sealedWebhook(t, liveKitTestKey, liveKitTestSecret), nil), nil))
	ids := map[string]bool{}
	for _, token := range []string{
		liveKitToken(t, liveKitTestKey, liveKitTestSecret, body, 5*time.Minute),
		liveKitToken(t, liveKitTestKey, liveKitTestSecret, body, 5*time.Minute),
		liveKitToken(t, liveKitTestKey, liveKitTestSecret, body, 4*time.Minute),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, liveKitRequest(body, token))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var reply map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		ids[reply["id"]] = true
	}
	if len(ids) != 1 {
		t.Errorf("one event delivered three times staged %d rows: %v", len(ids), ids)
	}
}

// TestTheLiveKitSourceNeedsItsSealedCredential: the source answers only while
// calls.media.webhook holds a usable key pair, and is never admitted without one. A
// cluster that never configured a media server has none, and the receiver answers 404,
// as for any unknown source; a credential that cannot be read is the same 404, and the
// node says why in its log.
func TestTheLiveKitSourceNeedsItsSealedCredential(t *testing.T) {
	body := liveKitEventBody(t)
	token := liveKitToken(t, liveKitTestKey, liveKitTestSecret, body, 5*time.Minute)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	for name, tc := range map[string]struct {
		secrets func(context.Context, string) (string, error)
		logged  bool
	}{
		"no media server configured":            {webhookSecrets("", memql.ErrVariableNotFound), false},
		"a credential that cannot be decrypted": {webhookSecrets("", memql.ErrSecretUndecryptable), true},
		"a credential missing its secret":       {webhookSecrets(sealedWebhook(t, liveKitTestKey, ""), nil), true},
		"something that is no credential":       {webhookSecrets("not json", nil), true},
		"a lookup that failed":                  {webhookSecrets("", errors.New("the database is gone")), true},
	} {
		t.Run(name, func(t *testing.T) {
			logs.Reset()
			eng := &stagingEngine{}
			rec := httptest.NewRecorder()
			liveKitReceiver(eng, liveKitWebhookResolver(tc.secrets, logger)).ServeHTTP(rec, liveKitRequest(body, token))
			if rec.Code != http.StatusNotFound || len(eng.calls) != 0 {
				t.Errorf("answered %d with %d staged, want 404 and nothing", rec.Code, len(eng.calls))
			}
			if got := logs.Len() > 0; got != tc.logged {
				t.Errorf("logged %v, want %v: %s", got, tc.logged, logs.String())
			}
			if strings.Contains(logs.String(), liveKitTestSecret) {
				t.Error("the log carries the webhook secret")
			}
		})
	}
}

// TestTheRegisteredSourcesAreComposed: the receiver holds ONE registered resolver
// (inbound.Handler.SetRegisteredSource), and the cluster registers two sources for
// itself, so the bff hands it both in one: each answers its own name and nothing else,
// in turn, and a name neither answers falls through to the connectors exactly as before.
// With neither wired the composition is no resolver at all.
func TestTheRegisteredSourcesAreComposed(t *testing.T) {
	answers := func(source string, secret string) inbound.RegisteredSource {
		return func(_ context.Context, name string) (inbound.SourceConfig, bool) {
			return inbound.SourceConfig{Secret: secret}, name == source
		}
	}
	both := firstRegisteredSource(answers(githubWebhookSource, "for-github"), answers(liveKitWebhookSource, "for-livekit"))
	for name, want := range map[string]string{githubWebhookSource: "for-github", liveKitWebhookSource: "for-livekit"} {
		got, ok := both(context.Background(), name)
		if !ok || got.Secret != want {
			t.Errorf("%s resolved %+v, %v", name, got, ok)
		}
	}
	if _, ok := both(context.Background(), "nobody"); ok {
		t.Error("a name neither source answers resolved")
	}
	if firstRegisteredSource(nil, nil) != nil {
		t.Error("with no source wired the composition is a resolver, which changes nothing but says something is registered")
	}
	only := firstRegisteredSource(nil, answers(liveKitWebhookSource, "for-livekit"))
	if _, ok := only(context.Background(), liveKitWebhookSource); !ok {
		t.Error("a nil resolver beside a real one hid it")
	}
	if liveKitWebhookSource != calls.WebhookSource {
		t.Errorf("the bff registers %q and the calls automation reads %q", liveKitWebhookSource, calls.WebhookSource)
	}
}
