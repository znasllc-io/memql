package shopify

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// wholesale_gate_test.go -- who may reach the B2B writes.
//
// The five wholesale builtins carry no @requiresCapability, and a builtin
// without one has no rank floor: every signed-in caller of any role reaches
// their handlers. The managed grant holds write_customers, so the handler is
// the only thing between any signed-in account and a wholesale price on any
// customer of any store. Each handler therefore admits the wholesale pack's
// own call alone -- internal origin, which engineCaller stamps after the
// pack's checks -- and refuses everyone else before anything is read: the
// store's owner and a cluster owner included, who reach a grant through the
// pack (wholesaleRetryAccess), never around it.

// wholesaleBuiltins is the five, by the name a caller writes, with the
// arguments a real call carries beside the store.
func wholesaleBuiltins(i *Integration) []struct {
	builtin string
	handle  mirrorHandler
	args    map[string]any
} {
	return []struct {
		builtin string
		handle  mirrorHandler
		args    map[string]any
	}{
		{"shopifyProvisionWholesale", i.handleProvisionWholesale, map[string]any{"applicationId": "app-1", "companyName": "Acme Trade", "buyerEmail": "buyer@example.com"}},
		{"shopifyRevokeWholesale", i.handleRevokeWholesale, map[string]any{"reference": WholesaleAccountRefs{CompanyGID: "gid://shopify/Company/1", CatalogGID: "gid://shopify/CompanyLocationCatalog/1"}.Encode()}},
		{"shopifyTagWholesaleCustomer", i.handleTagWholesaleCustomer, map[string]any{"buyerEmail": "buyer@example.com"}},
		{"shopifyUntagWholesaleCustomer", i.handleUntagWholesaleCustomer, map[string]any{"buyerEmail": "buyer@example.com"}},
		{"shopifyWholesaleCatalogHeadroom", i.handleWholesaleCatalogHeadroom, map[string]any{}},
	}
}

func onStore(args map[string]any, storeID string) map[string]any {
	out := map[string]any{"storeId": storeID}
	for k, v := range args {
		out[k] = v
	}
	return out
}

func TestTheWholesaleBuiltinsRefuseACallerWhoIsNotThePack(t *testing.T) {
	for n := range wholesaleBuiltins(&Integration{}) {
		// A signed-in writer and a developer, neither the store's owner: the
		// same refusal for a store that exists and one that does not, and
		// nothing read, not even the store row.
		for _, who := range []context.Context{actorCtx("user-2", auth.RoleWriter), actorCtx("user-2", auth.RoleDeveloper), context.Background()} {
			for _, storeID := range []string{testStoreID, "no-such-store"} {
				h := newHarness(t)
				withStoreRow(h, nil)
				b := wholesaleBuiltins(NewIntegration(h.conn))[n]
				nodes, err := b.handle(who, onStore(b.args, storeID), 0)
				if want := "shopify: " + b.builtin + " is called by the wholesale pack, never by a client"; err == nil || err.Error() != want {
					t.Errorf("%s on %q: nodes %v, err %v; want %q", b.builtin, storeID, nodes, err, want)
				}
				onlyTheStoreRowWasRead(t, h, "a refused "+b.builtin+" on "+storeID)
				if calls := h.engine.calls(); len(calls) != 0 {
					t.Errorf("%s on %q: the store row was read before the refusal: %v", b.builtin, storeID, calls)
				}
			}
		}
	}
}

func TestTheWholesaleBuiltinsAdmitThePacksCallAndRefuseTheStoresOwnerAndAClusterOwner(t *testing.T) {
	// The pack's own call: internal origin, whoever its actor is -- an
	// automation's is a reader.
	automation := auth.ContextWithInternalOrigin(actorCtx("system:automation", auth.RoleReader))
	for n := range wholesaleBuiltins(&Integration{}) {
		for name, who := range map[string]context.Context{
			"internal origin":             auth.ContextWithInternalOrigin(context.Background()),
			"an automation's reader step": automation,
		} {
			h := newHarness(t)
			withStoreRow(h, map[string]any{"plan": "Shopify Plus"})
			b := wholesaleBuiltins(NewIntegration(h.conn))[n]
			_, err := b.handle(who, onStore(b.args, testStoreID), 0)
			if err != nil && strings.Contains(err.Error(), "never by a client") {
				t.Errorf("%s: %s was refused: %v", b.builtin, name, err)
			}
			// Past the gate, the handler asks Shopify -- except the headroom
			// read, which a Plus store answers without asking.
			if b.builtin != "shopifyWholesaleCatalogHeadroom" && len(h.admin.seen()) == 0 {
				t.Errorf("%s: %s was admitted and Shopify was never asked (err %v)", b.builtin, name, err)
			}
		}
		// The interim rule admitted these two; the pack's rule does not.
		for name, who := range map[string]context.Context{
			"the store's owner": actorCtx("user-1", auth.RoleWriter),
			"a cluster owner":   actorCtx("user-9", auth.RoleOwner),
		} {
			h := newHarness(t)
			withStoreRow(h, map[string]any{"plan": "Shopify Plus"})
			b := wholesaleBuiltins(NewIntegration(h.conn))[n]
			_, err := b.handle(who, onStore(b.args, testStoreID), 0)
			if want := "shopify: " + b.builtin + " is called by the wholesale pack, never by a client"; err == nil || err.Error() != want {
				t.Errorf("%s: %s: err %v, want %q", b.builtin, name, err, want)
			}
			if got := len(h.admin.seen()); got != 0 {
				t.Errorf("%s: %s was refused and Shopify was still asked %d times", b.builtin, name, got)
			}
		}
	}
}

// A store that does not exist: the pack's own call is told so, as before; a
// cluster owner is refused like anybody else, without the store being read.
func TestOnlyThePacksCallIsToldAWholesaleStoreDoesNotExist(t *testing.T) {
	for n := range wholesaleBuiltins(&Integration{}) {
		h := newHarness(t)
		b := wholesaleBuiltins(NewIntegration(h.conn))[n]
		_, err := b.handle(auth.ContextWithInternalOrigin(context.Background()), onStore(b.args, "no-such-store"), 0)
		if want := `shopify: store "no-such-store" is not configured`; err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", b.builtin, err, want)
		}
		_, err = b.handle(actorCtx("user-9", auth.RoleOwner), onStore(b.args, "no-such-store"), 0)
		if want := "shopify: " + b.builtin + " is called by the wholesale pack, never by a client"; err == nil || err.Error() != want {
			t.Errorf("%s: a cluster owner: err = %v, want %q", b.builtin, err, want)
		}
	}
}

// EACH BUILTIN SAYS WHO MAY CALL IT, in the capability's description and in
// the declaration a caller reads.
func TestTheWholesaleBuiltinsSayWhoMayCallThem(t *testing.T) {
	said := func(text string) bool {
		return strings.Contains(text, "the wholesale pack") && strings.Contains(text, "internal origin") &&
			!strings.Contains(text, "the store's owner") && !strings.Contains(text, "a cluster owner")
	}
	executors := map[string]string{}
	for _, c := range NewIntegration(nil).Capabilities() {
		executors[c.Name] = c.Description
	}
	eng := newRealEngine(t)
	for _, b := range wholesaleBuiltins(&Integration{}) {
		fn, err := eng.Functions().Get(b.builtin)
		if err != nil || fn == nil || !fn.IsBuiltin() {
			t.Fatalf("%s is not a builtin in the function registry: %v", b.builtin, err)
		}
		if !said(fn.Description) {
			t.Errorf("%s's declaration does not say who may call it: %q", b.builtin, fn.Description)
		}
		executor := strings.TrimPrefix(b.builtin, "shopify")
		executor = strings.ToLower(executor[:1]) + executor[1:]
		if !said(executors[executor]) {
			t.Errorf("the %s capability does not say who may call it: %q", executor, executors[executor])
		}
	}
}
