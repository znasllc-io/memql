package memql

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// memql#3063: workerTokensForUser is @serverOnly, and this pins BOTH halves of
// why that is safe -- against the real loaded construct rather than a fixture.
//
// The exposure it closes: the filter keys on a caller-supplied userId with no
// actor check, and the shape is identityFull, which projects `credentials` --
// keyHash, registeredBy, lastSeenAt, lastConnectedFromIP for the worker_token
// variant. So any authenticated caller could read another user's worker-token
// digest and last-connect IP by passing their id. @public enforced nothing; it
// only suppressed the classifier.
//
// Why a test and not just the annotation: the annotation is only half the
// change. The other half is the internal-origin stamp inside
// component/identity/workertoken's ListForUser, and REMOVING THAT STAMP BREAKS
// NOTHING that any test could see -- measured during this change. A guard whose
// two halves are each invisible to the suite is the shape this repo keeps
// finding (memql#2965, #2971, #2972, #2985), so both halves are asserted here:
// the gate refuses a client-origin call, and an internal-origin call succeeds.
//
// Postgres-gated, like its neighbours in this package.
func TestWorkerTokensForUserIsServerOnlyAndInternalOriginPasses(t *testing.T) {
	eng, _, ctx := readMergeTestEngine(t)

	const q = `query workerTokensForUser(userId:"user-3063-probe")`

	t.Run("a client-origin call is refused", func(t *testing.T) {
		// readMergeTestEngine's context carries a token but NOT internal
		// origin, which is exactly the shape a wire call arrives in.
		_, err := eng.Execute(ctx, q)
		if err == nil {
			t.Fatal("workerTokensForUser answered a client-origin call. It projects identityFull, " +
				"so that is another user's worker-token keyHash and lastConnectedFromIP on the " +
				"wire behind a caller-supplied id (memql#3063). Either @serverOnly was dropped " +
				"from the query or the gate stopped being enforced.")
		}
		if !strings.Contains(err.Error(), "server-only") {
			t.Errorf("refused, but not by the server-only gate -- so this test would keep passing "+
				"if the gate were removed and something else happened to fail: %v", err)
		}
	})

	t.Run("an internal-origin call succeeds", func(t *testing.T) {
		// The half that makes the annotation safe rather than merely strict.
		// component/identity/workertoken's ListForUser stamps exactly this on
		// a context scoped to the one query; if the stamp stopped satisfying
		// the gate, the worker-token revoke ownership check would break at
		// runtime and nothing else here would notice.
		if _, err := eng.Execute(auth.ContextWithInternalOrigin(ctx), q); err != nil {
			t.Fatalf("an internal-origin call must be allowed -- this is the path "+
				"component/identity/workertoken.ListForUser uses, and the revoke ownership "+
				"check in component/grpc/worker_token_handlers.go depends on it: %v", err)
		}
	})
}
