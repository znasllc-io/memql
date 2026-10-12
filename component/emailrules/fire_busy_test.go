package emailrules

// fire_busy_test.go -- a lock that was busy is "nothing attempted, try later"
// (memql#6022, F9 fix round 3).
//
// A created rule sends once per row: it claims the row first, and a firing that
// finds the claim taken sends nothing. The send it makes can find the pod's lock
// busy (the campaigns domain's receipt lock, or the transport's) before it has
// attempted anything. That firing used to end with the claim spent and no mail, and
// nothing could ever fire for the row again.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// busySends is a rowAddressEngine whose send answers busy for the first n calls,
// the way the engine hands back a handler's error: the type is gone and the words
// remain.
type busySends struct {
	rowAddressEngine
	busyFor int
	sends   int
	other   error // when set, every send answers this instead
}

func (e *busySends) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "builtin campaignSendToRecipient") {
		e.mu.Lock()
		e.sends++
		e.calls = append(e.calls, q)
		busy, other := e.busyFor > 0, e.other
		if busy {
			e.busyFor--
		}
		e.mu.Unlock()
		switch {
		case other != nil:
			return nil, other
		case busy:
			return nil, errors.New("builtin campaignSendToRecipient: campaigns.sendToRecipient: " + memql.ErrWriteGateBusy.Error())
		}
		return rowsEnvelope([]map[string]any{{"sent": true}}), nil
	}
	return e.rowAddressEngine.Execute(ctx, q)
}

func (e *busySends) sendCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sends
}

// releasableClaims is the claim and its release, held in memory.
type releasableClaims struct {
	mu       sync.Mutex
	held     map[string]bool
	released int
}

func (c *releasableClaims) claim(_ context.Context, ruleID, rowID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held == nil {
		c.held = map[string]bool{}
	}
	if c.held[ruleID+"|"+rowID] {
		return false
	}
	c.held[ruleID+"|"+rowID] = true
	return true
}

func (c *releasableClaims) release(_ context.Context, ruleID, rowID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.held, ruleID+"|"+rowID)
	c.released++
}

func (c *releasableClaims) isHeld(ruleID, rowID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held[ruleID+"|"+rowID]
}

// noPauses makes the firing's patience instant.
func noPauses(t *testing.T) {
	t.Helper()
	was := busyPauses
	busyPauses = []time.Duration{0, 0, 0}
	t.Cleanup(func() { busyPauses = was })
}

const (
	busyRule = "v1:campaigns:emailRule:ab12cd34"
	busyRow  = "v1:accounts:account:org-1"
)

func busyEvent() map[string]any {
	return map[string]any{
		"timestamp": "2026-10-01T01:00:00.000001Z",
		"payload":   map[string]any{"primaryContactEmail": "new@example.test"},
	}
}

// TestABusySendReleasesACreatedRulesClaimSoTheRowFiresLater: every attempt finds the
// lock busy. The firing says so in its refusals and sends nothing, and the row's
// claim is given back: the next firing for the row -- the event delivered again --
// is the first, not a duplicate, and sends.
func TestABusySendReleasesACreatedRulesClaimSoTheRowFiresLater(t *testing.T) {
	noPauses(t)
	rule := rowAddressRule()
	rule["eventKind"] = "created"
	e := &busySends{rowAddressEngine: rowAddressEngine{fireEngine: fireEngine{rule: rule}}, busyFor: 100}
	claims := &releasableClaims{}
	firer := NewFirer(e).WithFirstFireClaim(claims.claim).WithFirstFireRelease(claims.release)

	out, err := firer.Fire(context.Background(), busyRule, busyRow, busyEvent())
	if err != nil {
		t.Fatalf("a busy lock is not a failure of the rule (it would count toward the rule's breaker): %v", err)
	}
	if !out.Busy || out.Sent != 0 {
		t.Fatalf("the firing was not reported busy and unsent: %+v", out)
	}
	if len(out.Refusals) != 1 || !strings.Contains(out.Refusals[0], "busy") || !strings.Contains(out.Refusals[0], "nothing was sent") {
		t.Errorf("the refusal does not say that nothing was sent because the cluster was busy: %v", out.Refusals)
	}
	if claims.isHeld(busyRule, busyRow) {
		t.Fatal("the row's claim is still held: nothing could ever fire for this row again")
	}

	// The same row, delivered again, with the lock free.
	e.mu.Lock()
	e.busyFor = 0
	e.mu.Unlock()
	out, err = firer.Fire(context.Background(), busyRule, busyRow, busyEvent())
	if err != nil {
		t.Fatal(err)
	}
	if out.Duplicate || out.Sent != 1 || out.Busy {
		t.Errorf("the row could not fire once the lock was free: %+v", out)
	}
	if !claims.isHeld(busyRule, busyRow) {
		t.Error("the row that sent holds no claim: a third delivery would send again")
	}
}

// TestABusySendIsRetriedWithinTheFiringBeforeItGivesUp: the lock is usually busy for
// a moment. A few short pauses inside the firing keep the row from needing a second
// delivery at all.
func TestABusySendIsRetriedWithinTheFiringBeforeItGivesUp(t *testing.T) {
	noPauses(t)
	rule := rowAddressRule()
	rule["eventKind"] = "created"
	e := &busySends{rowAddressEngine: rowAddressEngine{fireEngine: fireEngine{rule: rule}}, busyFor: 2}
	claims := &releasableClaims{}
	firer := NewFirer(e).WithFirstFireClaim(claims.claim).WithFirstFireRelease(claims.release)

	out, err := firer.Fire(context.Background(), busyRule, busyRow, busyEvent())
	if err != nil {
		t.Fatal(err)
	}
	if out.Busy || out.Sent != 1 || len(out.Refusals) != 0 {
		t.Errorf("a lock that was busy twice and then free did not end in a send: %+v", out)
	}
	if got := e.sendCalls(); got != 3 {
		t.Errorf("%d send calls, want 3 (busy, busy, sent)", got)
	}
	if claims.released != 0 || !claims.isHeld(busyRule, busyRow) {
		t.Errorf("a firing that sent released its claim (%d releases)", claims.released)
	}
}

// TestAFailureThatIsNotBusyIsNotRetriedAndKeepsTheClaim: only a lock that was busy
// is "nothing attempted". A refusal on the merits is final for the firing, as it
// always was, and its claim stays.
func TestAFailureThatIsNotBusyIsNotRetriedAndKeepsTheClaim(t *testing.T) {
	noPauses(t)
	rule := rowAddressRule()
	rule["eventKind"] = "created"
	e := &busySends{rowAddressEngine: rowAddressEngine{fireEngine: fireEngine{rule: rule}}, other: errors.New("campaigns.sendToRecipient: publish the reviewed template before sending")}
	claims := &releasableClaims{}
	firer := NewFirer(e).WithFirstFireClaim(claims.claim).WithFirstFireRelease(claims.release)

	out, err := firer.Fire(context.Background(), busyRule, busyRow, busyEvent())
	if err != nil {
		t.Fatal(err)
	}
	if out.Busy || out.Sent != 0 || len(out.Refusals) != 1 {
		t.Fatalf("a refusal on the merits was reported as %+v", out)
	}
	if got := e.sendCalls(); got != 1 {
		t.Errorf("a refusal on the merits was tried %d times, want once", got)
	}
	if claims.released != 0 || !claims.isHeld(busyRule, busyRow) {
		t.Errorf("a firing that was refused on the merits gave its claim back (%d releases)", claims.released)
	}
}

// TestABusyAudienceFiringStopsAtTheFirstBusyRecipient: a rule that mails an audience
// does not spend the same busy lock on every recipient.
func TestABusyAudienceFiringStopsAtTheFirstBusyRecipient(t *testing.T) {
	noPauses(t)
	rule := activeRule("created")
	rule["recipientMode"] = ModeAudience
	rule["audienceId"] = "v1:campaigns:audience:aud-1"
	e := &audienceBusy{busySends: busySends{rowAddressEngine: rowAddressEngine{fireEngine: fireEngine{rule: rule}}, busyFor: 1000}}
	claims := &releasableClaims{}
	firer := NewFirer(e).WithFirstFireClaim(claims.claim).WithFirstFireRelease(claims.release)

	out, err := firer.Fire(context.Background(), busyRule, "v1:accounts:account:org-2", map[string]any{"timestamp": "2026-10-01T01:00:00.000001Z", "payload": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Busy {
		t.Fatalf("the firing was not reported busy: %+v", out)
	}
	if got, most := e.sendCalls(), len(busyPauses)+1; got > most {
		t.Errorf("%d send calls for an audience of three: the firing went on to the next recipient after a busy lock (at most %d for the first)", got, most)
	}
	if claims.isHeld(busyRule, "v1:accounts:account:org-2") {
		t.Error("a busy firing kept the row's claim")
	}
}

// audienceBusy answers the audience read the audience lane makes.
type audienceBusy struct{ busySends }

func (e *audienceBusy) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "query sendableRecipientsForAudience") {
		var rows []map[string]any
		for i := 0; i < 3; i++ {
			rows = append(rows, map[string]any{"id": fmt.Sprintf("v1:campaigns:recipient:r%d", i), "email": fmt.Sprintf("r%d@example.test", i), "subscriptionStatus": "subscribed"})
		}
		return rowsEnvelope(rows), nil
	}
	return e.busySends.Execute(ctx, q)
}

// TestAnUpdatedRuleThatFindsTheLockBusySaysSoAndHasNoClaimToGiveBack: an updated rule
// fires on every change and claims nothing; the refusal tells the operator to make
// the change again.
func TestAnUpdatedRuleThatFindsTheLockBusySaysSoAndHasNoClaimToGiveBack(t *testing.T) {
	noPauses(t)
	e := &busySends{rowAddressEngine: rowAddressEngine{fireEngine: fireEngine{rule: rowAddressRule()}}, busyFor: 100}
	claims := &releasableClaims{}
	firer := NewFirer(e).WithFirstFireClaim(claims.claim).WithFirstFireRelease(claims.release)

	out, err := firer.Fire(context.Background(), busyRule, busyRow, busyEvent())
	if err != nil {
		t.Fatal(err)
	}
	if !out.Busy || out.Sent != 0 || len(out.Refusals) != 1 {
		t.Fatalf("an updated rule's busy firing was reported as %+v", out)
	}
	if claims.released != 0 {
		t.Errorf("an updated rule has no claim, and %d were released", claims.released)
	}
}
