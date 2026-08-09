package deploycontrol

// refusal.go carries the audit id of a REFUSED deploy-control call back to the
// caller who was refused (memql#3334).
//
// ===========================================================================
// THE QUESTION #3334 ASKED, AND THE ANSWER
// ===========================================================================
//
// (1) Are refusals audited today? YES, and they always have been.
// Service.authorizeWith emits a blocked v1:identity:auditEvent -- category
// admin, action deployment_console_<verb>, outcome blocked, with the actor,
// their role, and the action's detail map -- BEFORE it returns the status
// error. TestStreamedDenialEmitsBlockedAuditLikeUnary has asserted exactly one
// such event per refused call, on both the unary and the streamed surface,
// since #3311. What did not exist was any way for the caller to learn the id:
// emitAudit returned it and authorizeWith dropped it on the floor.
//
// (2) Should the REFUSED caller see that id, or only an admin reading the
// audit log? The refused caller sees it. Three reasons, in the order they
// matter:
//
//   - It reveals nothing the refusal does not already reveal. The message
//     beside it is "deploy console: rollback requires owner role (have
//     \"admin\")" -- it names the required role AND the caller's own. Against
//     that, an opaque random token is not a disclosure. It is not a resource
//     identifier the caller can dereference either: the audit log is
//     admin-gated, so holding the id grants exactly nothing.
//
//   - Enumeration buys an attacker nothing. Each attempt mints a FRESH random
//     id; there is no space to walk, no id that names another user's event,
//     and no read the id unlocks. The only thing it confirms is "your attempt
//     was recorded" -- which the operator guide states publicly, and which
//     deters rather than assists.
//
//   - The support need is real and one-directional. An operator denied a
//     legitimate rollback has to be able to say "here is the reference".
//     Without one, the person with the problem and the person who can resolve
//     it are reduced to correlating on a timestamp and a guess at which
//     replica served them. That is the cost of withholding; the benefit of
//     withholding is, per the two points above, zero.
//
// This is also the call memQL already made on the neighbouring surface.
// adminops.Result (memql#3324) documents its AuditEventId as "Present on
// refusals" and IdentityAdminResult carries it on the denied path, for the
// same operator argument. The deploy console being the one gated write surface
// that stayed silent was an inconsistency, not a policy.
//
// The line NOT crossed: the caller gets the id and nothing else. Not the event
// body, not the detail map, not who else has tried. A receipt, not a read.
//
// ===========================================================================
// HOW IT TRAVELS -- one mechanism, three surfaces
// ===========================================================================
//
// The gate's signature is (actor, error): a refusal has no ActionResult to
// hang a field on, because the action never ran. So the id rides as a gRPC
// error DETAIL (RefusalInfo) on the status the gate already returns.
//
//	unary      : status error -> RefusalInfo detail          (dialed clients)
//	streamed   : Dispatch.failResult lifts the detail onto
//	             DeployControlResult.audit_event_id           (portal, VS Code)
//	forwarded  : the receiving node produces that same
//	             DeployControlResult and the bff marshals it
//	             back verbatim -- nothing extra to plumb       (memql#3380)
//
// The forwarded path is free precisely because #3380 forwards the RESULT
// rather than re-deriving one: the hop already carries whatever Dispatch
// produced. A refusal decided by the owner-only gate on the identity node
// therefore reaches a browser talking to a bff with its audit id intact.

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// refusalError builds the status error a gate refusal returns, with the audit
// event's correlation id attached as a RefusalInfo detail.
//
// A failure to attach the detail is deliberately swallowed: the refusal itself
// is the security-relevant outcome and must be returned whatever happens to
// the correlation metadata. Degrading to a plain status matches the behaviour
// before #3334 rather than turning a denial into an Internal error.
func refusalError(code codes.Code, auditEventID, message string) error {
	st := status.New(code, message)
	if auditEventID == "" {
		return st.Err()
	}
	withDetail, err := st.WithDetails(&memqlv1.RefusalInfo{AuditEventId: auditEventID})
	if err != nil {
		return st.Err()
	}
	return withDetail.Err()
}

// AuditEventIdFromError returns the audit correlation id a refusal carries, or
// "" for any error that is not one.
//
// Exported because the id is only useful to whoever is presenting the refusal:
// the streamed bridge (Dispatch) calls it to populate
// DeployControlResult.audit_event_id, and a dialed Go client calls it to show
// an operator the reference. "" is a normal answer -- an INVALID_ARGUMENT
// rejection runs before the gate and writes no event, so there is no id to
// find.
func AuditEventIdFromError(err error) string {
	if err == nil {
		return ""
	}
	st, ok := status.FromError(err)
	if !ok || st == nil {
		return ""
	}
	for _, detail := range st.Details() {
		if info, isRefusal := detail.(*memqlv1.RefusalInfo); isRefusal {
			return info.GetAuditEventId()
		}
	}
	return ""
}
