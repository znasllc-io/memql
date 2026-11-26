package calls

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

// In mock mode nothing but a result ever finishes a checkout, so a checkout a
// stranger starts and abandons must hold no capacity; the allowance is checked
// when a tester applies a password-checked successful payment.
func TestCommerceMockCheckoutBeginHoldsNoVerificationSlot(t *testing.T) {
	cw, one, c := commerceFixture(t, "tester-mock-slot")
	if cw == nil {
		return
	}
	c.MaxPendingVerifications = 1
	two := accountRegister(t, cw, newSoftwareAuthenticator(t))
	testerBegin(t, cw, one, "tester-mock-slot-one-01")
	op := testerBegin(t, cw, two, "tester-mock-slot-two-01")
	paid, err := testerApply(cw, cw.svc, two, "checkout", op, "paid", testTesterCode)
	require.NoError(t, err)
	require.EqualValues(t, 1000, *paid.Account.BalanceCents)
}

func TestCommerceMockAbandonedCheckoutsHoldNothing(t *testing.T) {
	cw, tester, c := commerceFixture(t, "tester-mock-abandoned")
	if cw == nil {
		return
	}
	// callkey.ai's envelope: 80,000 cents of room at a 50% fulfillment reserve.
	c.AvailableCashCents, c.ProtectedCents, c.FixedCents, c.FulfillmentReserveBPS = 100000, 10000, 10000, 5000
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		stranger := accountRegister(t, cw, newSoftwareAuthenticator(t))
		commerceDo(t, cw, stranger, callrules.AccountRequest{Action: "checkout_begin", AmountCents: 50000, IdempotencyKey: fmt.Sprintf("tester-abandoned-%02d-01", i), AdultDeclared: true, TermsAccepted: true})
	}
	op := testerBegin(t, cw, tester, "tester-abandoned-tester-1")
	paid, err := testerApply(cw, cw.svc, tester, "checkout", op, "paid", testTesterCode)
	require.NoError(t, err)
	require.EqualValues(t, 1000, *paid.Account.BalanceCents)
	require.NoError(t, cw.svc.commerceCapacity(ctx, *c, 0, 1, 0, 1), "abandoned checkouts must leave room for calls")
}

func TestCommerceMockPaidRefusedWithoutRoom(t *testing.T) {
	cw, k, c := commerceFixture(t, "tester-mock-no-room")
	if cw == nil {
		return
	}
	op := testerBegin(t, cw, k, "tester-no-room-check-01")
	// Other purchases used the room after this checkout began.
	cash := c.AvailableCashCents
	c.AvailableCashCents = c.ProtectedCents + c.FixedCents + 100
	_, err := testerApply(cw, cw.svc, k, "checkout", op, "paid", testTesterCode)
	require.ErrorContains(t, err, "business_reserve")
	require.Zero(t, testerBalance(t, cw, k))
	state := testerState(t, cw, k)
	require.Equal(t, "pending", state.latest("checkout").Status)
	require.Zero(t, state.PaidCents)

	c.AvailableCashCents = cash
	paid, err := testerApply(cw, cw.svc, k, "checkout", op, "paid", testTesterCode)
	require.NoError(t, err)
	require.EqualValues(t, 1000, *paid.Account.BalanceCents)
	again, err := testerApply(cw, cw.svc, k, "checkout", op, "paid", testTesterCode)
	require.NoError(t, err)
	require.EqualValues(t, 1000, *again.Account.BalanceCents)
}
