package calls

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/integrations/calls/providers"
)

// A fixed, test-only tester password and its hash line (salt 0x01..0x10). Never
// configured on an installation.
const (
	testTesterPassword     = "TQ7ZK-M4WHX-CR8NE-P3VJB"
	testTesterPasswordHash = "v1$AQIDBAUGBwgJCgsMDQ4PEA$H82ui-STcfwRoIkZ4dzTUem4-2MysKX6WGEbuQtCmhY"
)

func testTesterHash(t *testing.T) *testerPasswordHash {
	t.Helper()
	h, ok := parseTesterPasswordHash(testTesterPasswordHash)
	require.True(t, ok)
	require.True(t, h.matches(testTesterPassword))
	return h
}

func TestCommerceRequestTesterPasswordPlacement(t *testing.T) {
	const pw = "K7QPM-3XWRT-9HNVA-ZD4ME"
	result := func(action, outcome, password string) callrules.AccountRequest {
		return callrules.AccountRequest{Action: action, AccountID: "account-1", OperationID: "operation-1", Outcome: outcome, TesterPassword: password}
	}
	begin := func(action, password string) callrules.AccountRequest {
		r := callrules.AccountRequest{Action: action, AccountID: "account-1", IdempotencyKey: "tester-password-0001", AdultDeclared: true, TermsAccepted: true, TesterPassword: password}
		if action == "checkout_begin" {
			r.AmountCents = 1000
		}
		return r
	}

	// The gated results carry it; nothing else may.
	require.True(t, validCommerceRequest(result("mock_checkout_complete", "paid", pw)))
	require.True(t, validCommerceRequest(result("mock_verification_complete", "approved", pw)))
	require.True(t, validCommerceRequest(result("mock_checkout_complete", "paid", strings.Repeat("A", 64))))
	require.False(t, validCommerceRequest(result("mock_checkout_complete", "paid", strings.Repeat("A", 65))))
	require.False(t, validCommerceRequest(result("mock_checkout_complete", "paid", strings.Repeat("A", 10000))))

	// Backing out never needs it and never accepts it.
	require.True(t, validCommerceRequest(result("mock_checkout_complete", "cancelled", "")))
	require.True(t, validCommerceRequest(result("mock_verification_complete", "abandoned", "")))
	require.False(t, validCommerceRequest(result("mock_checkout_complete", "cancelled", pw)))
	require.False(t, validCommerceRequest(result("mock_verification_complete", "abandoned", pw)))

	for _, action := range []string{"checkout_refresh", "verification_refresh"} {
		require.True(t, validCommerceRequest(result(action, "", "")), action)
		require.False(t, validCommerceRequest(result(action, "", pw)), action)
	}
	for _, action := range []string{"checkout_begin", "verification_begin"} {
		require.True(t, validCommerceRequest(begin(action, "")), action)
		require.False(t, validCommerceRequest(begin(action, pw)), action)
	}

	require.False(t, validAccountRequest(callrules.AccountRequest{Action: "login_begin", TesterPassword: pw}))
	require.False(t, validAccountRequest(callrules.AccountRequest{Action: "simulate_topup", AccountID: "account-1", AmountCents: 1000, IdempotencyKey: "tester-password-0002", TesterPassword: pw}))
}

func TestTesterFailuresCountDownThenPause(t *testing.T) {
	v := newCommerceState("site-1", "key-1", providers.Mock)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	for want := 4; want >= 1; want-- {
		left, pause := v.recordTesterFailure(now)
		require.Equal(t, want, left)
		require.Zero(t, pause)
		require.Zero(t, v.testerPause(now))
		now = now.Add(time.Minute)
	}
	left, pause := v.recordTesterFailure(now)
	require.Zero(t, left)
	require.Equal(t, 15*time.Minute, pause)
	require.Empty(t, v.TesterFailures)
	require.Equal(t, now.Add(15*time.Minute), v.TesterPausedUntil)
	require.Equal(t, 15*time.Minute, v.testerPause(now))
	require.Zero(t, v.testerPause(now.Add(15*time.Minute)))
}

func TestTesterPauseLastsFromFifthFailure(t *testing.T) {
	v := newCommerceState("site-1", "key-1", providers.Mock)
	start := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	for _, minute := range []int{0, 3, 6, 9, 14} {
		v.recordTesterFailure(start.Add(time.Duration(minute) * time.Minute))
	}
	require.Equal(t, 14*time.Minute, v.testerPause(start.Add(15*time.Minute)))
}

func TestTesterFailuresPruneAfterWindow(t *testing.T) {
	v := newCommerceState("site-1", "key-1", providers.Mock)
	start := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	for minute := 0; minute < 4; minute++ {
		v.recordTesterFailure(start.Add(time.Duration(minute) * time.Minute))
	}
	left, pause := v.recordTesterFailure(start.Add(19 * time.Minute))
	require.Equal(t, 4, left)
	require.Zero(t, pause)
	require.Len(t, v.TesterFailures, 1)
}

func TestTesterPauseClampsClockRollback(t *testing.T) {
	v := newCommerceState("site-1", "key-1", providers.Mock)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	v.TesterPausedUntil = now.Add(time.Hour)
	require.Equal(t, 15*time.Minute, v.testerPause(now))
	// Failures recorded "in the future" before a rollback still prune and count
	// as at most now.
	v = newCommerceState("site-1", "key-1", providers.Mock)
	v.TesterFailures = []time.Time{now.Add(time.Hour), now.Add(time.Hour)}
	left, pause := v.recordTesterFailure(now)
	require.Equal(t, 2, left)
	require.Zero(t, pause)
}

func TestTesterBackOutOutcomes(t *testing.T) {
	require.True(t, testerBackOut("checkout", "cancelled"))
	require.True(t, testerBackOut("verification", "abandoned"))
	for _, c := range [][2]string{{"checkout", "paid"}, {"checkout", "declined"}, {"checkout", "abandoned"}, {"verification", "cancelled"}, {"verification", "approved"}, {"verification", "expired"}, {"auto_topup", "cancelled"}} {
		require.False(t, testerBackOut(c[0], c[1]), c)
	}
}

func TestTesterGateOrder(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	c := CommerceConfig{Mode: providers.Mock, TesterPassword: testTesterHash(t)}
	req := func(outcome, password string) callrules.AccountRequest {
		return callrules.AccountRequest{Action: "mock_checkout_complete", Outcome: outcome, TesterPassword: password}
	}
	v := newCommerceState("site-1", "key-1", providers.Mock)

	changed, err := testerGate(c, v, "checkout", req("cancelled", ""), now)
	require.NoError(t, err)
	require.False(t, changed)

	changed, err = testerGate(CommerceConfig{Mode: providers.Mock}, v, "checkout", req("paid", testTesterPassword), now)
	require.False(t, changed)
	require.Equal(t, callrules.ReasonTesterAccessUnavailable, callrules.ReasonOf(err))
	require.ErrorIs(t, err, callrules.ErrForbidden)

	changed, err = testerGate(c, v, "checkout", req("paid", ""), now)
	require.False(t, changed)
	require.Equal(t, callrules.ReasonTesterPasswordRequired, callrules.ReasonOf(err))
	require.ErrorIs(t, err, callrules.ErrInvalid)

	for want := 4; want >= 1; want-- {
		changed, err = testerGate(c, v, "checkout", req("paid", "WRONG-WRONG-WRONG-WRONG"), now)
		require.True(t, changed)
		var incorrect *callrules.TesterPasswordIncorrect
		require.True(t, errors.As(err, &incorrect))
		require.Equal(t, want, incorrect.AttemptsLeft)
	}
	changed, err = testerGate(c, v, "checkout", req("paid", "WRONG-WRONG-WRONG-WRONG"), now)
	require.True(t, changed)
	var paused *callrules.TesterPasswordPaused
	require.True(t, errors.As(err, &paused))
	require.Equal(t, 15*time.Minute, paused.RetryAfter)

	// Paused refuses even the right password, so parallel requests cannot exceed
	// the limit; backing out still works.
	changed, err = testerGate(c, v, "checkout", req("paid", testTesterPassword), now.Add(time.Minute))
	require.False(t, changed)
	require.True(t, errors.As(err, &paused))
	require.Equal(t, 14*time.Minute, paused.RetryAfter)
	_, err = testerGate(c, v, "checkout", req("cancelled", ""), now.Add(time.Minute))
	require.NoError(t, err)

	later := now.Add(15 * time.Minute)
	changed, err = testerGate(c, v, "checkout", req("paid", " "+strings.ToLower(testTesterPassword)+"\n"), later)
	require.NoError(t, err)
	require.True(t, changed, "success clears the expired pause")
	require.Empty(t, v.TesterFailures)
	require.True(t, v.TesterPausedUntil.IsZero())
	changed, err = testerGate(c, v, "checkout", req("paid", testTesterPassword), later)
	require.NoError(t, err)
	require.False(t, changed, "nothing to clear")
}

func TestCommerceFromRowRejectsSixTesterFailures(t *testing.T) {
	row := func(failures int) map[string]any {
		v := newCommerceState("site-1", "key-1", providers.Mock)
		for i := 0; i < failures; i++ {
			v.TesterFailures = append(v.TesterFailures, time.Date(2026, 11, 25, 12, i, 0, 0, time.UTC))
		}
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		return map[string]any{"id": commerceID("site-1", "key-1"), "siteId": "site-1", "keyId": "key-1", "createdAt": time.Now().UTC(), "stateData": string(raw)}
	}
	got, err := commerceFromRow(row(5))
	require.NoError(t, err)
	require.Len(t, got.TesterFailures, 5)
	_, err = commerceFromRow(row(6))
	require.Error(t, err)
}
