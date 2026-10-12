package campaigns

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// recipient_consent_db_test.go -- a public form never reopens a mailbox that
// opted out (memql#6022, F9 fix round 1), against a REAL engine and a REAL
// Postgres.
//
// The signup used to refuse when ANY row for the mailbox was not subscribed.
// Finding the member by digest looked at one row, and could not see a row that
// has no digest at all -- every recipient written before the field existed, by a
// replica still running the previous release, or by a raw write. Each case
// below is a state a mailbox can really be in; the form must do nothing for the
// ones that opted out and must not add a second row for the ones that did not.

const consentMailbox = "subscriber@example.test"

// addStamped adds a recipient through addRecipient, which stamps its digest, and
// then sets its state by hand when it is not subscribed -- what the app's
// "honour an unsubscribe by hand" does: it flips the row and nothing else, so
// there is no suppression entry for the form to find.
func (n *digestNewsletter) addStamped(t *testing.T, short, email, status string) string {
	t.Helper()
	id := n.suffix + "-" + short
	if err := n.store.AddRecipient(n.owner, id, n.audience, email, "", "manual", n.org, nil); err != nil {
		t.Fatal(err)
	}
	if status != "" && status != "subscribed" {
		if err := n.store.SetRecipientSubscription(n.owner, id, status, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// plantRaw writes a recipient with a raw insert as its owner: no addRecipient,
// so no stamp. That is the row a previous release wrote, or a raw write leaves.
// digest, when set, is written as given (a forged one, for the cases that need it).
func (n *digestNewsletter) plantRaw(t *testing.T, short, email, status, digest string) string {
	t.Helper()
	id := n.suffix + "-" + short
	payload := fmt.Sprintf(`{"ownerUserId":%s,"accountId":%s,"audienceId":%s,"email":%s,"source":"manual"`,
		langparser.QuoteString(n.user), langparser.QuoteString(n.org), langparser.QuoteString(n.audience), langparser.QuoteString(email))
	if status != "" {
		payload += `,"subscriptionStatus":` + langparser.QuoteString(status)
	}
	if digest != "" {
		payload += `,"emailDigest":` + langparser.QuoteString(digest)
	}
	payload += "}"
	n.execute(t, n.owner, fmt.Sprintf(`insert("v1:campaigns:recipient", id=%s, payload=%s)`, langparser.QuoteString(id), payload))
	return id
}

// signup posts the form for the mailbox.
func (n *digestNewsletter) signup(t *testing.T) {
	t.Helper()
	n.args["email"] = consentMailbox
	if _, err := n.worker.handleSubscribe(n.owner, n.args, 0); err != nil {
		t.Fatalf("the form was refused with an error rather than quietly accepted: %v", err)
	}
}

func (n *digestNewsletter) signupRow(t *testing.T) map[string]any {
	t.Helper()
	row, err := n.worker.newsletterRow(n.owner, "newsletterSignupById", "signupId", newsletterSignupID(n.audience, consentMailbox))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func (n *digestNewsletter) consentRow(t *testing.T) map[string]any {
	t.Helper()
	row, err := n.worker.newsletterRow(n.owner, "newsletterConsentById", "eventId", sha256Hex("newsletter-consent\x00"+newsletterSignupID(n.audience, consentMailbox)))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// assertTheFormDidNothing: no signup (so no welcome can ever follow), no consent
// grant, and no row added to the audience. rosterBefore is its size before the form.
func (n *digestNewsletter) assertTheFormDidNothing(t *testing.T, rosterBefore int) {
	t.Helper()
	if row := n.signupRow(t); row != nil {
		t.Errorf("the form recorded a signup for a mailbox that opted out: %v", row)
	}
	if row := n.consentRow(t); row != nil {
		t.Errorf("the form recorded consent for a mailbox that opted out: %v", row)
	}
	if got := len(n.roster(t)); got != rosterBefore {
		t.Errorf("the audience holds %d rows after the form, was %d: the form added a row for a mailbox that opted out", got, rosterBefore)
	}
}

// TestAHandOptedOutDuplicateRefusesTheFormWhicheverRowComesFirst: a mailbox can
// hold more than one row (a manual add beside an earlier row, a race), and the
// engine lists them by the time of their last write. The form refuses when ANY
// of them is not subscribed, whichever the list puts first.
func TestAHandOptedOutDuplicateRefusesTheFormWhicheverRowComesFirst(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows func(n *digestNewsletter, t *testing.T)
	}{
		{"the opted-out row was written after its sibling", func(n *digestNewsletter, t *testing.T) {
			n.addStamped(t, "a", " Subscriber@Example.TEST ", "subscribed")
			n.addStamped(t, "b", "SUBSCRIBER@example.test", "unsubscribed")
		}},
		{"the opted-out row was written before its sibling", func(n *digestNewsletter, t *testing.T) {
			n.addStamped(t, "a", " Subscriber@Example.TEST ", "unsubscribed")
			n.addStamped(t, "b", "SUBSCRIBER@example.test", "subscribed")
		}},
		{"the original row was opted out by hand after its sibling was added", func(n *digestNewsletter, t *testing.T) {
			a := n.addStamped(t, "a", " Subscriber@Example.TEST ", "subscribed")
			n.addStamped(t, "b", "SUBSCRIBER@example.test", "subscribed")
			if err := n.store.SetRecipientSubscription(n.owner, a, "unsubscribed", time.Now()); err != nil {
				t.Fatal(err)
			}
		}},
		{"the sibling bounced", func(n *digestNewsletter, t *testing.T) {
			n.addStamped(t, "a", " Subscriber@Example.TEST ", "subscribed")
			n.addStamped(t, "b", "SUBSCRIBER@example.test", "bounced")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newEmptyNewsletter(t, "consent-dup")
			tc.rows(n, t)
			before := len(n.roster(t))
			if before != 2 {
				t.Fatalf("setup: the audience holds %d rows, want 2", before)
			}
			n.signup(t)
			n.assertTheFormDidNothing(t, before)
		})
	}
}

// TestADigestLessOptOutRefusesTheForm: a row with no digest is invisible to a
// lookup by digest, and an opt-out on it must still stop the form. The old scan
// found it by comparing addresses.
func TestADigestLessOptOutRefusesTheForm(t *testing.T) {
	for _, status := range []string{"unsubscribed", "bounced", "complained"} {
		t.Run(status, func(t *testing.T) {
			n := newEmptyNewsletter(t, "consent-bare")
			n.plantRaw(t, "old", " Subscriber@Example.TEST ", status, "")
			n.signup(t)
			n.assertTheFormDidNothing(t, 1)
		})
	}
}

// TestADigestLessSubscriberIsAdoptedNotDuplicated: a subscribed row with no
// digest is the member. The form uses it; a second subscribed row for the same
// mailbox would put the person on every campaign twice.
func TestADigestLessSubscriberIsAdoptedNotDuplicated(t *testing.T) {
	n := newEmptyNewsletter(t, "consent-adopt")
	old := n.plantRaw(t, "old", " Subscriber@Example.TEST ", "subscribed", "")
	n.signup(t)

	signup := n.signupRow(t)
	if signup == nil {
		t.Fatal("the form recorded no signup for a subscribed member")
	}
	if got := bare(str(signup, "recipientId")); got != old {
		t.Errorf("the signup names recipient %q, want the member that was there, %q", got, old)
	}
	if got := len(n.roster(t)); got != 1 {
		t.Errorf("the audience holds %d rows after the form, want the 1 that was there", got)
	}
	if n.consentRow(t) == nil {
		t.Error("no consent was recorded for the signup")
	}
}

// TestADigestLessOptOutBesideAStampedSubscriberStillRefuses: the lookup by digest
// finds the stamped row, which is subscribed, and the opt-out is on a sibling that
// has no digest. The mailbox opted out; the form must not welcome it.
func TestADigestLessOptOutBesideAStampedSubscriberStillRefuses(t *testing.T) {
	for _, stampedFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("stamped row written first=%v", stampedFirst), func(t *testing.T) {
			n := newEmptyNewsletter(t, "consent-mixed")
			if stampedFirst {
				n.addStamped(t, "new", " Subscriber@Example.TEST ", "subscribed")
				n.plantRaw(t, "old", "SUBSCRIBER@example.test", "unsubscribed", "")
			} else {
				n.plantRaw(t, "old", "SUBSCRIBER@example.test", "unsubscribed", "")
				n.addStamped(t, "new", " Subscriber@Example.TEST ", "subscribed")
			}
			n.signup(t)
			n.assertTheFormDidNothing(t, 2)
		})
	}
}

// TestAForgedDigestIsAMissNotAMember: a digest is a key, and a row can carry a
// digest that is not the digest of its own address (a raw write). The form checks
// the address before it believes the key: a row for someone else is not this
// mailbox, whatever it says, and neither blocks nor stands in for the signup.
func TestAForgedDigestIsAMissNotAMember(t *testing.T) {
	for _, status := range []string{"unsubscribed", "subscribed"} {
		t.Run(status, func(t *testing.T) {
			n := newEmptyNewsletter(t, "consent-forged")
			forged := n.plantRaw(t, "forged", "someone-else@example.test", status, EmailDigest(consentMailbox))
			n.signup(t)

			signup := n.signupRow(t)
			if signup == nil {
				t.Fatalf("a row for another address that carries this mailbox's digest (%s) stopped the form", status)
			}
			if got := bare(str(signup, "recipientId")); got == forged {
				t.Errorf("the signup adopted the row for another address, %q", forged)
			}
			if got := len(n.roster(t)); got != 2 {
				t.Errorf("the audience holds %d rows, want the forged one and the new member", got)
			}
		})
	}
}

// TestADigestLessOptOutBeyondTheFirstPageStillRefuses: the look at rows with no
// digest is one bounded page. An audience that has not been stamped yet can hold
// far more than a page of them, and the opt-out may be on any one: a full page
// that does not name the mailbox proves nothing, so the form reads the roster.
func TestADigestLessOptOutBeyondTheFirstPageStillRefuses(t *testing.T) {
	n := newEmptyNewsletter(t, "consent-page")
	for i := 0; i < digestLessWindow+10; i++ {
		n.plantRaw(t, fmt.Sprintf("filler-%03d", i), fmt.Sprintf("filler%03d@example.test", i), "subscribed", "")
	}
	n.plantRaw(t, "zz-optout", "Subscriber@Example.TEST", "unsubscribed", "") // written last: beyond the first page
	before := len(n.roster(t))

	n.signup(t)
	n.assertTheFormDidNothing(t, before)
}

// endlessAudience answers the reads a signup makes about a mailbox as an audience
// too large to search to the end would: a full page of rows with no digest, none of
// them the mailbox, and then a roster that never runs out.
type endlessAudience struct {
	Engine
	pages *int
}

func (e endlessAudience) Execute(ctx context.Context, q string) (any, error) {
	switch {
	case strings.HasPrefix(q, "query recipientsWithoutDigestInAudience"):
		rows := make([]any, 0, digestLessWindow)
		for i := 0; i < digestLessWindow; i++ {
			rows = append(rows, map[string]any{"id": fmt.Sprintf("v1:campaigns:recipient:stranger-%02d", i), "email": fmt.Sprintf("stranger%02d@example.test", i), "subscriptionStatus": "subscribed"})
		}
		return map[string]any{"output": rows}, nil
	case strings.HasPrefix(q, "query audienceRosterForSend"):
		// A walk that does not stop at its limit would never end; fail it here
		// instead, so the test says so rather than hanging.
		if *e.pages++; *e.pages > 50 {
			return nil, fmt.Errorf("the roster walk read %d pages of an audience that never ends and did not stop", *e.pages)
		}
		return map[string]any{
			"output": []any{map[string]any{"id": "v1:campaigns:recipient:someone", "email": "someone@example.test", "subscriptionStatus": "subscribed"}},
			"cursor": "more",
		}, nil
	}
	return e.Engine.Execute(ctx, q)
}

// TestASignupIsNotTakenWhenTheAudienceCannotBeSearchedToTheEnd: past the scan limit
// "I did not find an opt-out" proves nothing, and a public form that guessed would
// welcome a mailbox that may have opted out. It says it cannot take the
// subscription for now, and writes nothing: no signup, no consent, no row.
func TestASignupIsNotTakenWhenTheAudienceCannotBeSearchedToTheEnd(t *testing.T) {
	n := newEmptyNewsletter(t, "consent-undecidable")
	limit := mailboxRosterScanPages
	mailboxRosterScanPages = 3
	actual := n.worker.store.engine
	pages := 0
	n.worker.store.engine = endlessAudience{Engine: actual, pages: &pages}
	n.args["email"] = consentMailbox
	_, err := n.worker.handleSubscribe(n.owner, n.args, 0)
	n.worker.store.engine = actual
	mailboxRosterScanPages = limit

	if err == nil || !strings.Contains(err.Error(), "try again later") {
		t.Fatalf("the form answered %v after %d roster pages; want it to say it cannot take the subscription for now", err, pages)
	}
	if pages != 3 {
		t.Errorf("the roster was read in %d pages, want it to stop at the limit of 3", pages)
	}
	n.assertTheFormDidNothing(t, 0)
}
