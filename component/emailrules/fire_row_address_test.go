package emailrules

// fire_row_address_test.go -- a row-address firing enrols the address into the
// rule's audience before it mails it, and the enrolment names the rule's
// organization (memql#6022).
//
// A recipient is an organization-scoped row, and the send refuses one whose
// organization is not the campaign's. addRecipient stamps "" on a recipient that
// arrives without an accountId, so an address enrolled with none was a recipient
// the very send that followed it would refuse.

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// rowAddressEngine is fireEngine plus the one answer a row-address firing asks
// for that the operational lane never does: the receipt for the send. Without
// it the send is refused for want of a readable receipt and the firing ends in
// a refusal instead of a send.
type rowAddressEngine struct {
	fireEngine

	// member, when set, is the audience's answer to the lookup of the address by
	// digest: the membership that is already there. Unset, nobody has the digest.
	member map[string]any
	// lookupFresh records, for each lookup by digest, whether it was made under a
	// fresh-read context -- the lookup decides whether to write, so it must be.
	lookupFresh []bool
}

func (e *rowAddressEngine) Execute(ctx context.Context, q string) (any, error) {
	out, err := e.fireEngine.Execute(ctx, q) // records the call and answers the shared reads
	switch {
	case strings.HasPrefix(q, "query recipientInAudienceByDigest"):
		e.mu.Lock()
		e.lookupFresh = append(e.lookupFresh, memql.FreshReadFromContext(ctx))
		member := e.member
		e.mu.Unlock()
		if member != nil {
			return rowsEnvelope([]map[string]any{member}), nil
		}
	case strings.HasPrefix(q, "builtin campaignSendToRecipient"):
		return rowsEnvelope([]map[string]any{{"sent": true}}), nil
	}
	return out, err
}

// callIndex returns the position of the first recorded call that starts with
// prefix, or -1.
func (e *rowAddressEngine) callIndex(prefix string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, c := range e.calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// callsWithPrefix returns every recorded call that starts with prefix, in order.
func (e *rowAddressEngine) callsWithPrefix(prefix string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, c := range e.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// rowAddressRule is an organization's rule: when the organization changes, the
// address in its primaryContactEmail joins the organization's audience and is
// mailed. Its accountId is stored in canonical form, as the rule row carries it.
func rowAddressRule() map[string]any {
	row := activeRule("updated")
	row["triggerConcept"] = "v1:accounts:account"
	row["recipientMode"] = ModeRowAddress
	row["recipientField"] = "primaryContactEmail"
	row["audienceId"] = "v1:campaigns:audience:aud-1"
	row["accountId"] = "v1:accounts:account:org-1"
	return row
}

// fireForNewContact fires rule for an update that brought a contact address the
// audience has never seen, and checks the firing enrolled it exactly once and
// went on to send. It returns the enrolment's rendered call.
func fireForNewContact(t *testing.T, rule map[string]any) string {
	t.Helper()
	e := &rowAddressEngine{fireEngine: fireEngine{rule: rule}}
	event := map[string]any{
		"timestamp": "2026-10-01T01:00:00.000001Z",
		"payload":   map[string]any{"primaryContactEmail": "new@example.test"},
	}

	out, err := NewFirer(e).Fire(context.Background(), "v1:campaigns:emailRule:ab12cd34", "v1:accounts:account:org-1", event)
	if err != nil {
		t.Fatal(err)
	}
	if out.Sent != 1 || len(out.Refusals) != 0 {
		t.Fatalf("the firing did not enrol and send: %+v", out)
	}
	enrolments := e.callsWithPrefix("mutation addRecipient(")
	if len(enrolments) != 1 {
		t.Fatalf("want one enrolment for the new address, got %d: %v", len(enrolments), enrolments)
	}
	return enrolments[0]
}

// TestRowAddressEnrollmentCarriesTheRuleOrganization: the new contact address is
// not yet in the audience, so the firing enrols it -- and the enrolment carries
// the rule's organization as a BARE id, the form every mutation argument takes.
func TestRowAddressEnrollmentCarriesTheRuleOrganization(t *testing.T) {
	enrolment := fireForNewContact(t, rowAddressRule())

	if !strings.Contains(enrolment, `accountId: "org-1"`) {
		t.Errorf("the enrolment must name the rule's organization as a bare id (accountId: \"org-1\"); "+
			"without it the recipient is stamped with none and the send that follows refuses it:\n%s", enrolment)
	}
}

// TestRowAddressEnrollmentOmitsAnOrganizationTheRuleDoesNotName: a rule that
// predates organizations has none to copy. The argument is left off rather than
// sent empty -- the optional-field rule the rest of the tree follows -- and
// addRecipient stamps "" for the absent one, which is what it always did.
func TestRowAddressEnrollmentOmitsAnOrganizationTheRuleDoesNotName(t *testing.T) {
	rule := rowAddressRule()
	delete(rule, "accountId")

	enrolment := fireForNewContact(t, rule)

	if strings.Contains(enrolment, "accountId") {
		t.Errorf("a rule with no organization still sent an accountId argument:\n%s", enrolment)
	}
}

// fireForContact fires rowAddressRule for an update whose contact address is
// address, against an audience that answers the lookup by digest with member.
func fireForContact(t *testing.T, address string, member map[string]any) (*rowAddressEngine, FireOutcome) {
	t.Helper()
	e := &rowAddressEngine{fireEngine: fireEngine{rule: rowAddressRule()}, member: member}
	event := map[string]any{
		"timestamp": "2026-10-01T01:00:00.000001Z",
		"payload":   map[string]any{"primaryContactEmail": address},
	}
	out, err := NewFirer(e).Fire(context.Background(), "v1:campaigns:emailRule:ab12cd34", "v1:accounts:account:org-1", event)
	if err != nil {
		t.Fatal(err)
	}
	return e, out
}

// TestRowAddressFindsTheRecipientByDigest: the address on the triggering row is
// already a member of the rule's audience, so the firing finds it with ONE keyed
// read by the digest of its normalized spelling -- no roster read, no second
// enrolment -- and the send targets that member.
func TestRowAddressFindsTheRecipientByDigest(t *testing.T) {
	e, out := fireForContact(t, " New@Example.TEST ", map[string]any{
		"id": "v1:campaigns:recipient:rcp-existing", "subscriptionStatus": "subscribed",
	})

	if out.Sent != 1 || len(out.Refusals) != 0 {
		t.Fatalf("the firing did not send to the existing member: %+v", out)
	}
	if enrolments := e.callsWithPrefix("mutation addRecipient("); len(enrolments) != 0 {
		t.Errorf("an address that is already a member was enrolled again: %v", enrolments)
	}
	if scans := e.callsWithPrefix("query sendableRecipientsForAudience"); len(scans) != 0 {
		t.Errorf("the firing read the audience's roster to find one address: %v", scans)
	}
	want := `query recipientInAudienceByDigest(audienceId: "aud-1", emailDigest: "` + addressDigest("new@example.test") + `")`
	if lookups := e.callsWithPrefix("query recipientInAudienceByDigest("); len(lookups) != 1 || lookups[0] != want {
		t.Errorf("the lookups were %q, want exactly %q", lookups, want)
	}
	sends := e.callsWithPrefix("builtin campaignSendToRecipient(")
	if len(sends) != 1 || !strings.Contains(sends[0], `recipientId: "rcp-existing"`) {
		t.Errorf("the send did not target the member the lookup found: %v", sends)
	}
	if len(e.lookupFresh) != 1 || !e.lookupFresh[0] {
		t.Errorf("the lookup was not a fresh read (%v): a replica's cached miss would enrol a second membership for the same mailbox", e.lookupFresh)
	}
}

// TestRowAddressLooksTheAddressUpBeforeEnrollingIt: nobody has the digest, so the
// firing asks first and enrols second, and the enrolment is the only write.
func TestRowAddressLooksTheAddressUpBeforeEnrollingIt(t *testing.T) {
	e, out := fireForContact(t, "new@example.test", nil)

	if out.Sent != 1 || len(out.Refusals) != 0 {
		t.Fatalf("the firing did not enrol and send: %+v", out)
	}
	lookup, enrol := e.callIndex("query recipientInAudienceByDigest("), e.callIndex("mutation addRecipient(")
	if lookup < 0 || enrol < 0 || lookup > enrol {
		t.Errorf("the lookup was call %d and the enrolment call %d: the address must be looked up before it is enrolled", lookup, enrol)
	}
	if scans := e.callsWithPrefix("query sendableRecipientsForAudience"); len(scans) != 0 {
		t.Errorf("the firing read the audience's roster: %v", scans)
	}
}

// TestRowAddressDoesNotEnrolAnUnsubscribedMemberAgain: the lookup answers for a
// member in ANY state. The roster this replaces listed only the subscribed, so
// someone who had unsubscribed read as "not in the audience" and was enrolled
// again as subscribed. Found, they are left as they are; the send path decides
// what a send to them is.
func TestRowAddressDoesNotEnrolAnUnsubscribedMemberAgain(t *testing.T) {
	e, _ := fireForContact(t, "gone@example.test", map[string]any{
		"id": "v1:campaigns:recipient:rcp-gone", "subscriptionStatus": "unsubscribed",
	})

	if enrolments := e.callsWithPrefix("mutation addRecipient("); len(enrolments) != 0 {
		t.Errorf("a member who unsubscribed was enrolled again as subscribed: %v", enrolments)
	}
	sends := e.callsWithPrefix("builtin campaignSendToRecipient(")
	if len(sends) != 1 || !strings.Contains(sends[0], `recipientId: "rcp-gone"`) {
		t.Errorf("the send is the path that refuses an unsubscribed member, and it was not asked about them: %v", sends)
	}
}

// TestRowAddressRefusesAnAddressWithNoUsableDigest: an address that cannot be
// digested cannot be looked up, deduplicated or suppressed, so it is refused
// with the same sentence as one with no "@" -- before any read or write.
func TestRowAddressRefusesAnAddressWithNoUsableDigest(t *testing.T) {
	for _, address := range []string{"no-at-sign", "a b@c.test", "@leading", "trailing@"} {
		e, out := fireForContact(t, address, nil)

		if len(out.Refusals) != 1 || out.Refusals[0] != `"primaryContactEmail" is not an address` {
			t.Errorf("%q: refusals = %q", address, out.Refusals)
		}
		for _, prefix := range []string{"query recipientInAudienceByDigest(", "mutation addRecipient(", "builtin campaignSendToRecipient("} {
			if calls := e.callsWithPrefix(prefix); len(calls) != 0 {
				t.Errorf("%q: an unusable address still reached %s: %v", address, prefix, calls)
			}
		}
		if out.Recipients != 0 || out.Sent != 0 {
			t.Errorf("%q: %+v", address, out)
		}
	}
}
