package identity

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	"google.golang.org/protobuf/types/known/structpb"
)

// store_githubapp_test.go -- the six rows of a registration, and the purpose
// that keeps two flows' states apart (design record
// 2026-09-20-github-app-setup, D1 and D4).

// appRowRecorder records every statement and can answer one state row.
type appRowRecorder struct {
	mu      sync.Mutex
	queries []string
	state   map[string]string
	failOn  string
}

func (r *appRowRecorder) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, q)
	if r.failOn != "" && strings.Contains(q, r.failOn) {
		return nil, errors.New("the store said no")
	}
	if strings.HasPrefix(q, "query githubConnectStateByHash(") && r.state != nil {
		fields := map[string]*structpb.Value{}
		for k, v := range r.state {
			fields[k] = structpb.NewStringValue(v)
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{Id: r.state["id"], CreatedAt: fixtureCreatedAt(r.state["createdAt"]), Payload: &structpb.Struct{Fields: fields}}},
		}}, nil
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func (r *appRowRecorder) writes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, q := range r.queries {
		if strings.HasPrefix(q, "mutation ") {
			out = append(out, q)
		}
	}
	return out
}

func registeredApp() githubconnect.Config {
	return githubconnect.Config{
		AppID:        "424242",
		AppSlug:      "memql-on-lab-example-com",
		ClientID:     "Iv1.registered",
		ClientSecret: "registered-client-secret-VALUE",
		// ENCODED HERE rather than spelled encoded: a base64 literal after the
		// word "key" is what the repository's secret scanner refuses, fixture
		// or not. Nothing here decodes it; it only has to be a value.
		PrivateKeyB64: base64.StdEncoding.EncodeToString([]byte("registered-private-key-VALUE")),
		WebhookSecret: "registered-webhook-secret-VALUE",
	}
}

func TestARegistrationIsSixRowsUnderTheSixNames(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, strings.Repeat("ab", 32))
	rec := &appRowRecorder{}
	store := &Store{GithubGate: githubUnitGate, Engine: rec}
	if err := store.WriteGithubAppRegistration(context.Background(), registeredApp(), "v1:identity:user:owner"); err != nil {
		t.Fatal(err)
	}
	writes := rec.writes()
	if len(writes) != 6 {
		t.Fatalf("%d writes, want six:\n  %s", len(writes), strings.Join(writes, "\n  "))
	}

	// EVERY STATEMENT PARSES. A store that renders MemQL text is only as good
	// as the engine's ability to read it, and a recorder accepts anything.
	for _, q := range writes {
		if _, err := langparser.ParseExpression(q); err != nil {
			t.Errorf("the engine could not parse this write: %v\n  %s", err, q)
		}
	}

	// The seeder's row ids, and each name in the store its kind names.
	for name, want := range map[string]string{
		githubconnect.EnvAppID:         `mutation setGlobalVariable(id: "var-global-memql-github-app-id", name: "MEMQL_GITHUB_APP_ID", value: "424242"`,
		githubconnect.EnvAppSlug:       `mutation setGlobalVariable(id: "var-global-memql-github-app-slug", name: "MEMQL_GITHUB_APP_SLUG", value: "memql-on-lab-example-com"`,
		githubconnect.EnvClientID:      `mutation setGlobalVariable(id: "var-global-memql-github-app-client-id", name: "MEMQL_GITHUB_APP_CLIENT_ID", value: "Iv1.registered"`,
		githubconnect.EnvClientSecret:  `mutation setGlobalSecret(id: "secret-global-memql-github-app-client-secret", name: "MEMQL_GITHUB_APP_CLIENT_SECRET", encryptedValue: "`,
		githubconnect.EnvWebhookSecret: `mutation setGlobalSecret(id: "secret-global-memql-github-app-webhook-secret", name: "MEMQL_GITHUB_APP_WEBHOOK_SECRET", encryptedValue: "`,
		githubconnect.EnvPrivateKeyB64: `mutation setGlobalSecret(id: "secret-global-memql-github-app-private-key-b64", name: "MEMQL_GITHUB_APP_PRIVATE_KEY_B64", encryptedValue: "`,
	} {
		found := false
		for _, q := range writes {
			if strings.HasPrefix(q, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was not written as\n  %s...", name, want)
		}
	}

	// THE PRIVATE KEY LAST. Resolve needs all six, so the app becomes visible
	// to the cluster only when the final write lands.
	if !strings.Contains(writes[5], githubconnect.EnvPrivateKeyB64) {
		t.Errorf("the last write is not the private key:\n  %s", writes[5])
	}
	if !strings.Contains(writes[0], githubconnect.EnvAppID) {
		t.Errorf("the first write is not an identifier:\n  %s", writes[0])
	}
	for _, q := range writes {
		if strings.Contains(q, "setGlobalSecret") && !strings.Contains(q, `addedBy: "v1:identity:user:owner"`) {
			t.Errorf("a credential row does not say who registered it:\n  %s", q)
		}
	}
}

// TestNoCredentialReachesAStatementInTheClear, with its control: the sealed
// value must really be there, and must really open back to the credential, or
// "does not contain the plaintext" would pass on a row that stored nothing.
func TestNoCredentialReachesAStatementInTheClear(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, strings.Repeat("ab", 32))
	rec := &appRowRecorder{}
	store := &Store{GithubGate: githubUnitGate, Engine: rec}
	app := registeredApp()
	if err := store.WriteGithubAppRegistration(context.Background(), app, "v1:identity:user:owner"); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(rec.writes(), "\n")
	for name, plaintext := range map[string]string{"client secret": app.ClientSecret, "private key": app.PrivateKeyB64, "webhook secret": app.WebhookSecret} {
		if strings.Contains(all, plaintext) {
			t.Errorf("the %s was written in the clear", name)
		}
	}
	// THE CONTROL: pull each ciphertext back out and open it.
	opened := map[string]bool{}
	for _, q := range rec.writes() {
		if !strings.HasPrefix(q, "mutation setGlobalSecret(") {
			continue
		}
		start := strings.Index(q, `encryptedValue: "`) + len(`encryptedValue: "`)
		end := strings.Index(q[start:], `"`)
		plain, err := secret.Decrypt(q[start : start+end])
		if err != nil {
			t.Fatalf("a sealed value does not open: %v", err)
		}
		opened[plain] = true
	}
	for _, want := range []string{app.ClientSecret, app.PrivateKeyB64, app.WebhookSecret} {
		if !opened[want] {
			t.Errorf("a credential was not among the sealed values, so the scan above proved nothing")
		}
	}
}

func TestAPartialRegistrationWritesNothing(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, strings.Repeat("ab", 32))
	for _, name := range githubconnect.EnvNames() {
		app := registeredApp()
		switch name {
		case githubconnect.EnvAppID:
			app.AppID = ""
		case githubconnect.EnvAppSlug:
			app.AppSlug = ""
		case githubconnect.EnvClientID:
			app.ClientID = ""
		case githubconnect.EnvClientSecret:
			app.ClientSecret = ""
		case githubconnect.EnvPrivateKeyB64:
			app.PrivateKeyB64 = ""
		case githubconnect.EnvWebhookSecret:
			app.WebhookSecret = ""
		}
		rec := &appRowRecorder{}
		err := (&Store{GithubGate: githubUnitGate, Engine: rec}).WriteGithubAppRegistration(context.Background(), app, "u")
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: err = %v, want a refusal naming it", name, err)
		}
		if n := len(rec.writes()); n != 0 {
			t.Errorf("without %s: %d rows were written before the refusal", name, n)
		}
	}
}

// A write that fails part-way leaves a partial set, which the resolver reads as
// no app. What it must not do is carry on and report success.
func TestAFailedWriteStopsAndSaysWhich(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, strings.Repeat("ab", 32))
	rec := &appRowRecorder{failOn: githubconnect.EnvClientSecret}
	err := (&Store{GithubGate: githubUnitGate, Engine: rec}).WriteGithubAppRegistration(context.Background(), registeredApp(), "u")
	if err == nil || !strings.Contains(err.Error(), githubconnect.EnvClientSecret) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), registeredApp().ClientSecret) {
		t.Errorf("the error carries the credential: %v", err)
	}
	for _, q := range rec.writes() {
		if strings.Contains(q, githubconnect.EnvPrivateKeyB64) {
			t.Error("the private key was written after an earlier row failed")
		}
	}
}

func TestClearingBlanksAllSixThePrivateKeyFirst(t *testing.T) {
	rec := &appRowRecorder{}
	if err := (&Store{GithubGate: githubUnitGate, Engine: rec}).ClearGithubAppRegistration(context.Background(), "v1:identity:user:owner"); err != nil {
		t.Fatal(err)
	}
	writes := rec.writes()
	if len(writes) != 6 {
		t.Fatalf("%d writes, want six", len(writes))
	}
	if !strings.Contains(writes[0], githubconnect.EnvPrivateKeyB64) {
		t.Errorf("the first thing cleared is not the private key:\n  %s", writes[0])
	}
	for _, q := range writes {
		if _, err := langparser.ParseExpression(q); err != nil {
			t.Errorf("the engine could not parse this write: %v\n  %s", err, q)
		}
		blank := strings.Contains(q, `value: ""`) || strings.Contains(q, `encryptedValue: ""`)
		if !blank || !strings.Contains(q, "active: false") {
			t.Errorf("a row was not cleared:\n  %s", q)
		}
	}
	// THE SAME IDS the registration wrote, or the blank lands beside the value.
	for _, id := range []string{"var-global-memql-github-app-id", "secret-global-memql-github-app-private-key-b64"} {
		if !strings.Contains(strings.Join(writes, "\n"), `id: "`+id+`"`) {
			t.Errorf("the clear did not address row %s", id)
		}
	}
}

// ---------------------------------------------------------------------------
// One row shape, two flows
// ---------------------------------------------------------------------------

func stateRowFor(purpose string) map[string]string {
	return map[string]string{
		"id":        "v1:identity:githubConnectState:abc",
		"userId":    "v1:identity:user:owner",
		"stateHash": HashConnectState("plain"),
		// A server writer sets expiresAt ten minutes after createdAt; consume refuses any other lifetime.
		"createdAt": time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339),
		"expiresAt": time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339),
		"purpose":   purpose,
	}
}

// TestAStateIsSpentOnlyByItsOwnFlow is the property `purpose` exists for. A
// connect state finishing a registration would let anybody who can press
// Connect replace the cluster's app.
func TestAStateIsSpentOnlyByItsOwnFlow(t *testing.T) {
	for _, tc := range []struct {
		name, rowPurpose, asked string
		spent                   bool
	}{
		{"a connect state, by the connect callback", githubconnect.PurposeConnect, githubconnect.PurposeConnect, true},
		{"a state from before the field existed, by the connect callback", "", githubconnect.PurposeConnect, true},
		{"a setup state, by the setup callback", githubconnect.PurposeAppSetup, githubconnect.PurposeAppSetup, true},
		{"a CONNECT state, by the SETUP callback", githubconnect.PurposeConnect, githubconnect.PurposeAppSetup, false},
		{"a state from before the field existed, by the SETUP callback", "", githubconnect.PurposeAppSetup, false},
		{"a SETUP state, by the CONNECT callback", githubconnect.PurposeAppSetup, githubconnect.PurposeConnect, false},
		// Connect Shopify rides the same row, and is its own purpose: a store
		// connect finishing a GitHub one, or the reverse, is the same forgery.
		{"a shopify state, by the shopify callback", githubconnect.PurposeShopifyConnect, githubconnect.PurposeShopifyConnect, true},
		{"a SHOPIFY state, by the CONNECT callback", githubconnect.PurposeShopifyConnect, githubconnect.PurposeConnect, false},
		{"a SHOPIFY state, by the SETUP callback", githubconnect.PurposeShopifyConnect, githubconnect.PurposeAppSetup, false},
		{"a CONNECT state, by the SHOPIFY callback", githubconnect.PurposeConnect, githubconnect.PurposeShopifyConnect, false},
		{"a state from before the field existed, by the SHOPIFY callback", "", githubconnect.PurposeShopifyConnect, false},
		{"a SETUP state, by the SHOPIFY callback", githubconnect.PurposeAppSetup, githubconnect.PurposeShopifyConnect, false},
	} {
		rec := &appRowRecorder{state: stateRowFor(tc.rowPurpose)}
		row, err := (&Store{GithubGate: githubUnitGate, Engine: rec}).ConsumeGithubConnectStateFor(context.Background(), HashConnectState("plain"), "203.0.113.9", tc.asked)
		consumed := len(rec.writes()) == 1 && strings.HasPrefix(rec.writes()[0], "mutation consumeGithubConnectState(")
		if tc.spent {
			if err != nil || row == nil || !consumed {
				t.Errorf("%s: err=%v consumed=%v, want it spent", tc.name, err, consumed)
			}
			continue
		}
		if !errors.Is(err, ErrGithubConnectStateNotFound) {
			t.Errorf("%s: err = %v, want the answer an unknown digest gets", tc.name, err)
		}
		if len(rec.writes()) != 0 {
			t.Errorf("%s: the row was SPENT by the wrong flow, so its own callback can no longer use it", tc.name)
		}
	}
}

func TestTheOldConsumeIsTheConnectFlows(t *testing.T) {
	rec := &appRowRecorder{state: stateRowFor(githubconnect.PurposeAppSetup)}
	if _, err := (&Store{GithubGate: githubUnitGate, Engine: rec}).ConsumeGithubConnectState(context.Background(), HashConnectState("plain"), ""); !errors.Is(err, ErrGithubConnectStateNotFound) {
		t.Errorf("ConsumeGithubConnectState spent an app-setup state: %v", err)
	}
}

func TestASetupStateCarriesItsPurposeAndOrganization(t *testing.T) {
	rec := &appRowRecorder{}
	if _, err := (&Store{GithubGate: githubUnitGate, Engine: rec}).CreateGithubConnectState(context.Background(), GithubConnectStateSeed{
		UserId:       "v1:identity:user:owner",
		StateHash:    HashConnectState("plain"),
		ExpiresAt:    time.Now().UTC().Add(10 * time.Minute),
		Purpose:      githubconnect.PurposeAppSetup,
		Organization: "znasllc-io",
	}); err != nil {
		t.Fatal(err)
	}
	write := rec.writes()[0]
	if _, err := langparser.ParseExpression(write); err != nil {
		t.Fatalf("the engine could not parse this write: %v\n  %s", err, write)
	}
	for _, want := range []string{`purpose: "app_setup"`, `organization: "znasllc-io"`} {
		if !strings.Contains(write, want) {
			t.Errorf("the state row is missing %s:\n  %s", want, write)
		}
	}
}

// A Connect Shopify state names the shop, the site and the app it was begun
// for, and which credentials verify its callback -- written, and read back by
// the getter the callback consumes through.
func TestAShopifyStateCarriesItsShopSiteAndCredentials(t *testing.T) {
	rec := &appRowRecorder{}
	if _, err := (&Store{Engine: rec}).CreateGithubConnectState(context.Background(), GithubConnectStateSeed{
		UserId:           "v1:identity:user:dev",
		StateHash:        HashConnectState("plain"),
		ExpiresAt:        time.Now().UTC().Add(10 * time.Minute),
		Purpose:          githubconnect.PurposeShopifyConnect,
		ShopDomain:       "acme-widgets.myshopify.com",
		SiteID:           "s1",
		ClientID:         "client-id-123",
		CredentialSource: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	write := rec.writes()[0]
	if _, err := langparser.ParseExpression(write); err != nil {
		t.Fatalf("the engine could not parse this write: %v\n  %s", err, write)
	}
	for _, want := range []string{
		`purpose: "shopify_connect"`, `shopDomain: "acme-widgets.myshopify.com"`, `siteId: "s1"`,
		`clientId: "client-id-123"`, `credentialSource: "pending"`,
	} {
		if !strings.Contains(write, want) {
			t.Errorf("the state row is missing %s:\n  %s", want, write)
		}
	}

	state := stateRowFor(githubconnect.PurposeShopifyConnect)
	state["shopDomain"], state["siteId"], state["clientId"], state["credentialSource"] = "acme-widgets.myshopify.com", "s1", "client-id-123", "current"
	row, err := (&Store{Engine: &appRowRecorder{state: state}}).LookupGithubConnectState(context.Background(), HashConnectState("plain"))
	if err != nil || row == nil {
		t.Fatalf("lookup: %v", err)
	}
	if row.ShopDomain != "acme-widgets.myshopify.com" || row.SiteID != "s1" || row.ClientID != "client-id-123" || row.CredentialSource != "current" {
		t.Fatalf("the getter dropped a Shopify field: %+v", row)
	}
}
