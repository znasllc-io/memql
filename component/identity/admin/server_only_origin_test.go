package admin

// server_only_origin_test.go -- memql#2991.
//
// `updateUser` is @serverOnly, enforced as
// `fn.ServerOnly && !auth.OriginFromContext(ctx).IsInternal()`. This package
// holds its ONE production caller, so if that call site stops stamping an
// internal origin, every admin user edit fails -- and it fails in the admin UI
// at runtime, not at build time and not in any DSL test.
//
// WHY THIS IS BEHAVIOURAL AND NOT A SOURCE SCAN. The first version of this
// guard lived in package `dsl` and grepped this package's handlers.go for
// "mutation updateUser(userId:" followed by "auth.ContextWithInternalOrigin"
// within 600 characters. Review defeated it three separate ways, each of them
// something an ordinary change would do:
//
//   - a SECOND, unstamped call site -- strings.Index finds only the first, so
//     the scan stayed green while the new call escalated a role;
//   - a COMMENT naming the helper, with the real call unstamped -- a substring
//     check over a window cannot tell code from prose;
//   - reordering the fmt args to `(payload: %s, userId: %q)` -- the scan then
//     matched nothing and t.Skip'd, which is a PASS. That exact spelling is
//     written elsewhere in this same changeset
//     (component/automations/steps/account_deletion_sweep_db_test.go), so it is
//     the natural drift, not a contrived one.
//
// Its stated justification -- "exercising it needs a live engine and database"
// -- was also wrong. AdminServer.Engine is identity.EngineExecutor, a
// one-method interface, and this file needs no database. The sibling guard at
// component/identity/server_only_origin_test.go had already established the
// pattern for the Store; this is the AdminServer half.

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// updateUserOriginRecorder captures the origin of EVERY context an updateUser
// mutation is executed with.
//
// A slice, not a map keyed by construct name. A map is last-write-wins, so an
// unstamped call placed BEFORE the stamped one is overwritten and vanishes --
// the mirror image of the `strings.Index` "only sees the first" flaw this file
// was written to replace, and it was in the first version of this recorder
// (caught in review). Recording every call means order cannot hide one.
type updateUserOriginRecorder struct {
	seen []auth.CallOrigin
}

func (r *updateUserOriginRecorder) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	if strings.Contains(query, "updateUser(") {
		r.seen = append(r.seen, auth.OriginFromContext(ctx))
	}
	return nil, nil
}

// TestAdminUpdateUserStampsInternalOrigin pins that the admin server's only
// path to the @serverOnly updateUser mutation carries an internal origin.
//
// Failing-first: drop the auth.ContextWithInternalOrigin wrapper in
// (*AdminServer).updateUser and this reports OriginClient. Unlike the source
// scan it replaces, it is indifferent to argument order, to how the query
// string is built, and to comments -- and because it asserts over EVERY
// recorded call rather than one, a second unstamped call site cannot hide
// behind position.
func TestAdminUpdateUserStampsInternalOrigin(t *testing.T) {
	rec := &updateUserOriginRecorder{}
	s := &AdminServer{Engine: rec}

	err := s.updateUser(context.Background(), &userView{
		ID:           "v1:identity:user:someone",
		PrimaryEmail: "someone@example.com",
		Role:         "reader",
		Active:       true,
	})
	if err != nil {
		t.Fatalf("updateUser: %v", err)
	}

	if len(rec.seen) == 0 {
		t.Fatal("(*AdminServer).updateUser never executed a query naming `updateUser` -- " +
			"if the mutation was renamed, move this guard with it (memql#2991)")
	}
	for i, got := range rec.seen {
		if got.IsInternal() {
			continue
		}
		t.Errorf("the admin server executed the @serverOnly `updateUser` mutation with origin %v, "+
			"not internal (call %d of %d).\nThat call is REFUSED at runtime -- @serverOnly is "+
			"enforced as `fn.ServerOnly && !auth.OriginFromContext(ctx).IsInternal()` -- and it "+
			"fails in the admin UI rather than in any test. Wrap the context the way the sibling "+
			"calls in handlers.go, the PAT store and the identity store all do (memql#2991).",
			got, i+1, len(rec.seen))
	}
}
