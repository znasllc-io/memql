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
// engine. The AUDIT row records the caller too, written with internal origin
// stamped for that one write: auditEvent's create admits a cluster owner or
// server code (memql#5624), and an admin granting is neither.

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
		return effectiveNodes("", "", nil, nil, codeGrantCallerNotPermitted), nil
	}
	decisions := componentAuth.EffectiveCapabilities(ctx, subject)
	organizationEntries := []memql.OrganizationCapability{}
	if resolver, ok := i.engine.(interface {
		ResolveOrganizationCapabilities(context.Context, []componentAuth.Decision) []memql.OrganizationCapability
	}); ok {
		organizationEntries = resolver.ResolveOrganizationCapabilities(ctx, decisions)
	}
	return effectiveNodes(string(subject.Role), memql.BareShortId(subject.UserId), decisions, organizationEntries, ""), nil
}

// ---------------------------------------------------------------------
// effectiveAccessForSubject
// ---------------------------------------------------------------------
//
// WHAT SOMEBODY ELSE CAN OPEN, AND WHY (access clarity plan, task 1). The
// read above answers the caller and nobody else, so an operator's screen used
// to assemble a colleague's answer from the admin-floored grant reads and the
// public catalog, restating the resolver in TypeScript -- and the restatement
// missed both of the engine's organization rules: an organization role's
// limits (auth.ScopedRoleMayUse) and the per-organization decisions the action
// guards make (component/memql's OrganizationCapable). This read runs the
// engine's own resolver for the named person instead. Nothing here decides
// anything: the decisions are the resolver's, and this file only names them.
//
// A REFUSAL IS AN ERROR, never a row. Every row shares one shape, and a
// refusal dressed as one would be drawn as an answer. Each message starts
// with its code, the groups integration's convention, so a client matches the
// contract rather than the sentence.
const (
	codeAccessCallerNotPermitted    = "access_caller_not_permitted"
	codeAccessSubjectOutranksCaller = "access_subject_outranks_caller"
	codeAccessUnknownSubject        = "access_unknown_subject"
	codeAccessInvalid               = "access_invalid"
	codeAccessUnavailable           = "access_unavailable"
)

// A row's `source`: where its answer came from, in an operator's words.
const (
	accessSourceRole            = "role"
	accessSourceGroup           = "group"
	accessSourcePerson          = "person"
	accessSourceScopedRoleLimit = "scoped-role-limit"
	accessSourceNotGranted      = "not-granted"
)

// accessRow is one row of effectiveAccessForSubject.
type accessRow struct {
	Verb         string `json:"verb"`
	ResourceType string `json:"resourceType"`
	Held         bool   `json:"held"`
	Source       string `json:"source"`
	SourceId     string `json:"sourceId"`
	AccountId    string `json:"accountId"`
}

// organizationAccessResolver is the engine's per-organization resolution for
// a NAMED person (memql.MemQLEngine.ResolveOrganizationDecisionsFor). Declared
// here, as the caller's read declares its own, so this package's doubles need
// not stand up an engine.
type organizationAccessResolver interface {
	ResolveOrganizationDecisionsFor(ctx context.Context, userId string, role componentAuth.Role, known []componentAuth.Decision) []memql.OrganizationDecision
}

func accessRefusal(code, message string) error {
	return fmt.Errorf("%s: %s", code, message)
}

func (i *Integration) handleEffectiveAccessForSubject(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	kind := strings.ToLower(strings.TrimSpace(stringArg(args, "subjectKind")))
	subject := memql.BareShortId(strings.TrimSpace(stringArg(args, "subjectId")))
	if kind != componentAuth.SubjectKindUser && kind != componentAuth.SubjectKindGroup {
		return nil, accessRefusal(codeAccessInvalid, "subjectKind must be user or group")
	}
	if subject == "" {
		return nil, accessRefusal(codeAccessInvalid, "subjectId is required")
	}
	callerRank, err := accessCallerRank(ctx)
	if err != nil {
		return nil, err
	}
	if i.engine == nil {
		return nil, accessRefusal(codeAccessUnavailable, "this node has no engine to resolve access with")
	}
	var rows []accessRow
	if kind == componentAuth.SubjectKindUser {
		rows, err = i.personAccess(ctx, callerRank, subject)
	} else {
		rows, err = i.groupAccess(ctx, subject)
	}
	if err != nil {
		return nil, err
	}
	return accessNodes(rows), nil
}

// accessCallerRank applies the read's floor and answers the caller's rank.
//
// THE GRANT READS' FLOOR, restated in Go because a builtin cannot carry
// @requiresRank (its annotation set is closed): grantsForSubject and
// grantsForResource are @requiresRank("admin"), and this read answers the
// same question about a named person with the resolution on top, so it
// refuses what refuseBelowRequiredRank refuses for that floor -- no caller, a
// floor the ladder cannot resolve, an organization role at ANY rank (an admin
// floor asks for a cluster role, and rank is not the question), and a rank
// below admin's. Internal origin is not exempted, unlike the annotation: the
// read is a client's, and no server path asks it.
func accessCallerRank(ctx context.Context) (int, error) {
	access, _ := componentAuth.AccessFromContext(ctx)
	if access == nil {
		return 0, accessRefusal(codeAccessCallerNotPermitted, "this call carries no caller identity")
	}
	role := componentAuth.Role(strings.ToLower(strings.TrimSpace(string(access.Role))))
	floor := componentAuth.RoleRank(componentAuth.RoleAdmin)
	if floor <= 0 {
		return 0, accessRefusal(codeAccessCallerNotPermitted,
			"the admin rung does not resolve on this cluster's ladder, so the floor cannot be applied")
	}
	if componentAuth.RoleAccountScope(role) != "" {
		return 0, accessRefusal(codeAccessCallerNotPermitted,
			"reading somebody's access needs a cluster role; yours belongs to one organization")
	}
	rank := componentAuth.RoleRank(role)
	if rank < floor {
		return 0, accessRefusal(codeAccessCallerNotPermitted,
			fmt.Sprintf("reading somebody's access needs the admin role or above; yours is %q", string(role)))
	}
	return rank, nil
}

// personAccess answers what one person can open, and why.
//
// THE RANK RULE FIRST: nobody reads a person who outranks them, so a
// developer looking at Access sees nothing for the owner. The person's role
// comes off their user row under the synthetic operator, the grant guards'
// read; a role that ranks nowhere cannot be compared, and "cannot compare"
// refuses.
//
// THEN THE ENGINE'S RESOLVER, FOR THEM. The subject is built the way role
// testing builds one (component/identity/roletest/service.go): an
// AccessContext carrying their id and role, through SubjectFromContext, so
// their groups come from the installed membership source. Two answers:
//
//   - EffectiveCapabilities, the same call the person's own read makes:
//     every pair any role holds or their grants name, with provenance and the
//     role's limits. Only the `app:` pairs become rows -- a door
//     (`read app:<id>`), a floored section (`read app:<id>/<section>`), a part
//     (`execute app:<id>/<part>`).
//   - The engine's per-organization decisions for them. A pair answered there
//     is answered per organization ONLY: a merged row would have to pick one
//     organization's answer for all of them, which is the confusion this read
//     exists to end. Which pairs those are is the engine's to say; the data
//     verbs arrive this way too. An operator, and a person in no
//     organization, has none, and keeps one row per pair.
func (i *Integration) personAccess(ctx context.Context, callerRank int, userId string) ([]accessRow, error) {
	role, found, err := i.userRole(ctx, userId)
	if err != nil {
		return nil, accessRefusal(codeAccessUnknownSubject, "the person could not be read: "+err.Error())
	}
	if !found {
		return nil, accessRefusal(codeAccessUnknownSubject, "no active user by that id")
	}
	rank := componentAuth.RoleRank(componentAuth.Role(role))
	if rank <= 0 {
		return nil, accessRefusal(codeAccessSubjectOutranksCaller, fmt.Sprintf(
			"the person's role %q ranks nowhere on this cluster's ladder, so it cannot be compared to yours", role))
	}
	if rank > callerRank {
		// No rank in the sentence: it would say which rung a person holds to
		// somebody the rule has just refused to tell anything about them.
		return nil, accessRefusal(codeAccessSubjectOutranksCaller,
			"that person ranks above you, and nobody reads the access of somebody who outranks them")
	}
	resolver, ok := i.engine.(organizationAccessResolver)
	if !ok {
		// Answering every pair globally instead would read an organization's
		// door as open everywhere: a confident wrong picture, not a degraded one.
		return nil, accessRefusal(codeAccessUnavailable, "this node cannot resolve per-organization access")
	}

	ctx = componentAuth.ContextWithGrantMemo(ctx)
	personCtx := componentAuth.ContextWithAccess(ctx, &componentAuth.AccessContext{UserId: userId, Role: componentAuth.Role(role)})
	person, _ := componentAuth.SubjectFromContext(personCtx)
	decisions := componentAuth.EffectiveCapabilities(personCtx, person)
	organizations := resolver.ResolveOrganizationDecisionsFor(ctx, userId, componentAuth.Role(role), decisions)

	perOrganization := map[componentAuth.VerbResource]bool{}
	for _, d := range organizations {
		perOrganization[componentAuth.VerbResource{Verb: d.Verb, Resource: d.Resource}] = true
	}
	rows := make([]accessRow, 0, len(decisions)+len(organizations))
	for _, d := range decisions {
		if !strings.HasPrefix(d.Resource, "app:") || perOrganization[componentAuth.VerbResource{Verb: d.Verb, Resource: d.Resource}] {
			continue
		}
		rows = append(rows, accessRowFrom(d, role, ""))
	}
	for _, d := range organizations {
		rows = append(rows, accessRowFrom(d.Decision, role, d.AccountID))
	}
	sortAccessRows(rows)
	return rows, nil
}

// accessRowFrom names one decision in the read's vocabulary.
//
// A BARRED pair is the role's limit, and which limit says who to blame: an
// organization role's (`scoped-role-limit`, "an organization role can't open
// this") or a global role's -- Fleet below the roles that manage it -- which
// is the role's own answer (`role`, held false). Neither is "not granted":
// a grant may well name it, and none can lift it. Otherwise the level that
// answered names itself, and a role-level answer that is not held is
// `not-granted`: nothing the person holds reaches it.
func accessRowFrom(d componentAuth.Decision, role, accountId string) accessRow {
	row := accessRow{Verb: d.Verb, ResourceType: d.Resource, Held: d.Held, AccountId: accountId}
	switch {
	case d.Barred && componentAuth.RoleAccountScope(componentAuth.Role(role)) != "":
		row.Held, row.Source, row.SourceId = false, accessSourceScopedRoleLimit, role
	case d.Barred:
		row.Held, row.Source, row.SourceId = false, accessSourceRole, role
	case d.Source == componentAuth.SourceUser:
		row.Source, row.SourceId = accessSourcePerson, memql.BareShortId(d.SourceId)
	case d.Source == componentAuth.SourceGroup:
		row.Source, row.SourceId = accessSourceGroup, memql.BareShortId(d.SourceId)
	case d.Held:
		row.Source, row.SourceId = accessSourceRole, role
	default:
		row.Source = accessSourceNotGranted
	}
	return row
}

// groupAccess answers a group's own grants: one row per ACTIVE grant naming
// it, held from its effect. accountId is the group's own organization, the
// one its grants come from: a tied group's grants reach that organization's
// per-organization decisions and no other's. A group tied to none answers ""
// (its grants reach every organization's decisions, and the global ones).
//
// No rank rule: a group is not a person, and the grant reads this replaces
// for a group carry the admin floor alone. A group that is not there, or is
// archived, refuses -- an archived group grants nobody anything, and an empty
// answer would say it grants nothing for a different reason. The rows come
// from the grant source the resolver folds at the group level, so the page
// and the gates read one set.
func (i *Integration) groupAccess(ctx context.Context, groupId string) ([]accessRow, error) {
	groups, err := i.rows(ctx, `query groupById(groupId: `+langparser.QuoteString(groupId)+`)`)
	if err != nil {
		return nil, accessRefusal(codeAccessUnknownSubject, "the group could not be read: "+err.Error())
	}
	if len(groups) == 0 {
		return nil, accessRefusal(codeAccessUnknownSubject, "no active group by that id")
	}
	status, _ := groups[0]["status"].(string)
	if strings.TrimSpace(status) != "active" {
		return nil, accessRefusal(codeAccessUnknownSubject, "no active group by that id")
	}
	account, _ := groups[0]["accountId"].(string)
	account = memql.BareShortId(strings.TrimSpace(account))
	src := componentAuth.InstalledGrantSource()
	if src == nil {
		return nil, accessRefusal(codeAccessUnavailable, "this node has no grant source to read the group's grants from")
	}
	grants, err := src.ActiveGrantsForGroups(ctx, []string{groupId})
	if err != nil {
		return nil, accessRefusal(codeAccessUnavailable, "the group's grants could not be read: "+err.Error())
	}
	rows := make([]accessRow, 0, len(grants))
	for _, g := range grants {
		if g.Inactive || g.Verb == "" || g.Resource == "" {
			continue
		}
		rows = append(rows, accessRow{
			Verb: g.Verb, ResourceType: g.Resource, Held: g.Effect == componentAuth.GrantAllow,
			Source: accessSourceGroup, SourceId: groupId, AccountId: account,
		})
	}
	sortAccessRows(rows)
	return rows, nil
}

// sortAccessRows orders rows by organization (the unscoped ones first), then
// resource, then verb, so two reads of one subject draw the same list.
func sortAccessRows(rows []accessRow) {
	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].AccountId != rows[b].AccountId {
			return rows[a].AccountId < rows[b].AccountId
		}
		if rows[a].ResourceType != rows[b].ResourceType {
			return rows[a].ResourceType < rows[b].ResourceType
		}
		return rows[a].Verb < rows[b].Verb
	})
}

// accessNodes renders one node per row. EVERY ID IS DISTINCT: a top-level
// builtin's reply is keyed by node id (component/memql's nodesToMap), so two
// rows sharing one would arrive as one. Numbered in the rows' order, which is
// also the order a client reading the reply by id sees them in.
func accessNodes(rows []accessRow) []memorynodes.MemoryNode {
	now := time.Now().UTC()
	out := make([]memorynodes.MemoryNode, 0, len(rows))
	for n, row := range rows {
		payload, _ := json.Marshal(row)
		out = append(out, memorynodes.MemoryNode{
			ID:        fmt.Sprintf("integration:rbac:effectiveAccess:%05d", n),
			Concept:   "integration:rbac:effectiveAccess",
			Type:      memorynodes.NodeTypeObject,
			CreatedAt: now,
			Payload:   payload,
		})
	}
	return out
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
// `grant`, the subject, verb, resource and effect in detail. Stamped with
// internal origin for this one write, for auditRole's reason (memql#5624).
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
	_, _ = i.engine.Execute(componentAuth.ContextWithInternalOrigin(ctx), query)
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
func effectiveNodes(role, userId string, decisions []componentAuth.Decision, organizationEntries []memql.OrganizationCapability, code string) []memorynodes.MemoryNode {
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
		"ok":                  code == "",
		"code":                code,
		"role":                role,
		"userId":              userId,
		"entries":             entries,
		"organizationEntries": organizationEntries,
	})
	return []memorynodes.MemoryNode{{
		ID:        "integration:rbac:effectiveCapabilities",
		Concept:   "integration:rbac:effectiveCapabilities",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}
}
