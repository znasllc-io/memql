package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE CREATE HALF OF THE CLUSTER-OWNER TIER (memql#5624).
//
// The row-authz write guard judges a write against the row already stored at
// its target id, and a create has none, so on a @rowAuthz(clusterOwner)
// concept the guard answers nil (rowauthz_write_guard.go says so in as many
// words: "NOT creates"). The owned tier got its create-side answer in
// memql#3175 -- the server stamps the owner field on a raw insert() -- and
// this tier has no owner field to stamp. So a create was judged only by
// whatever its mutation declared, and the raw insert() literal declares
// nothing: it is public language, names no construct, and neither
// @requiresRank nor @serverOnly on the concept's mutations ever sees it.
//
// THE RULE. A create on a cluster-owner-tier concept is admitted for the
// tier's own audience and refused for everyone else:
//
//   - a cluster owner -- the write guard's second escape;
//   - trusted server-side Go stamping internal origin for this one write --
//     its first. Every @serverOnly creator, the seed materializer, the
//     registered tree's automations and the audit writers arrive this way;
//   - the connector the concept's @origin or @mirroredTo names
//     (connectorAdmission, epic memql#4378 D4). The sync runtime writes a
//     mirror under the connector's actor alone, and the mirror guard has
//     already refused everyone else by the time this runs.
//
// Everyone else is a principal the tier does not admit to an EXISTING row of
// the concept, so a create was the one write that handed them a row they could
// then neither read nor change -- and on some of these concepts the create
// alone is the harm. Connect Shopify found two (its design record, D13 and
// D15): a store row whose token reference names a secret its author does not
// own, which the edge then publishes to every visitor, and a pack row under a
// fresh id that overrides the pack's switch. It closed those two concepts with
// a two-entry table; this replaces the table with the tier.
//
// THE WAY IN FOR A LEGITIMATE NON-OWNER CREATOR is the existing one: a
// server-side writer that has already authorized its caller stamps internal
// origin for its one write (call_origin.go; which packages may stamp at all is
// TestOnlyAllowlistedPackagesStampInternalOrigin's). The sweep that preceded
// this rule found one such family, the audit writers, which recorded a
// caller's decision under the caller's own actor; they stamp now. No concept
// needed a declared exception, so none is offered: an exception table nothing
// uses is a door nobody reviews.
//
// WHY AT THE WRITE SEAM. executeWrite is where the named mutation, the raw
// literal, an inline or authored mutation's body and a tool handler converge,
// so one check covers them all, and a floor declared on each create mutation
// would be a second place for the rule to drift.
//
// The caller decides that the write is a create; a write onto an existing row
// is the write guard's to judge.
func refuseClusterOwnerTierCreate(ctx context.Context, conceptName string) error {
	decl := rowAuthzDeclFor(conceptName)
	if decl == nil || decl.Tier != langparser.RowAuthzClusterOwner {
		return nil
	}
	if _, escaped := rowAuthzWriteEscapeFor(ctx, decl); escaped {
		return nil
	}
	if admitted, isConnector := connectorAdmission(ctx, conceptName); isConnector && admitted {
		return nil
	}
	holds := "holds no role"
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil && strings.TrimSpace(string(ac.Role)) != "" {
		holds = fmt.Sprintf("holds %q", string(ac.Role))
	}
	return fmt.Errorf(
		"row-authz: creating a %s row is refused. The concept declares %s, and a create has no "+
			"stored row for the write guard to judge, so the engine judges it here: a create needs "+
			"the %q role, trusted server-side code stamping internal origin for the write, or the "+
			"connector the concept names. This caller %s (memql#5624)",
		conceptName, rowAuthzTierDescription(decl), string(auth.RoleOwner), holds)
}
