package memql

import (
	"context"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// OrganizationCapability describes an existing capability in one authorized
// organization. It is discovery data, never a substitute for action guards.
type OrganizationCapability struct {
	AccountID string `json:"accountId"`
	Verb      string `json:"verb"`
	Resource  string `json:"resource"`
	Effect    string `json:"effect"`
}

// OrganizationDecision is one organization's answer for one (verb, resource)
// pair, with the provenance the resolver reported for it: which level
// answered, who at that level, and whether a limit on the role barred it.
type OrganizationDecision struct {
	AccountID string
	auth.Decision
}

// ResolveOrganizationCapabilities complements the global capability answer.
// A deny in Beta must not hide an app the caller may use in Acme. Every target
// decision goes through the same resolver used by resource/action enforcement.
// Operators retain their global discovery contract; finite memberships get a
// complete allow/deny answer for the supported organization surfaces.
func (e *MemQLEngine) ResolveOrganizationCapabilities(ctx context.Context, known []auth.Decision) []OrganizationCapability {
	decisions := e.resolveOrganizationDecisions(ctx, known)
	out := make([]OrganizationCapability, 0, len(decisions))
	for _, d := range decisions {
		effect := auth.GrantDeny
		if d.Held {
			effect = auth.GrantAllow
		}
		out = append(out, OrganizationCapability{AccountID: d.AccountID, Verb: d.Verb, Resource: d.Resource, Effect: effect})
	}
	return out
}

// ResolveOrganizationDecisionsFor is the same resolution asked about a NAMED
// PERSON rather than the caller on ctx: the per-organization half of an
// operator's read of what somebody can open (integration.rbac's
// effectiveAccessForSubject, which applies the read's floor before it asks).
//
// The person is built the way role testing builds one -- an AccessContext
// carrying their id and their role, on memos of their own (ContextForPerson,
// which says why nothing of the caller's request memo is kept) -- so every
// decision below runs through the action guards' own resolver for THEM: their
// account scope (narrowed to an organization role's account), their groups
// filtered to the organization being answered, the grants on both.
//
// Empty for an operator (they keep the global answer, as the caller's own
// read does) and for a person whose scope reaches no organization.
func (e *MemQLEngine) ResolveOrganizationDecisionsFor(ctx context.Context, userId string, role auth.Role, known []auth.Decision) []OrganizationDecision {
	userId = strings.TrimSpace(userId)
	if e == nil || userId == "" {
		return []OrganizationDecision{}
	}
	return e.resolveOrganizationDecisions(e.ContextForPerson(ctx, userId, role), known)
}

// ContextForPerson re-points ctx at a NAMED PERSON for a read about them: an
// AccessContext carrying their id and role, on a FRESH ACCOUNT-SCOPE MEMO.
//
// Fresh, and none of the caller's request memo kept, for two reasons:
//
//   - The memo holds ONE resolved scope per request, and the caller's own --
//     an operator's everyAccount -- would answer for the person: no
//     organization decisions at all, read as "this person has none".
//   - The per-user MEMBERSHIP CACHE lives in the same memo, so it goes too and
//     the person's memberships are read anew rather than served from whatever
//     the request read before. That is what an operator's read of somebody
//     needs: it refuses when any read behind the answer failed
//     (auth.ContextWithReadFailures), and a membership answer memoised before
//     that holder was listening could rest on a failure it never heard. The
//     cost is the person's membership read once per context built, on an
//     operator's read of one person.
//
// The grant memo is not replaced here: the read that refuses on a failure
// brings a fresh one with its holder, and without a holder a memo keyed by
// subject hands the person nothing but their own grants.
func (e *MemQLEngine) ContextForPerson(ctx context.Context, userId string, role auth.Role) context.Context {
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: strings.TrimSpace(userId), Role: role})
	if e == nil {
		return ctx
	}
	return context.WithValue(ctx, accountScopeMemoKey{}, &accountScopeMemo{engine: e})
}

// resolveOrganizationDecisions answers, for the actor on ctx, every pair the
// engine decides per organization in every organization their scope reaches.
// The pairs are the organization surfaces -- opening Email and Deployables,
// the data verbs -- plus the organization deployable parts among `known`.
func (e *MemQLEngine) resolveOrganizationDecisions(ctx context.Context, known []auth.Decision) []OrganizationDecision {
	out := []OrganizationDecision{}
	if e == nil {
		return out
	}
	ctx = contextWithAccountScopeMemo(ctx, e)
	ctx = auth.ContextWithGrantMemo(ctx)
	scope := e.accountScopeFor(ctx)
	if scope == nil || scope.everyAccount {
		return out
	}
	accounts := map[string]bool{}
	for account := range scope.accounts {
		accounts[BareShortId(account)] = true
	}
	pairs := map[auth.VerbResource]bool{
		{Verb: auth.VerbRead, Resource: "app:email"}:         true,
		{Verb: auth.VerbRead, Resource: "app:deployables"}:   true,
		{Verb: auth.VerbRead, Resource: auth.ResourceData}:   true,
		{Verb: auth.VerbCreate, Resource: auth.ResourceData}: true,
		{Verb: auth.VerbUpdate, Resource: auth.ResourceData}: true,
	}
	for _, d := range known {
		if d.Verb == auth.VerbExecute && organizationDeployableCapability(d.Resource) {
			pairs[auth.VerbResource{Verb: d.Verb, Resource: d.Resource}] = true
		}
	}
	for account := range accounts {
		for pair := range pairs {
			out = append(out, OrganizationDecision{AccountID: account, Decision: e.organizationDecision(ctx, account, pair.Verb, pair.Resource)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.AccountID != b.AccountID {
			return a.AccountID < b.AccountID
		}
		if a.Resource != b.Resource {
			return a.Resource < b.Resource
		}
		return a.Verb < b.Verb
	})
	return out
}
