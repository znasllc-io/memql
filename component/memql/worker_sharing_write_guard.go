package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// A MACHINE'S SHARING IS ITS OWNER'S CONSENT, AND ONLY SERVER CODE WRITES IT
// (memql#5658, the root cause #5335 named and left in place).
//
// v1:worker:registration declares @rowAuthz(owner="ownerUserId", clusterOwner),
// the owned tier with the admin gate ORed in, so the write guard's cluster-owner
// escape admits a cluster owner onto every machine in the cluster
// (rowAuthzWriteEscapeFor; only rankStrict withdraws it). #5335 closed the
// mutation route -- setWorkerSharing is @serverOnly and fleetSetSharing, its one
// renderer, resolves the machine through the caller's own -- but the raw
// insert() literal names no construct and never consults @serverOnly, and
// neither does the update{} body of an inline or authored mutation. Each of
// those reaches executeWrite under the caller's actor, and for a cluster owner
// the escape let it rewrite somebody else's consent. Since #5344 the block names
// people and groups, so one such write could lend a machine to anyone.
//
// WHY A FIELD GUARD AND NOT A NARROWER TIER. Dropping clusterOwner from the tier
// narrows every READ as well: the operator Fleet view's cluster-wide list
// (allWorkersWithStatus), the lookup fleetRevokeMachine's operator arm runs, and
// every graph.node.* event for a machine the operator does not own -- row
// admission gates subscriptions too, and no internal-origin builtin can stand in
// for one. The defect is one write of one block, so the guard is one block wide
// and every other write on the row keeps the tier it has.
//
// KEYED ON A CHANGE AND ON ORIGIN, the construct ladder's shape
// (construct_ladder_write_guard.go). Every heartbeat, rename and label edit
// writes this row and the read-merge carries the block through unchanged, so it
// is judged only when the final row differs from the stored one (prior is nil on
// a create, where a block that is set is a change). Only internal origin passes:
// fleetSetSharing stamps it for its one write, after resolving the machine
// through the caller's own and checking every person and group named against the
// ones the owner may pick. No caller -- the owner and a cluster owner included --
// has a reason to write the block another way, and an owner's raw write would
// skip that check.
//
// FUTURE FIELDS. A new consent field on this row is protected by adding its name
// to workerRegistrationConsentFields, and is not protected until then: there is
// no annotation that marks a field server-written, and a field on another
// concept needs a guard of its own, as the construct ladder has.
var workerRegistrationConsentFields = []string{"sharing"}

// validateWorkerConsentServerOnly refuses a write to a v1:worker:registration
// that changes the owner's consent without internal origin. It also refuses
// moving an EXISTING row's owner, because that is the two-step route to the same
// write: take the machine, then lend it through fleetSetSharing as its owner.
// The owner field is @serverSet, stamped from the actor on the create and never
// accepted from caller args, so nothing legitimate moves it on a client write;
// ownership transfer (memql#4838) writes under internal origin. A create is not
// judged on the owner: the worker store creates the row under the owner's
// borrowed actor, and its owner is whatever that stamp produced.
func validateWorkerConsentServerOnly(ctx context.Context, prior, final map[string]any) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	for _, field := range workerRegistrationConsentFields {
		if payloadFieldChanged(prior, final, field) {
			return errWorkerConsentWrite(field,
				"its owner's consent to lend the machine, which only fleetSetSharing writes, through the caller's own machines")
		}
	}
	if prior != nil && registrationOwnerChanged(prior, final) {
		return errWorkerConsentWrite("ownerUserId",
			"who paired the machine and so whose consent its sharing records; moving it would let fleetSetSharing treat somebody else as its owner")
	}
	return nil
}

// registrationOwnerChanged compares the stored owner with the one about to be
// written, tolerating the bare and canonical spellings of one id: the stored
// value is canonical, and a template stamping actor.userId writes it bare.
func registrationOwnerChanged(prior, final map[string]any) bool {
	before := strings.TrimSpace(stringFromAny(prior["ownerUserId"]))
	after := strings.TrimSpace(stringFromAny(final["ownerUserId"]))
	if before == "" || after == "" {
		return before != after
	}
	return !sameRowAuthzOwner(before, after)
}

func errWorkerConsentWrite(field, what string) error {
	return fmt.Errorf(
		"%s: write to `%s` refused -- it is %s. Only server code writes it, under internal origin: a raw "+
			"insert() or an inline update{} never consults setWorkerSharing's @serverOnly, and the row's "+
			"cluster-owner escape is right for reading and offboarding a machine, never for giving its "+
			"owner's consent. See component/memql/worker_sharing_write_guard.go and memql#5658.",
		WorkerRegistrationConcept, field, what,
	)
}
