package memql

// THE ACCOUNT'S DOMAIN WALK, GUARDED (epic memql#5165, design D9 and D10).
//
// Two rules that a mutation body cannot state, beside the self-archive guard
// and for the same reason its siblings live here: one reads an environment
// value, and both judge a COMBINATION of fields rather than a single one.
//
//  1. `joinOnDomain: true` BEFORE `domainStatus == "verified"`. The flag says
//     "a person arriving with a verified email on this domain joins this
//     account's group". Before ownership is proven, the domain is a string
//     somebody typed -- so accepting the flag would let anyone who can create
//     an account claim `gmail.com` and collect every arriving Gmail user into
//     their group. Refused as `domain_not_verified`, never silently corrected:
//     an operator who set the flag meant to set it, and quietly storing
//     `false` would leave them believing joining is on.
//
//  2. A `memqlDomain` UNDER THIS CLUSTER'S OWN DOMAIN, or equal to one of its
//     front-door hosts. Reserving `os.<domain>` or `api.<domain>` for a client
//     is a request to serve a name the platform already answers on, and the
//     custom-domain policy refuses the same two things for the same reason
//     (platform_custom_domain_policy.go). The cluster's domain comes from the
//     environment, so no mutation body can ask. The operator's OWN row is the
//     one account whose address legitimately sits there, and a name it only
//     inherited or was given by default is normalized away before this rule
//     runs rather than refused (memql#5792, normalizeSelfAccountReservation).
//
// NO ESCAPE FOR EITHER, deliberately -- not internal origin, not the cluster
// owner. The reconciler and the seed both write values that PASS these rules,
// so an escape would buy nothing but a way for a future writer to put an
// unservable name into the row and discover it at certificate-issuance time.
//
// A NOTE ON WHAT IS NOT GUARDED HERE. `domainStatus: "verified"` itself is
// writable by the reconciler and by anyone who may write the row, and that is
// not an oversight: `@requiresRank("admin")` on the account mutations is what
// bounds who may write at all, and a client's domain is the operator's record
// to state. The rule this file enforces is narrower and sharper -- that the
// two things which CONSUME verification cannot get ahead of it.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// The typed codes. Written as constants because three places have to agree on
// them: this file, the tests, and the OS keys that copy them (design section
// D's rule for every refusal in this epic).
const (
	accountDomainNotVerifiedCode     = "domain_not_verified"
	accountDomainUnderClusterCode    = "domain_under_cluster_domain"
	accountDomainIsFrontDoorHostCode = "domain_is_front_door_host"
)

// validateAccountDomainPolicy runs both rules against the MERGED payload.
//
// The merged payload, not the delta, for the reason the self-archive guard
// reads it: whatever combination of caller arguments and stored row produced
// this state, the question is the same one -- and a caller who flips
// `joinOnDomain` on a row whose stored status is `unverified` sends a delta
// naming only the flag.
func (e *MemQLEngine) validateAccountDomainPolicy(ctx context.Context, payload map[string]any) error {
	if payload == nil {
		return nil
	}
	if err := validateAccountJoinOnDomain(payload); err != nil {
		return err
	}
	if err := validateAccountMemqlDomain(payload); err != nil {
		return err
	}
	// The two refusals that need a READ, and therefore an engine (epic
	// memql#5168, design D7/H): the three hosts a reserved name would serve
	// must not already be answered by a live deployable or a live custom
	// domain. They run LAST because the two above are answerable from the name
	// alone and cost nothing -- there is no reason to query the database about
	// a name that is refused for its shape.
	return e.validateAccountFrontDoorHosts(ctx, payload)
}

// validateAccountJoinOnDomain refuses joining before ownership is proven (D9).
func validateAccountJoinOnDomain(payload map[string]any) error {
	joining, present := payload["joinOnDomain"]
	if !present {
		return nil
	}
	on, isBool := joining.(bool)
	if !isBool || !on {
		return nil
	}
	if strings.TrimSpace(stringFromAny(payload["domainStatus"])) == "verified" {
		return nil
	}
	return fmt.Errorf(
		"v1:accounts:account: %s -- joinOnDomain may only be set once domainStatus is \"verified\" "+
			"(epic memql#5165, D9). Until ownership is proven the domain is a string somebody typed, "+
			"and joining on it would place every arriving person with an address on that domain into "+
			"this account's group. Publish the TXT record at _memql-verify.<domain> and let the "+
			"reconciler verify it first",
		accountDomainNotVerifiedCode)
}

// validateAccountMemqlDomain refuses a reserved name this cluster already
// serves (D10).
//
// Reuses the front-door derivation rather than a second list of hosts. One
// derivation, three consumers is the rule the front door already states, and a
// second copy here would disagree exactly when a role is added -- which is the
// moment the check matters.
func validateAccountMemqlDomain(payload map[string]any) error {
	name := strings.ToLower(strings.TrimSpace(stringFromAny(payload["memqlDomain"])))
	if name == "" {
		return nil
	}
	domain := customDomainPolicyDomain()
	if domain == "" {
		// Unreachable through customDomainPolicyDomain, which never returns
		// empty. Stated anyway: an empty domain makes both tests below admit
		// everything, and admitting a name without knowing our own domain is
		// the fail-OPEN direction on a guard whose whole job is to refuse
		// names we already answer on.
		return fmt.Errorf(
			"v1:accounts:account: cannot check memqlDomain %q -- this cluster's own domain did not "+
				"resolve, and admitting a name without one would admit every name",
			name)
	}
	if accountNameInClusterSpace(name, domain) {
		return fmt.Errorf(
			"v1:accounts:account: %s -- %q is under this cluster's own domain (%s), which the platform "+
				"already routes through its one `*.%s` front-door rule. A reserved MemQL name is a "+
				"host this cluster will serve FOR the client and must not collide with a host it "+
				"already serves for itself",
			accountDomainUnderClusterCode, name, domain, domain)
	}
	for _, h := range frontdoor.Hosts(domain) {
		if name == strings.ToLower(strings.TrimSpace(h.Name)) {
			return fmt.Errorf(
				"v1:accounts:account: %s -- %q is this cluster's own %s host. Reserving it would put a "+
					"client's name on the host the platform's own %s surface answers on",
				accountDomainIsFrontDoorHostCode, name, h.Role, h.Role)
		}
	}
	return nil
}

// accountNameInClusterSpace reports whether a reserved name is this cluster's
// own domain or a name under it -- the space the platform already answers on
// through its one `*.<domain>` front-door rule. One predicate for the guard
// above and the operator normalization below, so the two cannot disagree about
// what is "ours".
func accountNameInClusterSpace(name, domain string) bool {
	return name == frontdoor.Apex(domain) || strings.HasSuffix(name, frontdoor.DomainDerivationSuffix(domain))
}

// normalizeSelfAccountReservation keeps the operator's cluster address from
// being judged as a client reservation (memql#5792).
//
// The self account is the company that OPERATES this cluster. Its front door
// is the cluster's own -- `memql.<domain>` for an operator whose company domain
// is the cluster's parent, which is the ordinary install -- so a reserved name
// in cluster space is not a collision with somebody else's host. It is the
// operator's address restated, and there is nothing for the front-door sweep
// to open. Two writers put such a name on the row without anybody asking for
// one: the default fill above (`memql.` + the domain an owner typed) and the
// stored row of a cluster seeded before the installation hostname was kept
// out of it. Judging either against the client guard refused EVERY profile
// edit of the operator organization, naming the cluster's own domain rather
// than anything the owner submitted.
//
// So an INHERITED or DERIVED name in cluster space is cleared on the self row,
// together with the reservation stamps that described it: the row then states
// what is true, that the operator holds no separate reservation. Clearing it
// is also the repair for a stored legacy row -- the next write of any kind
// stores the corrected value (the boot sweep in
// account_self_reservation_repair.go makes that write for a row nobody edits).
//
// A name the caller ASKED FOR is left for the guard. `requested` and
// `requestedNamed` are the write's own delta, captured before the read-merge,
// and an owner who types a cluster-space name on purpose is refused exactly as
// before. Client accounts never reach this branch: their collision guard is
// unchanged.
func normalizeSelfAccountReservation(id string, payload map[string]any, requested any, requestedNamed bool) {
	if payload == nil || BareShortId(strings.TrimSpace(id)) != BareShortId(selfAccountId) {
		return
	}
	name := normalizeDomainValue(stringFromAny(payload["memqlDomain"]))
	if name == "" || !accountNameInClusterSpace(name, customDomainPolicyDomain()) {
		return
	}
	if requestedNamed && normalizeDomainValue(stringFromAny(requested)) == name {
		return
	}
	payload["memqlDomain"] = ""
	payload["memqlReservedAt"] = ""
	// The reason is the front-door sweep's answer about this name. With the
	// name gone the sweep never reads the row again, so a stale refusal here
	// would sit on the operator's Domain rail forever.
	payload["memqlReservationReason"] = ""
}

// resetAccountDomainOnChange clears the ownership walk when a client's domain
// changes, and fills the reserved name when it is empty (epic memql#5165,
// section F).
//
// PROOF OF ONE NAME IS NOT PROOF OF ANOTHER. An account verified for acme.com
// that is re-pointed at acme.co.uk has proven nothing about the second, so the
// status, the token, the failure text, the verification date and the
// reservation all go -- and `joinOnDomain` goes with them, because the flag
// says "a person on this domain joins this group" and the domain it named is
// gone.
//
// A NORMALIZER RATHER THAN A REFUSAL, and that is the deliberate half. The
// alternative was refusing a write that changes a verified domain and making
// an operator clear the fields themselves; that turns a legitimate correction
// -- a client rebranded, somebody typed it wrong -- into a support question,
// and every field it would ask them to clear is one the walk sets again on its
// own within two minutes.
//
// It reads the PRIOR value, which is why it cannot live in a mutation body: a
// mutation sees the merged payload, where the new domain has already replaced
// the old one and nothing records that a change happened at all.
func resetAccountDomainOnChange(payload map[string]any, priorDomain string, priorExisted bool) {
	if payload == nil {
		return
	}
	incoming := normalizeDomainValue(stringFromAny(payload["domain"]))

	if priorExisted && incoming != normalizeDomainValue(priorDomain) {
		payload["domainStatus"] = AccountDomainStatusUnverified
		payload["domainToken"] = ""
		payload["domainFailureReason"] = ""
		payload["domainFailureDetail"] = ""
		payload["domainVerifiedAt"] = ""
		payload["memqlReservedAt"] = ""
		payload["joinOnDomain"] = false
		// The RESERVED NAME is cleared too, and then refilled below from
		// the new domain. Keeping `memql.acme.com` on an account that now
		// says acme.co.uk would reserve a name derived from a domain the
		// row no longer claims.
		payload["memqlDomain"] = ""
	}

	// Fill the default reserved name when the account has a domain and no
	// name yet. `memql.<domain>` is a starting point an operator may edit,
	// not a claim: nothing is reserved until the walk verifies ownership
	// and stamps memqlReservedAt.
	if incoming != "" && strings.TrimSpace(stringFromAny(payload["memqlDomain"])) == "" {
		payload["memqlDomain"] = "memql." + incoming
	}
}

// AccountDomainStatusUnverified is the state a changed domain returns to.
// Exported so integrations/customdomain's walk and this reset cannot disagree
// about the spelling of the state one of them writes and the other reads.
const AccountDomainStatusUnverified = "unverified"

// normalizeDomainValue lowercases and trims a domain for comparison.
//
// COMPARED NORMALIZED, because `Acme.com` and `acme.com` are the same name to
// DNS and to every person, and treating a case change as a domain change would
// throw away a verification for nothing.
func normalizeDomainValue(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}
