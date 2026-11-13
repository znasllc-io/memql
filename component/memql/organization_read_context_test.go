package memql

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func TestOrganizationReadContextPreservesActorAndReplacesScopeEvidence(t *testing.T) {
	e := &MemQLEngine{}
	for _, actor := range []*auth.AccessContext{
		{UserId: "scoped", Role: "store-moderator"},
		{UserId: "borrowed", Role: auth.RoleWriter, Unranked: true},
		{UserId: "synthetic", Role: auth.RoleWriter, Unranked: true, Synthetic: true},
	} {
		ctx := auth.ContextWithAccess(context.Background(), actor)
		old := &accountScopeMemo{engine: e, scope: &accountScope{everyAccount: true}}
		ctx = context.WithValue(ctx, accountScopeMemoKey{}, old)
		fresh, failures := e.OrganizationReadContext(ctx)
		got, ok := auth.AccessFromContext(fresh)
		if !ok || got != actor {
			t.Fatalf("organization read replaced the exact actor: got %+v, want %+v", got, actor)
		}
		memo, _ := fresh.Value(accountScopeMemoKey{}).(*accountScopeMemo)
		if failures == nil || memo == nil || memo == old || memo.engine != e || memo.scope != nil || len(memo.members) != 0 {
			t.Fatal("organization read reused stale scope or membership evidence")
		}
		if original, _ := ctx.Value(accountScopeMemoKey{}).(*accountScopeMemo); original != old {
			t.Fatal("refresh changed the parent request's memo")
		}
	}
}
