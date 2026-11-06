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
// carrying their id and their role -- so every decision below runs through
// the action guards' own resolver for THEM: their account scope (narrowed to
// an organization role's account), their groups filtered to the organization
// being answered, the grants on both.
//
// A FRESH ACCOUNT-SCOPE MEMO, and this is the one thing that cannot be
// shared with the caller's request. The memo holds ONE resolved scope per
// request, and the caller's own -- an operator's everyAccount -- would answer
// for the person: no organization decisions at all, read as "this person
// has none". The membership and grant memos are keyed by user and by
// subject, so the caller's entries in them cannot leak into these.
//
// Empty for an operator (they keep the global answer, as the caller's own
// read does) and for a person in no organization.
func (e *MemQLEngine) ResolveOrganizationDecisionsFor(ctx context.Context, userId string, role auth.Role, known []auth.Decision) []OrganizationDecision {
	userId = strings.TrimSpace(userId)
	if e == nil || userId == "" {
		return []OrganizationDecision{}
	}
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: userId, Role: role})
	ctx = context.WithValue(ctx, accountScopeMemoKey{}, &accountScopeMemo{engine: e})
	return e.resolveOrganizationDecisions(ctx, known)
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
