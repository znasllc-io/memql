package emailrules

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// lateMemberEngine answers the first lookup of the mailbox with nobody and every
// later one with a member: someone added the address between the plan and the enrolment.
type lateMemberEngine struct {
	rowAddressEngine
	lookups int
	mu2     sync.Mutex
}

func (e *lateMemberEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "query recipientInAudienceByDigest") {
		e.mu2.Lock()
		e.lookups++
		if e.lookups > 1 {
			e.byDigest = []map[string]any{memberRow("added-by-hand", "new@example.test", "subscribed")}
		}
		e.mu2.Unlock()
	}
	return e.rowAddressEngine.Execute(ctx, q)
}

// An enrolment is made under the audience's lock and looks the mailbox up again,
// so an address another path added meanwhile is mailed through that row.
func TestRowAddressEnrolmentRechecksTheMailboxUnderTheAudienceLock(t *testing.T) {
	e := &lateMemberEngine{rowAddressEngine: rowAddressEngine{fireEngine: fireEngine{rule: rowAddressRule()}}}
	held, released := 0, 0
	gate := func(_ context.Context, audience string) (func(), error) {
		held++
		return func() { released++ }, nil
	}
	event := map[string]any{"timestamp": "2026-10-01T01:00:00.000001Z", "payload": map[string]any{"primaryContactEmail": "new@example.test"}}
	out, err := NewFirer(e).WithMembershipGate(gate).Fire(context.Background(), "v1:campaigns:emailRule:ab12cd34", "v1:accounts:account:org-1", event)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.callsWithPrefix("mutation addRecipient("); len(got) != 0 {
		t.Fatalf("the address was enrolled again: %v", got)
	}
	if out.Sent != 1 || held != 1 || released != 1 {
		t.Fatalf("outcome %+v, lock held %d released %d", out, held, released)
	}
	if ids := e.sendsTo(); len(ids) != 1 || ids[0] != "added-by-hand" {
		t.Fatalf("the send went to %v", ids)
	}
}

// A busy audience lock attempts nothing, so a created rule's one-time claim goes
// back with it and the row fires once the audience is free.
func TestABusyAudienceLockReleasesACreatedRulesClaim(t *testing.T) {
	noPauses(t)
	rule := rowAddressRule()
	rule["eventKind"] = "created"
	e := &busySends{rowAddressEngine: rowAddressEngine{fireEngine: fireEngine{rule: rule}}}
	claims := &releasableClaims{}
	busy := true
	gate := func(context.Context, string) (func(), error) {
		if busy {
			return nil, memql.ErrWriteGateBusy
		}
		return func() {}, nil
	}
	firer := NewFirer(e).WithFirstFireClaim(claims.claim).WithFirstFireRelease(claims.release).WithMembershipGate(gate)
	out, err := firer.Fire(context.Background(), busyRule, busyRow, busyEvent())
	if err != nil || !out.Busy || out.Sent != 0 {
		t.Fatalf("a busy audience lock: %+v %v", out, err)
	}
	if claims.isHeld(busyRule, busyRow) {
		t.Fatal("the row's claim is still held: the welcome for this row can never be sent")
	}
	busy = false
	out, err = firer.Fire(context.Background(), busyRule, busyRow, busyEvent())
	if err != nil || out.Duplicate || out.Sent != 1 {
		t.Fatalf("the row could not fire once the audience was free: %+v %v", out, err)
	}
}

// Any other failure to take the lock is an error, not "busy".
func TestAnAudienceLockThatCannotBeTakenIsNotReportedBusy(t *testing.T) {
	noPauses(t)
	e := &busySends{rowAddressEngine: rowAddressEngine{fireEngine: fireEngine{rule: rowAddressRule()}}}
	gate := func(context.Context, string) (func(), error) { return nil, errors.New("no write-gate pool") }
	out, err := NewFirer(e).WithMembershipGate(gate).Fire(context.Background(), busyRule, busyRow, busyEvent())
	if err == nil || out.Busy {
		t.Fatalf("a missing pool was reported as busy: %+v %v", out, err)
	}
}

// The lock is given back before the send: a send can pause for seconds.
func TestTheAudienceLockIsReleasedBeforeTheSend(t *testing.T) {
	e := &sendWatcher{rowAddressEngine: rowAddressEngine{fireEngine: fireEngine{rule: rowAddressRule()}}}
	gate := func(context.Context, string) (func(), error) {
		e.held = true
		return func() { e.held = false }, nil
	}
	out, err := NewFirer(e).WithMembershipGate(gate).Fire(context.Background(), busyRule, busyRow, busyEvent())
	if err != nil || out.Sent != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	if e.heldAtSend {
		t.Fatal("the audience lock was still held while the email was sent")
	}
}

type sendWatcher struct {
	rowAddressEngine
	held, heldAtSend bool
}

func (e *sendWatcher) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "builtin campaignSendToRecipient") && e.held {
		e.heldAtSend = true
	}
	return e.rowAddressEngine.Execute(ctx, q)
}
