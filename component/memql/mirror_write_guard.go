package memql

// A MIRROR IS READ-ONLY BY CONSTRUCTION (epic memql#4378, D3).
//
// A concept declaring `@origin("<connector>")` holds a faithful copy of
// data whose system of record is somewhere else. Changes to it are made
// AT THE ORIGIN. A write here would produce a row MemQL believes and the
// origin has never heard of -- and the next reconciliation sweep would
// silently revert it, so the writer's change is lost and nobody is told.
//
// This guard is what turns that from a convention into a property. It is
// the reason a reader may trust the badge: "Mirror of shopify" means
// nobody in MemQL has edited this, because nobody in MemQL CAN.
//
// # It is deliberately stricter than the row-authz write guard
//
// rowauthz_write_guard.go enumerates exactly two escapes -- internal
// origin, and cluster owner -- and argues each. NEITHER APPLIES HERE,
// and that difference is the design rather than an oversight:
//
//   - INTERNAL ORIGIN says "trusted server-side Go is doing this". True
//     of the connector, and true of every other engine path as well. It
//     answers "is the engine writing" when the question is "is the
//     SHOPIFY CONNECTOR writing", so admitting it would let any internal
//     writer edit any mirror and the badge would mean nothing.
//   - CLUSTER OWNER is the operator of the deployment, and on the owned
//     tier refusing them would make administration impossible. Here it
//     would not help them: an operator's edit to a mirror is reverted by
//     the next reconcile exactly like anyone else's. The way an operator
//     changes mirrored data is to change it at the origin, or to MOVE
//     the origin (the runbook in docs/public/concepts/data-origins.md).
//     Refusing is the honest answer; accepting would be a write that
//     appears to work and does not last.
//
// The one writer admitted is the connector the concept's own @origin
// names, acting under auth.ConnectorActor -- an actor no request can
// mint (component/auth/connector_actor.go).
//
// # Where it sits, and what that reaches
//
// executeWrite is the single mutation write chokepoint (memql#1709), so
// placing the guard there covers insert(), update(), the soft-delete /
// status-flip class, raw client-supplied inserts, tool handlers and
// staged writes with one check. It runs BEFORE the payload is parsed and
// before the read-merge round-trip, because the refusal is a property of
// the CONCEPT and nothing the caller sent can change it -- the same
// argument and the same position as the retired-concept refusal it sits
// beside.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// AuditActionMirrorWriteRefused is the audit action stamped when a
// write to a mirror is refused. Lower_snake_case to match the
// v1:identity:auditEvent.action convention.
//
// It is also the machine-readable token the refusal message carries, so
// an operator grepping logs and an operator reading the audit trail are
// looking for the same string.
const AuditActionMirrorWriteRefused = "mirror_write_refused"

// ErrMirrorWriteRefused is the sentinel every mirror refusal wraps, so
// a caller can tell "this concept is read-only here" from "this write
// failed" without matching on a message.
var ErrMirrorWriteRefused = errors.New(AuditActionMirrorWriteRefused)

// MirrorWriteRefusal is the typed refusal. It names the origin, because
// the only useful thing to tell the caller is WHERE the change has to be
// made instead.
type MirrorWriteRefusal struct {
	Concept string
	Origin  string
	// Actor is the caller's identity as the engine resolved it, or
	// "(no caller identity)". Carried for the audit line, not for the
	// message -- telling a caller who they are is not information.
	Actor string
}

func (e *MirrorWriteRefusal) Error() string {
	return fmt.Sprintf(
		"%s{%s}: %s is a MIRROR of %q -- MemQL holds a faithful copy and does not own it, so it is read-only here by construction. "+
			"Change the record at %s; MemQL's copy follows. Only the %q connector writes this concept, and it does so under its own actor. "+
			"If MemQL is meant to own this data, that is a change of origin, not a write (docs/public/concepts/data-origins.md)",
		AuditActionMirrorWriteRefused, e.Origin, e.Concept, e.Origin, e.Origin, e.Origin)
}

func (e *MirrorWriteRefusal) Unwrap() error { return ErrMirrorWriteRefused }

// IsMirrorWriteRefused reports whether err is a mirror refusal.
func IsMirrorWriteRefused(err error) bool { return errors.Is(err, ErrMirrorWriteRefused) }

// guardMirrorWrite refuses a write to a mirror concept from anyone but
// the connector its origin names.
//
// Returns nil for every concept that is not a mirror, which is all but a
// handful: native and origin concepts are written normally, and the
// outbox is what carries an origin concept's changes outward.
func (e *MemQLEngine) guardMirrorWrite(ctx context.Context, conceptName string) error {
	conceptName = strings.TrimSpace(conceptName)
	if conceptName == "" {
		return nil
	}
	c, err := memorynodes.Get(conceptName)
	if err != nil || c == nil {
		// A concept the registry cannot produce has declared nothing.
		// executeWrite has already resolved it through the registry by
		// the time this runs, so this is a defensive answer rather than
		// a reachable one -- and "declared nothing" is the correct
		// answer to give for a concept that cannot say otherwise.
		return nil
	}
	if !c.IsMirror() {
		return nil
	}
	origin := c.EffectiveOrigin()

	if name, ok := auth.ConnectorFromContext(ctx); ok && name == origin {
		return nil
	}

	refusal := &MirrorWriteRefusal{
		Concept: conceptName,
		Origin:  origin,
		Actor:   mirrorWriteActorDescription(ctx),
	}
	e.auditMirrorWriteRefusal(ctx, refusal)
	return refusal
}

// mirrorWriteActorDescription renders the caller for the audit line.
//
// A connector that is not this concept's origin is named as a connector,
// because "the quickBooks connector tried to write a shopify mirror" is a
// materially different operational event from "a user did" -- the first
// is a misconfigured domain list, the second is someone reaching for the
// wrong surface.
func mirrorWriteActorDescription(ctx context.Context) string {
	if name, ok := auth.ConnectorFromContext(ctx); ok {
		return "connector:" + name
	}
	if id := strings.TrimSpace(rowAuthzActorUserId(ctx)); id != "" {
		return id
	}
	if auth.OriginFromContext(ctx).IsInternal() {
		// Internal origin is NOT an escape here (see the file header),
		// but it is worth distinguishing in the trail: a refusal against
		// internal Go is a bug in a server-side writer, not an attempt.
		return "(internal server-side call, no actor)"
	}
	return "(no caller identity)"
}

// auditMirrorWriteRefusal records the refusal on the governance trail.
//
// It reuses the authored-audit sink, which the app already adapts onto
// v1:identity:auditEvent (app/engine_authored.go). The type's name is
// about its FIRST caller, not its contract -- action, actor, detail and
// a timestamp is what an audit row is, and standing up a second sink
// would mean a second wiring in every binary for a strictly identical
// payload.
//
// A nil sink is normal: not every binary wires audit, and the refusal
// itself is still returned to the caller. An audit that cannot be
// written must never turn a refusal into an acceptance, so this returns
// nothing and the caller does not branch on it.
func (e *MemQLEngine) auditMirrorWriteRefusal(ctx context.Context, refusal *MirrorWriteRefusal) {
	if e == nil || refusal == nil {
		return
	}
	sink := e.authoredAuditSink()
	if sink == nil {
		return
	}
	sink.EmitAuthoredAudit(ctx, AuthoredAuditEvent{
		Action:      AuditActionMirrorWriteRefused,
		OwnerUserId: strings.TrimSpace(rowAuthzActorUserId(ctx)),
		Detail: map[string]any{
			"concept": refusal.Concept,
			"origin":  refusal.Origin,
			"actor":   refusal.Actor,
		},
		OccurredAt: time.Now().UTC(),
	})
}
