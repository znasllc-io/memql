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

// The two fields that make an outbound row PROTECTED (memql#5480).
// targetSecret names the v1:platform:globalSecret whose value is the URL a
// webhook row is POSTed to; serverStaged says server code staged the row,
// through a @serverOnly mutation, because a server reports its outcome.
const (
	outboundTargetSecretField = "targetSecret"
	outboundServerStagedField = "serverStaged"
)

// outboundProtectedRowFields is every field of an outbound row, in the order a
// refusal names the first one a write changes: what protects the row, where it
// is sent, what it sends and in whose name, then its delivery state. On a
// protected row none of them is a client's to write (memql#5480).
var outboundProtectedRowFields = []string{
	"targetSecret",
	"serverStaged",
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

// validateProtectedOutboundWrite refuses every write without internal origin
// to a PROTECTED v1:platform:outboundRequest: one whose stored or final version
// names a globalSecret as its webhook target, or says server code staged it
// (memql#5480).
//
// Both are server-written end to end, each for its own reason:
//
//   - A Discord webhook's URL carries its token, so a secret row names the
//     secret holding the URL instead of carrying it, and the worker POSTs the
//     row's body there. Whoever writes the row chooses what is posted to a
//     channel a cluster owner configured.
//   - The pipelines notify stage says a notification was delivered when the
//     row it staged reads `sent`, its email rows as much as its Discord ones.
//     Whoever writes a server-staged row's delivery state decides what the
//     stage reports, and whoever re-stages one at its id decides where it
//     goes and what it says.
//
// So such a row is staged by @serverOnly mutations (stageOutboundRequestToSecret,
// stageServerOutboundRequest) and stamped by the outbound worker, all under
// internal origin. @serverOnly bars only the named calls; the raw insert(...)
// literal never consults it (the memql#5623 pattern), and the concept declares
// no row tier (memql#5804), so the row-authz write guard passes any signed-in
// caller. Probed against a real database before this guard existed, a client
// insert naming a secret landed, a re-version of a staged secret row under a
// foreign body landed, and a client stamp marking one sent landed.
//
// Keyed on ORIGIN, like validateConstructLadderServerOnly, and on the row.
// Without internal origin:
//
//   - targetSecret and serverStaged may not change: naming a secret or
//     clearing one, and marking a row server-staged or unmarking it, are
//     server-side Go's -- so a raw insert() setting either is refused;
//   - a row whose stored or final version is protected takes no write -- not
//     its content, not its delivery state, not even an unchanged rewrite. An
//     operator who wants a failed one sent again re-runs the notify step,
//     which stages a fresh delivery.
//
// A row protected on neither side -- one a product or a client staged -- is
// not this guard's: its content and delivery state stay as open as the
// missing tier leaves them. Changes are judged between the stored row (nil on
// a create, where a field that is set is a change) and the row about to be
// written; absent, JSON null and the empty string are one unset value, as in
// the language's `==`.
func validateProtectedOutboundWrite(ctx context.Context, prior, final map[string]any) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	for _, field := range []string{outboundTargetSecretField, outboundServerStagedField} {
		if payloadFieldChanged(prior, final, field) {
			return errProtectedOutboundWrite(field)
		}
	}
	if !outboundRowProtected(prior) && !outboundRowProtected(final) {
		return nil
	}
	for _, field := range outboundProtectedRowFields {
		if payloadFieldChanged(prior, final, field) {
			return errProtectedOutboundWrite(field)
		}
	}
	return errProtectedOutboundWrite("")
}

// errProtectedOutboundWrite names the first field the refused write changed,
// or none when it changed nothing.
func errProtectedOutboundWrite(field string) error {
	what := "write refused"
	if field != "" {
		what = "write to `" + field + "` refused"
	}
	return fmt.Errorf(
		"%s: %s -- this row is server-written: server code staged it (serverStaged), or it names a "+
			"globalSecret as its webhook target (targetSecret), and such a row is written only by "+
			"server-side Go under internal origin: staged through stageServerOutboundRequest or "+
			"stageOutboundRequestToSecret, its delivery state stamped by the outbound worker. A raw "+
			"insert(), a re-stage or a status stamp from anywhere else would choose where the row goes "+
			"or what it says, or fake a delivery the pipelines notify stage then reports. To send a "+
			"failed one again, re-run the notify step. See "+
			"component/memql/outbound_protected_row_write_guard.go and memql#5480.",
		conceptPlatformOutboundRequest, what,
	)
}

// outboundRowProtected reports whether an outbound row is server-written: it
// names a globalSecret as its webhook target, or server code staged it.
func outboundRowProtected(row map[string]any) bool {
	s, _ := row[outboundTargetSecretField].(string)
	return strings.TrimSpace(s) != "" || outboundRowServerStaged(row)
}

// outboundRowServerStaged reads serverStaged, failing closed: a "true" written
// as a string is as much a claim that the row is the server's as the boolean.
func outboundRowServerStaged(row map[string]any) bool {
	switch v := row[outboundServerStagedField].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}
