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
)

// rowAddressEngine is fireEngine plus the one answer a row-address firing asks
// for that the operational lane never does: the receipt for the send. Without
// it the send is refused for want of a readable receipt and the firing ends in
// a refusal instead of a send.
type rowAddressEngine struct {
	fireEngine
}

func (e *rowAddressEngine) Execute(ctx context.Context, q string) (any, error) {
	out, err := e.fireEngine.Execute(ctx, q) // records the call and answers the shared reads
	if strings.HasPrefix(q, "builtin campaignSendToRecipient") {
		return rowsEnvelope([]map[string]any{{"sent": true}}), nil
	}
	return out, err
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
