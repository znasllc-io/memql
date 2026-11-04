package groups

// domainjoin.go -- an account's own domain as a way in (epic memql#5165, D9).
//
// When a person arrives -- an invitation accepted, a first sign-in -- and their
// email address is on a domain an account has PROVEN it owns and has switched
// joining on for, they land in that account's group.
//
// # Three conditions, and every one of them is load-bearing
//
//  1. THE ADDRESS MUST BE VERIFIED, and the seam that knows says so. An
//     unverified claim is a string a provider did not check, and joining on it
//     is the same class of mistake as LINKING on it -- which the OIDC
//     federation rule already refuses by name. There is no default here: the
//     caller passes the flag, because only the caller knows.
//
//  2. THE ACCOUNT'S DOMAIN MUST BE `verified`. Before that the domain is a
//     string somebody typed, and anyone who can create an account could claim
//     gmail.com and collect every arriving Gmail user.
//
//  3. `joinOnDomain` MUST BE ON. Proving you own a domain is not the same as
//     asking for everyone on it, and a client whose staff share a domain with
//     their customers wants exactly one of those.
//
// # Never retroactive
//
// This runs at ARRIVAL and touches nobody already here. Turning the flag on
// does not sweep the existing roster, and that is the decision rather than an
// omission: a sweep would silently place people who joined the cluster for
// some other reason, and the person turning the flag on has no list of who it
// would move.

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// ApplyDomainJoin places a newly-arrived person in the group of the account
// whose verified domain matches their verified email address.
//
// IDEMPOTENT, and returns the group it joined them to (empty when it did
// nothing) so a caller can log the fact. A no-op is the ordinary answer: most
// people arrive on an address no account claims.
//
// IT NEVER FAILS AN ARRIVAL. Every refusal path returns ("", nil) rather than
// an error, because this is the last step of provisioning a user and the row
// is already written -- reporting a failure here would surface as "sign-in
// failed" to somebody whose account exists and works. A genuine engine error
// is returned, and its callers log rather than abort for the same reason.
func (i *Integration) ApplyDomainJoin(ctx context.Context, userID, email string, emailVerified bool) (string, error) {
	if i == nil || i.store == nil {
		return "", nil
	}
	userID = memql.BareShortId(strings.TrimSpace(userID))
	if userID == "" || !emailVerified {
		return "", nil
	}
	domain := emailDomain(email)
	if domain == "" {
		return "", nil
	}

	account, err := i.store.AccountByVerifiedJoinDomain(ctx, domain)
	if err != nil {
		return "", err
	}
	if account == "" {
		return "", nil
	}

	groupID := AccountGroupID(account)
	group, err := i.store.GroupByID(ctx, groupID)
	if err != nil {
		return "", err
	}
	// The account is verified and joining, but its group is missing or
	// archived -- an account archived between the read and here, or a
	// cluster whose backfill has not run. Joining somebody to a group that
	// grants nothing is worse than not joining them: it reads as success on
	// every screen.
	if group == nil || group.Status != StatusActive {
		return "", nil
	}

	m := Membership{
		ID:      MembershipID(groupID, userID),
		GroupID: groupID,
		UserID:  userID,
		Origin:  OriginDomain,
		Status:  StatusActive,
	}
	// addedBy is EMPTY, and that is the honest value: no person did this.
	// Stamping a synthetic id would put a principal that does not exist
	// into the answer to "who let them in".
	if err := i.store.WriteMembership(ctx, m, "", ""); err != nil {
		return "", err
	}
	return groupID, nil
}

// emailDomain reads the domain half of an address, lowercased.
//
// The LAST `@`, because a local part may legally contain one in a quoted
// form; splitting on the first would read `"a@b"@acme.com` as domain
// `b"@acme.com`, which matches nothing -- a miss rather than a wrong join, but
// a miss nobody would be able to explain.
func emailDomain(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.TrimSpace(email[at+1:])
}

// PlaceInvitedMember writes one membership from an accepted invitation.
//
// NO CALLER GUARD, and the reason is that the guard already ran: every group
// on an invitation was validated at ISSUE against an active group and against
// the rank rule, by a caller who held `update` on group. The person arriving
// here has no authority of their own yet -- they are being provisioned -- so
// checking theirs would refuse every legitimate placement.
//
// `issuedBy` is the INVITER, recorded as who put them there. That is the
// honest answer: an admin decided this, at issue, and the arrival merely
// executed it.
func (i *Integration) PlaceInvitedMember(ctx context.Context, groupID, userID, issuedBy string) error {
	if i == nil || i.store == nil {
		return nil
	}
	groupID = memql.BareShortId(strings.TrimSpace(groupID))
	userID = memql.BareShortId(strings.TrimSpace(userID))
	if groupID == "" || userID == "" {
		return nil
	}
	// The group is RE-READ rather than trusted from the invitation: it may
	// have been archived between issue and acceptance, and a membership in
	// an archived group grants nothing while reading as success.
	group, err := i.store.GroupByID(ctx, groupID)
	if err != nil {
		return err
	}
	if group == nil || group.Status != StatusActive {
		return refusal(CodeGroupNotActive,
			groupID+" is not an active group, so the invitation's placement was not made")
	}
	return i.store.WriteMembership(ctx, Membership{
		ID:      MembershipID(groupID, userID),
		GroupID: groupID,
		UserID:  userID,
		Origin:  OriginInvitation,
		Status:  StatusActive,
	}, memql.BareShortId(strings.TrimSpace(issuedBy)), "")
}

// GroupJoinsOrganization reports whether a membership of `groupID` makes a
// person a member of organization `accountID`: the group exists, is ACTIVE,
// and is tied to that organization. It is D10 asked again when an invitation
// carrying a role scoped to `accountID` is accepted, because the group was
// judged at issue and may have been archived, or re-tied, since.
//
// An unreadable group is an ERROR rather than a "no", so the caller decides;
// the acceptance seam refuses on either.
func (i *Integration) GroupJoinsOrganization(ctx context.Context, groupID, accountID string) (bool, error) {
	if i == nil || i.store == nil {
		return false, nil
	}
	groupID = memql.BareShortId(strings.TrimSpace(groupID))
	accountID = memql.BareShortId(strings.TrimSpace(accountID))
	if groupID == "" || accountID == "" {
		return false, nil
	}
	group, err := i.store.GroupByID(ctx, groupID)
	if err != nil {
		return false, err
	}
	return group != nil && group.Status == StatusActive && memql.BareShortId(group.AccountID) == accountID, nil
}
