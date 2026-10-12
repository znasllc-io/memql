package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// constructMembershipFields are the fields of a v1:authoring:construct that
// decide WHAT an active bundle arms and where: which bundle the construct
// belongs to, what kind and name it registers under, its source, and the
// namespace and origin it resolves through. Everything else on the row -- its
// status, which follows the bundle and which the OS arms and retires a
// catalogued automation by; the catalog, ladder and reuse fields, each held by
// its own writers and guards; the compiled-form cache -- is not membership.
var constructMembershipFields = []string{
	"bundleId",
	"kind",
	"name",
	"source",
	"targetNamespace",
	"origin",
}

// bundleStatusesGatedPast are the bundle statuses whose members are frozen:
// the bundle has passed its dry run (Gate 2) or gone further. Before that --
// draft, validated (Gate 1 only), failed -- the gates have not yet seen the
// bundle as it will run, so a member may still be written.
var bundleStatusesGatedPast = map[BundleStatus]bool{
	BundleDryRunPassed: true,
	BundleActive:       true,
	BundlePaused:       true,
	BundleRetired:      true,
}

// validateConstructMembership refuses a write that adds a construct to a
// bundle, or changes what one arms, unless it is the server's write into a
// bundle the gates have not yet passed (memql#6022).
//
// An active bundle arms every construct it holds -- PlanBundleActivation
// registers each member, and every replica's reconcile pass and boot re-arm do
// the same -- so the members must be what the bundle's gates saw. Two ways
// round that were live: a construct added to an already-active bundle was
// armed by the next reconcile pass, and one created in advance under a rule's
// predictable next bundle id would ride along with the gated activation.
// createAuthoringConstruct is @serverOnly, but a raw insert(...) or
// update(...) never consults that, so the rule is held here, at the write
// chokepoint, beside the construct ladder's guard:
//
//   - a construct is CREATED, or one of constructMembershipFields CHANGES,
//     only under internal origin -- the pipelines that run the gates; and
//   - only while its bundle exists, belongs to the construct's owner, and has
//     not passed its dry run (bundleStatusesGatedPast). A construct moved
//     between bundles needs both bundles to qualify.
//
// prior is the stored row, nil on a create.
func (e *MemQLEngine) validateConstructMembership(ctx context.Context, prior, final map[string]any) error {
	what := ""
	if prior == nil {
		what = "adding a construct to a bundle"
	} else {
		for _, field := range constructMembershipFields {
			if payloadFieldChanged(prior, final, field) {
				what = "changing `" + field + "` on a construct"
				break
			}
		}
	}
	if what == "" {
		return nil
	}
	if !auth.OriginFromContext(ctx).IsInternal() {
		return errConstructMembership(what, "only the server pipelines that run a bundle's gates write its members, under internal origin")
	}
	bundles := []string{constructString(final, "bundleId")}
	if before := constructString(prior, "bundleId"); prior != nil && before != "" && BareShortId(before) != BareShortId(bundles[0]) {
		bundles = append(bundles, before)
	}
	owner := constructString(final, "ownerUserId")
	for _, bundleId := range bundles {
		status, bundleOwner, exists, err := e.bundleRowForGuard(ctx, bundleId)
		if err != nil {
			return fmt.Errorf("%s: read bundle %q to check %s: %w", memorynodes.ConceptAuthoringConstruct, bundleId, what, err)
		}
		switch {
		case !exists:
			return errConstructMembership(what, fmt.Sprintf("its bundle %q does not exist -- a construct joins a bundle that is already there, and only before its gates run", bundleId))
		case bundleStatusesGatedPast[BundleStatus(status)]:
			return errConstructMembership(what, fmt.Sprintf("its bundle %q reads %s -- the gates have seen its members, and an active bundle arms exactly what they saw", bundleId, status))
		case BareShortId(bundleOwner) != BareShortId(owner):
			return errConstructMembership(what, fmt.Sprintf("its bundle %q belongs to someone else", bundleId))
		}
	}
	return nil
}

func errConstructMembership(what, why string) error {
	return fmt.Errorf("%s: %s refused -- %s. See component/memql/authoring_construct_write_guard.go and memql#6022.",
		memorynodes.ConceptAuthoringConstruct, what, why)
}

// bundleRowForGuard reads a bundle's latest status and owner straight off the
// table: the guard needs the truth about the row, not what the writer may
// read of it.
func (e *MemQLEngine) bundleRowForGuard(ctx context.Context, bundleId string) (status, owner string, exists bool, err error) {
	db := e.database()
	if db == nil {
		return "", "", false, fmt.Errorf("the construct write guard requires a database")
	}
	id := strings.TrimSpace(bundleId)
	if id == "" {
		return "", "", false, nil
	}
	if !strings.HasPrefix(id, conceptAuthoringBundle+":") {
		id = conceptAuthoringBundle + ":" + id
	}
	var node memorynodes.MemoryNode
	err = db.NewSelect().Model(&node).
		Where("concept = ?", conceptAuthoringBundle).
		Where("id = ?", id).
		OrderExpr(`"createdAt" DESC`).
		Limit(1).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	var payload map[string]any
	if err := json.Unmarshal(node.Payload, &payload); err != nil {
		return "", "", false, fmt.Errorf("decode bundle %q: %w", id, err)
	}
	return constructString(payload, "status"), constructString(payload, "ownerUserId"), true, nil
}

func constructString(row map[string]any, key string) string {
	s, _ := row[key].(string)
	return strings.TrimSpace(s)
}
