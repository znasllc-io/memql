package vendorkeys

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

// audit.go -- the record of every act on a key (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// safety rule S7).
//
// Every add, removal, rename, change of default, Auto switch and change of
// lending writes one v1:identity:auditEvent, and so does a key going from good
// to bad and coming back (observe.go). The event names the key by ID, the
// vendor, and who did it. It never carries the key's value, its fingerprint or
// its NAME: a name is its owner's word for a credential, the log outlives the
// key and is read by more people than the row is, and the id is enough to find
// the row for anybody who may read it.

// The audit vocabulary for a person's acts. `vendorKey` is declared on
// v1:identity:auditEvent's targetType enum and on createAuditEvent's args.
const (
	auditAdded          = "vendor_key_added"
	auditRenamed        = "vendor_key_renamed"
	auditDefaultChanged = "vendor_key_default_changed"
	auditAutoChanged    = "vendor_key_auto_changed"
	auditRemoved        = "vendor_key_removed"

	// auditWriteTimeout bounds the one write that outlives its request.
	auditWriteTimeout = 5 * time.Second
)

// audit writes the event for an act that landed. It does not fail the act: the
// key has changed, and saying it did not would be the lie. A missing event is
// logged with the action and the id.
func (i *Integration) audit(ctx context.Context, by caller, action, keyId string, detail map[string]any) {
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()
	if err := i.store.writePersonAudit(detached, by, action, keyId, detail); err != nil {
		i.logger.Warn("vendorkeys: the audit event for an API key did not land", "action", action, keySubject(keyId), "reason", failureReason(err))
	}
}

// writePersonAudit writes the event on the caller's own context, with internal
// origin stamped for this one write: the audit log's create admits a cluster
// owner or server code, and a developer adding a key is neither. It runs as
// the caller and not as the cluster, because this row is the record of who
// asked, and its createdBy should say what its actorUserId says.
func (s *Store) writePersonAudit(ctx context.Context, by caller, action, keyId string, detail map[string]any) error {
	call, err := langparser.RenderCall("createAuditEvent", map[string]any{
		"eventId":     "vendor-key-" + id.NewShortId(),
		"occurredAt":  s.now().UTC().Format(time.RFC3339Nano),
		"category":    "configuration",
		"action":      action,
		"actorUserId": by.userId,
		"actorEmail":  by.email,
		"actorRole":   by.role,
		"targetType":  auditTargetType,
		"targetId":    keyId,
		"outcome":     "success",
		"detail":      detail,
	})
	return s.write(auth.ContextWithInternalOrigin(ctx), "the audit event", call, err)
}
