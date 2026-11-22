package memql

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/metrics"
)

// Tests for the one sign-in a process keeps per vendor (design record
// 2026-11-21-vendor-accounts-and-api-keys, decisions A1, A2, A4 and A5).
//
// The vendor these tests stand in for REFUSES A REPEATED jti, which is the
// rule Anthropic documents and the reason the invariant exists: N clients and
// a provider reload used to make N sign-ins with one identity token, and only
// the first could be accepted.

// signInHarness is a token file, a vendor that remembers every jti it has
// accepted, and a clock the test moves.
type signInHarness struct {
	t         *testing.T
	tokenFile string

	mu          sync.Mutex
	now         time.Time
	exchanges   int
	issued      int // bearers handed out, so each has a name of its own
	accepted    map[string]bool
	refused     map[string]bool // jtis this vendor refuses outright
	faulting    bool            // the vendor does not answer
	allowReplay bool            // the OpenAI shape: a repeated jti is accepted
	lifetime    time.Duration   // 0 means the vendor names no expiry
	presented   []string

	// hang, when set, makes every exchange block until it is closed: a vendor
	// that has accepted the connection and says nothing.
	hang chan struct{}
	// takes is how long an exchange lasts on the harness clock.
	takes time.Duration
	// inFlight counts exchanges that have started and not yet returned.
	inFlight int
}

var signInEpoch = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

func newSignInHarness(t *testing.T) *signInHarness {
	t.Helper()
	isolateVendorSignIns(t)
	vendorSignInStatusFile.Store(true)
	return &signInHarness{
		t:         t,
		tokenFile: filepath.Join(t.TempDir(), "token"),
		now:       signInEpoch,
		accepted:  map[string]bool{},
		refused:   map[string]bool{},
		lifetime:  time.Hour,
	}
}

// writeToken replaces the identity token, the way kubelet rotates a projected
// one: same path, new content. Issued now, valid for ten minutes.
func (h *signInHarness) writeToken(jti string) string {
	h.t.Helper()
	return h.writeTokenIssued(jti, h.clock(), h.clock().Add(10*time.Minute))
}

func (h *signInHarness) writeTokenIssued(jti string, iat, exp time.Time) string {
	h.t.Helper()
	payload, err := json.Marshal(map[string]any{
		"aud": []any{anthropicAudience},
		"sub": "system:serviceaccount:memql:memql-engine",
		"jti": jti,
		"iat": iat.Unix(),
		"exp": exp.Unix(),
	})
	if err != nil {
		h.t.Fatalf("marshal claims: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	token := header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".c2ln"
	if err := os.WriteFile(h.tokenFile, []byte(token), 0o600); err != nil {
		h.t.Fatalf("write token: %v", err)
	}
	return token
}

// useWallClock starts the harness at the real time, for the tests that go
// through processVendorSignIn, which runs on the real clock.
func (h *signInHarness) useWallClock() {
	h.mu.Lock()
	h.now = time.Now()
	h.mu.Unlock()
}

// resetVendorSignInsForTest forgets every sign-in this process holds.
func resetVendorSignInsForTest() {
	vendorSignInsMu.Lock()
	vendorSignIns = map[string]*vendorSignIn{}
	vendorSignInsMu.Unlock()
}

// isolateVendorSignIns gives a test a process with no sign-in in it and a
// temporary directory of its own, and leaves status files switched off unless
// the test turns them on. Every test that builds a federated provider calls
// it: the sign-ins are process state, and a test must not inherit one.
func isolateVendorSignIns(t *testing.T) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	resetVendorSignInsForTest()
	vendorSignInStatusFile.Store(false)
	t.Cleanup(func() {
		resetVendorSignInsForTest()
		vendorSignInStatusFile.Store(false)
	})
}

func (h *signInHarness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *signInHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func (h *signInHarness) exchangeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exchanges
}

// exchange is the vendor. Every call is one presentation of an identity token.
func (h *signInHarness) exchange(ctx context.Context, identityToken string) (string, time.Time, error) {
	h.mu.Lock()
	h.exchanges++
	h.inFlight++
	h.presented = append(h.presented, identityToken)
	hang := h.hang
	h.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-ctx.Done():
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.inFlight--
	h.now = h.now.Add(h.takes)
	if hang != nil && ctx.Err() != nil {
		return "", time.Time{}, ctx.Err()
	}
	if h.faulting {
		return "", time.Time{}, errors.New("dial tcp: connection refused")
	}
	claims, err := parseJWTClaims(identityToken)
	if err != nil {
		return "", time.Time{}, &vendorExchangeRefused{Status: 400, Body: "not a token"}
	}
	jti, _ := claims["jti"].(string)
	if h.refused[jti] {
		return "", time.Time{}, &vendorExchangeRefused{Status: 401, Body: "match_subject_prefix"}
	}
	if h.accepted[jti] && !h.allowReplay {
		return "", time.Time{}, &vendorExchangeRefused{Status: 401, Body: "jti_reused"}
	}
	h.accepted[jti] = true
	h.issued++
	bearer := fmt.Sprintf("bearer-%d", h.issued)
	if h.lifetime == 0 {
		return bearer, time.Time{}, nil
	}
	return bearer, h.now.Add(h.lifetime), nil
}

func (h *signInHarness) config(replayProtected bool, fingerprint string) vendorSignInConfig {
	return vendorSignInConfig{
		Vendor:          metrics.FederationVendorAnthropic,
		Fingerprint:     fingerprint,
		TokenFile:       h.tokenFile,
		ReplayProtected: replayProtected,
		Ids:             map[string]string{"federationRuleId": "fdrl_test"},
		Exchange:        h.exchange,
	}
}

// signIn builds a sign-in on the harness clock, in a process that started at
// the epoch.
func (h *signInHarness) signIn(replayProtected bool) *vendorSignIn {
	return newVendorSignIn(h.config(replayProtected, "fp-1"), h.clock, signInEpoch)
}

func mustBearer(t *testing.T, s *vendorSignIn) string {
	t.Helper()
	bearer, _, err := s.Bearer(context.Background())
	if err != nil {
		t.Fatalf("Bearer: %v", err)
	}
	return bearer
}

func TestSignInExchangesOncePerIdentityToken(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	s := h.signIn(true)

	for i := 0; i < 50; i++ {
		if got := mustBearer(t, s); got != "bearer-1" {
			t.Fatalf("call %d: bearer = %q, want bearer-1", i, got)
		}
	}
	if got := h.exchangeCount(); got != 1 {
		t.Fatalf("50 calls with one identity token made %d sign-ins, want 1", got)
	}
}

func TestSignInIsPerformedOnceUnderConcurrentCallers(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	s := h.signIn(true)

	const callers = 32
	bearers := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bearers[i], _, errs[i] = s.Bearer(context.Background())
		}(i)
	}
	wg.Wait()

	for i := range bearers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if bearers[i] != "bearer-1" {
			t.Fatalf("caller %d got %q, want bearer-1", i, bearers[i])
		}
	}
	if got := h.exchangeCount(); got != 1 {
		t.Fatalf("%d cold callers made %d sign-ins, want 1", callers, got)
	}
}

func TestSignInUsesTheRotatedTokenNearExpiry(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	s := h.signIn(true)
	mustBearer(t, s)

	h.advance(time.Hour - 100*time.Second)
	h.writeToken("jti-2")

	// The call that finds the bearer near its end starts the renewal and is
	// answered with the bearer in hand, which still works. The renewal runs
	// behind it (TestACallerWithAValidBearerDoesNotWaitForARefresh).
	if got := mustBearer(t, s); got != "bearer-1" {
		t.Fatalf("with 100s left the caller got %q, want the bearer still in hand", got)
	}
	settle(t, s)
	if got := mustBearer(t, s); got != "bearer-2" {
		t.Fatalf("after the renewal with the new identity token, bearer = %q, want bearer-2", got)
	}
	if got := h.exchangeCount(); got != 2 {
		t.Fatalf("sign-ins = %d, want 2", got)
	}
}

func TestSignInNeverPresentsAnAcceptedTokenAgain(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	s := h.signIn(true)
	mustBearer(t, s)

	// Near expiry with the same token on disk: the bearer in hand is still
	// valid, so it is handed out and nothing is presented.
	h.advance(time.Hour - 100*time.Second)
	if got := mustBearer(t, s); got != "bearer-1" {
		t.Fatalf("near expiry with no new token, bearer = %q, want bearer-1", got)
	}

	// Past expiry it cannot be handed out, and the token still must not be
	// presented a second time.
	h.advance(200 * time.Second)
	_, _, err := s.Bearer(context.Background())
	var waiting *vendorSignInWaiting
	if !errors.As(err, &waiting) {
		t.Fatalf("past expiry with a used token, err = %v, want a *vendorSignInWaiting", err)
	}
	if got := h.exchangeCount(); got != 1 {
		t.Fatalf("an accepted identity token was presented %d times, want 1", got)
	}
	rec, ok := s.Record()
	if !ok || rec.Outcome != "waiting" {
		t.Fatalf("record = %+v (ok=%v), want outcome waiting", rec, ok)
	}
}

func TestSignInWithoutReplayProtectionMayPresentAgain(t *testing.T) {
	h := newSignInHarness(t)
	h.allowReplay = true
	h.writeToken("jti-1")
	s := h.signIn(false)
	mustBearer(t, s)

	h.advance(time.Hour + time.Second)
	if got := mustBearer(t, s); got != "bearer-2" {
		t.Fatalf("bearer = %q, want bearer-2 from a second presentation", got)
	}
	if got := h.exchangeCount(); got != 2 {
		t.Fatalf("sign-ins = %d, want 2", got)
	}
}

func TestSignInDoesNotRetryARefusalUntilTheFileChanges(t *testing.T) {
	h := newSignInHarness(t)
	h.refused["jti-1"] = true
	h.writeToken("jti-1")
	s := h.signIn(true)

	for i := 0; i < 21; i++ {
		if _, _, err := s.Bearer(context.Background()); err == nil {
			t.Fatalf("call %d: a refused sign-in returned no error", i)
		}
	}
	if got := h.exchangeCount(); got != 1 {
		t.Fatalf("a refused identity token was presented %d times, want 1", got)
	}

	h.writeToken("jti-2")
	if got := mustBearer(t, s); got != "bearer-1" {
		t.Fatalf("after the file changed, bearer = %q, want bearer-1", got)
	}
	if got := h.exchangeCount(); got != 2 {
		t.Fatalf("sign-ins = %d, want 2", got)
	}
}

func TestAllowRetryPresentsARefusedTokenOnceMore(t *testing.T) {
	h := newSignInHarness(t)
	h.refused["jti-1"] = true
	h.writeToken("jti-1")
	s := h.signIn(true)
	_, _, _ = s.Bearer(context.Background())

	s.AllowRetry()
	s.AllowRetry()
	for i := 0; i < 20; i++ {
		_, _, _ = s.Bearer(context.Background())
	}
	if got := h.exchangeCount(); got != 2 {
		t.Fatalf("after AllowRetry a refused token was presented %d times in all, want 2", got)
	}
}

func TestAllowRetryNeverRepresentsAnAcceptedToken(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	s := h.signIn(true)
	mustBearer(t, s)

	s.AllowRetry()
	h.advance(time.Hour + time.Second)
	if _, _, err := s.Bearer(context.Background()); err == nil {
		t.Fatal("an expired bearer with a used token returned no error")
	}
	if got := h.exchangeCount(); got != 1 {
		t.Fatalf("AllowRetry let an accepted token be presented %d times, want 1", got)
	}
}

func TestSignInKeepsTheLastGoodBearerThroughARefusal(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	s := h.signIn(true)
	mustBearer(t, s)

	h.advance(time.Hour - 100*time.Second)
	h.refused["jti-2"] = true
	h.writeToken("jti-2")

	if got := mustBearer(t, s); got != "bearer-1" {
		t.Fatalf("through a refusal, bearer = %q, want the last good bearer-1", got)
	}
	settle(t, s)
	if got := mustBearer(t, s); got != "bearer-1" {
		t.Fatalf("after the refusal, bearer = %q, want the last good bearer-1", got)
	}
	rec, _ := s.Record()
	if rec.Outcome != "denied" || rec.Status != 401 || rec.Detail != "match_subject_prefix" {
		t.Fatalf("record = %+v, want denied / 401 / the vendor's reason", rec)
	}

	h.advance(101 * time.Second)
	if _, _, err := s.Bearer(context.Background()); err == nil {
		t.Fatal("an expired bearer was handed out after a refusal")
	}
}

func TestSignInRetriesATransportFaultNoFasterThanFiveSeconds(t *testing.T) {
	h := newSignInHarness(t)
	h.faulting = true
	h.writeToken("jti-1")
	s := h.signIn(true)

	for i := 0; i < 10; i++ {
		if _, _, err := s.Bearer(context.Background()); err == nil {
			t.Fatal("a sign-in the vendor never answered returned no error")
		}
	}
	if got := h.exchangeCount(); got != 1 {
		t.Fatalf("ten calls inside five seconds made %d attempts, want 1", got)
	}

	h.advance(5 * time.Second)
	_, _, _ = s.Bearer(context.Background())
	if got := h.exchangeCount(); got != 2 {
		t.Fatalf("after five seconds attempts = %d, want 2", got)
	}

	h.mu.Lock()
	h.faulting = false
	h.mu.Unlock()
	h.advance(5 * time.Second)
	if got := mustBearer(t, s); got != "bearer-1" {
		t.Fatalf("once the vendor answers, bearer = %q, want bearer-1", got)
	}
}

func TestSignInReadFailurePresentsNothing(t *testing.T) {
	h := newSignInHarness(t)
	s := h.signIn(true)

	if _, _, err := s.Bearer(context.Background()); err == nil {
		t.Fatal("a missing identity token returned no error")
	}
	if got := h.exchangeCount(); got != 0 {
		t.Fatalf("a missing identity token led to %d sign-ins, want 0", got)
	}

	h.writeToken("jti-1")
	mustBearer(t, s)

	// The file disappears for a moment while a new one is due. The bearer in
	// hand is still valid, and nothing was marked as presented by the failed
	// read: the next token is still signed in with.
	h.advance(time.Hour - 100*time.Second)
	if err := os.Remove(h.tokenFile); err != nil {
		t.Fatal(err)
	}
	if got := mustBearer(t, s); got != "bearer-1" {
		t.Fatalf("with the file unreadable, bearer = %q, want bearer-1", got)
	}
	h.writeToken("jti-2")
	mustBearer(t, s)
	settle(t, s)
	if got := mustBearer(t, s); got != "bearer-2" {
		t.Fatalf("once the file is back, bearer = %q, want bearer-2", got)
	}
}

func TestSignInWithoutExpiryIsShortLived(t *testing.T) {
	h := newSignInHarness(t)
	h.lifetime = 0
	h.writeToken("jti-1")
	s := h.signIn(true)

	_, expiresAt, err := s.Bearer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := signInEpoch.Add(60 * time.Second); !expiresAt.Equal(want) {
		t.Fatalf("a bearer with no stated expiry expires at %s, want %s", expiresAt, want)
	}
}

func TestRefusalOfATokenOlderThanTheProcessMayBeReplay(t *testing.T) {
	h := newSignInHarness(t)
	h.refused["old"] = true
	h.refused["new"] = true

	// Issued 100 seconds before the process started, valid for 600.
	iat := signInEpoch.Add(-100 * time.Second)
	h.writeTokenIssued("old", iat, iat.Add(600*time.Second))
	s := h.signIn(true)
	_, _, _ = s.Bearer(context.Background())

	rec, _ := s.Record()
	if !rec.MayBeReplay {
		t.Fatalf("a refused token issued before the process started is not marked as a possible replay: %+v", rec)
	}
	if want := iat.Add(480 * time.Second); !rec.NextTokenDue.Equal(want) {
		t.Fatalf("NextTokenDue = %s, want %s (80%% of the token's 600 seconds)", rec.NextTokenDue, want)
	}

	// A token issued after the process started cannot have been used by an
	// earlier process, so its refusal is a plain refusal.
	h.advance(10 * time.Second)
	h.writeTokenIssued("new", h.clock(), h.clock().Add(600*time.Second))
	_, _, _ = s.Bearer(context.Background())
	rec, _ = s.Record()
	if rec.MayBeReplay {
		t.Fatalf("a refused token issued after the process started is marked as a possible replay: %+v", rec)
	}
}

func TestSignInAfterConfigChangeWaitsForRotation(t *testing.T) {
	h := newSignInHarness(t)
	h.useWallClock()
	h.writeToken("jti-1")

	first := processVendorSignIn(h.config(true, "fp-old-ids"))
	if got := mustBearer(t, first); got != "bearer-1" {
		t.Fatalf("bearer = %q, want bearer-1", got)
	}

	// The operator changes an id. The identity token on disk is the one the
	// vendor has already accepted.
	second := processVendorSignIn(h.config(true, "fp-new-ids"))
	if second == first {
		t.Fatal("a changed configuration kept the old sign-in")
	}
	bearer, _, err := second.Bearer(context.Background())
	var waiting *vendorSignInWaiting
	if !errors.As(err, &waiting) {
		t.Fatalf("under new ids with a used token: bearer=%q err=%v, want a *vendorSignInWaiting", bearer, err)
	}
	if got := h.exchangeCount(); got != 1 {
		t.Fatalf("the used token was presented again under the new ids: %d sign-ins, want 1", got)
	}
	rec, _ := second.Record()
	if rec.Outcome != "waiting" || rec.Fingerprint != "fp-new-ids" {
		t.Fatalf("record = %+v, want waiting under fp-new-ids", rec)
	}

	h.writeToken("jti-2")
	if got := mustBearer(t, second); got != "bearer-2" {
		t.Fatalf("with a new identity token, bearer = %q, want bearer-2", got)
	}
}

func TestProcessVendorSignInIsSharedAndSurvivesTheSameConfig(t *testing.T) {
	h := newSignInHarness(t)
	h.useWallClock()
	h.writeToken("jti-1")

	a := processVendorSignIn(h.config(true, "fp-1"))
	mustBearer(t, a)
	b := processVendorSignIn(h.config(true, "fp-1"))
	if a != b {
		t.Fatal("the same configuration produced two sign-ins")
	}
	mustBearer(t, b)
	if got := h.exchangeCount(); got != 1 {
		t.Fatalf("two holders of one configuration made %d sign-ins, want 1", got)
	}
	cur, ok := currentVendorSignIn(metrics.FederationVendorAnthropic)
	if !ok || cur != a {
		t.Fatal("currentVendorSignIn does not return the process's sign-in")
	}
}

func TestStatusFileCarriesNoCredential(t *testing.T) {
	h := newSignInHarness(t)
	identityToken := h.writeToken("jti-1")
	s := h.signIn(true)
	bearer := mustBearer(t, s)

	path := vendorSignInStatusPath(metrics.FederationVendorAnthropic)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the sign-in wrote no status file: %v", err)
	}
	if bytes.Contains(raw, []byte(bearer)) {
		t.Fatal("the status file holds the bearer")
	}
	if bytes.Contains(raw, []byte(identityToken)) || bytes.Contains(raw, []byte(strings.Split(identityToken, ".")[1])) {
		t.Fatal("the status file holds the identity token")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("status file mode = %o, want 600", mode)
	}

	rec, ok, err := readVendorSignInStatus(metrics.FederationVendorAnthropic)
	if err != nil || !ok {
		t.Fatalf("readVendorSignInStatus: ok=%v err=%v", ok, err)
	}
	if rec.Outcome != "ok" || rec.Fingerprint != "fp-1" || rec.TokenSubject != "system:serviceaccount:memql:memql-engine" {
		t.Fatalf("status = %+v, want ok / fp-1 / the token's subject", rec)
	}
	if want := signInEpoch.Add(time.Hour); !rec.ExpiresAt.Equal(want) {
		t.Fatalf("status ExpiresAt = %s, want %s", rec.ExpiresAt, want)
	}
	if rec.Ids["federationRuleId"] != "fdrl_test" {
		t.Fatalf("status ids = %v, want the configured rule id", rec.Ids)
	}
}

func TestReadVendorSignInStatusSaysWhenThereIsNone(t *testing.T) {
	newSignInHarness(t)
	_, ok, err := readVendorSignInStatus(metrics.FederationVendorOpenAI)
	if err != nil || ok {
		t.Fatalf("with no status file: ok=%v err=%v, want false and no error", ok, err)
	}
}

func TestStatusFileFailureDoesNotBreakSignIn(t *testing.T) {
	h := newSignInHarness(t)
	// A temporary directory that is a file: nothing can be written under it.
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", notADir)

	h.writeToken("jti-1")
	s := h.signIn(true)
	if got := mustBearer(t, s); got != "bearer-1" {
		t.Fatalf("bearer = %q, want bearer-1 even though the status file cannot be written", got)
	}
	if _, ok, _ := readVendorSignInStatus(metrics.FederationVendorAnthropic); ok {
		t.Fatal("a status was read back from a directory nothing can be written to")
	}
}

func TestInvalidateForcesASignInOnlyWithANewToken(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	s := h.signIn(true)
	mustBearer(t, s)

	// A stale bearer is named: the one in hand is kept.
	s.Invalidate("some-earlier-bearer")
	if got := mustBearer(t, s); got != "bearer-1" {
		t.Fatalf("invalidating another bearer dropped the current one: %q", got)
	}

	// The vendor answered an API call with 401: the bearer is no good.
	s.Invalidate("bearer-1")
	if bearer, _, err := s.Bearer(context.Background()); err == nil {
		t.Fatalf("an invalidated bearer was handed out again: %q", bearer)
	}
	if got := h.exchangeCount(); got != 1 {
		t.Fatalf("Invalidate presented the used token again: %d sign-ins, want 1", got)
	}

	h.writeToken("jti-2")
	if got := mustBearer(t, s); got != "bearer-2" {
		t.Fatalf("with a new identity token, bearer = %q, want bearer-2", got)
	}
}

// A claim that is not a time a clock could hold reads as ABSENT, never as a
// date. A bare int64(...) on an out-of-range float is implementation-defined
// (memql#4779), and a made-up date here would call a fresh token replayed, or
// promise a rotation at a time that never comes.
func TestIdentityTokenTimesIgnoreANumberNoClockCouldHold(t *testing.T) {
	token := func(iat, exp any) string {
		payload, err := json.Marshal(map[string]any{"iat": iat, "exp": exp})
		if err != nil {
			t.Fatalf("marshal claims: %v", err)
		}
		return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".c2ln"
	}

	for name, v := range map[string]float64{"too large": 1e300, "too small": -1e300} {
		iat, exp := identityTokenTimes(token(v, v))
		if !iat.IsZero() || !exp.IsZero() {
			t.Errorf("%s (%g): read as iat=%s exp=%s, want both absent", name, v, iat, exp)
		}
		if due := nextIdentityTokenDue(token(v, v)); !due.IsZero() {
			t.Errorf("%s (%g): a rotation was promised for %s out of a time no clock holds", name, v, due)
		}
	}

	// A fractional time is legal (RFC 7519's NumericDate) and reads to the second.
	iat, exp := identityTokenTimes(token(1700000000.75, 1700000600))
	if iat.Unix() != 1700000000 || exp.Unix() != 1700000600 {
		t.Errorf("fractional iat read as %d and exp as %d, want 1700000000 and 1700000600", iat.Unix(), exp.Unix())
	}
}

// ---------------------------------------------------------------------------
// A sign-in that hangs, and callers that should not pay for it
// ---------------------------------------------------------------------------

// waitUntilSignIn polls until cond holds. The sign-in runs its exchange on a goroutine
// of its own, so a test that asserts on it has to wait for it to get there.
func waitUntilSignIn(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 5s for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *signInHarness) flying() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inFlight
}

// settle waits until no exchange is in flight for s.
func settle(t *testing.T, s *vendorSignIn) {
	t.Helper()
	waitUntilSignIn(t, "the sign-in to finish", func() bool { return !s.signingIn() })
}

// A vendor that hangs is ONE attempt, however many callers are waiting. The
// lock used to be held across the exchange and the back-off measured from the
// attempt's start, so each waiter in turn made a 20 second attempt of its own:
// five callers were five sign-ins and 100 seconds.
func TestAHungSignInIsOneAttemptForEveryWaiter(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	h.hang = make(chan struct{})
	s := h.signIn(true)

	const callers = 5
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			_, _, err := s.Bearer(context.Background())
			results <- err
		}()
	}
	waitUntilSignIn(t, "the exchange to start", func() bool { return h.flying() == 1 })
	// Give the other callers time to arrive and find it in flight.
	time.Sleep(50 * time.Millisecond)
	if got := h.exchangeCount(); got != 1 {
		t.Fatalf("%d exchanges are in flight for one hung vendor, want 1", got)
	}

	close(h.hang)
	for i := 0; i < callers; i++ {
		if err := <-results; err != nil {
			t.Fatalf("a waiter did not get the bearer the one sign-in bought: %v", err)
		}
	}
	if got := h.exchangeCount(); got != 1 {
		t.Fatalf("the callers made %d sign-ins, want 1", got)
	}
}

// A waiter whose own context ends stops waiting. It does not stop the sign-in,
// which serves everybody else.
func TestAWaiterLeavesWhenItsContextEnds(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	h.hang = make(chan struct{})
	s := h.signIn(true)

	ctx, cancel := context.WithCancel(context.Background())
	left := make(chan error, 1)
	go func() {
		_, _, err := s.Bearer(ctx)
		left <- err
	}()
	waitUntilSignIn(t, "the exchange to start", func() bool { return h.flying() == 1 })
	cancel()
	select {
	case err := <-left:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the cancelled waiter returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a caller whose context ended kept waiting for the sign-in")
	}

	close(h.hang)
	settle(t, s)
	if got := mustBearer(t, s); got != "bearer-1" {
		t.Fatalf("the sign-in the cancelled caller started was thrown away: got %q", got)
	}
	if got := h.exchangeCount(); got != 1 {
		t.Fatalf("exchanges = %d, want 1", got)
	}
}

// A caller that already holds a working bearer never waits on its renewal.
func TestACallerWithAValidBearerDoesNotWaitForARefresh(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	s := h.signIn(true)
	if got := mustBearer(t, s); got != "bearer-1" {
		t.Fatalf("first bearer = %q", got)
	}

	// Inside the renewal window, with a new token on disk and a vendor that
	// hangs: the bearer in hand has 110 seconds left.
	h.advance(time.Hour - 110*time.Second)
	h.writeToken("jti-2")
	h.hang = make(chan struct{})

	type answer struct {
		bearer string
		err    error
	}
	got := make(chan answer, 1)
	go func() {
		bearer, _, err := s.Bearer(context.Background())
		got <- answer{bearer, err}
	}()
	select {
	case a := <-got:
		if a.err != nil || a.bearer != "bearer-1" {
			t.Fatalf("got (%q, %v), want the bearer still in hand", a.bearer, a.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a caller holding a valid bearer waited for its renewal")
	}
	waitUntilSignIn(t, "the renewal to start", func() bool { return h.flying() == 1 })

	close(h.hang)
	settle(t, s)
	if got := mustBearer(t, s); got != "bearer-2" {
		t.Fatalf("after the renewal the bearer is %q, want the new one", got)
	}
	if got := h.exchangeCount(); got != 2 {
		t.Fatalf("exchanges = %d, want 2", got)
	}
}

// The pause after a sign-in that got no answer is counted from when it ENDED.
// Counted from its start, an attempt that took longer than the pause left no
// pause at all.
func TestBackOffIsMeasuredFromTheAttemptsEnd(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	h.faulting = true
	h.takes = 20 * time.Second
	s := h.signIn(true)

	if _, _, err := s.Bearer(context.Background()); err == nil {
		t.Fatal("a vendor that does not answer returned a bearer")
	}
	// The clock is now 20 seconds on. A caller arriving straight away must not
	// start another attempt.
	if _, _, err := s.Bearer(context.Background()); err == nil {
		t.Fatal("the second call returned a bearer")
	}
	if got := h.exchangeCount(); got != 1 {
		t.Fatalf("a second sign-in started %s after the first ended, want none inside %s (exchanges = %d)",
			time.Duration(0), vendorSignInRetryBackoff, got)
	}
	h.advance(vendorSignInRetryBackoff + time.Second)
	_, _, _ = s.Bearer(context.Background())
	if got := h.exchangeCount(); got != 2 {
		t.Fatalf("after the pause exchanges = %d, want 2", got)
	}
}

// "This may only be a replay" is something only a vendor with a replay rule
// can mean. OpenAI has none, so a refusal there is a refusal.
func TestOnlyAReplayProtectedVendorCallsARefusalAPossibleReplay(t *testing.T) {
	for name, replayProtected := range map[string]bool{"replay rule": true, "no replay rule": false} {
		t.Run(name, func(t *testing.T) {
			h := newSignInHarness(t)
			// Issued a minute before the process started, as every new pod's
			// first token is: kubelet mints it before the container starts.
			h.writeTokenIssued("jti-old", signInEpoch.Add(-time.Minute), signInEpoch.Add(9*time.Minute))
			h.refused["jti-old"] = true
			s := h.signIn(replayProtected)
			if _, _, err := s.Bearer(context.Background()); err == nil {
				t.Fatal("a refused sign-in returned a bearer")
			}
			rec, ok := s.Record()
			if !ok || rec.Outcome != signInOutcomeDenied {
				t.Fatalf("record = %+v, want denied", rec)
			}
			if rec.MayBeReplay != replayProtected {
				t.Fatalf("MayBeReplay = %v, want %v", rec.MayBeReplay, replayProtected)
			}
		})
	}
}

// What the record says after the sign-in: whether the vendor's API took the
// bearer. An accepted sign-in is not yet a usable vendor.
func TestTheRecordSaysWhetherTheBearerWasProven(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	s := h.signIn(true)
	bearer := mustBearer(t, s)

	rec, _ := s.Record()
	if rec.Proof != "" {
		t.Fatalf("a bearer nothing has used is recorded as %q", rec.Proof)
	}

	s.NoteModelList(0, &vendorExchangeRefused{Status: 403, Body: "permission_error"})
	if rec, _ = s.Record(); rec.Proof != signInProofRefused || rec.ProofStatus != 403 {
		t.Fatalf("after a refused model list the record is %+v", rec)
	}

	s.NoteModelList(7, nil)
	if rec, _ = s.Record(); rec.Proof != signInProofOK || rec.ModelsListed != 7 {
		t.Fatalf("after a model list the record is %+v", rec)
	}

	// A served call proves a bearer as well as a model list does.
	h.advance(time.Hour)
	h.writeToken("jti-2")
	next := mustBearer(t, s)
	if rec, _ = s.Record(); rec.Proof != "" {
		t.Fatalf("a new bearer inherited the proof of the last one: %+v", rec)
	}
	s.NoteServed(bearer) // the OLD bearer says nothing about the new one
	if rec, _ = s.Record(); rec.Proof != "" {
		t.Fatalf("a call served on an earlier bearer proved the new one: %+v", rec)
	}
	s.NoteServed(next)
	if rec, _ = s.Record(); rec.Proof != signInProofOK {
		t.Fatalf("a served call did not prove the bearer: %+v", rec)
	}
}

// Every node renews its bearer about once an hour, and the call that starts a
// renewal is served with the bearer already in hand. If the renewal cleared the
// proof, a node with little traffic would read as "not verified" from each
// renewal until its next call, for a vendor that had been working all along.
func TestARenewalWhileTheBearerStillWorksKeepsTheProof(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	s := h.signIn(true)
	first := mustBearer(t, s)
	s.NoteModelList(4, nil)
	provenAt := h.clock()

	h.advance(59 * time.Minute) // inside the renewal window; the bearer still works
	h.writeToken("jti-2")
	if got := mustBearer(t, s); got != first {
		t.Fatalf("the caller was handed %q, want the bearer in hand %q", got, first)
	}
	settle(t, s)
	rec, _ := s.Record()
	if rec.Proof != signInProofOK || rec.ModelsListed != 4 || !rec.ProofAt.Equal(provenAt) {
		t.Fatalf("after a renewal the record is %+v, want the proof kept as it was", rec)
	}

	// The first call served with the new bearer is its own proof.
	next := mustBearer(t, s)
	if next == first {
		t.Fatal("the renewal bought no new bearer")
	}
	h.advance(time.Minute)
	s.NoteServed(next)
	if rec, _ = s.Record(); !rec.ProofAt.Equal(h.clock()) {
		t.Fatalf("a call served with the new bearer did not renew the proof: %+v", rec)
	}
}

// A reload lists the vendor's models again with the bearer in hand. When that
// list fails, for a timeout or a refusal, the record says so. The next call the
// vendor serves with the same bearer is newer evidence and must be allowed to
// say so too: the record would otherwise read "no answer" on a node that is
// serving calls.
func TestAServedCallOvertakesAFailedModelList(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	s := h.signIn(true)
	bearer := mustBearer(t, s)
	s.NoteServed(bearer)
	if rec, _ := s.Record(); rec.Proof != signInProofOK {
		t.Fatalf("a served call did not prove the bearer: %+v", rec)
	}

	s.NoteModelList(0, errors.New("context deadline exceeded"))
	if rec, _ := s.Record(); rec.Proof != signInProofError {
		t.Fatalf("a failed model list is not on the record: %+v", rec)
	}

	h.advance(time.Minute)
	s.NoteServed(bearer)
	rec, _ := s.Record()
	if rec.Proof != signInProofOK || !rec.ProofAt.Equal(h.clock()) {
		t.Fatalf("after the vendor served a call the record still reads %+v", rec)
	}
}

// The engine asks whether kubelet has replaced the identity token before it
// spends a sign-in pass on it.
func TestASignInKnowsWhenANewTokenIsOnDisk(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	s := h.signIn(true)
	if !s.newTokenOnDisk() {
		t.Fatal("a token that was never presented is not seen as new")
	}
	mustBearer(t, s)
	if s.newTokenOnDisk() {
		t.Fatal("the token just presented is seen as new")
	}
	h.writeToken("jti-2")
	if !s.newTokenOnDisk() {
		t.Fatal("a replaced token is not seen as new")
	}
}

// A bearer the vendor stopped accepting is on the record, not only gone.
func TestADroppedBearerIsOnTheRecord(t *testing.T) {
	h := newSignInHarness(t)
	h.writeToken("jti-1")
	s := h.signIn(true)
	bearer := mustBearer(t, s)
	s.NoteModelList(3, nil)

	h.advance(10 * time.Minute)
	s.Invalidate(bearer)
	rec, _ := s.Record()
	if rec.BearerDroppedAt.IsZero() {
		t.Fatalf("the record still reads as a working sign-in: %+v", rec)
	}
	onDisk, found, err := readVendorSignInStatus(metrics.FederationVendorAnthropic)
	if err != nil || !found || onDisk.BearerDroppedAt.IsZero() {
		t.Fatalf("the status file does not say the bearer was dropped: %+v found=%v err=%v", onDisk, found, err)
	}
}

// advanceClock moves a sign-in's clock forward. For a test of a sign-in the
// process built, which reads the wall clock and cannot be handed another.
func (s *vendorSignIn) advanceClock(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := s.now
	s.now = func() time.Time { return base().Add(d) }
}

// signingIn reports whether an exchange is in progress. Test scaffolding: a
// test that has just caused one waits on it before asserting.
func (s *vendorSignIn) signingIn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flight != nil
}
