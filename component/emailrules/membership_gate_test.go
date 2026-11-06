package emailrules

import (
	"context"
	"strings"
	"sync"
	"testing"
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
