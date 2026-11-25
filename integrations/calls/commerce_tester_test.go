package calls

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

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
