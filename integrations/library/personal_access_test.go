package library

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// Edit adapters use the engine's real authorization policy with its compiled
// catalog; database tests separately exercise persisted grants and ownership.
type libraryTestAuthority struct{}

func (libraryTestAuthority) GlobalDataCapable(ctx context.Context, verb string) bool {
	return (&memql.MemQLEngine{}).GlobalDataCapable(ctx, verb)
}
func (libraryTestAuthority) PersonalAppCapable(ctx context.Context, app string) bool {
	return (&memql.MemQLEngine{}).PersonalAppCapable(ctx, app)
}

func TestDocumentAuthorityMissingRefusesWrites(t *testing.T) {
	i := &Integration{}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner", Role: auth.RoleOwner})
	if i.canManageDocument(ctx, auth.VerbCreate) {
		t.Fatal("missing engine authorization allowed a document write")
	}
}
