package campaigns

import "context"

// mailbox.go -- which rows of an audience hold one mailbox, and what a public
// form does about them (memql#6022, F9).
//
// A mailbox is not one row. The same address can sit in an audience under two
// recipient ids (a manual add beside an earlier row, a race, a raw write), in
// different spellings, in different subscription states -- and it can sit there
// WITHOUT a digest, because every recipient written before the digest existed,
// or by a replica still running the previous release, has none until the digest
// sweep reaches it. The old signup compared addresses across the whole roster
// and refused when ANY row for the mailbox was not subscribed. Finding the row
// by digest is much cheaper and sees less; this file puts back what it does not
// see, and the rule that matters: ANY row that opted out, opts the mailbox out.

const (
	// digestWindow mirrors the `paginate` on recipientInAudienceByDigest: wider
	// than the duplicates any one mailbox holds. Kept as a named constant so a
	// reader comparing the Go and the DSL finds one value, and held to it by a test.
	digestWindow = 50

	// digestLessWindow mirrors the `paginate` on
	// recipientsWithoutDigestInAudience: one page of the audience's rows that
	// carry no digest. A read that returns this many may have left more behind.
	digestLessWindow = 50
)

// MailboxMembers returns every row of the audience that holds this mailbox,
// found by the digest of the address and, for the rows that carry none, by
// comparing addresses. Oldest first within each source.
//
//  1. The rows that carry the digest. Each row's OWN address is compared before
//     the row is believed: a digest is a key, a raw write can set one that is not
//     its row's, and a row for someone else is neither a member nor a reason to
//     refuse.
//  2. One page of the audience's rows that carry no digest, compared by address.
//     This is read for every mailbox, whether or not step 1 found anything: a
//     stamped subscribed row beside a digest-less row that opted out is the same
//     mailbox, and the opt-out wins.
//  3. When that page is FULL, nothing is known about the rows beyond it -- an
//     audience nobody has stamped yet holds thousands of them -- so the roster is
//     read, as the signup always did, and the answer comes from the whole audience.
//     This is the cost of the transition and it ends when the sweep has been
//     through: afterwards the page is nearly empty.
//
// Every read here decides a write, so the caller's ctx must be a fresh read.
func (s *Store) MailboxMembers(ctx context.Context, audienceID, address string) ([]Recipient, error) {
	want := NormalizeEmail(address)
	digest := EmailDigest(address)
	if want == "" || digest == "" {
		return nil, nil
	}
	stamped, err := s.RecipientsInAudienceByDigest(ctx, audienceID, digest)
	if err != nil {
		return nil, err
	}
	members := holdingMailbox(stamped, want)

	unstamped, err := s.RecipientsWithoutDigestInAudience(ctx, audienceID)
	if err != nil {
		return nil, err
	}
	members = append(members, holdingMailbox(unstamped, want)...)

	if len(unstamped) >= digestLessWindow {
		roster, err := s.Roster(ctx, audienceID)
		if err != nil {
			return nil, err
		}
		return holdingMailbox(roster, want), nil
	}
	return members, nil
}

// holdingMailbox keeps the rows whose own address normalizes to want.
func holdingMailbox(rows []Recipient, want string) []Recipient {
	var out []Recipient
	for _, r := range rows {
		if NormalizeEmail(r.Email) == want {
			out = append(out, r)
		}
	}
	return out
}

// optedOut reports whether a recipient row is in any state but subscribed. A
// row with no state at all predates the field and is subscribed, as everywhere
// else in this package.
func (r Recipient) optedOut() bool {
	return r.SubscriptionStatus != "" && r.SubscriptionStatus != "subscribed"
}

// mailboxVerdict is what a public form does about the rows a mailbox holds:
// refuse when ANY of them opted out (unsubscribed, bounced or complained),
// otherwise use the first -- the oldest -- as the member, and when there are none
// the mailbox is new.
func mailboxVerdict(members []Recipient) (member Recipient, found, refuse bool) {
	for _, r := range members {
		if r.optedOut() {
			return r, true, true
		}
	}
	if len(members) > 0 {
		return members[0], true, false
	}
	return Recipient{}, false, false
}
