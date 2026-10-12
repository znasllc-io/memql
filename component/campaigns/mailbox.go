package campaigns

import (
	"context"
	"errors"
	"fmt"
)

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

// mailboxRosterScanPages is the most pages of the roster (500 rows each) that
// MailboxMembers reads, when a full page of digest-less rows sends it there, before
// it gives up trying to prove a mailbox absent. The walk is the cost the digest
// removed, and it is paid on every signup while the rows have no digest: measured at
// about two seconds a page with 140,000 to 240,000 recipients in the cluster (41
// pages in 86 s, 201 pages in 8 min 25 s), because the engine collapses every
// current recipient version in the cluster to answer any recipient query. A walk of
// a large audience takes minutes and a visitor's request does not wait for that.
// Ten pages are 5,000 recipients, which covers the audiences the signup was built
// for. A variable only so a test can lower it.
//
// An audience can be larger -- the ceiling is 250,000 -- and past the limit the
// answer is "cannot tell", not "not there".
var mailboxRosterScanPages = 10

// ErrMailboxUndecidable is what MailboxMembers returns when it cannot rule out
// that a mailbox opted out: the audience holds a page of rows without a digest and
// more pages of roster than the scan will read. The caller must not read it as "no
// such mailbox" -- a public form says it cannot take the subscription for now.
var ErrMailboxUndecidable = errors.New("campaigns: cannot rule out an earlier opt-out for this address")

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
//     read, as the signup always did, and the answer comes from the audience. This
//     is the cost of the transition and it ends when the sweep has been through:
//     afterwards the page is nearly empty. The walk stops as soon as it meets a row
//     that opted out (nothing further can change the answer) and gives up with
//     ErrMailboxUndecidable after mailboxRosterScanPages pages without having seen
//     the end of the audience.
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
		return s.mailboxFromRoster(ctx, audienceID, want)
	}
	return members, nil
}

// mailboxFromRoster answers from the audience, a page at a time, keeping only the
// rows that hold the mailbox. It returns as soon as one of them has opted out, and
// ErrMailboxUndecidable if it has read mailboxRosterScanPages pages and the audience
// goes on: Store.Roster would return what it had read and call it the audience,
// which is how an opt-out beyond its cap is missed.
func (s *Store) mailboxFromRoster(ctx context.Context, audienceID, want string) ([]Recipient, error) {
	var held []Recipient
	cursor := ""
	for pages := 1; ; pages++ {
		page, next, err := s.RosterPage(ctx, audienceID, cursor)
		if err != nil {
			return nil, err
		}
		held = append(held, holdingMailbox(page, want)...)
		for _, r := range held {
			if r.optedOut() {
				return held, nil
			}
		}
		if next == "" {
			return held, nil
		}
		if pages >= mailboxRosterScanPages {
			return nil, fmt.Errorf("%w: read %d pages of the audience and it holds more, some of its rows without a digest yet", ErrMailboxUndecidable, pages)
		}
		cursor = next
	}
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
