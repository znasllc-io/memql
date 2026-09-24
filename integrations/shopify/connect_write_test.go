package shopify

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	componentIdentity "github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// connect_write_test.go -- the callback's writes (design 12.6 steps 11-15)
// against the recording engine and a fake Shopify Admin API: which statements
// they build, in what order, under whose identity, and what they never carry.
// connect_hop_db_test.go runs the same writes against a real engine, across
// two engines, through the identity node's real route.

const writeMintedToken = "minted-storefront-token-value"

// writeHarness is the callback harness with a fake Admin API answering the
// four operations the writes make.
type writeHarness struct {
	*callbackHarness
	admin *fakeAdmin
}

func newWriteHarness(t *testing.T) *writeHarness {
	t.Helper()
	h := &writeHarness{callbackHarness: newCallbackHarness(t), admin: newFakeAdmin(t)}
	conn := h.integ.connector
	conn.admin.endpoint = func(Store) string { return h.admin.server.URL + "/graphql" }
	conn.admin.sleep = func(context.Context, time.Duration) error { return nil }
	conn.deliver = func(s Store) string { return "https://api.example.test/inbound/shopify-" + s.ID }
	conn.background = func(f func()) { f() }
	// The Admin token the writes seal is what the webhook reconcile resolves
	// back; the recording engine keeps no rows, so the resolver answers it.
	h.secrets[storeSecretName(connectStoreID, suffixAdminToken)] = callbackAdminToken
	h.admin.reply("ShopifyConnectPlan", map[string]any{"shop": map[string]any{"plan": map[string]any{"publicDisplayName": "Basic"}}})
	h.admin.reply("ShopifyStorefrontTokenCreate", map[string]any{"storefrontAccessTokenCreate": map[string]any{
		"storefrontAccessToken": map[string]any{"accessToken": writeMintedToken}, "userErrors": []any{},
	}})
	h.admin.reply("ShopifyWebhookSubscriptions", map[string]any{"webhookSubscriptions": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": false}, "nodes": []any{},
	}})
	h.admin.reply("ShopifyWebhookCreate", map[string]any{"webhookSubscriptionCreate": map[string]any{
		"webhookSubscription": map[string]any{"id": "gid://shopify/WebhookSubscription/1"}, "userErrors": []any{},
	}})
	h.pendingApp(connectClientID)
	return h
}

func (h *writeHarness) grant() componentIdentity.ShopifyConnectGrant {
	return componentIdentity.ShopifyConnectGrant{
		StoreID: connectStoreID, AccessToken: callbackAdminToken, ClientSecret: connectSecret, Scopes: ConnectScopes(), Role: string(auth.RoleDeveloper),
	}
}

func (h *writeHarness) write(source string) string {
	result, _ := h.writeKept(source)
	return result
}

// writeKept is write with what the writes report they kept.
func (h *writeHarness) writeKept(source string) (string, string) {
	return h.integ.connector.WriteShopifyConnect(context.Background(), callbackState(source), h.grant())
}

// statementIndex is the position of the first statement naming `name` whose
// text contains every fragment, or -1.
func (h *writeHarness) statementIndex(name string, fragments ...string) int {
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
next:
	for i, c := range h.engine.log {
		if callName(c.query) != name {
			continue
		}
		for _, f := range fragments {
			if !strings.Contains(c.query, f) {
				continue next
			}
		}
		return i
	}
	return -1
}

// assertNoCredential fails when any statement, log line or Admin request URL
// carries a credential's plaintext.
func (h *writeHarness) assertNoCredential(t *testing.T) {
	t.Helper()
	secrets := []string{callbackAdminToken, connectSecret, writeMintedToken, callbackCode}
	h.engine.mu.Lock()
	log := append([]connectCall(nil), h.engine.log...)
	h.engine.mu.Unlock()
	for _, c := range log {
		for _, s := range secrets {
			if strings.Contains(c.query, s) {
				t.Errorf("a statement carries a credential: %s", c.query)
			}
		}
	}
	for _, s := range secrets {
		if strings.Contains(h.logs.String(), s) {
			t.Errorf("a credential reached the log:\n%s", h.logs.String())
		}
	}
}

func q(s string) string { return `"` + s + `"` }

var sealedValueRe = regexp.MustCompile(`encryptedValue: "([^"]*)"`)

// sealedValue is the plaintext the first setGlobalSecret for name sealed, or
// "" when none did.
func (h *writeHarness) sealedValue(t *testing.T, name string) string {
	t.Helper()
	i := h.statementIndex("setGlobalSecret", q(name))
	if i < 0 {
		return ""
	}
	h.engine.mu.Lock()
	stmt := h.engine.log[i].query
	h.engine.mu.Unlock()
	m := sealedValueRe.FindStringSubmatch(stmt)
	if m == nil || m[1] == "" {
		return ""
	}
	plain, err := secret.Decrypt(m[1])
	if err != nil {
		t.Fatalf("open the value sealed for %s: %v", name, err)
	}
	return plain
}

// TestASaveBetweenAuthorizeAndWriteIsNotPromoted is D12's race: a Save landing
// after Shopify approved one app and before the writes keep it. What becomes the
// live webhook secret is the secret that verified the callback and exchanged the
// code -- the approved app's -- never whatever the pending row holds by then,
// and the racing Save stays pending rather than being wiped as if it had been
// promoted.
func TestASaveBetweenAuthorizeAndWriteIsNotPromoted(t *testing.T) {
	const racingSecret = "shpss_the_racing_secret_value"
	h := newWriteHarness(t)
	c := h.integ.connector
	state := callbackState(credentialSourcePending)

	grant, result := c.AuthorizeShopifyConnect(context.Background(), state, callbackCode)
	if result != "" {
		t.Fatalf("authorize refused %q", result)
	}
	// The racing Save: another app's pair, sealed over the pending rows.
	h.secrets[callbackPendingName] = racingSecret
	h.pendingApp("racing-client")

	if got, _ := c.WriteShopifyConnect(context.Background(), state, grant); got != connectResultConnected {
		t.Fatalf("result = %q, want connected", got)
	}
	webhookName := storeSecretName(connectStoreID, suffixWebhookSecret)
	if got := h.sealedValue(t, webhookName); got != connectSecret {
		if got == racingSecret {
			t.Fatal("the racing Save became the live webhook secret without an approval")
		}
		t.Fatalf("the promoted webhook secret is not the approved app's (sealed %d bytes)", len(got))
	}
	if h.statementIndex("createStore", `appClientId: "`+connectClientID+`"`) < 0 {
		t.Error("the promoted app is not the one Shopify approved")
	}
	pendingSecret := storeSecretName(connectStoreID, suffixPendingClientSecret)
	pendingID := storeSecretName(connectStoreID, suffixPendingClientID)
	if h.statementIndex("setGlobalSecret", q(pendingSecret), `encryptedValue: ""`) >= 0 ||
		h.statementIndex("setGlobalVariable", q(pendingID), `value: ""`) >= 0 {
		t.Error("the racing Save was wiped as though it had been approved")
	}
	h.assertNoCredential(t)
	if strings.Contains(h.logs.String(), racingSecret) {
		t.Error("the racing secret reached the log")
	}
}

func TestAFirstConnectKeepsEverythingItWasGiven(t *testing.T) {
	h := newWriteHarness(t)
	adminName := storeSecretName(connectStoreID, suffixAdminToken)
	webhookName := storeSecretName(connectStoreID, suffixWebhookSecret)
	storefrontName := storeSecretName(connectStoreID, suffixStorefrontToken)

	if got := h.write(credentialSourcePending); got != "connected" {
		t.Fatalf("result = %q, want connected", got)
	}

	// 11. The Admin token, and the pending secret promoted to the name the
	// first-boot seed uses.
	sealAdmin := h.statementIndex("setGlobalSecret", q(adminName))
	sealWebhook := h.statementIndex("setGlobalSecret", q(webhookName))
	if sealAdmin < 0 || sealWebhook < 0 {
		t.Fatalf("the Admin token (%d) or the promoted secret (%d) was not sealed", sealAdmin, sealWebhook)
	}
	if h.statementIndex("setGlobalSecret", q(adminName), `addedBy: "`+connectDev+`"`) < 0 {
		t.Error("the sealed Admin token does not name the person who connected as addedBy")
	}

	// 12. One store row, created because none existed, complete in one write.
	creates := h.engine.callsNamed("createStore")
	if len(creates) != 1 {
		t.Fatalf("createStore ran %d times, want 1", len(creates))
	}
	create := creates[0].query
	for _, want := range []string{
		`storeId: "` + connectStoreID + `"`, `domain: "` + connectShop + `"`,
		`appClientId: "` + connectClientID + `"`, `adminTokenRef: "` + adminName + `"`,
		`webhookSecretRef: "` + webhookName + `"`, `apiVersion: "` + generated.APIVersion + `"`,
		`plan: "Basic"`, `ownerUserId: "` + connectDev + `"`,
	} {
		if !strings.Contains(create, want) {
			t.Errorf("createStore lacks %s: %s", want, create)
		}
	}
	createAt := h.statementIndex("createStore")
	if createAt < sealAdmin || createAt < sealWebhook {
		t.Error("the store row was written before the secrets it points at")
	}
	if h.statementIndex("createStore", `scopesGranted: [`, `"`+StorefrontScopes[0]+`"`) < 0 {
		t.Error("the granted scopes were not recorded")
	}

	// The pending pair is cleared by overwriting it with blanks -- and only
	// AFTER the store row points at what it was promoted to.
	pendingSecret := storeSecretName(connectStoreID, suffixPendingClientSecret)
	pendingID := storeSecretName(connectStoreID, suffixPendingClientID)
	clearSecret := h.statementIndex("setGlobalSecret", q(pendingSecret), `encryptedValue: ""`, `active: false`, `id: "sec-p"`)
	clearID := h.statementIndex("setGlobalVariable", q(pendingID), `value: ""`, `active: false`, `id: "var-p"`)
	if clearSecret < 0 || clearID < 0 {
		t.Fatalf("the pending pair was not cleared at its own rows (secret %d, id %d)", clearSecret, clearID)
	}
	if clearSecret < createAt || clearID < createAt {
		t.Error("the pending pair was cleared before the store row was written: a failure between them would leave nothing to Connect with")
	}

	// 13. A Storefront token minted, sealed and pointed at straight away.
	if h.admin.countOp("ShopifyStorefrontTokenCreate") != 1 {
		t.Fatalf("minted %d Storefront tokens, want 1", h.admin.countOp("ShopifyStorefrontTokenCreate"))
	}
	for _, r := range h.admin.seen() {
		if r.Operation == "ShopifyStorefrontTokenCreate" {
			input, _ := r.Variables["input"].(map[string]any)
			if input["title"] != "MemQL storefront" {
				t.Errorf("the mint's input is %v", r.Variables)
			}
		}
		if r.Token != callbackAdminToken {
			t.Errorf("%s was sent with token %q, want the one Shopify issued", r.Operation, r.Token)
		}
	}
	sealSF := h.statementIndex("setGlobalSecret", q(storefrontName))
	pointSF := h.statementIndex("updateStore", `storefrontTokenRef: "`+storefrontName+`"`)
	if sealSF < 0 || pointSF < sealSF {
		t.Errorf("the Storefront token was not sealed then pointed at (seal %d, ref %d)", sealSF, pointSF)
	}

	// 14. The attach, under the PERSON, never borrowed and never internal.
	attaches := h.engine.callsNamed("updateSiteStoreBinding")
	if len(attaches) != 1 {
		t.Fatalf("updateSiteStoreBinding ran %d times, want 1", len(attaches))
	}
	a := attaches[0]
	if a.userId != connectDev || a.role != auth.RoleDeveloper || a.internal {
		t.Errorf("the attach ran as %+v, want the developer under their own role", a)
	}
	if !strings.Contains(a.query, `storeId: "`+connectStoreID+`"`) || !strings.Contains(a.query, `siteId: "s1"`) {
		t.Errorf("the attach names %s", a.query)
	}

	// 15. The webhooks, and their health recorded.
	if h.admin.countOp("ShopifyWebhookCreate") != len(generated.SubscribedTopics) {
		t.Errorf("created %d subscriptions, want %d", h.admin.countOp("ShopifyWebhookCreate"), len(generated.SubscribedTopics))
	}
	if len(h.engine.callsNamed("recordStoreHealth")) != 1 {
		t.Error("the subscription health was not recorded")
	}

	// Every write but the attach is the deployment's.
	for _, name := range []string{"setGlobalSecret", "setGlobalVariable", "createStore", "updateStore", "recordStoreHealth"} {
		for _, c := range h.engine.callsNamed(name) {
			if c.role != auth.RoleOwner || !c.internal || c.userId == connectDev {
				t.Errorf("%s ran as %+v, want operatorContext", name, c)
			}
		}
	}
	h.assertNoCredential(t)
}

func TestAReconnectUpdatesTheStoreInPlace(t *testing.T) {
	h := newWriteHarness(t)
	h.engine.setRows(namedRowsQuery(conceptGlobalVariable, storeSecretName(connectStoreID, suffixPendingClientID)), nil)
	webhookName := storeSecretName(connectStoreID, suffixWebhookSecret)
	h.secrets[webhookName] = "the-current-secret"
	h.store(map[string]any{
		"appClientId": connectClientID, "webhookSecretRef": webhookName, "ownerUserId": "the-first-owner",
		"adminTokenRef": storeSecretName(connectStoreID, suffixAdminToken), "storefrontTokenRef": "SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN",
	})
	h.engine.setRows("siteById", []map[string]any{{
		"id": connectSiteID, "kind": "shopify_storefront", "status": "draft", "ownerUserId": connectDev,
		"packageId": "v1:platform:package:p1", "packageDeployableName": "storefront",
		"binding": map[string]any{"storeId": connectStoreID},
	}})

	if got, kept := h.writeKept(credentialSourceCurrent); got != "reconnected" || kept != "reconnected" {
		t.Fatalf("result = %q kept %q, want reconnected and kept", got, kept)
	}
	if n := len(h.engine.callsNamed("createStore")); n != 0 {
		t.Fatalf("a reconnect ran createStore %d times: it re-stamps status, scopes and health", n)
	}
	var update string
	for _, c := range h.engine.callsNamed("updateStore") {
		if strings.Contains(c.query, `adminTokenRef:`) {
			update = c.query
		}
	}
	if update == "" {
		t.Fatal("the store row was not updated with the new Admin token")
	}
	for _, absent := range []string{`ownerUserId:`, `appClientId:`, `webhookSecretRef:`} {
		if strings.Contains(update, absent) {
			t.Errorf("a reconnect with the current app wrote %s: %s", absent, update)
		}
	}
	if h.statementIndex("setGlobalSecret", q(webhookName)) >= 0 {
		t.Error("a reconnect with the current app re-sealed the webhook secret")
	}
	if n := len(h.engine.callsNamed("setGlobalVariable")); n != 0 {
		t.Error("a reconnect with the current app touched the pending rows")
	}
	if h.admin.countOp("ShopifyStorefrontTokenCreate") != 0 {
		t.Error("a Storefront token was minted for a store that has one")
	}
	if n := len(h.engine.callsNamed("updateSiteStoreBinding")); n != 0 {
		t.Error("an attach already in place was written again")
	}
	h.assertNoCredential(t)
}

func TestTheOwnerIsSetOnlyWhenTheStoreHasNone(t *testing.T) {
	h := newWriteHarness(t)
	h.store(map[string]any{"appClientId": "old", "webhookSecretRef": "W", "storefrontTokenRef": "S"})
	if got := h.write(credentialSourcePending); got != "connected" {
		t.Fatalf("result = %q", got)
	}
	if h.statementIndex("updateStore", `ownerUserId: "`+connectDev+`"`) < 0 {
		t.Error("a store with no owner did not get the person who connected it")
	}
	if len(h.engine.callsNamed("createStore")) != 0 {
		t.Error("an existing row was created again")
	}
	// Pending promoted onto an existing row: its app and webhook secret move.
	if h.statementIndex("updateStore", `appClientId: "`+connectClientID+`"`, `webhookSecretRef: "`+storeSecretName(connectStoreID, suffixWebhookSecret)+`"`) < 0 {
		t.Error("the promoted app was not written onto the existing store")
	}
}

func TestAFailedMintKeepsOnlyTheAdminConnection(t *testing.T) {
	for name, arrange := range map[string]func(h *writeHarness){
		"userErrors": func(h *writeHarness) {
			h.admin.userError("ShopifyStorefrontTokenCreate", "storefrontAccessTokenCreate", "Access denied "+connectSecret)
		},
		"no token": func(h *writeHarness) {
			h.admin.reply("ShopifyStorefrontTokenCreate", map[string]any{"storefrontAccessTokenCreate": map[string]any{"userErrors": []any{}}})
		},
		"refused": func(h *writeHarness) { h.admin.httpStatus("ShopifyStorefrontTokenCreate", http.StatusForbidden) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newWriteHarness(t)
			arrange(h)
			got, kept := h.writeKept(credentialSourcePending)
			if got != "storefront_token_failed" {
				t.Fatalf("result = %q, want storefront_token_failed", got)
			}
			// The Admin connection WAS kept, and the audit has to say so.
			if kept != connectResultConnected {
				t.Errorf("kept = %q, want connected: the store's credentials changed", kept)
			}
			if h.statementIndex("createStore", `adminTokenRef:`) < 0 {
				t.Fatal("the Admin connection was not saved before the mint")
			}
			if h.statementIndex("setGlobalSecret", q(storeSecretName(connectStoreID, suffixStorefrontToken))) >= 0 ||
				h.statementIndex("updateStore", `storefrontTokenRef:`) >= 0 {
				t.Error("a failed mint wrote a Storefront token")
			}
			if n := len(h.engine.callsNamed("updateSiteStoreBinding")); n != 0 {
				t.Error("a failed mint went on to attach")
			}
			if h.admin.countOp("ShopifyWebhookSubscriptions") != 0 || len(h.engine.callsNamed("recordStoreHealth")) != 0 {
				t.Error("a failed mint went on to the webhooks")
			}
			h.assertNoCredential(t)
		})
	}
}

func TestAWebhookFailureDoesNotUndoTheConnection(t *testing.T) {
	h := newWriteHarness(t)
	h.admin.httpStatus("ShopifyWebhookSubscriptions", http.StatusForbidden)
	if got := h.write(credentialSourcePending); got != "connected" {
		t.Fatalf("result = %q, want connected", got)
	}
	health := h.engine.callsNamed("recordStoreHealth")
	if len(health) != 1 || !strings.Contains(health[0].query, `"failed": [`) {
		t.Fatalf("the failure was not recorded on the store's health: %v", health)
	}
	if strings.Contains(health[0].query, `"failed": []`) {
		t.Errorf("the health records no failure: %s", health[0].query)
	}
	h.assertNoCredential(t)
}

func TestARefusedAttachIsPermissionLost(t *testing.T) {
	h := newWriteHarness(t)
	h.engine.fail["updateSiteStoreBinding"] = errors.New("v1:platform:site: capability_not_held")
	if got, kept := h.writeKept(credentialSourcePending); got != connectReasonPermissionLost || kept != connectResultConnected {
		t.Fatalf("result = %q kept %q, want %s with the connection kept", got, kept, connectReasonPermissionLost)
	}
	if h.admin.countOp("ShopifyWebhookSubscriptions") != 0 {
		t.Error("a refused attach went on to the webhooks")
	}
}

// Step 15 is handed off rather than run inline: every subscribed topic is one
// paced Admin call, and a browser is parked on the callback's redirect.
func TestTheWebhooksDoNotHoldTheRedirect(t *testing.T) {
	h := newWriteHarness(t)
	var deferred []func()
	h.integ.connector.background = func(f func()) { deferred = append(deferred, f) }
	if got := h.write(credentialSourcePending); got != "connected" {
		t.Fatalf("result = %q", got)
	}
	if h.admin.countOp("ShopifyWebhookSubscriptions") != 0 || len(h.engine.callsNamed("recordStoreHealth")) != 0 {
		t.Fatal("the webhooks were registered before the callback answered")
	}
	if len(deferred) != 1 {
		t.Fatalf("%d background jobs, want 1", len(deferred))
	}
	deferred[0]()
	if h.admin.countOp("ShopifyWebhookCreate") != len(generated.SubscribedTopics) || len(h.engine.callsNamed("recordStoreHealth")) != 1 {
		t.Fatal("the handed-off job did not register the webhooks and record their health")
	}
}

// TestAStoreRowThatCouldNotBeWrittenKeepsNothing: a failure inside steps 11-12
// reports nothing kept, so the trail records a refusal rather than a
// connection the store row never took.
func TestAStoreRowThatCouldNotBeWrittenKeepsNothing(t *testing.T) {
	h := newWriteHarness(t)
	h.engine.fail["createStore"] = errors.New("the database went away")
	if got, kept := h.writeKept(credentialSourcePending); got != connectReasonExchangeFailed || kept != "" {
		t.Fatalf("result = %q kept %q, want %s and nothing kept", got, kept, connectReasonExchangeFailed)
	}
}

func TestReconnectAuditsResealedCredentialsWhenStoreUpdateFails(t *testing.T) {
	h := newWriteHarness(t)
	h.store(map[string]any{"adminTokenRef": storeSecretName(connectStoreID, suffixAdminToken)})
	h.engine.fail["updateStore"] = errors.New("store write failed")
	if got, kept := h.writeKept(credentialSourceCurrent); got != connectReasonExchangeFailed || kept != connectResultReconnected {
		t.Fatalf("result = %q kept %q; an already referenced credential changed", got, kept)
	}
}
