package planner

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func TestCompileActorsPreservePersistedOwnerAssertion(t *testing.T) {
	ctx, err := auth.ContextWithPersistedOwner(context.Background(), "v1:identity:user:compile-owner", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, next := range map[string]context.Context{"classifier": systemActorContext(ctx), "authoring": ownerActorContext(ctx, "v1:identity:user:compile-owner")} {
		t.Run(name, func(t *testing.T) {
			token := auth.TokenInfoFromContext(next)
			if token == nil || token.Claims["sub"] != "v1:identity:user:compile-owner" || token.Claims["role"] != string(auth.RoleWriter) {
				t.Fatalf("borrowed user's attribution was replaced: %+v", token)
			}
		})
	}
}
