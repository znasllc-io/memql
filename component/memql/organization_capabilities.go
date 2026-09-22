package memql

import (
	"context"
	"sort"

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

// ResolveOrganizationCapabilities complements the global capability answer.
// A deny in Beta must not hide an app the caller may use in Acme. Every target
// decision goes through the same resolver used by resource/action enforcement.
// Operators retain their global discovery contract; finite memberships get a
// complete allow/deny answer for the supported organization surfaces.
func (e *MemQLEngine) ResolveOrganizationCapabilities(ctx context.Context, known []auth.Decision) []OrganizationCapability {
	out := []OrganizationCapability{}
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
		{Verb: auth.VerbRead, Resource: "app:campaigns"}:     true,
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
			effect := auth.GrantDeny
			if e.OrganizationCapable(ctx, account, pair.Verb, pair.Resource) {
				effect = auth.GrantAllow
			}
			out = append(out, OrganizationCapability{AccountID: account, Verb: pair.Verb, Resource: pair.Resource, Effect: effect})
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
