package vendorkeys

import (
	"context"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// observe.go -- what a real call learned about a key, written where every
// replica reads it (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K1, K11 and K13, safety rule S7, mesh rule M3).
//
// Every call a key pays for is evidence about it. A served call says the
// vendor accepts it; a 401 says it does not; a 402 or a spend limit says its
// account is out of credit. The request middleware classifies the answer
// (component/memql/vendor_account_middleware.go) and reports it here, from
// whichever replica made the call.
//
// # One writer at a time, and the newest evidence wins
//
// A write is a read-merge of the whole row, and verdicts for one key arrive
// from several replicas at once. So every write of a state row happens under a
// write gate keyed on the key (memql.AcquireWriteGate), after a fresh read made
// while holding it. Without the gate, a replica that read "good" a moment
// before another wrote "refused" would write its merge of the stale row and
// put the key back in service.
//
// `evidenceAt` is the time of the evidence behind `state`: when the call that
// produced it STARTED. A verdict with evidence no newer than the stored one is
// dropped. That is what makes a long call harmless: it can start before a key
// is revoked and finish, successfully, after the refusal was recorded, and its
// late good verdict carries the earlier time.
//
// # What each verdict writes
//
//   - good: state good, with goodAt and lastUsedAt. A good that changes nothing
//     but those stamps is written at most once an hour, so a busy key costs one
//     write an hour and not one a call.
//   - refused: state refused, with the vendor's words. The key is passed over
//     until a check or a later served call says otherwise.
//   - credit: state credit, held for one hour from the evidence. After that
//     the next real call may try the key again.
//
// `heldUntil` and `detail` are never cleared: both are read only under the
// state they belong to, a datetime cannot be cleared, and a stale value beside
// `good` decides nothing.
//
// # A fall and a recovery are audited
//
// When the write takes a key from good to refused or out of credit, or back to
// good, one audit event is written beside it (S7). Bad to bad is not a second
// fall. The event names the key by id and never by label or fingerprint.
//
// # A node that cannot coordinate writes nothing
//
// With no gate pool this node skips the write and says so at debug. The
// verdict is not lost: the router has already moved the call to another
// account on the vendor's answer, and the next call that key pays for on a
// node that can write will record the same thing.

const (
	// goodStampEvery is how often a served call re-stamps a key that is
	// already good.
	goodStampEvery = time.Hour

	// stateGatePrefix keys the write gate of one key's state row.
	stateGatePrefix = "vendorKeyState:"

	// The audit vocabulary. `vendorKey` is declared on v1:identity:auditEvent's
	// targetType enum and on createAuditEvent's args.
	auditTargetType = "vendorKey"
	auditWentBad    = "vendor_key_went_bad"
	auditRecovered  = "vendor_key_recovered"

	// maxDetail bounds the vendor's words kept on the row, as the mutation does.
	maxDetail = 300
)

// Observe implements memql.VendorKeys.
func (r *Resolver) Observe(ctx context.Context, a memql.VendorAccount, verdict memql.VendorKeyVerdict, at time.Time, detail string) {
	keyId := memql.BareShortId(a.KeyId)
	if a.Kind != memql.VendorAccountKey || keyId == "" || at.IsZero() {
		return
	}
	if err := r.store.observe(ctx, keyId, verdict, at.UTC(), detail, false); err != nil {
		r.store.logger.Warn("vendorkeys: what a call learned about an API key was not recorded",
			"keyId", keyId, "verdict", string(verdict), "error", err)
	}
}

// observe records one verdict under the key's state gate.
//
// checked says the verdict came from a CHECK (the models were listed) rather
// than from a call the key paid for. A check stamps checkedAt and is always
// written, since being asked is what a person wanted recorded; it is not a
// use, so it leaves lastUsedAt alone.
func (s *Store) observe(ctx context.Context, keyId string, verdict memql.VendorKeyVerdict, at time.Time, detail string, checked bool) error {
	at = at.UTC()
	release, ok, err := s.lockState(ctx, keyId)
	if err != nil {
		return err
	}
	if !ok {
		s.logger.Debug("vendorkeys: this node has no gate pool; a verdict about an API key is not recorded here", "keyId", keyId)
		return nil
	}
	defer release()

	state, found, err := s.state(ctx, keyId)
	if err != nil {
		return err
	}
	if !found {
		// No record to move. A key is created with its state; a verdict about
		// an id with neither is about nothing this cluster holds.
		return nil
	}
	if !at.After(state.EvidenceAt) {
		return nil
	}
	// Nanoseconds, because evidence is compared: two verdicts inside one
	// second must still be ordered.
	stamp := at.Format(time.RFC3339Nano)
	fields := map[string]any{"evidenceAt": stamp}
	if checked {
		fields["checkedAt"] = stamp
	} else {
		fields["lastUsedAt"] = stamp
	}
	switch verdict {
	case memql.VendorKeyGood:
		if !checked && state.State == StateGood && at.Sub(state.GoodAt) < goodStampEvery {
			return nil
		}
		fields["state"], fields["goodAt"] = StateGood, stamp
	case memql.VendorKeyRefused:
		fields["state"], fields["detail"] = StateRefused, bounded(detail)
	case memql.VendorKeyCredit:
		fields["state"], fields["detail"] = StateCredit, bounded(detail)
		fields["heldUntil"] = at.Add(creditHold).Format(time.RFC3339Nano)
	default:
		return nil
	}
	if err := s.writeState(ctx, keyId, fields); err != nil {
		return err
	}
	next, _ := fields["state"].(string)
	if action := transition(state.State, next); action != "" {
		// The verdict is recorded; the audit event is the record of the fall
		// beside it. A failure to write it is reported and does not undo the
		// state, which is what stops the key being spent.
		if err := s.writeAudit(ctx, action, keyId, map[string]any{"vendor": state.Vendor, "state": next, "was": state.State}); err != nil {
			s.logger.Warn("vendorkeys: the audit event for an API key's change of state was not written",
				"keyId", keyId, "action", action, "error", err)
		}
	}
	return nil
}

// transition names the audit action for a change of state, or "" when the
// change is neither a fall nor a recovery.
func transition(was, now string) string {
	bad := func(s string) bool { return s == StateRefused || s == StateCredit }
	switch {
	case !bad(was) && bad(now):
		return auditWentBad
	case bad(was) && now == StateGood:
		return auditRecovered
	}
	return ""
}

// lockState takes one key's state gate. ok is false when this node has no gate
// pool, which is a node that cannot coordinate and so must not write.
func (s *Store) lockState(ctx context.Context, keyId string) (release func(), ok bool, err error) {
	if s.gate == nil {
		return nil, false, nil
	}
	pool := s.gate()
	if pool == nil {
		return nil, false, nil
	}
	release, err = memql.AcquireWriteGate(ctx, pool, stateGatePrefix+keyId)
	if err != nil {
		return nil, false, scrubbed(err)
	}
	return release, true, nil
}

// writeAudit writes one v1:identity:auditEvent about a key, as the cluster
// acting: the engine observed the change, and no person made it.
func (s *Store) writeAudit(ctx context.Context, action, keyId string, detail map[string]any) error {
	call, err := langparser.RenderCall("createAuditEvent", map[string]any{
		"eventId":    "vendor-key-" + id.NewShortId(),
		"occurredAt": s.now().UTC().Format(time.RFC3339Nano),
		"category":   "configuration",
		"action":     action,
		"actorRole":  "system",
		"targetType": auditTargetType,
		"targetId":   keyId,
		"outcome":    "success",
		"detail":     detail,
	})
	return s.write(asCluster(ctx), "the audit event", call, err)
}

func bounded(detail string) string {
	if r := []rune(detail); len(r) > maxDetail {
		return string(r[:maxDetail])
	}
	return detail
}
