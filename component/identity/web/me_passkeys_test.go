package web

// Handler tests for passkey management (memql#3409).
//
// The cross-user scoping half lives in me_passkeys_actor_3409_test.go.
// This file covers the rest of the acceptance criteria, and the one that
// carries the most judgement is the LAST-CREDENTIAL warning: what counts
// as "the last remaining credential" is a decision, not a count of
// passkeys, and these tests are where that decision is written down.
//
// The decision: a sign-in route is a `magic_link` or `passkey` identity
// row, and nothing else. A user holding a magic-link credential still
// has a way in after revoking their only passkey, and warning them about
// a lockout would be false. A user with no magic-link row does not, and
// that is the case the interstitial exists for.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/webauthn"
)

// onlyPasskeyNoEmail: one passkey, NO magic-link row. Revoking it leaves
// nothing.
func onlyPasskeyNoEmail() *fakePasskeyAdapter {
	return &fakePasskeyAdapter{
		rows: map[string][]webauthn.Row{
			aliceId: {{ID: alicePasskeyId, UserId: aliceId, Label: "Alice's YubiKey", Active: true}},
		},
		routes: map[string][]webauthn.SignInRoute{
			aliceId: {{ID: alicePasskeyId, UserId: aliceId, IdentityType: "passkey", Active: true}},
		},
	}
}

func revokeRequest(t *testing.T, s *Server, signIn func(*http.Request, string), userId, id string, confirm bool) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"id": {id}}
	if confirm {
		form.Set("confirm", "yes")
	}
	rec := httptest.NewRecorder()
	s.handleMePasskeysRevoke(rec, postForm(t, signIn, userId, "/me/devices/passkeys/revoke", form))
	return rec
}

// TestRevokingTheLastCredentialWarnsAboutTheLockout is the acceptance
// criterion. The first POST must NOT revoke; it must render a page that
// says, in words, that nothing would be left.
func TestRevokingTheLastCredentialWarnsAboutTheLockout(t *testing.T) {
	adapter := onlyPasskeyNoEmail()
	s, _, signIn := passkeyServer(t, adapter)

	rec := revokeRequest(t, s, signIn, aliceId, alicePasskeyId, false)

	if len(adapter.revokes) != 0 {
		t.Fatalf("the unconfirmed revoke went through (%v). A revoke that would leave no sign-in "+
			"route must be warned about first, not performed and explained afterwards", adapter.revokes)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 rendering the confirmation", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no way to sign in") {
		t.Errorf("the warning does not say a lockout would follow.\nbody=%s", body)
	}
	// The warning must name what is missing, not merely that something
	// is: "you have no other credential" is what a user needs to act on.
	if !strings.Contains(body, "no magic-link") {
		t.Errorf("the warning does not name the missing magic-link route.\nbody=%s", body)
	}
	if !strings.Contains(body, "Revoke anyway") {
		t.Errorf("the warning offers no way through; a warning that cannot be dismissed is a "+
			"refusal wearing a warning's clothes.\nbody=%s", body)
	}
}

// TestAMagicLinkIdentityMeansItIsNotTheLastCredential is the other half
// of the decision, and the reason the count is not "how many passkeys".
// Same single passkey, but this user can still receive a sign-in link --
// so the interstitial must confirm rather than cry lockout.
func TestAMagicLinkIdentityMeansItIsNotTheLastCredential(t *testing.T) {
	adapter := aliceOwnsOnePasskey() // one passkey + one magic_link row
	s, _, signIn := passkeyServer(t, adapter)

	rec := revokeRequest(t, s, signIn, aliceId, alicePasskeyId, false)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 rendering the confirmation", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "no way to sign in") {
		t.Errorf("a user with a magic-link credential was warned about a lockout they are not "+
			"facing. A warning that fires when nothing is wrong is one people learn to click "+
			"through.\nbody=%s", body)
	}
	if !strings.Contains(body, "sign-in link sent to your email") {
		t.Errorf("the confirmation does not name the route that would remain.\nbody=%s", body)
	}
	if len(adapter.revokes) != 0 {
		t.Errorf("the unconfirmed revoke went through (%v)", adapter.revokes)
	}
}

// TestOtherPasskeysCountAsRemainingRoutes covers the third shape: no
// email route, but a second passkey. The user is not locked out, and
// what remains must be named as passkeys rather than as email.
func TestOtherPasskeysCountAsRemainingRoutes(t *testing.T) {
	adapter := onlyPasskeyNoEmail()
	const secondId = "v1:identity:identity:alice-phone"
	adapter.rows[aliceId] = append(adapter.rows[aliceId],
		webauthn.Row{ID: secondId, UserId: aliceId, Label: "Alice's phone", Active: true})
	adapter.routes[aliceId] = append(adapter.routes[aliceId],
		webauthn.SignInRoute{ID: secondId, UserId: aliceId, IdentityType: "passkey", Active: true})

	s, _, signIn := passkeyServer(t, adapter)
	body := revokeRequest(t, s, signIn, aliceId, alicePasskeyId, false).Body.String()

	if strings.Contains(body, "no way to sign in") {
		t.Errorf("warned about a lockout with a second passkey enrolled.\nbody=%s", body)
	}
	if !strings.Contains(body, "1 other passkey") {
		t.Errorf("the confirmation does not name the remaining passkey.\nbody=%s", body)
	}
}

// TestConfirmedRevokeIsSoftAndAudited covers the revoke itself: the row
// stays (active=false, still listed as revoked) and the audit event
// carries the two facts an auditor wants -- what was revoked, and
// whether the account still has a way in.
func TestConfirmedRevokeIsSoftAndAudited(t *testing.T) {
	adapter := aliceOwnsOnePasskey()
	s, audit, signIn := passkeyServer(t, adapter)

	rec := revokeRequest(t, s, signIn, aliceId, alicePasskeyId, true)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303 after a successful revoke", rec.Code)
	}
	if len(adapter.revokes) != 1 || adapter.revokes[0] != alicePasskeyId {
		t.Fatalf("revokes = %v, want exactly [%s]", adapter.revokes, alicePasskeyId)
	}

	// SOFT DELETE: the row survives and is still rendered, marked revoked.
	// A user who wonders why an old authenticator will not re-enrol can
	// see why, and the credential id stays taken.
	rows := adapter.rows[aliceId]
	if len(rows) != 1 {
		t.Fatalf("the row was removed (%d rows left); revocation must be a soft delete", len(rows))
	}
	if rows[0].Active {
		t.Error("revoke did not set active=false")
	}
	page := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/me/devices", nil)
	signIn(getReq, aliceId)
	s.handleMeDevices(page, getReq)
	if !strings.Contains(page.Body.String(), "revoked") {
		t.Errorf("the revoked passkey is not shown as revoked in the listing.\nbody=%s", page.Body.String())
	}

	ev, ok := audit.find("passkey_revoked")
	if !ok {
		t.Fatalf("no passkey_revoked audit event; got %+v", audit.events)
	}
	if ev.Outcome != identity.AuditOutcomeSuccess || ev.ActorUserId != aliceId || ev.TargetId != alicePasskeyId {
		t.Errorf("audit event = %+v, want success/%s/%s", ev, aliceId, alicePasskeyId)
	}
	if ev.Detail["label"] != "Alice's YubiKey" {
		t.Errorf("audit detail label = %v, want the revoked credential's label", ev.Detail["label"])
	}
	if last, _ := ev.Detail["lastCredential"].(bool); last {
		t.Error("audit recorded lastCredential=true for a user who still holds a magic-link route")
	}
}

// TestRevokingTheLastCredentialAuditsItAsSuch pins the flag in the case
// that matters. "lastCredential": true is the line an operator would
// alert on, so it has to be true exactly when it is true.
func TestRevokingTheLastCredentialAuditsItAsSuch(t *testing.T) {
	adapter := onlyPasskeyNoEmail()
	s, audit, signIn := passkeyServer(t, adapter)

	revokeRequest(t, s, signIn, aliceId, alicePasskeyId, true)

	ev, ok := audit.find("passkey_revoked")
	if !ok {
		t.Fatalf("no passkey_revoked audit event; got %+v", audit.events)
	}
	if last, _ := ev.Detail["lastCredential"].(bool); !last {
		t.Errorf("audit detail lastCredential = %v, want true -- this revoke left the account "+
			"with no sign-in route", ev.Detail["lastCredential"])
	}
	if n, _ := ev.Detail["remainingRoutes"].(int); n != 0 {
		t.Errorf("audit detail remainingRoutes = %v, want 0", ev.Detail["remainingRoutes"])
	}
}

// TestRevokeFailsClosedWhenTheRouteCountIsUnavailable. Without the count
// there is no way to tell a routine revoke from a lockout, and doing one
// blind is exactly the outcome the warning exists to prevent -- so the
// handler refuses rather than proceeding without the check.
func TestRevokeFailsClosedWhenTheRouteCountIsUnavailable(t *testing.T) {
	adapter := aliceOwnsOnePasskey()
	adapter.routesErr = errors.New("engine unavailable")
	s, _, signIn := passkeyServer(t, adapter)

	rec := revokeRequest(t, s, signIn, aliceId, alicePasskeyId, true)

	if len(adapter.revokes) != 0 {
		t.Fatalf("the revoke proceeded without knowing what would remain (%v)", adapter.revokes)
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303 with an error flash", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "couldn") {
		t.Errorf("redirect %q does not carry an explanation", rec.Header().Get("Location"))
	}
}

// TestRenameIsAuditedWithBothNames. The audit value of a rename is the
// pair: an event saying only "renamed" cannot be reconciled against a
// list that now shows the new name.
func TestRenameIsAuditedWithBothNames(t *testing.T) {
	adapter := aliceOwnsOnePasskey()
	s, audit, signIn := passkeyServer(t, adapter)

	form := url.Values{"id": {alicePasskeyId}, "label": {"Work laptop"}}
	rec := httptest.NewRecorder()
	s.handleMePasskeysRename(rec, postForm(t, signIn, aliceId, "/me/devices/passkeys/rename", form))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", rec.Code)
	}
	if len(adapter.renames) != 1 || adapter.renames[0].Label != "Work laptop" {
		t.Fatalf("renames = %+v, want one rename to \"Work laptop\"", adapter.renames)
	}
	ev, ok := audit.find("passkey_renamed")
	if !ok {
		t.Fatalf("no passkey_renamed audit event; got %+v", audit.events)
	}
	if ev.Detail["from"] != "Alice's YubiKey" || ev.Detail["to"] != "Work laptop" {
		t.Errorf("audit detail = %v, want both the old and the new name", ev.Detail)
	}
	if ev.Outcome != identity.AuditOutcomeSuccess {
		t.Errorf("outcome = %q, want success", ev.Outcome)
	}
}

// TestRenameRefusesABlankLabel. The label is the only editable field and
// the only thing distinguishing two authenticators in the list, so
// blanking it is never what the user meant.
func TestRenameRefusesABlankLabel(t *testing.T) {
	adapter := aliceOwnsOnePasskey()
	s, _, signIn := passkeyServer(t, adapter)

	form := url.Values{"id": {alicePasskeyId}, "label": {"   "}}
	rec := httptest.NewRecorder()
	s.handleMePasskeysRename(rec, postForm(t, signIn, aliceId, "/me/devices/passkeys/rename", form))

	if len(adapter.renames) != 0 {
		t.Fatalf("a blank label reached the store: %+v", adapter.renames)
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status %d, want 303 with an error flash", rec.Code)
	}
}

// TestThePageShowsBackupPostureAndAuthenticatorModel covers the listing
// acceptance criterion. The backup column is the one a user reads to
// decide whether a second passkey is optional: "Device-bound" has to
// say, in words, that losing the device loses the credential.
func TestThePageShowsBackupPostureAndAuthenticatorModel(t *testing.T) {
	adapter := &fakePasskeyAdapter{
		rows: map[string][]webauthn.Row{
			aliceId: {
				{
					ID: "v1:identity:identity:hw", UserId: aliceId, Label: "Hardware key", Active: true,
					AAGUID: "cb69481e-8ff7-4039-93ec-0a2729a154a8", // YubiKey 5 Series
					// BackupEligible=false: this credential cannot sync.
					Transports: []string{"usb", "nfc"},
				},
				{
					ID: "v1:identity:identity:cloud", UserId: aliceId, Label: "Phone", Active: true,
					AAGUID:         "fbfc3007-154e-4ecc-8c0b-6e020557d7bd", // iCloud Keychain
					BackupEligible: true, BackupState: true,
				},
			},
		},
		routes: map[string][]webauthn.SignInRoute{aliceId: {}},
	}
	s, _, signIn := passkeyServer(t, adapter)

	r := httptest.NewRequest(http.MethodGet, "/me/devices", nil)
	signIn(r, aliceId)
	rec := httptest.NewRecorder()
	s.handleMeDevices(rec, r)
	body := rec.Body.String()

	for _, want := range []string{
		"YubiKey 5 Series",        // AAGUID-derived model
		"iCloud Keychain",         // ... for both rows
		"Device-bound",            // the posture that matters
		"losing the device loses", // spelled out, not left as jargon
		"Synced",
		"usb, nfc", // transports
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the passkey listing does not carry %q.\nbody=%s", want, body)
		}
	}
}

// TestUnauthenticatedVisitorIsSentToSignIn. The page carries per-user
// credential rows now, so it is auth-gated server-side like /me/tokens
// rather than rendered as a shell that hydrates later.
func TestUnauthenticatedVisitorIsSentToSignIn(t *testing.T) {
	s, _, _ := passkeyServer(t, aliceOwnsOnePasskey())

	rec := httptest.NewRecorder()
	s.handleMeDevices(rec, httptest.NewRequest(http.MethodGet, "/me/devices", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303 to /login", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Errorf("Location = %q, want a /login redirect", loc)
	}
}
