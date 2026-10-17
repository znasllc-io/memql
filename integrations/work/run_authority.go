package work

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// runOwnerActor restores a test session before the first owner-scoped read on
// a receiving replica. An event's owner id is only a hint: it cannot authorize
// a reserved persona. Only the exact run's server-stamped grant can do that.
// Ordinary owners retain the existing borrowed-authority path.
func (i *Integration) runOwnerActor(ctx context.Context, runID, owner string) (context.Context, error) {
	if !auth.IsRoleTestUser(owner) {
		return ownerActor(ctx, owner), nil
	}
	if err := auth.ValidateRoleTestContext(ctx); err != nil {
		return nil, err
	}
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil && !ac.Synthetic &&
		memql.BareShortId(ac.UserId) != memql.BareShortId(owner) {
		return nil, fmt.Errorf("work: cannot restore another person's test authority")
	}
	// This service context is used for one fixed, run-id-scoped read only.
	// It is never returned or passed to compilation, model calls or writes.
	service := auth.ContextWithForwardedAuthority(ctx, auth.ForwardedAuthority{})
	claims := map[string]any{"sub": "system:work:run-authority", "role": "system"}
	service = auth.ContextWithClaims(service, claims)
	service = auth.ContextWithToken(service, &auth.TokenInfo{Subject: "system:work:run-authority", Claims: claims})
	service = auth.ContextWithAccess(service, &auth.AccessContext{
		UserId: "system:work:run-authority", Role: auth.RoleOwner, Synthetic: true, Unranked: true,
	})
	run, err := one(i.store().queryInternal(memql.ContextWithFreshRead(service),
		"query "+call("workRunById", map[string]any{"runId": runID})))
	if err != nil {
		return nil, err
	}
	if run == nil || memql.BareShortId(rowString(run, "ownerUserId")) != memql.BareShortId(owner) {
		return nil, fmt.Errorf("work: run does not belong to the requested test persona")
	}
	if err := auth.ValidateRoleTestContext(ctx); err != nil {
		return nil, err
	}
	grant := rowMap(run, "executionAuthority")
	if rowString(grant, "credentialClass") != auth.RoleTestClass {
		return nil, fmt.Errorf("work: a test persona requires a persisted role-test grant")
	}
	return auth.ContextWithPersistedOwner(memql.ContextWithFreshRead(ctx), rowString(run, "ownerUserId"),
		grant, i.ownerIdentityResolver())
}
