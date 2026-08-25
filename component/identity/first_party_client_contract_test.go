package identity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// The Go half of the first-party-client contract (memql#4515).
//
// identity carries the editor compiled in; the extension authorizes as it
// without registering. Nothing in the build links the two -- separate
// languages, separate modules, separate release artifacts -- so the constants
// they must agree on are written down once, in
// test/fixtures/first-party-client-contract.json, and asserted from both sides.
// The TypeScript half is editors/vscode/test/firstPartyClientContract.test.ts
// and reads the same file.
//
// A disagreement is invisible until a released extension meets a released
// cluster, and it presents as "Unknown client" or invalid_client -- which reads
// exactly like the 403 registration_disabled failure this epic removed, and
// would send the next reader down the same wrong path.

const firstPartyContractPath = "../../test/fixtures/first-party-client-contract.json"

type firstPartyClientContract struct {
	ClientId          string   `json:"clientId"`
	ClientName        string   `json:"clientName"`
	RedirectURI       string   `json:"redirectURI"`
	MinRole           string   `json:"minRole"`
	AcceptedCallbacks []string `json:"acceptedCallbacks"`
	RejectedCallbacks []string `json:"rejectedCallbacks"`
}

func loadFirstPartyContract(t *testing.T) firstPartyClientContract {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(firstPartyContractPath))
	if err != nil {
		t.Fatalf("read shared contract fixture: %v", err)
	}
	var c firstPartyClientContract
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse shared contract fixture: %v", err)
	}
	if c.ClientId == "" || c.RedirectURI == "" {
		t.Fatal("shared contract fixture is missing clientId or redirectURI")
	}
	return c
}

func TestFirstPartyClientContract_RegistryMatchesTheFixture(t *testing.T) {
	c := loadFirstPartyContract(t)

	if BuiltinClientVSCode != c.ClientId {
		t.Fatalf("BuiltinClientVSCode = %q, fixture says %q -- a released extension carries the fixture's value",
			BuiltinClientVSCode, c.ClientId)
	}

	entry := FindBuiltinClient(c.ClientId)
	if entry == nil {
		t.Fatalf("the registry carries no entry for %q", c.ClientId)
	}
	if entry.Name != c.ClientName {
		t.Errorf("Name = %q, fixture says %q", entry.Name, c.ClientName)
	}
	if len(entry.RedirectURIs) != 1 || entry.RedirectURIs[0] != c.RedirectURI {
		t.Errorf("RedirectURIs = %v, fixture says [%q]", entry.RedirectURIs, c.RedirectURI)
	}

	var declared auth.Role
	for _, b := range BuiltinClients() {
		if b.Client.ClientId == c.ClientId {
			declared = b.MinRole
		}
	}
	if string(declared) != c.MinRole {
		t.Errorf("MinRole = %q, fixture says %q", declared, c.MinRole)
	}
}

func TestFirstPartyClientContract_CallbackMatching(t *testing.T) {
	// Both halves assert the same callback lists. The Go side proves the
	// matcher accepts what the extension will actually present; the TS side
	// proves the extension presents nothing outside that set.
	c := loadFirstPartyContract(t)
	ctx := context.Background()
	cfg := Config{} // a hardened cluster: no static clients, DCR off

	for _, uri := range c.AcceptedCallbacks {
		if !ClientAllowsRedirectURI(ctx, cfg, nil, c.ClientId, uri) {
			t.Errorf("ClientAllowsRedirectURI(%q) = false, want true", uri)
		}
	}
	for _, uri := range c.RejectedCallbacks {
		if ClientAllowsRedirectURI(ctx, cfg, nil, c.ClientId, uri) {
			t.Errorf("ClientAllowsRedirectURI(%q) = true, want false", uri)
		}
	}
}
