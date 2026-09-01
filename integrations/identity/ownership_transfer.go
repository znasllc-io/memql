package identity

// OWNERSHIP TRANSFER (memql#4838, settling epic memql#4832's O1).
//
// D3 makes peer rows read-only at every rung, OWNER-TO-OWNER INCLUDED. That is
// the decision, and this is the hole it opens: no human can repair another
// human's rows, so an owner who leaves the company leaves rows no living
// principal can edit, permanently. The previous behaviour -- any cluster owner
// could write any row -- is what made that a non-problem.
//
// WHY TRANSFER AND NOT A WRITE ESCAPE. A break-glass escape is available to
// EVERYONE WHO CAN REACH IT, which is the whole cluster-owner tier, and it
// would hollow out D3 on the day it shipped. Transfer is narrow, names a new
// owner, and leaves the rank rules untouched: the row simply has a different
// owner afterward and every existing predicate keeps its meaning. This is the
// argument component/auth/maintenance_actor.go already makes for its own case,
// quoted there from the campaigns worker -- "a bypass is available to every
// caller that can reach it, whereas an identity is only as powerful as the
// queries it is used for".
//
// THE FOUR THINGS memql#4838 LEFT TO DESIGN, AND WHAT THEY ARE:
//
//  1. WHO MAY INVOKE IT -- a cluster owner. "A rank above both parties" has no
//     answer when both parties are owners, which is the case that motivates
//     the feature; requiring it would make the feature undefined exactly where
//     it is needed. The cluster owner is already the operator of the
//     deployment, and the action is audited.
//
//  2. PER-ROW OR PER-USER -- BOTH, because they are different jobs and the
//     issue says so: offboarding is per-user, a single stuck row is not.
//     `rowId` narrows to one row, `concept` to one concept, neither to
//     everything the departing principal owns. There is no "transfer
//     everything in the cluster" form: the argument naming the FROM principal
//     is always required, so a transfer always has a subject.
//
//  3. WHAT IT AUDITS -- v1:identity:auditEvent, the DECISIONS log, not
//     authActivity. This changes who may WRITE a set of rows, which is a
//     decision by any reading; authActivity is for routine mechanics two
//     orders of magnitude more numerous.
//
//  4. WHEN IT IS REFUSED -- when it would orphan a concept's own invariants.
//     A row with an EMPTY owner is CLUSTER-OWNED and is skipped rather than
//     transferred: `v1:accounts:account:self` has no meaningful second owner,
//     and handing the deployment's own company to a person is not a transfer,
//     it is a downgrade. A transfer INTO a principal who does not exist is
//     refused outright, because it recreates the exact problem it is here to
//     solve -- and does so silently, since the rows would still be there and
//     still unwritable.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// transferResult is what one transfer reports back.
type transferResult struct {
	Concept string `json:"concept"`
	RowId   string `json:"rowId"`
}

// handleTransferRowOwnership reassigns the declared owner field of every row
// one principal owns to another principal.
func (i *IdentityIntegration) handleTransferRowOwnership(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	from := strings.TrimSpace(stringArg(args, "fromUserId"))
	to := strings.TrimSpace(stringArg(args, "toUserId"))
	onlyConcept := strings.TrimSpace(stringArg(args, "concept"))
	onlyRow := strings.TrimSpace(stringArg(args, "rowId"))

	if from == "" || to == "" {
		return nil, fmt.Errorf("identity.transferRowOwnership requires fromUserId and toUserId")
	}
	if sameOwnerId(from, to) {
		// Not a no-op worth performing quietly: an operator who typed the
		// same id twice believes they moved something, and an audit row
		// saying a transfer happened would agree with them.
		return nil, fmt.Errorf(
			"identity.transferRowOwnership: fromUserId and toUserId name the same principal (%q). "+
				"Nothing would move, and an audit entry saying otherwise is worse than a refusal", from)
	}

	// WHO MAY INVOKE IT. Read through the access envelope rather than a role
	// string comparison, so this asks the same question IsClusterOwner answers
	// everywhere else.
	access, _ := componentAuth.AccessFromContext(ctx)
	if access == nil || !access.IsClusterOwner() {
		return nil, fmt.Errorf(
			"identity.transferRowOwnership is a cluster-owner action. It rewrites who may write " +
				"another principal's rows, and 'a rank above both parties' has no answer when both " +
				"are owners -- which is the case this exists for")
	}

	if i.engine == nil || i.db == nil {
		return nil, fmt.Errorf("identity.transferRowOwnership needs the engine and a database; this node has neither")
	}

	// THE DESTINATION MUST EXIST. Transferring into a principal who is not
	// there recreates the problem this closes, and does it silently: the rows
	// are still present and still unwritable, with an audit trail saying they
	// were handed over.
	if err := i.assertPrincipalExists(ctx, to); err != nil {
		return nil, err
	}

	moved := make([]transferResult, 0)
	for _, conceptName := range transferableConcepts(onlyConcept) {
		decl := conceptRowAuthz(conceptName)
		if decl == nil {
			continue
		}
		field := strings.TrimSpace(decl.Owner)
		rows, err := i.rowsOwnedBy(ctx, conceptName, field, from, onlyRow)
		if err != nil {
			return nil, err
		}
		for _, rowId := range rows {
			if err := i.reassignRow(ctx, conceptName, rowId, field, to); err != nil {
				return nil, fmt.Errorf("identity.transferRowOwnership: %s %s: %w", conceptName, rowId, err)
			}
			moved = append(moved, transferResult{Concept: conceptName, RowId: rowId})
		}
	}

	if err := i.auditTransfer(ctx, access, from, to, moved); err != nil {
		// The transfer HAPPENED. Failing the call now would tell the operator
		// it did not, which is a worse lie than a missing audit row -- so this
		// surfaces the audit failure without retracting the fact.
		return nil, fmt.Errorf(
			"identity.transferRowOwnership moved %d row(s) from %q to %q, but the audit entry "+
				"could not be written: %w. The transfer STOOD; record it by hand",
			len(moved), from, to, err)
	}

	payload, _ := json.Marshal(map[string]any{
		"fromUserId": from,
		"toUserId":   to,
		"moved":      len(moved),
		"rows":       moved,
	})
	return []memorynodes.MemoryNode{{
		ID:      "transfer:" + from + ":" + to,
		Concept: "integration:identity:ownershipTransfer",
		Payload: payload,
	}}, nil
}

// transferableConcepts lists the concepts whose rows carry a transferable
// owner, narrowed to one when the caller named it.
//
// A concept qualifies when it declares the OWNED tier over a named field. The
// self-owned form (`owner="id"`) is excluded: its "owner field" is the row's
// own id, so a transfer would mean renaming the row, which is a different and
// much worse operation than changing who owns it.
func transferableConcepts(only string) []string {
	names := make([]string, 0)
	for name := range memorynodes.All() {
		if only != "" && name != only {
			continue
		}
		decl := conceptRowAuthz(name)
		if decl == nil || decl.Tier != langparser.RowAuthzOwned {
			continue
		}
		field := strings.TrimSpace(decl.Owner)
		if field == "" || field == langparser.RowAuthzSelfOwnedField {
			continue
		}
		names = append(names, name)
	}
	// Sorted so a partial failure reports the same progress every time, and
	// so a transfer is replayable in the same order.
	sort.Strings(names)
	return names
}

func sameOwnerId(a, b string) bool {
	return strings.TrimSpace(a) == strings.TrimSpace(b) ||
		bareTail(a) != "" && bareTail(a) == bareTail(b)
}

// bareTail strips a canonical `{concept}:{shortId}` prefix, matching how the
// row gate compares two id spellings.
func bareTail(id string) string {
	id = strings.TrimSpace(id)
	if i := strings.LastIndex(id, ":"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// rowsOwnedBy lists the ids of a concept's LATEST rows whose declared owner
// field names the departing principal.
//
// Latest-version only, and that is not an optimisation: MemQL rows are
// append-only, so an id can have a version owned by the subject and a newer
// one owned by somebody else. Transferring on the strength of a superseded
// version would hand away a row that had already moved on.
// staged-data: MUST-NOT-GATE -- a staged row skipped here is left owned by the
// departing principal, which is precisely the permanently-unwritable row this
// whole capability exists to prevent (memql#4838). It would be skipped
// silently, the transfer would report success, and the row would surface as
// unwritable later with an audit trail saying it had been handed over.
// Ownership is orthogonal to publication: who may write a row is not a
// question about whether it is visible yet.
func (i *IdentityIntegration) rowsOwnedBy(ctx context.Context, conceptName, field, owner, onlyRow string) ([]string, error) {
	var nodes []memorynodes.MemoryNode
	if err := i.db().NewSelect().
		Model(&nodes).
		Where("concept = ?", conceptName).
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("reading %s: %w", conceptName, err)
	}
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for idx := range nodes {
		id := strings.TrimSpace(nodes[idx].ID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if onlyRow != "" && id != onlyRow {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(nodes[idx].Payload, &payload); err != nil {
			continue
		}
		stored := strings.TrimSpace(fmt.Sprint(payload[field]))
		if stored == "" || stored == "<nil>" {
			// CLUSTER-OWNED, and skipped rather than transferred. The `self`
			// account is the case: handing the deployment's own row to a
			// person is not a transfer, it is a downgrade, and there is no
			// second owner for a singleton to have.
			continue
		}
		if !sameOwnerId(stored, owner) {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// reassignRow writes a new version of one row carrying the new owner.
//
// A raw `insert(` onto an id that already has a row is an UPDATE in this
// engine -- MemQL has no hard delete and no replace; every write is an append
// onto the same id -- and executeWrite read-merges, so naming only the owner
// field leaves every other value exactly as it was. That merge is what makes
// this safe to run over an arbitrary concept whose fields this code has never
// heard of.
//
// INTERNAL ORIGIN is stamped for this one write, inline, as the argument to
// the one Execute that needs it. That is the shape the escape set requires and
// the reason it is scoped: stamping the request's own context would open every
// @serverOnly construct and the write guard for the rest of the request, which
// was built and refuted in memql#2989.
func (i *IdentityIntegration) reassignRow(ctx context.Context, conceptName, rowId, field, to string) error {
	payload, err := json.Marshal(map[string]any{field: to})
	if err != nil {
		return err
	}
	query := fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString(conceptName),
		langparser.QuoteString(rowId),
		string(payload))
	_, err = i.engine.Execute(componentAuth.ContextWithInternalOrigin(ctx), query)
	return err
}

// assertPrincipalExists refuses a transfer into a principal the cluster does
// not have.
// staged-data: MUST-NOT-GATE -- gating this reports a staged principal as
// NONEXISTENT and refuses the transfer outright, with a message saying the
// destination user does not exist in a cluster where they plainly do. A false
// denial on the safety check of a recovery path, and the operator's only
// remaining move would be to guess that staging is the cause.
func (i *IdentityIntegration) assertPrincipalExists(ctx context.Context, userId string) error {
	var nodes []memorynodes.MemoryNode
	if err := i.db().NewSelect().
		Model(&nodes).
		Where("concept = ?", conceptIdentityUser).
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx); err != nil {
		return fmt.Errorf("identity.transferRowOwnership: cannot read the principal table to "+
			"confirm %q exists: %w", userId, err)
	}
	for idx := range nodes {
		if !sameOwnerId(nodes[idx].ID, userId) {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(nodes[idx].Payload, &payload); err == nil {
			if active, ok := payload["active"].(bool); ok && !active {
				return fmt.Errorf(
					"identity.transferRowOwnership: %q is deactivated. Transferring into a "+
						"principal who cannot sign in leaves the rows exactly as unwritable as "+
						"they are now, with an audit trail saying they were handed over", userId)
			}
		}
		return nil
	}
	return fmt.Errorf(
		"identity.transferRowOwnership: %q names no principal in this cluster. A transfer into "+
			"a user who does not exist recreates the problem it is here to close, silently -- the "+
			"rows would still be there and still unwritable", userId)
}

// auditTransfer records the decision on v1:identity:auditEvent.
//
// ONE ROW FOR THE WHOLE TRANSFER, not one per row moved. The decision was
// taken once, by one person, about one pair of principals; a row per moved row
// would bury that decision in its own consequences and make an offboarding
// indistinguishable from a flood.
func (i *IdentityIntegration) auditTransfer(ctx context.Context, access *componentAuth.AccessContext, from, to string, moved []transferResult) error {
	detail, err := json.Marshal(map[string]any{
		"fromUserId": from,
		"toUserId":   to,
		"rowCount":   len(moved),
		"rows":       moved,
	})
	if err != nil {
		return err
	}
	query := fmt.Sprintf(
		`mutation createAuditEvent(eventId:%s, occurredAt:%s, category:%s, action:%s, actorUserId:%s, actorEmail:%s, actorRole:%s, targetType:%s, targetId:%s, outcome:%s, detail:%s)`,
		langparser.QuoteString(auditEventId(from, to)),
		langparser.QuoteString(time.Now().UTC().Format(time.RFC3339Nano)),
		langparser.QuoteString("authorization"),
		langparser.QuoteString("row_ownership_transferred"),
		langparser.QuoteString(access.UserId),
		langparser.QuoteString(access.PrimaryEmail),
		langparser.QuoteString(string(access.Role)),
		langparser.QuoteString("rowOwnership"),
		langparser.QuoteString(to),
		langparser.QuoteString("success"),
		string(detail))
	// NO INTERNAL-ORIGIN STAMP HERE, deliberately. createAuditEvent is not
	// @serverOnly, and v1:identity:auditEvent declares
	// `@rowAuthz(owner="actorUserId", clusterOwner)` -- the actor recorded IS
	// this caller, who is a cluster owner by the check above, so the ordinary
	// write path admits it. Stamping anyway would widen a call that does not
	// need widening, which is how an escape becomes ambient.
	_, err = i.engine.Execute(ctx, query)
	return err
}

// conceptIdentityUser names the principal table. Written out rather than
// imported from component/memql, which this package does not depend on.
const conceptIdentityUser = "v1:identity:user"

// conceptRowAuthz reads a concept's declared tier from the registry.
func conceptRowAuthz(name string) *langparser.RowAuthzDecl {
	c, err := memorynodes.Get(strings.TrimSpace(name))
	if err != nil || c == nil {
		return nil
	}
	return c.RowAuthz
}

// stringArg reads a string capability argument, tolerating an absent key.
func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	s, _ := args[key].(string)
	return s
}

// auditEventId derives the audit row's id from the pair and the instant, so a
// second transfer between the same two principals appends rather than
// colliding with the first.
func auditEventId(from, to string) string {
	return fmt.Sprintf("v1:identity:auditEvent:transfer-%s-%s-%d",
		bareTail(from), bareTail(to), time.Now().UTC().UnixNano())
}
