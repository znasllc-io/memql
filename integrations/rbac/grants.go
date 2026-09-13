package rbac

// GRANT GOVERNANCE (app access grants record, section 3; epic memql#5297)
// and the effective read (section 3 "Reads"; epic memql#5298).
//
// Two builtins write a v1:rbac:grant -- `grantSet` (allow or deny, one row)
// and `grantRevoke` -- and one reads the caller's resolved set back. They are
// Go for roles.go's reason: every guard is relational or set-valued, and a
// mutation body can express none of them. `writeGrant` and `deactivateGrant`
// are @serverOnly precisely so this is the only way in.
//
// THE ORDER OF THE GUARDS IS PART OF THE CONTRACT, exactly as it is for the
// role builtins, and it is the record's order (decision D5):
//
//  1. The caller holds `update` on `principal`.
//  2. The caller holds the capability being granted -- resolved for the
//     caller THEMSELVES through auth.CapableFor, so a grant naming the caller
//     counts. Nobody hands out, or denies, an app they do not have.
//  3. The subject ranks no higher than the caller. A user subject ranks as
//     their role; a group subject ranks as its highest-ranked ACTIVE member,
//     so a developer cannot deny an app to a group that contains the owner.
//     An empty group ranks zero. An unresolvable rank DENIES.
//  4. Nobody grants to themselves, mirroring the membership rule that nobody
//     adds themselves to a group.
//
// Only the owner can therefore write a grant whose subject is an owner, and
// not for themselves, so no grant ever bars the cluster owner.
//
// Each refusal is a typed code the OS prints beside the control. The codes
// are the contract; the sentences are for the person reading them.
//
// # The reads run under a synthetic operator, the writes under the caller
//
// Resolving a subject's rank means reading the subject's user row (a
// @serverOnly read) and, for a group, its membership rows (an admin-floored
// read). Both are asked under a system actor, the groups integration's
// discipline: the question is "what rank does this subject hold", a fact
// about the cluster, and answering it from the caller's own visibility would
// let two admins get different answers about one person. The WRITE keeps the
// caller's identity and stamps internal origin, as writeRole does, so
// `createdBy` on the grant row is the person who decided rather than the
// engine. The AUDIT row is written under the caller's plain context: the actor
// recorded IS this caller, and the ordinary write path admits it.

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
	"github.com/znasllc-io/memql/component/memql"
)

// Refusal codes, the record's five plus the two every builtin family carries
// for a malformed call and a row that is not there.
const (
	codeGrantCallerNotPermitted    = "grant_caller_not_permitted"
	codeGrantCapabilityNotHeld     = "grant_capability_not_held"
	codeGrantSubjectOutranksCaller = "grant_subject_outranks_caller"
	codeGrantSelf                  = "grant_self"
	codeGrantUnknownSubject        = "grant_unknown_subject"
	codeGrantInvalid               = "grant_invalid"
	codeGrantNotFound              = "grant_not_found"
)

// The audit vocabulary. `grant` is declared on v1:identity:auditEvent's
// targetType enum and on createAuditEvent's args, which
// test/dslconformance/identity_audit_enum_contract_test.go keeps in step.
const (
	auditTargetGrant   = "grant"
	auditActionSet     = "grant_set"
	auditActionRevoked = "grant_revoked"
)

// systemGrantsActor is the synthetic operator the subject-rank reads run
// under. A stable id rather than the caller's, so a read of somebody's role
// is never attributed to the person who asked about them.
const systemGrantsActor = "system:rbac:grants"

// grantDecision is what the two write builtins return before it becomes a
// node: {ok, grantId, code, message}.
type grantDecision struct {
	ok      bool
	grantId string
	code    string
	detail  string
}

func refuseGrant(grantId, code, detail string) grantDecision {
	return grantDecision{grantId: grantId, code: code, detail: detail}
}

// grantSpec is one grant as the builtins read it off their arguments or off
// an existing row.
type grantSpec struct {
	kind     string
	subject  string // BARE
	verb     string
	resource string
	effect   string
}

// ---------------------------------------------------------------------
// grantSet
// ---------------------------------------------------------------------

func (i *Integration) handleGrantSet(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	access, _ := componentAuth.AccessFromContext(ctx)
	if access == nil {
		return grantNodes(refuseGrant("", codeGrantCallerNotPermitted, "this call carries no caller identity")), nil
	}
	spec, d := parseGrantSpec(args)
	if d.code != "" {
		return grantNodes(d), nil
	}
	if d := i.checkGrantAuthority(ctx, access, spec); d.code != "" {
		return grantNodes(d), nil
	}

	id := componentAuth.GrantRowID(spec.kind, spec.subject, spec.verb, spec.resource)
	if err := i.exec(componentAuth.ContextWithInternalOrigin(ctx), fmt.Sprintf(
		`mutation writeGrant(grantId: %s, subjectKind: %s, subjectId: %s, verb: %s, resourceType: %s, effect: %s, grantedBy: %s)`,
		langparser.QuoteString(id), langparser.QuoteString(spec.kind), langparser.QuoteString(spec.subject),
		langparser.QuoteString(spec.verb), langparser.QuoteString(spec.resource), langparser.QuoteString(spec.effect),
		langparser.QuoteString(memql.BareShortId(access.UserId)))); err != nil {
		return nil, err
	}
	i.auditGrant(ctx, access, auditActionSet, id, spec)
	return grantNodes(grantDecision{ok: true, grantId: id}), nil
}

// ---------------------------------------------------------------------
// grantRevoke
// ---------------------------------------------------------------------

func (i *Integration) handleGrantRevoke(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	access, _ := componentAuth.AccessFromContext(ctx)
	if access == nil {
		return grantNodes(refuseGrant("", codeGrantCallerNotPermitted, "this call carries no caller identity")), nil
	}
	id := strings.TrimSpace(stringArg(args, "grantId"))
	if id == "" {
		return grantNodes(refuseGrant("", codeGrantInvalid, "grantId is required")), nil
	}
	id = memql.BareShortId(id)

	// The caller's own authority is checked BEFORE the row is read, so a
	// refusal never confirms whether a grant id exists to somebody who may
	// not revoke one.
	callerSubject, _ := componentAuth.SubjectFromContext(ctx)
	if !componentAuth.CapableFor(ctx, callerSubject, componentAuth.VerbUpdate, componentAuth.ResourcePrincipal) {
		return grantNodes(refuseGrant(id, codeGrantCallerNotPermitted,
			"your role does not hold update on principal, which is what writing or revoking a grant requires")), nil
	}
	spec, found, err := i.readGrant(ctx, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return grantNodes(refuseGrant(id, codeGrantNotFound, "no active grant by that id")), nil
	}
	// The remaining three rules apply to a revoke exactly as to a set: taking
	// a deny off somebody is handing them the app, and lifting an allow is
	// barring them from it, so the same authority is needed either way.
	if d := i.checkGrantAuthority(ctx, access, spec); d.code != "" {
		d.grantId = id
		return grantNodes(d), nil
	}

	if err := i.exec(componentAuth.ContextWithInternalOrigin(ctx), fmt.Sprintf(
		`mutation deactivateGrant(grantId: %s)`, langparser.QuoteString(id))); err != nil {
		return nil, err
	}
	i.auditGrant(ctx, access, auditActionRevoked, id, spec)
	return grantNodes(grantDecision{ok: true, grantId: id}), nil
}

// ---------------------------------------------------------------------
// effectiveCapabilities
// ---------------------------------------------------------------------

// handleEffectiveCapabilities answers the CALLER'S resolved set and nobody
// else's: it takes no arguments, so there is no subject to name. Every
// signed-in person may call it -- the read the OS decides its desktop from --
// which is why it is not floored at admin.
func (i *Integration) handleEffectiveCapabilities(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	subject, ok := componentAuth.SubjectFromContext(ctx)
	if !ok {
		return effectiveNodes("", "", nil, codeGrantCallerNotPermitted), nil
	}
	decisions := componentAuth.EffectiveCapabilities(ctx, subject)
	return effectiveNodes(string(subject.Role), memql.BareShortId(subject.UserId), decisions, ""), nil
}

// ---------------------------------------------------------------------
// the guards, shared by set and revoke
// ---------------------------------------------------------------------

// checkGrantAuthority applies the record's four rules, in order, to one grant
// the caller wants to write or take back.
func (i *Integration) checkGrantAuthority(ctx context.Context, access *componentAuth.AccessContext, spec grantSpec) grantDecision {
	callerSubject, _ := componentAuth.SubjectFromContext(ctx)

	// 1. The caller holds `update` on `principal`. A builtin carries no
	//    @requiresCapability, so the floor is repeated here -- and it is a
	//    grant rather than a rank, because "may you decide who reaches what"
	//    is a permission, not a rung.
	if !componentAuth.CapableFor(ctx, callerSubject, componentAuth.VerbUpdate, componentAuth.ResourcePrincipal) {
		return refuseGrant("", codeGrantCallerNotPermitted,
			"your role does not hold update on principal, which is what writing or revoking a grant requires")
	}
	// 2. The caller holds what they are granting -- or denying.
	if !componentAuth.CapableFor(ctx, callerSubject, spec.verb, spec.resource) {
		return refuseGrant("", codeGrantCapabilityNotHeld, fmt.Sprintf(
			"you do not hold %s on %s yourself, so you cannot grant or deny it", spec.verb, spec.resource))
	}
	// 3. The subject ranks no higher than the caller.
	callerRank := componentAuth.RoleRank(componentAuth.Role(strings.ToLower(strings.TrimSpace(string(access.Role)))))
	if callerRank <= 0 {
		// Rank 0 is what an unknown or retired role resolves to. A caller
		// whose rank cannot be resolved cannot be compared to anybody, and
		// "cannot compare" denies (record section 3).
		return refuseGrant("", codeGrantSubjectOutranksCaller, "your own rank could not be resolved")
	}
	subjectRank, d := i.subjectRank(ctx, spec)
	if d.code != "" {
		return d
	}
	if subjectRank > callerRank {
		return refuseGrant("", codeGrantSubjectOutranksCaller, fmt.Sprintf(
			"the %s ranks above you (%d over your %d); a grant may only name somebody you outrank or equal",
			spec.kind, subjectRank, callerRank))
	}
	// 4. Not yourself.
	if spec.kind == componentAuth.SubjectKindUser && sameUser(spec.subject, access.UserId) {
		return refuseGrant("", codeGrantSelf, "you cannot write a grant naming yourself")
	}
	return grantDecision{}
}

// subjectRank resolves the rank rule 3 compares: a user's role rank, or a
// group's highest-ranked active member (zero for an empty group). A subject
// that does not exist, or a group that is not active, is `grant_unknown_subject`;
// a user whose role ranks 0 is unresolvable, and denies.
func (i *Integration) subjectRank(ctx context.Context, spec grantSpec) (int, grantDecision) {
	switch spec.kind {
	case componentAuth.SubjectKindUser:
		role, found, err := i.userRole(ctx, spec.subject)
		if err != nil {
			return 0, refuseGrant("", codeGrantUnknownSubject, "the user could not be read: "+err.Error())
		}
		if !found {
			return 0, refuseGrant("", codeGrantUnknownSubject, "no active user by that id")
		}
		rank := componentAuth.RoleRank(componentAuth.Role(role))
		if rank <= 0 {
			return 0, refuseGrant("", codeGrantSubjectOutranksCaller, fmt.Sprintf(
				"the user's role %q ranks nowhere on this cluster's ladder, so it cannot be compared to yours", role))
		}
		return rank, grantDecision{}
	case componentAuth.SubjectKindGroup:
		active, found, err := i.groupIsActive(ctx, spec.subject)
		if err != nil {
			return 0, refuseGrant("", codeGrantUnknownSubject, "the group could not be read: "+err.Error())
		}
		if !found || !active {
			return 0, refuseGrant("", codeGrantUnknownSubject, "no active group by that id")
		}
		members, err := i.groupMemberIds(ctx, spec.subject)
		if err != nil {
			return 0, refuseGrant("", codeGrantUnknownSubject, "the group's members could not be read: "+err.Error())
		}
		highest := 0
		for _, member := range members {
			role, found, err := i.userRole(ctx, member)
			if err != nil {
				return 0, refuseGrant("", codeGrantUnknownSubject, "a member could not be read: "+err.Error())
			}
			if !found {
				// A membership row pointing at a user who is gone or
				// deactivated places nobody; it does not rank the group.
				continue
			}
			rank := componentAuth.RoleRank(componentAuth.Role(role))
			if rank <= 0 {
				return 0, refuseGrant("", codeGrantSubjectOutranksCaller, fmt.Sprintf(
					"a member of the group holds the role %q, which ranks nowhere on this cluster's ladder", role))
			}
			if rank > highest {
				highest = rank
			}
		}
		return highest, grantDecision{}
	}
	return 0, refuseGrant("", codeGrantInvalid, "subjectKind must be user or group")
}

// parseGrantSpec reads and validates grantSet's arguments. Every value is
// checked here rather than trusted to the mutation's enums, so the refusal
// carries a code rather than a parse error.
func parseGrantSpec(args map[string]any) (grantSpec, grantDecision) {
	spec := grantSpec{
		kind:     strings.ToLower(strings.TrimSpace(stringArg(args, "subjectKind"))),
		subject:  memql.BareShortId(strings.TrimSpace(stringArg(args, "subjectId"))),
		verb:     strings.ToLower(strings.TrimSpace(stringArg(args, "verb"))),
		resource: strings.TrimSpace(stringArg(args, "resourceType")),
		effect:   strings.ToLower(strings.TrimSpace(stringArg(args, "effect"))),
	}
	switch spec.kind {
	case componentAuth.SubjectKindUser, componentAuth.SubjectKindGroup:
	default:
		return spec, refuseGrant("", codeGrantInvalid, "subjectKind must be user or group")
	}
	if spec.subject == "" {
		return spec, refuseGrant("", codeGrantInvalid, "subjectId is required")
	}
	if !knownVerb(spec.verb) {
		return spec, refuseGrant("", codeGrantInvalid,
			fmt.Sprintf("%q is not one of the five verbs (read, create, update, delete, execute)", spec.verb))
	}
	if spec.resource == "" {
		return spec, refuseGrant("", codeGrantInvalid, "resourceType is required")
	}
	switch spec.effect {
	case componentAuth.GrantAllow, componentAuth.GrantDeny:
	default:
		return spec, refuseGrant("", codeGrantInvalid, "effect must be allow or deny")
	}
	return spec, grantDecision{}
}

func sameUser(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return a == b || memql.BareShortId(a) == memql.BareShortId(b)
}

// ---------------------------------------------------------------------
// the reads
// ---------------------------------------------------------------------

// systemReadContext is the synthetic operator the subject reads run under:
// the three surfaces a read consults (claims, token, AccessContext), a role of
// owner because IsClusterOwner reads the role and nothing else, and internal
// origin because userByIdSystem is @serverOnly. The groups integration's
// SystemActorContext, restated here rather than imported so this package's
// reads are attributed to this package's actor.
func systemReadContext(ctx context.Context) context.Context {
	claims := map[string]any{"sub": systemGrantsActor, "role": "owner"}
	ctx = componentAuth.ContextWithClaims(ctx, claims)
	ctx = componentAuth.ContextWithToken(ctx, componentAuth.BuildTokenInfo(claims))
	ctx = componentAuth.ContextWithAccess(ctx, &componentAuth.AccessContext{
		UserId: systemGrantsActor,
		Role:   componentAuth.RoleOwner,
	})
	return componentAuth.ContextWithInternalOrigin(ctx)
}

// rows runs one read under the synthetic operator and returns each node's
// payload as a map.
func (i *Integration) rows(ctx context.Context, query string) ([]map[string]any, error) {
	if i.engine == nil {
		return nil, errNoEngine
	}
	res, err := i.engine.Execute(systemReadContext(ctx), query)
	if err != nil {
		return nil, err
	}
	if res == nil || res.Bundle == nil {
		return nil, nil
	}
	out := make([]map[string]any, 0, len(res.Bundle.Nodes))
	for _, n := range res.Bundle.Nodes {
		if n == nil || n.Payload == nil {
			continue
		}
		m := n.Payload.AsMap()
		if m == nil {
			m = map[string]any{}
		}
		m["id"] = n.GetId()
		out = append(out, m)
	}
	return out, nil
}

// userRole reads one ACTIVE user's role slug; found is false for a user who
// is not there or is deactivated (userByIdSystem filters on isActiveRecord).
func (i *Integration) userRole(ctx context.Context, userId string) (role string, found bool, err error) {
	rows, err := i.rows(ctx, `query userByIdSystem(userId: `+langparser.QuoteString(userId)+`)`)
	if err != nil || len(rows) == 0 {
		return "", false, err
	}
	role, _ = rows[0]["role"].(string)
	return strings.ToLower(strings.TrimSpace(role)), true, nil
}

// groupIsActive reads one group's lifecycle flag.
func (i *Integration) groupIsActive(ctx context.Context, groupId string) (active bool, found bool, err error) {
	rows, err := i.rows(ctx, `query groupById(groupId: `+langparser.QuoteString(groupId)+`)`)
	if err != nil || len(rows) == 0 {
		return false, false, err
	}
	status, _ := rows[0]["status"].(string)
	return strings.TrimSpace(status) == "active", true, nil
}

// groupMemberIds reads the user ids of a group's ACTIVE memberships.
func (i *Integration) groupMemberIds(ctx context.Context, groupId string) ([]string, error) {
	rows, err := i.rows(ctx, `query membersOfGroup(groupId: `+langparser.QuoteString(groupId)+`)`)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if userId, _ := r["userId"].(string); strings.TrimSpace(userId) != "" {
			out = append(out, strings.TrimSpace(userId))
		}
	}
	return out, nil
}

// readGrant reads one ACTIVE grant by its derived id. An inactive grant reads
// as absent: revoking a revocation is not an operation.
func (i *Integration) readGrant(ctx context.Context, id string) (grantSpec, bool, error) {
	rows, err := i.rows(ctx, `query grantById(grantId: `+langparser.QuoteString(id)+`)`)
	if err != nil || len(rows) == 0 {
		return grantSpec{}, false, err
	}
	r := rows[0]
	str := func(k string) string { v, _ := r[k].(string); return strings.TrimSpace(v) }
	if active, present := r["active"].(bool); present && !active {
		return grantSpec{}, false, nil
	}
	return grantSpec{
		kind:     str("subjectKind"),
		subject:  memql.BareShortId(str("subjectId")),
		verb:     str("verb"),
		resource: str("resourceType"),
		effect:   str("effect"),
	}, true, nil
}

// ---------------------------------------------------------------------
// the audit line
// ---------------------------------------------------------------------

// auditGrant writes one v1:identity:auditEvent per decision, targetType
// `grant`, the subject, verb, resource and effect in detail. Under the
// caller's plain context, for auditRole's reason: the actor recorded IS this
// caller, and stamping internal origin would widen a call that does not need
// widening.
func (i *Integration) auditGrant(ctx context.Context, access *componentAuth.AccessContext, action, grantId string, spec grantSpec) {
	if i.engine == nil {
		return
	}
	body, err := json.Marshal(map[string]any{
		"grantId":      grantId,
		"subjectKind":  spec.kind,
		"subjectId":    spec.subject,
		"verb":         spec.verb,
		"resourceType": spec.resource,
		"effect":       spec.effect,
	})
	if err != nil {
		return
	}
	query := fmt.Sprintf(
		`mutation createAuditEvent(eventId:%s, occurredAt:%s, category:%s, action:%s, actorUserId:%s, actorEmail:%s, actorRole:%s, targetType:%s, targetId:%s, outcome:%s, detail:%s)`,
		langparser.QuoteString(fmt.Sprintf("audit-grant-%s-%d", grantId, time.Now().UTC().UnixNano())),
		langparser.QuoteString(time.Now().UTC().Format(time.RFC3339Nano)),
		langparser.QuoteString("authorization"),
		langparser.QuoteString(action),
		langparser.QuoteString(access.UserId),
		langparser.QuoteString(access.PrimaryEmail),
		langparser.QuoteString(string(access.Role)),
		langparser.QuoteString(auditTargetGrant),
		langparser.QuoteString(grantId),
		langparser.QuoteString("success"),
		string(body))
	_, _ = i.engine.Execute(ctx, query)
}

// ---------------------------------------------------------------------
// the nodes
// ---------------------------------------------------------------------

func grantNodes(d grantDecision) []memorynodes.MemoryNode {
	payload, _ := json.Marshal(map[string]any{
		"ok":      d.ok,
		"grantId": d.grantId,
		"code":    d.code,
		"message": d.detail,
	})
	return []memorynodes.MemoryNode{{
		ID:        "integration:rbac:grantDecision",
		Concept:   "integration:rbac:decision",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}
}

// effectiveNodes renders the effective set as one node: the caller's role and
// id, and one entry per pair -- `effect` in the grant vocabulary (allow /
// deny) and `source` naming the level that answered (role / group / user).
func effectiveNodes(role, userId string, decisions []componentAuth.Decision, code string) []memorynodes.MemoryNode {
	entries := make([]map[string]any, 0, len(decisions))
	for _, d := range decisions {
		effect := componentAuth.GrantDeny
		if d.Held {
			effect = componentAuth.GrantAllow
		}
		entries = append(entries, map[string]any{
			"verb":     d.Verb,
			"resource": d.Resource,
			"effect":   effect,
			"source":   d.Source,
		})
	}
	sort.SliceStable(entries, func(a, b int) bool {
		ra, rb := entries[a]["resource"].(string), entries[b]["resource"].(string)
		if ra != rb {
			return ra < rb
		}
		return entries[a]["verb"].(string) < entries[b]["verb"].(string)
	})
	payload, _ := json.Marshal(map[string]any{
		"ok":      code == "",
		"code":    code,
		"role":    role,
		"userId":  userId,
		"entries": entries,
	})
	return []memorynodes.MemoryNode{{
		ID:        "integration:rbac:effectiveCapabilities",
		Concept:   "integration:rbac:effectiveCapabilities",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}
}
