package shopify

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// mirror_reads_authz_test.go -- who may reach the mirror through a builtin.
//
// The connector reads its mirror as the operator (F10), so the mirror's own
// actor.isClusterOwner conjunct no longer keeps anybody out of what these
// builtins read. The handler does: the store's owner or a cluster owner, and
// anybody else is refused before a mirror read runs. Which store a caller
// names is read first, because the store row is what says who owns it; that
// read is the only engine call a refused caller costs.

type mirrorHandler func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error)

// mirrorHandlers is every builtin whose handler reads the mirror, with the
// arguments it needs beyond the store.
func mirrorHandlers(integ *Integration) map[string]struct {
	handle mirrorHandler
	args   map[string]any
} {
	return map[string]struct {
		handle mirrorHandler
		args   map[string]any
	}{
		"shopifyFetchProduct": {integ.handleFetchProduct, map[string]any{"handle": "linen-shirt"}},
		"commerceSold":        {integ.handleCommerceSold, map[string]any{}},
		"commerceStock":       {integ.handleCommerceStock, map[string]any{"locationGid": "gid://shopify/Location/1"}},
		"commerceCustomers":   {integ.handleCommerceCustomers, map[string]any{}},
		"commerceCompany":     {integ.handleCommerceCompany, map[string]any{"companyGid": "gid://shopify/Company/1"}},
	}
}

func withStore(args map[string]any, storeID string) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		out[k] = v
	}
	if storeID != "" {
		out["storeId"] = storeID
	}
	return out
}

func TestAMirrorBuiltinRefusesACallerWhoIsNeitherTheStoresOwnerNorAClusterOwner(t *testing.T) {
	for name, h := range mirrorHandlers(NewIntegration(newHarness(t).conn)) {
		// Named and defaulted: a caller who names no store is held to the
		// store the handler picks.
		for _, storeID := range []string{testStoreID, ""} {
			harness := newHarness(t)
			handle := mirrorHandlers(NewIntegration(harness.conn))[name].handle
			_, err := handle(actorCtx("user-2", auth.RoleWriter), withStore(h.args, storeID), 0)
			if err == nil || !strings.Contains(err.Error(), name+" "+actRefusal) {
				t.Errorf("%s (storeId %q): err = %v, want the refusal %q", name, storeID, err, actRefusal)
			}
			for _, call := range harness.engine.calls() {
				if callName(call) != "storesForConnector" {
					t.Errorf("%s (storeId %q): %s ran for a refused caller; only the store row may be read", name, storeID, callName(call))
				}
			}
			if n := len(harness.admin.seen()); n != 0 {
				t.Errorf("%s: Shopify was asked %d times for a refused caller", name, n)
			}
		}
	}
}

func TestAMirrorBuiltinAnswersTheStoresOwnerAndAClusterOwner(t *testing.T) {
	for _, who := range []context.Context{actorCtx("user-1", auth.RoleWriter), actorCtx("user-9", auth.RoleOwner)} {
		for name, h := range mirrorHandlers(NewIntegration(newHarness(t).conn)) {
			if _, err := h.handle(who, withStore(h.args, testStoreID), 0); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
}
