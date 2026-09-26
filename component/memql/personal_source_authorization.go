package memql

import (
	"context"

	"github.com/znasllc-io/memql/component/auth"
)

// These exact personal doors never choose an owning account: their handlers
// resolve only the caller's grant and installation binding. Delegation in one
// authorized account may make that personal setup available without granting
// targetless data authority or access to another person's credentials.
func personalSourceRequirement(name string) (verb, resource string) {
	switch name {
	case "sourceConnectionCreate", "sourceConnectionRemove", "sourceCredentialRevoke", "githubConnectBegin":
		return auth.VerbExecute, "app:deployables/sources"
	case "sourceInstallations", "sourceRepositories", "sourceProbe", "sourceConnectionsMine", "sourceConnectionById", "sourceCredentialsMine", "sourceCredentialById":
		return auth.VerbRead, "app:deployables"
	}
	return "", ""
}

func (e *MemQLEngine) personalSourceCapable(ctx context.Context, verb, resource string) bool {
	// Personal accounts are also managed outside Deployables. This grants no
	// package authority; every handler still resolves the caller's own rows.
	if e.GlobalDataCapable(ctx, auth.VerbRead) && e.OrganizationCapable(ctx, "", auth.VerbRead, "app:settings/connections") && e.OrganizationCapable(ctx, "", verb, "app:settings/connections") {
		return true
	}
	if e.GlobalDataCapable(ctx, auth.VerbRead) && e.OrganizationCapable(ctx, "", auth.VerbRead, "app:deployables") && e.OrganizationCapable(ctx, "", verb, resource) {
		return true
	}
	scope := e.accountScopeFor(ctx)
	if scope == nil {
		return false
	}
	for account := range scope.accounts {
		if e.OrganizationCapable(ctx, account, auth.VerbRead, auth.ResourceData) && e.OrganizationCapable(ctx, account, auth.VerbRead, "app:deployables") && e.OrganizationCapable(ctx, account, verb, resource) {
			return true
		}
	}
	return false
}

func personalSourceBuiltin(fn *Function) bool {
	if fn == nil || fn.FunctionKind != "builtin" {
		return false
	}
	switch fn.Name {
	case "sourceConnectionCreate", "sourceConnectionRemove", "sourceCredentialRevoke", "sourceInstallations", "sourceRepositories", "sourceProbe":
		return ConstructNamespaceForOrigin(fn.Origin) == "platform" && fn.Executor == "integration.packages."+fn.Name
	case "githubConnectBegin":
		return ConstructNamespaceForOrigin(fn.Origin) == "identity" && fn.Executor == "integration.identity.githubConnectBegin"
	}
	return false
}

func personalSourceQuery(fn *Function) bool {
	if fn == nil || fn.FunctionKind != "query" || ConstructNamespaceForOrigin(fn.Origin) != "platform" {
		return false
	}
	switch fn.Name {
	case "sourceConnectionsMine", "sourceConnectionById":
		return fn.BoundConcept == "v1:platform:sourceConnection"
	case "sourceCredentialsMine", "sourceCredentialById":
		return fn.BoundConcept == "v1:platform:sourceCredential"
	}
	return false
}
