package memql

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Settings > Vendors shows what the ANSWERING node's own sign-in got (design
// record docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions A5 and U6). Each node signs in for itself, so a row of a vendor that
// federates carries the sign-in of the process that answered the read, and
// never anything a credential could be rebuilt from.

// A bearer and an identity token no test presents anywhere: they are here so a
// row can be searched for them.
const (
	statusFixtureBearer = "bearer-held-by-the-sign-in-and-never-on-a-row"
	statusFixtureToken  = "identity-token-read-from-the-pod-and-never-on-a-row"
)

// holdSignIn makes the process's sign-in for a vendor one that holds this
// record, as a node that had signed in would.
func holdSignIn(t *testing.T, vendor string, rec vendorSignInRecord) {
	t.Helper()
	rec.Vendor = vendor
	s := newVendorSignIn(vendorSignInConfig{Vendor: vendor}, nil, time.Now())
	s.bearer = statusFixtureBearer
	s.record, s.hasRecord = rec, true
	vendorSignInsMu.Lock()
	vendorSignIns[vendor] = s
	vendorSignInsMu.Unlock()
}

// statusRows reads providerAuthStatus as a cluster owner, by provider name.
func statusRows(t *testing.T, e *MemQLEngine) (map[string]map[string]any, string) {
	t.Helper()
	nodes, err := e.evaluateProviderAuthStatusExpression(clusterOwnerCtx("user-owner"))
	if err != nil {
		t.Fatalf("providerAuthStatus: %v", err)
	}
	rows := map[string]map[string]any{}
	var all strings.Builder
	for _, node := range nodes {
		var row map[string]any
		if err := json.Unmarshal(node.Payload, &row); err != nil {
			t.Fatalf("row %s: %v", node.ID, err)
		}
		rows[node.ID] = row
		all.Write(node.Payload)
	}
	return rows, all.String()
}

func federatedRegistry(t *testing.T) *MemQLEngine {
	t.Helper()
	isolateVendorSignIns(t)
	e := &MemQLEngine{providers: newProviderRegistry()}
	e.providers.setEntry(&ProviderConfigEntry{
		Config: ProviderConfig{Name: "claude", Type: "Anthropic", Model: "claude-test", Auth: map[string]string{
			authKeyFederationRuleID: "fdrl_test", authKeyOrganizationID: "org-test",
			authKeyServiceAccountID: "svac_test", authKeyIdentityTokenFile: "/not-read-by-a-status-read",
		}},
		Client: &scriptedVendorClient{}, Available: true,
	})
	e.providers.setEntry(&ProviderConfigEntry{
		Config: ProviderConfig{Name: "gpt", Type: "OpenAI", Model: "gpt-test", Auth: map[string]string{
			authKeyIdentityProviderID: "idp_test", authKeyOpenAIServiceAccountID: "svc_test", authKeyOpenAITokenFile: "/not-read-by-a-status-read",
		}},
		Client: &scriptedVendorClient{}, Available: true,
	})
	e.providers.setEntry(&ProviderConfigEntry{
		Config: ProviderConfig{Name: "gpt-speech", Type: "OpenAITTS", Model: "tts-test", Auth: map[string]string{
			authKeyIdentityProviderID: "idp_test", authKeyOpenAIServiceAccountID: "svc_test", authKeyOpenAITokenFile: "/not-read-by-a-status-read",
		}},
		Client: &scriptedVendorClient{}, Available: true,
	})
	return e
}

func TestProviderAuthStatusCarriesTheAnsweringNodesSignIn(t *testing.T) {
	e := federatedRegistry(t)
	at := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	holdSignIn(t, VendorAnthropic, vendorSignInRecord{
		Outcome: signInOutcomeOK, At: at, ExpiresAt: at.Add(time.Hour),
		Proof: signInProofOK, ProofAt: at.Add(2 * time.Second), ModelsListed: 7,
		TokenSubject: statusFixtureToken, Fingerprint: "fp", Pid: 4242,
	})
	holdSignIn(t, VendorOpenAI, vendorSignInRecord{
		Outcome: signInOutcomeDenied, Status: 401, At: at.Add(time.Minute),
		Detail: "invalid_grant: the subject does not match", MayBeReplay: true, NextTokenDue: at.Add(9 * time.Minute),
	})

	rows, raw := statusRows(t, e)

	accepted, _ := rows["claude"]["signIn"].(map[string]any)
	if accepted == nil {
		t.Fatalf("the Anthropic row carries no sign-in: %v", rows["claude"])
	}
	if accepted["outcome"] != signInOutcomeOK || accepted["proof"] != signInProofOK {
		t.Errorf("sign-in = %v, want accepted and proven", accepted)
	}
	if accepted["at"] != "2026-11-22T10:00:00Z" || accepted["proofAt"] != "2026-11-22T10:00:02Z" {
		t.Errorf("sign-in times = at %v, proofAt %v", accepted["at"], accepted["proofAt"])
	}

	refused, _ := rows["gpt"]["signIn"].(map[string]any)
	if refused == nil {
		t.Fatalf("the OpenAI row carries no sign-in: %v", rows["gpt"])
	}
	if refused["outcome"] != signInOutcomeDenied || refused["detail"] != "invalid_grant: the subject does not match" {
		t.Errorf("sign-in = %v, want the refusal with the vendor's reason", refused)
	}
	if refused["mayBeReplay"] != true || refused["nextTokenDue"] != "2026-11-22T10:09:00Z" {
		t.Errorf("sign-in = %v, want it to say the next token settles it, and when", refused)
	}
	// Every record of a vendor shares the vendor's one sign-in.
	speech, _ := rows["gpt-speech"]["signIn"].(map[string]any)
	if speech == nil || speech["outcome"] != signInOutcomeDenied {
		t.Errorf("the speech record's row does not carry its vendor's sign-in: %v", rows["gpt-speech"])
	}

	// NOTHING A CREDENTIAL CAN BE REBUILT FROM: not the bearer, not the token's
	// subject, not the federation ids, not the process id.
	for _, secret := range []string{statusFixtureBearer, statusFixtureToken, "fdrl_test", "svac_test", "idp_test", "svc_test", "4242", `"fingerprint"`, `"ids"`} {
		if strings.Contains(raw, secret) {
			t.Errorf("a status row carries %q:\n%s", secret, raw)
		}
	}
}

// A node waiting for its next identity token, and one whose bearer the vendor's
// API stopped taking, each say so with the time that matters.
func TestProviderAuthStatusSaysWhenASignInIsWaiting(t *testing.T) {
	e := federatedRegistry(t)
	at := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	holdSignIn(t, VendorAnthropic, vendorSignInRecord{Outcome: signInOutcomeWaiting, At: at, NextTokenDue: at.Add(6 * time.Minute)})
	holdSignIn(t, VendorOpenAI, vendorSignInRecord{
		Outcome: signInOutcomeOK, At: at, Proof: signInProofRefused, ProofStatus: 401, ProofDetail: "the bearer was revoked",
		ProofAt: at.Add(time.Minute), BearerDroppedAt: at.Add(time.Minute),
	})

	rows, _ := statusRows(t, e)
	waiting, _ := rows["claude"]["signIn"].(map[string]any)
	if waiting["outcome"] != signInOutcomeWaiting || waiting["nextTokenDue"] != "2026-11-22T10:06:00Z" {
		t.Errorf("sign-in = %v, want waiting with the next token's time", waiting)
	}
	dropped, _ := rows["gpt"]["signIn"].(map[string]any)
	if dropped["proof"] != signInProofRefused || dropped["proofDetail"] != "the bearer was revoked" || dropped["bearerDroppedAt"] != "2026-11-22T10:01:00Z" {
		t.Errorf("sign-in = %v, want the API's refusal of the bearer and when it was dropped", dropped)
	}
	// A time that never happened is absent, never a zero date.
	if _, has := waiting["proofAt"]; has {
		t.Errorf("a sign-in nothing proved carries a proof time: %v", waiting)
	}
}

func TestProviderAuthStatusHasNoSignInForAVendorNobodySetUp(t *testing.T) {
	isolateVendorSignIns(t)
	e := &MemQLEngine{providers: newProviderRegistry()}
	e.providers.setEntry(&ProviderConfigEntry{Config: ProviderConfig{Name: "claude", Type: "Anthropic", Model: "claude-test"}, Client: &scriptedVendorClient{}})
	// A record that only says nobody set the vendor up is not a sign-in.
	holdSignIn(t, VendorOpenAI, vendorSignInRecord{Outcome: signInOutcomeNotConfigured, At: time.Now()})
	e.providers.setEntry(&ProviderConfigEntry{Config: ProviderConfig{Name: "gpt", Type: "OpenAI", Model: "gpt-test"}, Client: &scriptedVendorClient{}})

	rows, _ := statusRows(t, e)
	for _, name := range []string{"claude", "gpt"} {
		if _, has := rows[name]["signIn"]; has {
			t.Errorf("row %s carries a sign-in for a vendor nobody set up: %v", name, rows[name])
		}
	}
}
