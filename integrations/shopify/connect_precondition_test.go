package shopify

import (
	"context"
	"net/http"
	"testing"
)

// THE PRECONDITION BEHIND THIS PACKAGE'S INTERNAL-ORIGIN ALLOWLIST ENTRY, for
// its request-derived half (Connect Shopify 12.10).
//
// call_origin_conformance_test.go admits `integrations/shopify` for the
// connector's server-initiated mirror writes AND for the operatorContext writes
// Connect Shopify makes on a person's behalf -- sealing their app credentials
// and their Storefront token, and changing a store's token reference. Those are
// request-derived, and the argument for them is not that operatorContext is safe
// in general. It is that every such write is downstream of two checks: the
// store part (execute app:deployables/store), which the engine asks at the
// builtin before the handler runs, and the site checks of ResolveConnectSite,
// which the handler asks before anything else.
//
// The callback's writes (connect_write.go) meet the same two checks at step 8,
// re-asked of the person under their real role at the moment Shopify sends
// them back, and are reached only after it: the identity server asks for them
// only once AuthorizeShopifyConnect handed over a token
// (component/identity/http/shopify_callback_test.go holds that order), and
// TestWriteKeepsNothingWithoutAToken below holds the writes' own half.
//
// Both halves are measured here rather than trusted, for the reason
// integrations/identity/internal_origin_precondition_test.go gives: an
// allowlist entry whose stated reason has quietly stopped being true is worse
// than no entry.

// TestTheConnectBuiltinsDeclareTheStorePart is the first half: the engine's
// builtin gate (refuseBuiltinBelowRequiredCapability) asks what the
// declaration names, so the declaration is the check.
func TestTheConnectBuiltinsDeclareTheStorePart(t *testing.T) {
	eng := newRealEngine(t)
	for _, name := range []string{"shopifyConnectStatus", "shopifyStoreAppSave", "shopifyConnectBegin", "shopifyStorefrontTokenSet"} {
		fn, err := eng.Functions().Get(name)
		if err != nil || fn == nil {
			t.Fatalf("%s is not in the function registry: %v", name, err)
		}
		req := fn.RequiresCapability
		if req.Verb != "execute" || req.Resource != "app:deployables/store" {
			t.Errorf("%s requires %+v, want execute app:deployables/store -- without it the handler's "+
				"operatorContext writes are reachable by anybody who can write a storefront", name, req)
		}
		if !fn.IsBuiltin() {
			t.Errorf("%s is not a builtin", name)
		}
	}
}

// TestNoConnectWriteIsReachedWhenACheckRefuses is the second half.
func TestNoConnectWriteIsReachedWhenACheckRefuses(t *testing.T) {
	type handler = string
	const (
		save  handler = "save"
		token handler = "token"
		begin handler = "begin"
	)
	for _, tc := range []struct {
		name    string
		arrange func(h *connectHarness)
		edit    func(args map[string]any)
		on      []handler
		want    string
	}{
		{
			name: "no site id", edit: func(a map[string]any) { a["siteId"] = "" },
			on: []handler{save, token, begin}, want: connectReasonSiteNotWritable,
		},
		{
			name:    "a site the caller cannot read",
			arrange: func(h *connectHarness) { h.engine.setRows("siteById", nil) },
			on:      []handler{save, token, begin}, want: connectReasonSiteNotWritable,
		},
		{
			name:    "a site the caller reads and cannot write",
			arrange: func(h *connectHarness) { h.engine.writable["s1"] = false },
			on:      []handler{save, token, begin}, want: connectReasonSiteNotWritable,
		},
		{
			name:    "a site that is not a storefront",
			arrange: func(h *connectHarness) { h.site(connectSiteID, "static") },
			on:      []handler{save, token, begin}, want: connectReasonNotAStorefront,
		},
		{
			name:    "no run published the site",
			arrange: func(h *connectHarness) { h.run("v1:platform:site:another", connectShop) },
			on:      []handler{save, token, begin}, want: connectReasonStoreNotNamed,
		},
		{
			name:    "the run names something that is not a shop",
			arrange: func(h *connectHarness) { h.run(connectSiteID, "attacker.example") },
			on:      []handler{save, token, begin}, want: connectReasonStoreNotNamed,
		},
		{
			name:    "the store was purged",
			arrange: func(h *connectHarness) { h.store(map[string]any{"redactedAt": "2026-09-01T00:00:00Z"}) },
			on:      []handler{save, token, begin}, want: connectReasonStoreRedacted,
		},
		{
			name: "no client id", edit: func(a map[string]any) { a["clientId"] = "" },
			on: []handler{save}, want: connectReasonAppCredentialsInvalid,
		},
		{
			name: "no client secret", edit: func(a map[string]any) { a["clientSecret"] = " " },
			on: []handler{save}, want: connectReasonAppCredentialsInvalid,
		},
		{
			name: "two rows carry the pending secret's name",
			arrange: func(h *connectHarness) {
				name := storeSecretName(connectStoreID, suffixPendingClientSecret)
				h.engine.setRows(namedRowsQuery(conceptGlobalSecret, name), []map[string]any{
					{"id": "v1:platform:globalSecret:a", "name": name}, {"id": "v1:platform:globalSecret:b", "name": name},
				})
			},
			on: []handler{save}, want: connectReasonSecretNameAmbiguous,
		},
		{
			name: "two rows carry the pending client id's name",
			arrange: func(h *connectHarness) {
				name := storeSecretName(connectStoreID, suffixPendingClientID)
				h.engine.setRows(namedRowsQuery(conceptGlobalVariable, name), []map[string]any{
					{"id": "v1:platform:globalVariable:a", "name": name}, {"id": "v1:platform:globalVariable:b", "name": name},
				})
			},
			on: []handler{save}, want: connectReasonSecretNameAmbiguous,
		},
		{
			name: "no app saved",
			arrange: func(h *connectHarness) {
				h.engine.setRows(namedRowsQuery(conceptGlobalVariable, storeSecretName(connectStoreID, suffixPendingClientID)), nil)
			},
			on: []handler{begin}, want: connectReasonShopifyAppNotSaved,
		},
		{
			name: "a token for a store with no row",
			on:   []handler{token}, want: connectReasonStoreNotConnected,
		},
		{
			name: "a store bound to a site the caller cannot write",
			arrange: func(h *connectHarness) {
				h.store(map[string]any{"adminTokenRef": "A"})
				h.engine.setRows("sitesBoundToStore", []map[string]any{
					{"id": connectSiteID, "status": "draft", "binding": map[string]any{"storeId": connectStoreID}},
					{"id": "v1:platform:site:someone-elses", "status": "live", "previewBinding": map[string]any{"storeId": connectStoreID}},
				})
			},
			on: []handler{token}, want: connectReasonStoreInUse,
		},
		{
			name: "an empty token while a live site is bound",
			edit: func(a map[string]any) { a["token"] = "" },
			arrange: func(h *connectHarness) {
				h.store(map[string]any{"storefrontTokenRef": "S"})
				h.engine.setRows("sitesBoundToStore", []map[string]any{
					{"id": connectSiteID, "status": "live", "binding": map[string]any{"storeId": connectStoreID}},
				})
			},
			on: []handler{token}, want: connectReasonStorefrontTokenRequired,
		},
		{
			name: "a token Shopify refuses",
			arrange: func(h *connectHarness) {
				h.store(map[string]any{"adminTokenRef": "A"})
				h.storefront.status = http.StatusUnauthorized
			},
			on: []handler{token}, want: connectReasonStorefrontTokenInvalid,
		},
	} {
		for _, which := range tc.on {
			t.Run(tc.name+"/"+which, func(t *testing.T) {
				h := newConnectHarness(t)
				t.Setenv("MEMQL_IDENTITY_BASE_URL", connectIdentityBase)
				// A saved app, so a refusal below is the check's and not
				// "nothing to connect".
				h.pendingApp(connectClientID)
				if tc.arrange != nil {
					tc.arrange(h)
				}
				ctx := devCtx()
				fn := h.integ.handleStoreAppSave
				args := map[string]any{"siteId": "s1", "clientId": connectClientID, "clientSecret": connectSecret}
				switch which {
				case token:
					fn = h.integ.handleStorefrontTokenSet
					args = map[string]any{"siteId": "s1", "token": connectToken}
				case begin:
					fn = h.integ.handleConnectBegin
					args = map[string]any{"siteId": "s1", "returnPath": "/"}
				}
				if tc.edit != nil {
					tc.edit(args)
				}
				out := invoke(t, ctx, fn, args)
				if out["reason"] != tc.want {
					t.Fatalf("reason = %v, want %s", out["reason"], tc.want)
				}
				if w := h.engine.writes(); len(w) != 0 {
					t.Fatalf("a refused call wrote:\n%v", w)
				}
				if tc.want != connectReasonStorefrontTokenInvalid && h.storefront.requests() != 0 {
					t.Fatal("a refused call reached Shopify")
				}
			})
		}
	}
}

// TestACallWithNoCallerIsAnErrorNotAWrite: a write naming nobody is audited as
// nobody, so it is refused before any read.
func TestACallWithNoCallerIsAnErrorNotAWrite(t *testing.T) {
	for _, which := range []string{"save", "token", "begin"} {
		h := newConnectHarness(t)
		fn := h.integ.handleStoreAppSave
		switch which {
		case "token":
			fn = h.integ.handleStorefrontTokenSet
		case "begin":
			fn = h.integ.handleConnectBegin
		}
		if _, err := fn(context.Background(), map[string]any{"siteId": "s1", "clientId": "c", "clientSecret": "s", "token": "t"}, 0); err == nil {
			t.Fatalf("%s: a call with no caller was answered", which)
		}
		if len(h.engine.log) != 0 {
			t.Fatalf("%s: a call with no caller reached the engine: %+v", which, h.engine.log)
		}
	}
}

// TestWriteKeepsNothingWithoutAToken: a grant with no token, or no state,
// writes nothing and reaches nobody.
func TestWriteKeepsNothingWithoutAToken(t *testing.T) {
	h := newWriteHarness(t)
	g := h.grant()
	g.AccessToken = ""
	if got, kept := h.integ.connector.WriteShopifyConnect(context.Background(), callbackState(credentialSourcePending), g); got != connectReasonExchangeFailed || kept != "" {
		t.Fatalf("result = %q kept %q", got, kept)
	}
	if w := h.engine.writes(); len(w) != 0 {
		t.Fatalf("wrote %v", w)
	}
	if len(h.admin.seen()) != 0 {
		t.Fatal("reached Shopify")
	}
	if got, _ := h.integ.connector.WriteShopifyConnect(context.Background(), nil, h.grant()); got != connectReasonStateInvalid {
		t.Fatal("a nil state was written")
	}
	// A pending approval with no secret to promote: nothing to keep the
	// approved app with, so nothing is kept at all.
	g = h.grant()
	g.ClientSecret = ""
	if got, _ := h.integ.connector.WriteShopifyConnect(context.Background(), callbackState(credentialSourcePending), g); got != connectReasonExchangeFailed {
		t.Fatalf("a pending grant with no secret: result = %q", got)
	}
	if w := h.engine.writes(); len(w) != 0 {
		t.Fatalf("a pending grant with no secret wrote %v", w)
	}
}
