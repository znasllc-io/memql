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
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// rowAddressEngine is fireEngine plus the answers a row-address firing asks for
// that the operational lane never does: what the rule's audience holds for the
// mailbox, and the receipt for the send. Without the receipt the send is
// refused for want of a readable receipt and the firing ends in a refusal
// instead of a send.
type rowAddressEngine struct {
	fireEngine

	// byDigest is the audience's answer to the lookup of the address by digest,
	// oldest first: the memberships that carry it. Unset, nobody has the digest.
	byDigest []map[string]any
	// withoutDigest is its answer to the look at the rows that carry no digest
	// at all -- one page of them, as the engine returns it.
	withoutDigest []map[string]any
	// lookupFresh and withoutDigestFresh record, for each read, whether it was
	// made under a fresh-read context: both decide whether to write, so both must be.
	lookupFresh        []bool
	withoutDigestFresh []bool
	// failMailboxReads makes the lookups fail, as a database that is not answering does.
	failMailboxReads bool
}

func (e *rowAddressEngine) Execute(ctx context.Context, q string) (any, error) {
	out, err := e.fireEngine.Execute(ctx, q) // records the call and answers the shared reads
	switch {
	case strings.HasPrefix(q, "query recipientInAudienceByDigest"):
		e.mu.Lock()
		e.lookupFresh = append(e.lookupFresh, memql.FreshReadFromContext(ctx))
		rows, failing := e.byDigest, e.failMailboxReads
		e.mu.Unlock()
		if failing {
			return nil, errors.New("the database is not answering")
		}
		return rowsEnvelope(rows), nil
	case strings.HasPrefix(q, "query recipientsWithoutDigestInAudience"):
		e.mu.Lock()
		e.withoutDigestFresh = append(e.withoutDigestFresh, memql.FreshReadFromContext(ctx))
		rows := e.withoutDigest
		e.mu.Unlock()
		return rowsEnvelope(rows), nil
	case strings.HasPrefix(q, "builtin campaignSendToRecipient"):
		return rowsEnvelope([]map[string]any{{"sent": true}}), nil
	}
	return out, err
}

// memberRow is one recipient row as the reads return it.
func memberRow(short, email, status string) map[string]any {
	return map[string]any{"id": "v1:campaigns:recipient:" + short, "email": email, "subscriptionStatus": status}
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

// audienceAnswers is what the rule's audience holds for the mailbox.
type audienceAnswers struct {
	byDigest, withoutDigest []map[string]any
}

// fireForContact fires rowAddressRule for an update whose contact address is
// address, against an audience that answers as given.
func fireForContact(t *testing.T, address string, audience audienceAnswers) (*rowAddressEngine, FireOutcome) {
	t.Helper()
	e := &rowAddressEngine{fireEngine: fireEngine{rule: rowAddressRule()}, byDigest: audience.byDigest, withoutDigest: audience.withoutDigest}
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

// sendsTo returns the recipient ids the firing sent to, bare.
func (e *rowAddressEngine) sendsTo() []string {
	var ids []string
	for _, send := range e.callsWithPrefix("builtin campaignSendToRecipient(") {
		start := strings.Index(send, `recipientId: "`) + len(`recipientId: "`)
		ids = append(ids, send[start:start+strings.Index(send[start:], `"`)])
	}
	return ids
}

// TestRowAddressFindsTheRecipientByDigest: the address on the triggering row is
// already a member of the rule's audience, so the firing finds it by the digest
// of its normalized spelling -- no roster read, no second enrolment -- and the
// send targets that member.
func TestRowAddressFindsTheRecipientByDigest(t *testing.T) {
	e, out := fireForContact(t, " New@Example.TEST ", audienceAnswers{
		byDigest: []map[string]any{memberRow("rcp-existing", "NEW@example.test", "subscribed")},
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
	if ids := e.sendsTo(); len(ids) != 1 || ids[0] != "rcp-existing" {
		t.Errorf("the send did not target the member the lookup found: %v", ids)
	}
	if len(e.lookupFresh) != 1 || !e.lookupFresh[0] {
		t.Errorf("the lookup was not a fresh read (%v): a replica's cached miss would enrol a second membership for the same mailbox", e.lookupFresh)
	}
	if len(e.withoutDigestFresh) != 1 || !e.withoutDigestFresh[0] {
		t.Errorf("the look at the rows with no digest was %v (true = a fresh read), want one fresh read", e.withoutDigestFresh)
	}
}

// TestRowAddressLooksTheAddressUpBeforeEnrollingIt: nobody has the digest, so the
// firing asks first and enrols second, and the enrolment is the only write.
func TestRowAddressLooksTheAddressUpBeforeEnrollingIt(t *testing.T) {
	e, out := fireForContact(t, "new@example.test", audienceAnswers{})

	if out.Sent != 1 || len(out.Refusals) != 0 {
		t.Fatalf("the firing did not enrol and send: %+v", out)
	}
	lookup, digestLess, enrol := e.callIndex("query recipientInAudienceByDigest("), e.callIndex("query recipientsWithoutDigestInAudience("), e.callIndex("mutation addRecipient(")
	if lookup < 0 || digestLess < 0 || enrol < 0 || lookup > enrol || digestLess > enrol {
		t.Errorf("the lookup was call %d, the look at rows with no digest call %d and the enrolment call %d: the address must be looked for before it is enrolled", lookup, digestLess, enrol)
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
	e, _ := fireForContact(t, "gone@example.test", audienceAnswers{
		byDigest: []map[string]any{memberRow("rcp-gone", "gone@example.test", "unsubscribed")},
	})

	if enrolments := e.callsWithPrefix("mutation addRecipient("); len(enrolments) != 0 {
		t.Errorf("a member who unsubscribed was enrolled again as subscribed: %v", enrolments)
	}
	if ids := e.sendsTo(); len(ids) != 1 || ids[0] != "rcp-gone" {
		t.Errorf("the send is the path that refuses an unsubscribed member, and it was not asked about them: %v", ids)
	}
}

// TestRowAddressDoesNothingForAMailboxWithAnyRowThatOptedOut: a mailbox can hold
// more than one row, and one of them can have opted out by hand without the
// other hearing of it. A firing never enrols the mailbox again and never mails
// the sibling that is still subscribed: the send goes to the row that is not,
// where the send path refuses it and writes its ledger row.
func TestRowAddressDoesNothingForAMailboxWithAnyRowThatOptedOut(t *testing.T) {
	for _, tc := range []struct {
		name     string
		audience audienceAnswers
		want     string
	}{
		{"the opted-out row is listed second", audienceAnswers{byDigest: []map[string]any{
			memberRow("sibling", "gone@example.test", "subscribed"), memberRow("gone", " Gone@Example.TEST", "unsubscribed")}}, "gone"},
		{"the opted-out row is listed first", audienceAnswers{byDigest: []map[string]any{
			memberRow("gone", "gone@example.test", "bounced"), memberRow("sibling", "gone@example.test", "subscribed")}}, "gone"},
		{"the opted-out row has no digest", audienceAnswers{
			byDigest:      []map[string]any{memberRow("sibling", "gone@example.test", "subscribed")},
			withoutDigest: []map[string]any{memberRow("gone", "GONE@example.test", "complained")}}, "gone"},
		{"the only row has no digest", audienceAnswers{
			withoutDigest: []map[string]any{memberRow("other", "other@example.test", "subscribed"), memberRow("gone", "gone@example.test", "unsubscribed")}}, "gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := fireForContact(t, "gone@example.test", tc.audience)

			if enrolments := e.callsWithPrefix("mutation addRecipient("); len(enrolments) != 0 {
				t.Errorf("a mailbox that opted out was enrolled again: %v", enrolments)
			}
			if ids := e.sendsTo(); len(ids) != 1 || ids[0] != tc.want {
				t.Errorf("the send went to %v, want only the row that is not subscribed (%s): a subscribed sibling must not be mailed", ids, tc.want)
			}
		})
	}
}

// TestRowAddressAdoptsAMemberThatHasNoDigest: a subscribed row with no digest --
// written before the field existed, or by a replica on the previous release -- is
// the member. A second subscribed row for the mailbox would put the person on
// every campaign twice.
func TestRowAddressAdoptsAMemberThatHasNoDigest(t *testing.T) {
	e, out := fireForContact(t, "old@example.test", audienceAnswers{
		withoutDigest: []map[string]any{memberRow("someone", "someone@example.test", "subscribed"), memberRow("old", " Old@Example.test ", "subscribed")},
	})

	if out.Sent != 1 {
		t.Fatalf("the firing did not send to the member: %+v", out)
	}
	if enrolments := e.callsWithPrefix("mutation addRecipient("); len(enrolments) != 0 {
		t.Errorf("a member with no digest was enrolled again: %v", enrolments)
	}
	if ids := e.sendsTo(); len(ids) != 1 || ids[0] != "old" {
		t.Errorf("the send went to %v, want the member that was there", ids)
	}
}

// TestRowAddressChecksTheAddressBeforeBelievingTheDigest: a row can carry a digest
// that is not the digest of its own address. The firing checks the address, and a
// row for someone else is a miss: it neither blocks the firing nor stands in for
// the member.
func TestRowAddressChecksTheAddressBeforeBelievingTheDigest(t *testing.T) {
	for _, status := range []string{"unsubscribed", "subscribed"} {
		e, out := fireForContact(t, "new@example.test", audienceAnswers{
			byDigest: []map[string]any{memberRow("forged", "someone-else@example.test", status)},
		})

		if out.Sent != 1 || len(out.Refusals) != 0 {
			t.Fatalf("%s: the firing did not enrol and send: %+v", status, out)
		}
		if enrolments := e.callsWithPrefix("mutation addRecipient("); len(enrolments) != 1 {
			t.Errorf("%s: the address was not enrolled although the only row that carried its digest is another address's: %v", status, enrolments)
		}
		if ids := e.sendsTo(); len(ids) != 1 || ids[0] == "forged" {
			t.Errorf("%s: the send went to %v, the row for another address", status, ids)
		}
	}
}

// TestRowAddressRefusesWhenTheRowsWithNoDigestCannotRuleTheMailboxOut: the look at
// rows with no digest is one page. A full page that does not name the mailbox
// proves nothing about the rows beyond it, and a rule that cannot tell whether the
// mailbox opted out does not guess: nothing is enrolled or mailed, and the
// refusal says why.
func TestRowAddressRefusesWhenTheRowsWithNoDigestCannotRuleTheMailboxOut(t *testing.T) {
	page := func(n int) []map[string]any {
		var rows []map[string]any
		for i := 0; i < n; i++ {
			rows = append(rows, memberRow("filler"+string(rune('a'+i%26))+string(rune('a'+i/26)), "filler@example.test", "subscribed"))
		}
		return rows
	}

	e, out := fireForContact(t, "new@example.test", audienceAnswers{withoutDigest: page(digestLessWindow)})
	if len(out.Refusals) != 1 || !strings.Contains(out.Refusals[0], "older recipients") || !strings.Contains(out.Refusals[0], "Nothing was sent") {
		t.Errorf("refusals = %q, want one that says older recipients are still being indexed and that nothing was sent", out.Refusals)
	}
	for _, prefix := range []string{"mutation addRecipient(", "builtin campaignSendToRecipient("} {
		if calls := e.callsWithPrefix(prefix); len(calls) != 0 {
			t.Errorf("a page that could not rule the mailbox out still reached %s: %v", prefix, calls)
		}
	}

	// One short of a page is the whole set: the mailbox is not in it.
	e, out = fireForContact(t, "new@example.test", audienceAnswers{withoutDigest: page(digestLessWindow - 1)})
	if out.Sent != 1 || len(out.Refusals) != 0 || len(e.callsWithPrefix("mutation addRecipient(")) != 1 {
		t.Errorf("a page that was not full ruled the mailbox out, so the firing should have enrolled and sent: %+v", out)
	}
}

// TestRowAddressRefusesAnAddressWithNoUsableDigest: an address that cannot be
// digested cannot be looked up, deduplicated or suppressed, so it is refused
// with the same sentence as one with no "@" -- before any read or write.
func TestRowAddressRefusesAnAddressWithNoUsableDigest(t *testing.T) {
	for _, address := range []string{"no-at-sign", "a b@c.test", "@leading", "trailing@"} {
		e, out := fireForContact(t, address, audienceAnswers{})

		if len(out.Refusals) != 1 || out.Refusals[0] != `"primaryContactEmail" is not an address` {
			t.Errorf("%q: refusals = %q", address, out.Refusals)
		}
		for _, prefix := range []string{"query recipientInAudienceByDigest(", "query recipientsWithoutDigestInAudience(", "mutation addRecipient(", "builtin campaignSendToRecipient("} {
			if calls := e.callsWithPrefix(prefix); len(calls) != 0 {
				t.Errorf("%q: an unusable address still reached %s: %v", address, prefix, calls)
			}
		}
		if out.Recipients != 0 || out.Sent != 0 {
			t.Errorf("%q: %+v", address, out)
		}
	}
}

// TestARefusedRowAddressFiringKeepsTheCreatedRulesClaim: a created rule claims its
// row, once, before it sends, and the loser of that claim sends nothing. The
// refusal that says "could not tell yet" is not a send, so it must not use the
// claim up: the row's one creation would be gone with its mail, and the change it
// asks for -- try again later -- could never fire.
func TestARefusedRowAddressFiringKeepsTheCreatedRulesClaim(t *testing.T) {
	rule := rowAddressRule()
	rule["eventKind"] = "created"
	page := func(n int) []map[string]any {
		var rows []map[string]any
		for i := 0; i < n; i++ {
			rows = append(rows, memberRow(fmt.Sprintf("stranger%02d", i), fmt.Sprintf("stranger%02d@example.test", i), "subscribed"))
		}
		return rows
	}
	e := &rowAddressEngine{fireEngine: fireEngine{rule: rule}, withoutDigest: page(digestLessWindow)}
	firer := NewFirer(e).WithFirstFireClaim(keyClaim())
	event := map[string]any{
		"timestamp": "2026-10-01T01:00:00.000001Z",
		"payload":   map[string]any{"primaryContactEmail": "new@example.test"},
	}

	out, err := firer.Fire(context.Background(), "v1:campaigns:emailRule:ab12cd34", "v1:accounts:account:org-1", event)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Refusals) != 1 || !strings.Contains(out.Refusals[0], "older recipients") || out.Sent != 0 {
		t.Fatalf("the firing was not refused for want of an answer: %+v", out)
	}

	// The sweep has been through: the page is short, and the same row fires.
	e.withoutDigest = page(digestLessWindow - 1)
	out, err = firer.Fire(context.Background(), "v1:campaigns:emailRule:ab12cd34", "v1:accounts:account:org-1", event)
	if err != nil {
		t.Fatal(err)
	}
	if out.Duplicate || out.Sent != 1 || len(out.Refusals) != 0 {
		t.Errorf("the row could not fire once it could be decided: %+v (a refused firing used up the claim)", out)
	}
}

// TestARowAddressRuleThatCannotReadTheMailboxKeepsTheClaimToo: the same for a read
// that fails -- the firing returns the error, the automation may retry it, and a
// retry that found the claim already taken would lose the row's mail for good.
func TestARowAddressRuleThatCannotReadTheMailboxKeepsTheClaimToo(t *testing.T) {
	rule := rowAddressRule()
	rule["eventKind"] = "created"
	e := &rowAddressEngine{fireEngine: fireEngine{rule: rule}}
	claim := keyClaim()
	firer := NewFirer(e).WithFirstFireClaim(claim)
	event := map[string]any{
		"timestamp": "2026-10-01T01:00:00.000001Z",
		"payload":   map[string]any{"primaryContactEmail": "new@example.test"},
	}

	e.failMailboxReads = true
	if _, err := firer.Fire(context.Background(), "v1:campaigns:emailRule:ab12cd34", "v1:accounts:account:org-1", event); err == nil {
		t.Fatal("a firing that could not read the mailbox reported no error")
	}
	e.failMailboxReads = false
	out, err := firer.Fire(context.Background(), "v1:campaigns:emailRule:ab12cd34", "v1:accounts:account:org-1", event)
	if err != nil {
		t.Fatal(err)
	}
	if out.Duplicate || out.Sent != 1 {
		t.Errorf("the retry found the claim taken: %+v", out)
	}
}
