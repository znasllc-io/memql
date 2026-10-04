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

// outboundSecretRowFields is every field of an outbound row, in the order a
// refusal names the first one a write changes: where the row is sent, what it
// sends and in whose name, then its delivery state. On a row naming a secret
// none of them is a client's to write (memql#5480).
var outboundSecretRowFields = []string{
	"targetSecret",
	"medium",
	"target",
	"subject",
	"body",
	"dedupeKey",
	"requestedBy",
	"status",
	"attempts",
	"lastError",
	"nextAttemptAt",
	"sentAt",
}

// validateOutboundSecretTargetWrite refuses every write without internal
// origin to a v1:platform:outboundRequest whose stored or final version names
// a globalSecret as its webhook target (memql#5480).
//
// A Discord webhook's URL carries its token, so such a row names the secret
// holding the URL instead of carrying it, and the worker resolves the value
// and POSTs the row's body there. Whoever can write the row chooses what is
// posted to a channel a cluster owner configured, and whoever can write its
// status decides what the pipelines notify stage reports: the stage says
// "delivered" when the row it staged reads `sent`. So a secret row is
// server-written end to end -- staged by stageOutboundRequestToSecret and
// stamped by the outbound worker, both under internal origin. @serverOnly bars
// only the named stage; the raw insert(...) literal never consults it (the
// memql#5623 pattern), and the concept declares no row tier (memql#5804), so
// the row-authz write guard passes any signed-in caller. Probed against a real
// database, a client insert naming a secret landed, a re-version of a staged
// secret row under a foreign body landed, and a client stamp marking one sent
// landed.
//
// Keyed on ORIGIN, like validateConstructLadderServerOnly, and on the row:
//
//   - targetSecret may not change without internal origin: naming a secret,
//     renaming it and clearing it are all server-side Go's.
//   - a row whose stored or final version names a secret takes no write
//     without internal origin -- not its content, not its delivery state, not
//     even an unchanged rewrite. An operator who wants a failed one sent again
//     re-runs the notify step, which stages a fresh attempt.
//
// A plain row, naming no secret on either side, is not this guard's: its
// delivery state stays as open as the missing tier leaves it. Changes are
// judged between the stored row (nil on a create, where a field that is set
// is a change) and the row about to be written; absent, JSON null and the
// empty string are one unset value, as in the language's `==`.
func validateOutboundSecretTargetWrite(ctx context.Context, prior, final map[string]any) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	if payloadFieldChanged(prior, final, outboundTargetSecretField) {
		return errOutboundSecretTargetWrite(outboundTargetSecretField)
	}
	if !outboundRowNamesSecret(prior) && !outboundRowNamesSecret(final) {
		return nil
	}
	for _, field := range outboundSecretRowFields {
		if payloadFieldChanged(prior, final, field) {
			return errOutboundSecretTargetWrite(field)
		}
	}
	return errOutboundSecretTargetWrite("")
}

// errOutboundSecretTargetWrite names the first field the refused write
// changed, or none when it changed nothing.
func errOutboundSecretTargetWrite(field string) error {
	what := "write refused"
	if field != "" {
		what = "write to `" + field + "` refused"
	}
	return fmt.Errorf(
		"%s: %s -- this row names a globalSecret as its webhook target (targetSecret), and such a row "+
			"is written only by server-side Go under internal origin: staged through "+
			"stageOutboundRequestToSecret, its delivery state stamped by the outbound worker. A raw "+
			"insert(), a re-stage or a status stamp from anywhere else would choose what is posted to a "+
			"channel a cluster owner configured, or fake a delivery the pipelines notify stage then "+
			"reports. To send a failed one again, re-run the notify step. See "+
			"component/memql/outbound_secret_target_write_guard.go and memql#5480.",
		conceptPlatformOutboundRequest, what,
	)
}

// outboundRowNamesSecret reports whether an outbound row's webhook target is a
// globalSecret.
func outboundRowNamesSecret(row map[string]any) bool {
	s, _ := row[outboundTargetSecretField].(string)
	return strings.TrimSpace(s) != ""
}
