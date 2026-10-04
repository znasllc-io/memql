package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// conceptPlatformOutboundRequest is the outbound delivery outbox. Mirrors
// dsl/platform/concepts.memql:outboundRequest.
const conceptPlatformOutboundRequest = "v1:platform:outboundRequest"

// outboundTargetSecretField names the v1:platform:globalSecret whose value is
// the URL a webhook row is POSTed to (memql#5480).
const outboundTargetSecretField = "targetSecret"

// outboundSecretRowContent is what a secret row SENDS, and where, and in whose
// name: the fields the outbound worker reads to make the POST, plus the
// provenance the notify stage stamped. The rest of the row is its delivery
// lifecycle (status, attempts, lastError, nextAttemptAt, sentAt), which the
// worker stamps under its own system actor, without internal origin, and which
// this guard therefore leaves open.
var outboundSecretRowContent = []string{
	"medium",
	"target",
	"subject",
	"body",
	"dedupeKey",
	"requestedBy",
}

// validateOutboundSecretTargetWrite refuses a write to a
// v1:platform:outboundRequest, made without internal origin, that names a
// globalSecret as its webhook target or changes what a row naming one sends
// (memql#5480).
//
// A Discord webhook's URL carries its token, so such a row names the secret
// holding the URL instead of carrying it, and the worker resolves the value
// and POSTs the row's body there. Whoever can write that row chooses what is
// posted to a channel a cluster owner configured. Its one writer is
// stageOutboundRequestToSecret, which is @serverOnly; but the raw insert(...)
// literal never consults a mutation's @serverOnly (the memql#5623 pattern),
// and the concept declares no row tier (memql#5804), so the row-authz write
// guard passes any signed-in caller. Probed against a real database, a client
// insert naming a secret landed, and so did a re-version of a staged secret
// row under a foreign body.
//
// Two rules, both keyed on a CHANGE between the stored row (prior is nil on a
// create, where a field that is set is a change) and the row about to be
// written. Absent, JSON null and the empty string are one unset value, as in
// the language's `==`:
//
//   - targetSecret itself may not change: naming a secret, renaming it and
//     clearing it are all server-side Go's.
//   - on a row whose stored or final version names a secret, the content may
//     not change either. Otherwise a caller keeps the secret through the
//     read-merge and replaces the body under it.
//
// Keyed on ORIGIN, like validateConstructLadderServerOnly: the notify stage
// writes through stageOutboundRequestToSecret with internal origin stamped,
// and no caller -- a cluster owner included -- has a reason to write a secret
// row any other way. The status stamps stay open because nothing in them
// chooses where a POST goes or what it says. What a caller can still do with
// them (requeue a sent row, or mark one sent) belongs to the missing tier.
func validateOutboundSecretTargetWrite(ctx context.Context, prior, final map[string]any) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	if constructFieldChanged(prior, final, outboundTargetSecretField) {
		return errOutboundSecretTargetWrite(outboundTargetSecretField)
	}
	if !outboundRowNamesSecret(prior) && !outboundRowNamesSecret(final) {
		return nil
	}
	for _, field := range outboundSecretRowContent {
		if constructFieldChanged(prior, final, field) {
			return errOutboundSecretTargetWrite(field)
		}
	}
	return nil
}

func errOutboundSecretTargetWrite(field string) error {
	return fmt.Errorf(
		"%s: write to `%s` refused -- a row naming a globalSecret as its webhook target (targetSecret) "+
			"is written only by server-side Go, through stageOutboundRequestToSecret under internal "+
			"origin, and the outbound worker POSTs its body to the URL that secret holds. A raw insert() "+
			"or a re-stage never consults that mutation's @serverOnly, so a write from anywhere else would "+
			"choose what is posted to a channel a cluster owner configured. The delivery status fields "+
			"stay open. See component/memql/outbound_secret_target_write_guard.go and memql#5480.",
		conceptPlatformOutboundRequest, field,
	)
}

// outboundRowNamesSecret reports whether an outbound row's webhook target is a
// globalSecret.
func outboundRowNamesSecret(row map[string]any) bool {
	s, _ := row[outboundTargetSecretField].(string)
	return strings.TrimSpace(s) != ""
}
