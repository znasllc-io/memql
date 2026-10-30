package memql

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// site_access_guards.go -- the two engine-side rules deployable access codes
// need that no construct in the DSL can state (design record
// docs/superpowers/specs/2026-10-29-deployable-access-codes-design.md, D14 and
// D15; the review of Task 3 that found each).
//
// Both are narrow on purpose and both are named lists with their reasons
// beside them, like the row-version fence's, so the next reader finds the why
// where the what is.

const (
	siteAccessCodeConcept    = "v1:platform:siteAccessCode"
	siteAccessSessionConcept = "v1:platform:siteAccessSession"
)

// clientUnreadableConcepts are the concepts whose rows NO CLIENT READS, by any
// path, whatever the row tier would say. The value is the reason, and a concept
// does not go on this list without one.
//
// WHY A TIER IS NOT ENOUGH HERE. A concept's @rowAuthz decides who may see a
// row; it does not decide THROUGH WHICH DOOR. Every client path to an access
// code is a builtin (siteAccessCodesForSite and the rest) that passes two gates
// before it reads a row: the app:deployables/access part, and the organization
// gate that judges the part against the organization the call selected. A raw
// read -- `concept==v1:platform:siteAccessCode && siteId==...`, and its
// spellings parentOf, sort(paginate(...)), asOf and a subscription -- never
// reaches a builtin, so it passes neither. In review, a person who held the part
// in one organization read the labels, hints and digests of a site in another
// whose row tier admitted them, with the builtin refusing them for the same site.
// No working code was exposed (only a digest is stored), and the owner asked for
// the one clause that closes it rather than the argument that it is harmless.
//
// WHO STILL READS. The server does, and says so twice over: internal origin, which
// every handler under the builtins and the bff stamp on their own reads (and which
// the organization gate has already judged by then), and an unranked actor, which
// is how the retention sweep and the seed materializer are built and which no
// request can construct. Neither is reachable from the wire, so neither widens
// what a client sees.
//
// ONE EXIT IS WIDER THAN IT NEEDS TO BE, and it is recorded here rather than left
// to be found. Unranked also covers BORROWED AUTHORITY (auth.ContextWithUserActor),
// which server code uses to act for a named person and which production code
// constructs in dozens of places. None of them runs a raw query a client wrote
// today, so none reaches these rows; one that did (an agent tool that executes a
// person's own MemQL, say) would be admitted by this clause and decided by the
// row tier alone. Narrowing the exit to the cluster acting (Synthetic: the
// maintenance, seed and automation actors) closes that, costs the sweep nothing,
// and is a one-word change below. It is not made here because the ruling this
// implements names "an unranked actor".
//
// WHAT THIS IS NOT. It is not a tier: it grants nothing, and a caller that passes
// it is still decided by the concept's @rowAuthz row by row. It is the read-side
// counterpart of @serverWritten, which bars the same two concepts from a raw write.
var clientUnreadableConcepts = map[string]string{
	siteAccessCodeConcept:    "its row holds the digest of a bearer credential and the operator's notes on who may use it, and the only client path to it is a builtin that passes the access part and the organization gate; a raw read passes neither",
	siteAccessSessionConcept: "its row is what the bff honours in place of a code, with the digest of a live cookie, and nothing a person calls needs it except through the code's builtins; a raw read would hand the owner of a site the very rows the visitor protocol trusts",
}

// clientReadRefused reports whether this caller is a CLIENT asking for a row of a
// concept no client reads. Internal origin and an unranked actor are not clients;
// see clientUnreadableConcepts for what each is and why neither can be forged.
func clientReadRefused(ctx context.Context, conceptName string) bool {
	if _, listed := clientUnreadableConcepts[strings.TrimSpace(conceptName)]; !listed {
		return false
	}
	return !auth.OriginFromContext(ctx).IsInternal() && !rowAuthzActorIsUnranked(ctx)
}

// errSiteHasLiveAccessCodes is the refusal of a change to a site's organization
// while codes minted against it are still live. The text says what to do, because
// the person who meets it is moving a deployable and has no reason to know codes
// are in the way.
var errSiteHasLiveAccessCodes = errors.New("organization_transfer_refused: this deployable still has access codes; remove its access codes before changing its organization")

// refuseSiteMoveWithLiveAccessCodes refuses ANY change of a site's accountId
// while a code minted against the site has not been removed (D15).
//
// A code COPIES its site's accountId at mint time: the row tier's account arm and
// the organization gate both decide an act on the code from that copy, without
// reading the site again. A site that moved would leave the copy naming the
// organization it left, and the two answers go wrong in opposite directions --
// people of the old organization could still regenerate, re-time and remove a code
// on a site they can no longer read, and the new organization would see an empty
// panel for codes it cannot act on. Refusing the move is the smallest rule that
// keeps the copy true; the alternative, rewriting every code on every move, would
// need a writer for a @serverWritten row that nothing else has.
//
// WHICH CODES BLOCK. Every code that has not been removed, EXPIRED ONES
// INCLUDED, because setting a new expiry revives an expired code and the revived
// code must still be judged by the organization that owns the site. A removed code
// (revokedAt set) does not block: removal is final, nothing writes it back, and a
// site that once had a code would otherwise be pinned to its organization forever.
//
// ANY CHANGE COUNTS, tying a site that had no organization included: the codes of
// an untied site carry an empty accountId, and a tie would leave the copy empty
// where the site now has an organization.
//
// staged-data: MUST-NOT-GATE -- an unpublished code of the site is still a code
// naming the old organization; hiding it would let the move through.
func (e *MemQLEngine) refuseSiteMoveWithLiveAccessCodes(ctx context.Context, siteID string) error {
	if e.database() == nil {
		return fmt.Errorf("organization_unavailable: dependent records cannot be checked")
	}
	var rows []memorynodes.MemoryNode
	if err := e.database().NewSelect().Model(&rows).DistinctOn("id").Where("concept = ?", siteAccessCodeConcept).OrderExpr(`id ASC, "createdAt" DESC`).Scan(ctx); err != nil {
		return fmt.Errorf("organization_unavailable: %w", err)
	}
	for _, row := range rows {
		payload := accountRowPayload(row)
		if BareShortId(stringFromAny(payload["siteId"])) != BareShortId(siteID) {
			continue
		}
		if strings.TrimSpace(stringFromAny(payload["revokedAt"])) != "" {
			continue
		}
		return errSiteHasLiveAccessCodes
	}
	return nil
}
