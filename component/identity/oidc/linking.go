package oidc

import (
	"sort"
	"strings"
)

// ACCOUNT LINKING AND ROLE MAPPING: the policy half (memql#4611).
//
// provider.go / idtoken.go answer "who does the upstream say this is". This
// file answers "and what does this cluster do about it" -- as PURE functions
// over facts a caller looks up, so every branch is testable without a database
// and the security-relevant ones are named rather than implied.
//
// -----------------------------------------------------------------------------
// THE LINKING RULE, AND THE ONE WAY IT CAN GO WRONG
// -----------------------------------------------------------------------------
//
// An existing magic-link or passkey user who later signs in via the IdP must
// land on the SAME row, not a duplicate. Two rows for one person means two sets
// of grants, two audit trails, and a deprovisioning that removes one of them.
//
// The match order is (issuer, subject) first, then verified email:
//
//   1. (issuer, subject) is the STABLE identity and the only one the provider
//      guarantees. It survives a rename, an address change, and an address
//      being reassigned to somebody else.
//   2. Email is the BOOTSTRAP, used exactly once -- the first time a known
//      person arrives through the IdP, before any (issuer, subject) link
//      exists. After that the link is what matches.
//
// AND THE EMAIL MUST BE VERIFIED BY THE PROVIDER. This is the whole security
// story of the file. An unverified `email` claim is a string the directory did
// not check, so linking on it means anyone who can set their own email at the
// upstream can take over the matching MemQL account. That is account takeover
// via a claim, and it is the classic OIDC linking vulnerability. An unverified
// address therefore links to NOTHING -- it is treated as a stranger, and a
// stranger is subject to the registration mode like any other.

// LinkAction is what the caller should do with a verified upstream identity.
type LinkAction string

const (
	// LinkExisting: an established link resolved to a user. Sign them in.
	LinkExisting LinkAction = "link_existing"
	// LinkByEmail: no link yet, but a verified email matched an existing user.
	// Attach the identity to that row and sign them in.
	LinkByEmail LinkAction = "link_by_email"
	// LinkRegister: nobody matched. Whether they may register is the
	// registration mode's decision, not this function's.
	LinkRegister LinkAction = "link_register"
	// LinkRefuse: the claims cannot be acted on at all.
	LinkRefuse LinkAction = "link_refuse"
)

// LinkLookup is what the caller has already resolved from the cluster.
//
// PASSED IN rather than looked up here, so the decision is a pure function of
// stated facts. A reader can see every input to a security decision in one
// struct instead of inferring it from a query.
type LinkLookup struct {
	// UserIdByLink is the user an existing (issuer, subject) identity row
	// names, or "" when no such row exists.
	UserIdByLink string
	// UserIdByEmail is the user whose primary email equals the claim's, or ""
	// when nobody matches.
	UserIdByEmail string
	// EmailBelongsToActiveUser distinguishes "matched a live account" from
	// "matched a deactivated one". Linking into a deactivated account would
	// resurrect access somebody deliberately removed.
	EmailBelongsToActiveUser bool
}

// LinkDecision is the answer, with the reason it was reached.
type LinkDecision struct {
	Action LinkAction
	UserId string
	// Reason is a stable token for the audit trail. Every refusal keeps a
	// DISTINCT one -- memql#4601's constraint, and it is what makes a support
	// question answerable from the trail.
	Reason string
}

// DecideLink resolves a verified upstream identity to an action.
func DecideLink(c Claims, look LinkLookup) LinkDecision {
	if strings.TrimSpace(c.Subject) == "" || strings.TrimSpace(c.Issuer) == "" {
		return LinkDecision{Action: LinkRefuse, Reason: "oidc_claims_incomplete"}
	}

	// 1. THE ESTABLISHED LINK WINS, unconditionally and before email is even
	// considered. Once (issuer, subject) names a user, a changed email claim is
	// a changed email -- not a different person -- and re-matching on it would
	// move somebody's account when their surname changed.
	if id := strings.TrimSpace(look.UserIdByLink); id != "" {
		return LinkDecision{Action: LinkExisting, UserId: id, Reason: "oidc_subject_link"}
	}

	email := strings.ToLower(strings.TrimSpace(c.Email))
	if email == "" {
		return LinkDecision{Action: LinkRegister, Reason: "oidc_no_email_claim"}
	}

	// 2. EMAIL LINKING, AND ONLY WHEN THE PROVIDER VERIFIED IT.
	if !c.EmailVerified {
		// Not a refusal: this person may still be entitled to register. What
		// they may NOT do is inherit an existing account on an unverified
		// claim, which is account takeover.
		return LinkDecision{Action: LinkRegister, Reason: "oidc_email_unverified"}
	}
	if id := strings.TrimSpace(look.UserIdByEmail); id != "" {
		if !look.EmailBelongsToActiveUser {
			// Refuse rather than register. Registering would mint a SECOND row
			// for an address a deactivated one already holds, which is the
			// duplicate this whole file exists to prevent -- and it would hand
			// access back to somebody it was deliberately removed from.
			return LinkDecision{Action: LinkRefuse, Reason: "oidc_email_matches_deactivated_user"}
		}
		return LinkDecision{Action: LinkByEmail, UserId: id, Reason: "oidc_verified_email_link"}
	}
	return LinkDecision{Action: LinkRegister, Reason: "oidc_new_user"}
}

// -----------------------------------------------------------------------------
// ROLE MAPPING
// -----------------------------------------------------------------------------

// GroupRoleMap maps a directory group (id or name) to a cluster role slug.
//
// A MAP RATHER THAN AN ORDERED LIST, with the HIGHEST match winning, because
// group membership is a set and a person is legitimately in several. Taking the
// first match would make the outcome depend on the order the directory happened
// to return them, which is not a thing an operator can reason about.
type GroupRoleMap map[string]string

// MapRole resolves the cluster role for a set of directory groups.
//
// rank is injected (component/auth owns the rank model, and this package must
// not import it -- component/auth is below identity in the dependency order).
// Returns "" when nothing matched, which the caller reads as "the cluster
// default", NOT as "no access": whether an unmapped person may sign in at all
// is the registration mode's decision, and conflating the two would make a
// missing group mapping silently equivalent to a ban.
func (m GroupRoleMap) MapRole(groups []string, rank func(string) int) string {
	if len(m) == 0 || len(groups) == 0 {
		return ""
	}
	best, bestRank := "", -1
	// Sorted so the outcome is deterministic when two groups map to roles of
	// EQUAL rank -- otherwise the answer would depend on directory ordering.
	sorted := append([]string(nil), groups...)
	sort.Strings(sorted)
	for _, g := range sorted {
		role, ok := m[strings.TrimSpace(g)]
		if !ok {
			continue
		}
		if r := rank(role); r > bestRank {
			best, bestRank = role, r
		}
	}
	return best
}

// ParseGroupRoleMap reads the `group=role,group=role` operator spelling.
//
// Refuses silently-wrong input rather than dropping it: a mapping that does not
// parse is a mapping an operator believes is in force, and the roles it grants
// are the ones that matter most.
func ParseGroupRoleMap(raw string, validRole func(string) bool) (GroupRoleMap, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := GroupRoleMap{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.LastIndex(pair, "=")
		if eq <= 0 || eq == len(pair)-1 {
			return nil, &MapError{Entry: pair, Problem: "expected group=role"}
		}
		group := strings.TrimSpace(pair[:eq])
		role := strings.TrimSpace(pair[eq+1:])
		if group == "" || role == "" {
			return nil, &MapError{Entry: pair, Problem: "expected group=role"}
		}
		if validRole != nil && !validRole(role) {
			return nil, &MapError{Entry: pair, Problem: "unknown cluster role " + role}
		}
		out[group] = role
	}
	return out, nil
}

// MapError names the entry that failed, because a mapping is a list and
// "invalid" without the offending entry is unactionable.
type MapError struct {
	Entry   string
	Problem string
}

func (e *MapError) Error() string {
	return "group role mapping entry " + e.Entry + ": " + e.Problem
}
