package campaigns

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// token_rotation_test.go -- memql#3458.
//
// An unsubscribe link does not expire when the send finishes; it sits in a
// recipient's mailbox for as long as they keep the message. So rotating the
// signing secret does not degrade a feature, it retroactively breaks a legal
// obligation for every message ever sent -- silently on our side, and loudly
// on theirs (the recipient's next move after "This link is not valid" is the
// spam button).
//
// The four properties below are what makes rotation survivable:
//
//	a link signed with the PREVIOUS secret still works    TestTokenSignedWithThePreviousSecretStillVerifies
//	an unknown key id is refused, not 500'd               TestTokenNamingAnUnknownKeyIsRefusedNotCrashed
//	only the CURRENT secret ever signs                    TestOnlyTheCurrentSecretSigns
//	a cluster one rotation from breakage says so at boot  TestBootWarnsWhenTheSecretCannotBeRotated

const (
	currentSecret = "current-unsubscribe-signing-secret-for-tests"
	retiredSecret = "retired-unsubscribe-signing-secret-for-tests"
)

// rotatedConfig is the state a correct rotation leaves behind: the new
// secret signs, the old one still verifies.
func rotatedConfig() Config {
	return Config{
		UnsubscribeSecret:         currentSecret,
		UnsubscribeSecretPrevious: retiredSecret,
		UnsubscribeBaseURL:        "https://example.test",
	}
}

// TestTokenSignedWithThePreviousSecretStillVerifies -- THE acceptance
// criterion. The link was minted before the rotation and is sitting in a
// mailbox; it has to keep working afterwards.
func TestTokenSignedWithThePreviousSecretStillVerifies(t *testing.T) {
	tok, err := MintUnsubscribeToken(retiredSecret, testOwner, "r-1", testCampaign)
	if err != nil {
		t.Fatalf("mint under the pre-rotation secret: %v", err)
	}

	owner, recipient, campaign, err := ParseUnsubscribeToken(rotatedConfig().UnsubscribeKeys(), tok)
	if err != nil {
		t.Fatalf("a link minted before the rotation stopped verifying after it: %v", err)
	}
	if owner != testOwner || recipient != "r-1" || campaign != testCampaign {
		t.Fatalf("round trip across the rotation lost a field: %q %q %q", owner, recipient, campaign)
	}
}

// TestPreviousSecretUnsubscribesEndToEnd -- the property above, through
// the endpoint rather than the parser, because that is where a recipient
// meets it: the opt-out must actually happen.
func TestPreviousSecretUnsubscribesEndToEnd(t *testing.T) {
	engine := unsubscribeFixture()
	h := handlerFor(engine)
	h.cfg = rotatedConfig()

	tok, err := MintUnsubscribeToken(retiredSecret, testOwner, "r-1", testCampaign)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/unsubscribe?token="+url.QueryEscape(tok), nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if len(engine.mutations("recordSuppression")) != 1 {
		t.Error("a pre-rotation link rendered the confirmation but suppressed nobody")
	}
}

// TestTokenNamingAnUnknownKeyIsRefusedNotCrashed -- the second rotation.
// The secret that signed this link is no longer held by anybody, which is
// the one case that cannot be recovered. It must land on the existing
// "not valid" page, not a 500, and it must write nothing.
func TestTokenNamingAnUnknownKeyIsRefusedNotCrashed(t *testing.T) {
	tok, err := MintUnsubscribeToken("a-secret-nobody-holds-any-more", testOwner, "r-1", testCampaign)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if _, _, _, err := ParseUnsubscribeToken(rotatedConfig().UnsubscribeKeys(), tok); err == nil {
		t.Fatal("a token signed with a secret this node does not hold verified anyway")
	}

	engine := unsubscribeFixture()
	h := handlerFor(engine)
	h.cfg = rotatedConfig()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/unsubscribe?token="+url.QueryEscape(tok), nil))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (the existing 'not valid' page, never a 500)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "This link is not valid") {
		t.Errorf("body does not render the 'not valid' page: %s", rr.Body.String())
	}
	if n := len(engine.mutations("recordSuppression")) + len(engine.mutations("setRecipientSubscription")); n != 0 {
		t.Errorf("a token under an unknown key produced %d writes", n)
	}
}

// TestMalformedKeyIdIsRefused -- a hand-edited key id is the shape a
// prober sends. It is refused for the same reason a bad tag is, and by
// the same page.
func TestMalformedKeyIdIsRefused(t *testing.T) {
	good, err := MintUnsubscribeToken(currentSecret, testOwner, "r-1", testCampaign)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	parts := strings.Split(good, ".")
	for _, forgedKeyID := range []string{"", "deadbeef", "zzzz", strings.Repeat("a", 64)} {
		swapped := append([]string{}, parts...)
		swapped[1] = forgedKeyID
		if _, _, _, err := ParseUnsubscribeToken(rotatedConfig().UnsubscribeKeys(), strings.Join(swapped, ".")); err == nil {
			t.Errorf("key id %q verified", forgedKeyID)
		}
	}
}

// TestOnlyTheCurrentSecretSigns -- the previous secret is a VERIFY-only
// key. A mint that reached for it would extend the life of a secret the
// operator is retiring, which is the opposite of a rotation.
func TestOnlyTheCurrentSecretSigns(t *testing.T) {
	cfg := rotatedConfig()
	tok, err := MintUnsubscribeToken(cfg.UnsubscribeSecret, testOwner, "r-1", testCampaign)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		t.Fatalf("token %q has no key id segment", tok)
	}
	if parts[1] != UnsubscribeKeyID(currentSecret) {
		t.Errorf("token names key id %q, want the CURRENT secret's %q", parts[1], UnsubscribeKeyID(currentSecret))
	}
	if parts[1] == UnsubscribeKeyID(retiredSecret) {
		t.Error("the token was signed with the retired secret")
	}
	// A node that has finished the rotation (previous dropped) still
	// verifies what it just minted.
	if _, _, _, err := ParseUnsubscribeToken([]string{currentSecret}, tok); err != nil {
		t.Errorf("a freshly minted token did not verify under the current secret alone: %v", err)
	}
}

// TestKeyIdIsDerivedFromTheSecretNotItsPosition -- why the id is a
// digest rather than "current" / "previous": a token minted today is
// verified by a node on which that same secret has since become the
// PREVIOUS one. A positional label would be wrong the moment it rotated,
// which is precisely when it is needed.
func TestKeyIdIsDerivedFromTheSecretNotItsPosition(t *testing.T) {
	if UnsubscribeKeyID(currentSecret) == UnsubscribeKeyID(retiredSecret) {
		t.Fatal("two different secrets share a key id")
	}
	if UnsubscribeKeyID(currentSecret) != UnsubscribeKeyID(currentSecret) {
		t.Fatal("the key id is not stable for one secret")
	}
	if strings.Contains(UnsubscribeKeyID(currentSecret), currentSecret) {
		t.Fatal("the key id echoes the secret")
	}
}

// TestUnsubscribeKeysAreCurrentFirstAndDeduplicated -- the ring the
// verifier walks. Current first so the common case matches on the first
// try; deduplicated so "previous == current" (a rotation that did not
// happen) is one key rather than two.
func TestUnsubscribeKeysAreCurrentFirstAndDeduplicated(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want []string
	}{
		{"rotated", rotatedConfig(), []string{currentSecret, retiredSecret}},
		{"never rotated", Config{UnsubscribeSecret: currentSecret}, []string{currentSecret}},
		{"previous equals current", Config{UnsubscribeSecret: currentSecret, UnsubscribeSecretPrevious: currentSecret}, []string{currentSecret}},
		{"previous only", Config{UnsubscribeSecretPrevious: retiredSecret}, []string{retiredSecret}},
		{"neither", Config{}, nil},
		{"whitespace is not a key", Config{UnsubscribeSecret: "   "}, nil},
	}
	for _, tc := range cases {
		got := tc.cfg.UnsubscribeKeys()
		if len(got) != len(tc.want) {
			t.Errorf("%s: keys = %d, want %d", tc.name, len(got), len(tc.want))
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: key %d mismatch", tc.name, i)
			}
		}
	}
}

// TestNoKeysConfiguredRefusesEveryToken -- with nothing to verify
// against, every link renders "not valid". That is already true of the
// mint side (campaignStartSend refuses), and it must not become a panic
// on the endpoint that a mail client reaches unauthenticated.
func TestNoKeysConfiguredRefusesEveryToken(t *testing.T) {
	tok, err := MintUnsubscribeToken(currentSecret, testOwner, "r-1", testCampaign)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, _, _, err := ParseUnsubscribeToken(nil, tok); err == nil {
		t.Error("a token verified against an empty key ring")
	}
}

// --- the boot warning ---------------------------------------------------

// captureLogger returns a logger plus the buffer it writes to.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func sentJobRow(sentCount int) map[string]any {
	row := jobRow()
	row["status"] = "completed"
	row["sentCount"] = sentCount
	return row
}

// TestBootWarnsWhenTheSecretCannotBeRotated -- the fourth acceptance
// criterion, and the only one an operator ever sees. A cluster that has
// mailed people while holding exactly ONE unsubscribe secret is one
// rotation away from invalidating every link it has ever sent, and
// nothing else in the system reports that.
func TestBootWarnsWhenTheSecretCannotBeRotated(t *testing.T) {
	cases := []struct {
		name     string
		previous string
		jobs     []map[string]any
		wantWarn bool
	}{
		{"one secret, mail already sent", "", []map[string]any{sentJobRow(120)}, true},
		{"two secrets, mail already sent", retiredSecret, []map[string]any{sentJobRow(120)}, false},
		{"one secret, nothing ever sent", "", []map[string]any{sentJobRow(0)}, false},
		{"one secret, no send jobs at all", "", nil, false},
	}
	for _, tc := range cases {
		engine := &fakeEngine{jobs: tc.jobs}
		w := newTestWorker(t, engine, &recordingSender{})
		logger, buf := captureLogger()
		w.logger = logger
		w.cfg.UnsubscribeSecret = currentSecret
		w.cfg.UnsubscribeSecretPrevious = tc.previous

		w.WarnOnUnrotatableUnsubscribeSecret(context.Background())

		// level=WARN is part of the assertion, not decoration: this line
		// has to survive a log level that drops Info, or the operator it
		// exists for never sees it.
		warned := strings.Contains(buf.String(), "MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET_PREVIOUS") &&
			strings.Contains(buf.String(), "level=WARN")
		if warned != tc.wantWarn {
			t.Errorf("%s: warned = %v, want %v\nlog: %s", tc.name, warned, tc.wantWarn, buf.String())
		}
		if strings.Contains(buf.String(), currentSecret) || strings.Contains(buf.String(), retiredSecret) {
			t.Errorf("%s: the warning logged a secret", tc.name)
		}
	}
}

// TestBootWarningReadsUnderTheEngineIdentity -- the "has anything ever
// been sent" question spans owners, so it may only be asked under the
// engine's own cluster-owner identity, exactly like the job scan.
func TestBootWarningReadsUnderTheEngineIdentity(t *testing.T) {
	engine := &fakeEngine{jobs: []map[string]any{sentJobRow(1)}}
	w := newTestWorker(t, engine, &recordingSender{})
	w.cfg.UnsubscribeSecretPrevious = ""

	w.WarnOnUnrotatableUnsubscribeSecret(context.Background())

	engine.mu.Lock()
	defer engine.mu.Unlock()
	var found bool
	for _, c := range engine.calls {
		if strings.HasPrefix(c.query, "query recentSendJobs") {
			found = true
			if !c.isOwner || c.actorID != systemCampaignsActor {
				t.Errorf("the cross-owner read ran as %q (clusterOwner=%v), want the engine identity", c.actorID, c.isOwner)
			}
		}
	}
	if !found {
		t.Fatal("no cluster-wide send-job read happened, so the warning cannot know whether mail was sent")
	}
}

var _ = time.Now
