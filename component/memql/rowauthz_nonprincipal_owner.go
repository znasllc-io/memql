package memql

// A NON-PRINCIPAL CANNOT OWN A ROW (memql#4817), AND A SYSTEM ACTOR'S WRITE
// NEVER CHANGES WHO OWNS AN EXISTING ROW (memql#5437).
//
// dsl/accounts/concepts.memql says of `ownerUserId`:
//
//	"a write made as the DEPLOYMENT -- a cluster owner, a system actor, or
//	 trusted server-side Go -- has that stamp undone and produces a
//	 cluster-owned row instead, which is how the seeded `self` row gets its
//	 empty owner"
//
// and createClientAccount repeats it: "executeWrite undoes the stamp for a
// system actor". THERE WAS NO SUCH UNDO. Measured on a booted cluster:
//
//	v1:accounts:account:self | system:maintenance:seedSelfAccount
//
// `seedSelfAccount` runs under auth.MaintenanceActor, whose UserId is that
// literal, and `ownerUserId: actor.userId` resolved it verbatim. Three files
// reasoned from an invariant the tree did not have.
//
// WHY IT IS FIXED RATHER THAN DOCUMENTED AWAY. Correcting the three comments
// was the other option, and it was the right one while nothing read a row's
// owner except as a string: a bogus owner matches no real user, so the row was
// reachable through the cluster-owner branch alone -- exactly the outcome the
// empty-owner design intended. The rank rules (epic memql#4832) end that. They
// resolve the OWNER'S RANK, and a synthetic id resolves to no principal and
// therefore to no rank, so a system-owned row becomes a row whose visibility
// nothing can reason about. "The cluster's own rows are unowned" has to be
// true before D2 lands on a concept, which is why memql#4837 names this as its
// ordering dependency.
//
// THE RULE IS THE SITE STAMP'S, GENERALISED. applySiteOwnerStamp
// (platform_site_hostname_policy.go) already does this for v1:platform:site,
// and its reasoning transfers whole -- including the property that makes it
// safe:
//
//	IT ONLY EVER UNDOES THE ACTOR'S OWN ID.
//
// There is no path here that names a THIRD party, which is the property the
// owner gate is actually about; an operator writing a user's row arrives with
// that user's id in the payload, it is not the caller's own, and nothing is
// touched.
//
// WHAT THE UNDO PUTS BACK DEPENDS ON WHETHER THE ROW EXISTED (memql#5437).
//
//   - A CREATE has no owner to keep, so the stamp becomes present-and-empty:
//     "the caller owns it or nobody does", as before.
//   - A WRITE TO AN EXISTING ROW keeps the owner the row was STORED with,
//     exactly -- a present value, a present-and-empty one, or no key at all.
//     Blanking there was a transfer of ownership nobody asked for. Every
//     owned-tier mutation re-stamps `ownerUserId: actor.userId`, a shipped
//     automation runs as a synthetic actor at internal origin, and the undo
//     blanked the stamp AFTER the read-merge had already written it over the
//     stored owner: measured on a db-backed engine, a fleet-shaped
//     `requestInstanceSuspend` run as an automation turned
//     `v1:identity:user:<id>` into "" with no error, and the customer could no
//     longer read their own instance. The stored owner is captured before the
//     read-merge (captureRowOwnerBefore, in executeWrite) for this reason.
//   - A stored owner that IS the actor's own id is a legacy row this rule
//     exists to repair (the `self` account above was one), so it is blanked
//     rather than restored.
//
// THE DISCRIMINATOR IS AccessContext.Synthetic, AND NOT Unranked -- the two
// are different properties and this rule needs the narrower one.
//
// Unranked says "the rank rules do not govern this actor"; Synthetic says
// "this actor can never be a row's owner". Borrowed authority
// (ContextWithUserActor) is the first and not the second: its RoleWriter is a
// stand-in, but its UserId is a real person's and rows it creates are theirs.
// Keying this on Unranked blanked `ownerUserId` on every row written through
// borrowed authority -- the worker's delegation policy, an app session --
// leaving them owned by nobody, and three db-gated tests are what caught it.
//
// AND IT IS NOT AN ID PREFIX EITHER. The site
// version tests for a `system:` prefix, and this tree says in as many words
// why that is the weaker form: "the prefix exists so a log line is legible; it
// is not a protocol, and inferring an authorization decision from a string
// shape is how a value somebody can influence becomes a permission". The flag
// is set by the three SYNTHETIC constructors and by nothing a request can
// reach. (`ContextWithUserActor` IS request-reachable -- component/identity's
// web handlers call it on the request context -- which is exactly why the
// discriminator is Synthetic and not Unranked.)

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// rowOwnerBefore is the owner field of the row a write lands on, as it was
// STORED. It is captured before the read-merge, after which a stored owner and
// a freshly stamped one are the same map entry. The zero value is a create: no
// row existed, so there is no owner to keep.
type rowOwnerBefore struct {
	field   string // the concept's declared owner field; "" for a create
	value   any
	present bool
}

// captureRowOwnerBefore reads the declared owner field off a stored row. It is
// the one capture both restores in executeWrite use -- the organization
// boundary's and the undo below -- so the two cannot disagree about what the
// row was stored with.
func captureRowOwnerBefore(conceptName string, stored map[string]any) rowOwnerBefore {
	decl := rowAuthzDeclFor(conceptName)
	if decl == nil || decl.Owner == "" {
		return rowOwnerBefore{}
	}
	value, present := stored[decl.Owner]
	return rowOwnerBefore{field: decl.Owner, value: value, present: present}
}

// restore puts the stored owner back exactly: its value, present-and-empty
// included, or no key at all when the row had none.
func (b rowOwnerBefore) restore(payload map[string]any) {
	if b.field == "" {
		return
	}
	if b.present {
		payload[b.field] = b.value
	} else {
		delete(payload, b.field)
	}
}

// undoNonPrincipalOwnerStamp undoes a declared owner field that a
// non-principal actor just stamped with its own synthetic id: on a create it
// blanks it, and on a write to an existing row (before names that row's stored
// owner) it restores what the row was stored with.
//
// Runs BESIDE stampRowAuthzOwner in executeWrite, which is to say BEFORE
// canonicalizeRelationshipFields -- load-bearing for the same reason it is
// there for sites: the stamped value is still the BARE actor id at this
// point, and the comparison below expects that. After canonicalisation it
// would be `v1:identity:user:<id>` and the match would stop firing.
func undoNonPrincipalOwnerStamp(ctx context.Context, conceptName string, payload map[string]any, before rowOwnerBefore) {
	if payload == nil {
		return
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || !ac.Synthetic {
		return
	}
	decl := rowAuthzDeclFor(conceptName)
	if decl == nil || decl.Tier != langparser.RowAuthzOwned {
		return
	}
	field := strings.TrimSpace(decl.Owner)
	if field == "" || field == langparser.RowAuthzSelfOwnedField {
		// A tier naming no field, or the self-owned form whose "field" is
		// the row's own id -- there is no owner column to blank, and
		// deleting the id would be a different and much worse operation.
		return
	}
	stamped := strings.TrimSpace(stringFromAny(payload[field]))
	if stamped == "" {
		// Already cluster-owned. Nothing to undo, and deleting an absent
		// key would turn "present and empty" into "absent" -- which the
		// rank branch reads differently on purpose (an absent owner is
		// denied outright; a present-and-empty one is a deliberate
		// statement of cluster ownership).
		return
	}
	actor := strings.TrimSpace(ac.UserId)
	if !sameRowAuthzOwner(stamped, actor) {
		// Somebody else's id. A system actor provisioning a row FOR a user
		// is the case, and that row is genuinely theirs -- leaving it is
		// the whole reason this compares rather than blanking outright.
		return
	}
	// THE ROW EXISTED: it keeps the owner it was stored with (memql#5437).
	// Unless that owner IS this actor -- a row an earlier write of the same
	// actor left synthetic-owned, which the blank below repairs.
	if before.field == decl.Owner && !sameRowAuthzOwner(strings.TrimSpace(stringFromAny(before.value)), actor) {
		before.restore(payload)
		return
	}
	// A CREATE. Present-and-EMPTY rather than absent, deliberately. Both spell
	// "cluster-owned" to sameRowAuthzOwner, which refuses an empty owner
	// either way, but only the present form is a statement: the rank
	// branch admits a present-and-empty owner at the concept's declared
	// `unowned` floor and denies an absent one, because a row that never
	// said who owns it is not the same as a row that said "nobody".
	payload[field] = ""
}
