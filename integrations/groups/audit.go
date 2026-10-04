package groups

// audit.go -- the trail (epic memql#5165, section I).
//
// Five actions on v1:identity:auditEvent, the DECISIONS log: creating,
// editing and archiving a group, and placing or removing a person. Every one
// is a human decision about who reaches a client's work, which is exactly what
// that log is for -- as opposed to v1:identity:authActivity, which records
// routine mechanics two orders of magnitude more numerous.
//
// # The target type is the group, not the user
//
// component/identity/adminops hardcodes `targetType: "user"` whenever a target
// id is set, because every operation it serves targets a person. These do not:
// a membership is a decision ABOUT a person but TO a group, and the question an
// auditor asks of it -- "who could reach Acme's work, and since when" -- is
// answered by walking the group. So the writes here name their own target type,
// and `group` / `groupMembership` are declared on the concept's enum, which
// test/dslconformance/identity_audit_enum_contract_test.go walks.
//
// # A failed audit write does not fail the operation
//
// The row is already written by the time this runs, so returning the error
// would report a failure that did not happen and invite a retry that would
// write a second version. It is logged instead -- which is the same call
// adminops' own `finish` makes when the trail write fails after the operation.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

// The audit target types these writes use. Both are declared on
// v1:identity:auditEvent's `targetType` enum.
const (
	auditTargetGroup      = "group"
	auditTargetMembership = "groupMembership"
)

// log records one decision.
//
// `targetId` decides the target TYPE: a membership id carries the pair, a
// group id does not, so the two are told apart by which call site is writing
// rather than by parsing the id.
func (i *Integration) log(ctx context.Context, c caller, action, targetID string, detail map[string]any) {
	targetType := auditTargetGroup
	switch action {
	case "group_member_added", "group_member_removed":
		targetType = auditTargetMembership
	}
	i.writeAudit(ctx, c, action, targetType, targetID, detail)
}

func (i *Integration) writeAudit(ctx context.Context, c caller, action, targetType, targetID string, detail map[string]any) {
	if i == nil || i.store == nil || i.store.engine == nil {
		return
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		encoded = []byte("{}")
	}
	primaryEmail := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		primaryEmail = ac.PrimaryEmail
	}
	query := fmt.Sprintf(
		`mutation createAuditEvent(eventId:%s, occurredAt:%s, category:%s, action:%s, actorUserId:%s, actorEmail:%s, actorRole:%s, targetType:%s, targetId:%s, outcome:%s, detail:%s)`,
		langparser.QuoteString("v1:identity:auditEvent:"+id.NewShortId()),
		langparser.QuoteString(i.now().Format(time.RFC3339Nano)),
		langparser.QuoteString("admin"),
		langparser.QuoteString(action),
		langparser.QuoteString(c.userID),
		langparser.QuoteString(primaryEmail),
		langparser.QuoteString(string(c.role)),
		langparser.QuoteString(targetType),
		langparser.QuoteString(targetID),
		langparser.QuoteString("success"),
		string(encoded))
	// Internal origin for this one write (memql#5624). This said the caller's
	// unstamped context was enough because auditEvent "is owned by
	// actorUserId"; it never was. The concept is cluster-owner-tier, and its
	// create now admits a cluster owner or server code and nobody else -- so
	// an admin managing groups wrote no trail at all. The statement is
	// composed above with every value quoted, so the stamp reaches this one
	// createAuditEvent; the actor it records is still the caller.
	if _, err := i.store.engine.Execute(auth.ContextWithInternalOrigin(ctx), query); err != nil && i.warn != nil {
		i.warn("groups: audit write failed", "action", action, "target", targetID, "error", err.Error())
	}
}
