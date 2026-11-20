package calls

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

type accountTestKey struct {
	result             callrules.AccountResult
	handle, rp, origin string
	auth               *softwareAuthenticator
}

type interruptedClaimEngine struct {
	Engine
	failed bool
}

func (e *interruptedClaimEngine) Execute(ctx context.Context, query string) (any, error) {
	if !e.failed && strings.Contains(query, "createCallsCredential(") {
		e.failed = true
		return nil, errors.New("test interruption before credential persistence")
	}
	return e.Engine.Execute(ctx, query)
}

func TestAccountsInterruptedGuestClaimKeepsItsSessionAndHistory(t *testing.T) {
	cw := newCallWorld(t, "accounts-claim-interrupted", 20)
	if cw == nil {
		return
	}
	_, start := cw.started("Host")
	ctx := context.Background()
	_, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	require.NoError(t, cw.svc.Leave(ctx, guest))
	begin, err := cw.svc.Account(ctx, cw.ref, guest.Token, &callrules.AccountRequest{Action: "claim_begin"})
	require.NoError(t, err)
	challenge, rp, _ := accountCreation(t, begin)
	store := *cw.svc.store
	broken := &interruptedClaimEngine{Engine: store.engine}
	store.engine = broken
	service := *cw.svc
	service.store = &store
	_, err = service.Account(ctx, cw.ref, guest.Token, &callrules.AccountRequest{Action: "claim_finish", ChallengeID: begin.ChallengeID, Credential: newSoftwareAuthenticator(t).Attest(challenge, rp, "https://"+rp)})
	require.Error(t, err)
	require.True(t, broken.failed)
	still, err := cw.svc.Account(ctx, cw.ref, guest.Token, nil)
	require.NoError(t, err)
	require.True(t, still.Authenticated)
	require.True(t, still.Account.Unclaimed)
	require.Equal(t, guest.KeyID, still.Account.KeyID)
	begin, err = cw.svc.Account(ctx, cw.ref, guest.Token, &callrules.AccountRequest{Action: "claim_begin"})
	require.NoError(t, err)
	challenge, rp, _ = accountCreation(t, begin)
	kept, err := cw.svc.Account(ctx, cw.ref, guest.Token, &callrules.AccountRequest{Action: "claim_finish", ChallengeID: begin.ChallengeID, Credential: newSoftwareAuthenticator(t).Attest(challenge, rp, "https://"+rp)})
	require.NoError(t, err)
	require.Equal(t, guest.KeyID, kept.Account.KeyID)
	require.False(t, kept.Account.Unclaimed)
}

func TestAccountsClaimExpiryAndJoinAfterBeginAreRechecked(t *testing.T) {
	for _, changed := range []string{"expired", "joined"} {
		t.Run(changed, func(t *testing.T) {
			cw := newCallWorld(t, "accounts-claim-recheck", 20)
			if cw == nil {
				return
			}
			_, start := cw.started("Host")
			ctx := context.Background()
			_, guest, err := cw.join(start.Call.GuestToken, "Guest")
			require.NoError(t, err)
			require.NoError(t, cw.svc.Leave(ctx, guest))
			begin, err := cw.svc.Account(ctx, cw.ref, guest.Token, &callrules.AccountRequest{Action: "claim_begin"})
			require.NoError(t, err)
			challenge, rp, _ := accountCreation(t, begin)
			want := callrules.ErrForbidden
			if changed == "expired" {
				cw.clock.Advance(6 * time.Minute)
			} else {
				_, _, err = cw.rejoin(guest, start.Call.GuestToken)
				require.NoError(t, err)
				want = callrules.ErrConflict
			}
			_, err = cw.svc.Account(ctx, cw.ref, guest.Token, &callrules.AccountRequest{Action: "claim_finish", ChallengeID: begin.ChallengeID, Credential: newSoftwareAuthenticator(t).Attest(challenge, rp, "https://"+rp)})
			require.ErrorIs(t, err, want)
			still, err := cw.svc.Account(ctx, cw.ref, guest.Token, nil)
			require.NoError(t, err)
			require.True(t, still.Account.Unclaimed)
			require.Empty(t, still.Credentials)
		})
	}
}

func TestAccountsAssertionRejectsAnotherPrivateKeyForTheSameCredentialID(t *testing.T) {
	cw := newCallWorld(t, "accounts-signature", 20)
	if cw == nil {
		return
	}
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	attacker := newSoftwareAuthenticator(t)
	attacker.credentialID = append([]byte(nil), k.auth.credentialID...)
	k.auth = attacker
	_, err := accountAssert(t, cw, k, "", "login", "")
	require.ErrorIs(t, err, callrules.ErrForbidden)
}

func accountCreation(t *testing.T, out callrules.AccountResult) (challenge, rp, handle string) {
	t.Helper()
	var wire struct {
		Challenge string `json:"challenge"`
		RP        struct {
			ID string `json:"id"`
		} `json:"rp"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		AuthenticatorSelection struct{ ResidentKey, UserVerification string } `json:"authenticatorSelection"`
	}
	require.NoError(t, json.Unmarshal(out.CreationOptions, &wire))
	require.Equal(t, "required", wire.AuthenticatorSelection.ResidentKey)
	require.Equal(t, "required", wire.AuthenticatorSelection.UserVerification)
	raw, err := base64.RawURLEncoding.DecodeString(wire.User.ID)
	require.NoError(t, err)
	return wire.Challenge, wire.RP.ID, string(raw)
}
func accountRegister(t *testing.T, cw *callWorld, a *softwareAuthenticator) accountTestKey {
	t.Helper()
	begin, err := cw.svc.Account(context.Background(), cw.ref, "", &callrules.AccountRequest{Action: "register_begin"})
	require.NoError(t, err)
	challenge, rp, handle := accountCreation(t, begin)
	// Finish uses an independent Service and ceremony instance, like another pod.
	other := *cw.svc
	finished, err := other.Account(context.Background(), cw.ref, "", &callrules.AccountRequest{Action: "register_finish", ChallengeID: begin.ChallengeID, Label: "Laptop", Credential: a.Attest(challenge, rp, "https://"+rp)})
	require.NoError(t, err)
	require.NotNil(t, finished.Session)
	require.True(t, finished.Authenticated)
	require.NotEmpty(t, finished.Key)
	require.Zero(t, *finished.Account.BalanceCents)
	require.False(t, finished.Account.CanHost)
	require.False(t, finished.Account.Legacy)
	require.Len(t, finished.Credentials, 1)
	return accountTestKey{result: finished, handle: handle, rp: rp, origin: "https://" + rp, auth: a}
}
func accountAssert(t *testing.T, cw *callWorld, k accountTestKey, token, prefix, key string) (callrules.AccountResult, error) {
	t.Helper()
	begin, err := cw.svc.Account(context.Background(), cw.ref, token, &callrules.AccountRequest{Action: prefix + "_begin", Key: key})
	if err != nil {
		return callrules.AccountResult{}, err
	}
	var wire struct {
		Challenge string `json:"challenge"`
		RPID      string `json:"rpId"`
	}
	require.NoError(t, json.Unmarshal(begin.RequestOptions, &wire))
	k.auth.signCount++
	other := *cw.svc
	return other.Account(context.Background(), cw.ref, token, &callrules.AccountRequest{Action: prefix + "_finish", ChallengeID: begin.ChallengeID, Credential: k.auth.Assert(wire.Challenge, wire.RPID, k.origin, k.handle)})
}

func TestAccountsRegisterDiscoverSignInAndUseKeyWithoutFunding(t *testing.T) {
	cw := newCallWorld(t, "accounts-register", 20)
	if cw == nil {
		return
	}
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	ctx := context.Background()
	require.NotEqual(t, k.result.Account.KeyID, k.handle)
	_, err := cw.svc.Unlock(ctx, cw.ref, k.result.Key)
	require.ErrorIs(t, err, callrules.ErrForbidden)
	_, err = cw.svc.Start(ctx, *k.result.Session, callrules.StartBody{Name: "Guest", Confirm: confirmed})
	require.ErrorIs(t, err, callrules.ErrForbidden)
	_, err = cw.svc.Schedule(ctx, *k.result.Session, callrules.ScheduleRequest{Start: cw.clock.Now().Add(time.Hour), Minutes: 30})
	require.ErrorIs(t, err, callrules.ErrForbidden)
	logout, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "logout"})
	require.NoError(t, err)
	require.False(t, logout.Authenticated)
	require.True(t, logout.ClearSession)
	_, err = cw.svc.Session(ctx, cw.ref, k.result.Session.Token)
	require.ErrorIs(t, err, callrules.ErrNoSession)
	signed, err := accountAssert(t, cw, k, "", "login", "")
	require.NoError(t, err)
	require.Equal(t, k.result.Account.KeyID, signed.Account.KeyID)
	require.Empty(t, signed.Key)
	typed, err := accountAssert(t, cw, k, "", "login", k.result.Key)
	require.NoError(t, err)
	require.Equal(t, k.result.Account.KeyID, typed.Account.KeyID)
	_, err = cw.svc.Account(ctx, cw.ref, "", &callrules.AccountRequest{Action: "login_begin", Key: cw.mintKey("active")})
	require.ErrorIs(t, err, callrules.ErrForbidden)
	require.Contains(t, err.Error(), "legacy_key")
}

func TestAccountsRejectWrongOriginRPChallengeAndMissingVerification(t *testing.T) {
	for _, attack := range []string{"origin", "rp", "challenge", "verification", "cross_site"} {
		t.Run(attack, func(t *testing.T) {
			cw := newCallWorld(t, "accounts-reject-"+attack, 20)
			if cw == nil {
				return
			}
			a := newSoftwareAuthenticator(t)
			begin, err := cw.svc.Account(context.Background(), cw.ref, "", &callrules.AccountRequest{Action: "register_begin"})
			require.NoError(t, err)
			challenge, rp, _ := accountCreation(t, begin)
			origin := "https://" + rp
			switch attack {
			case "origin":
				origin = "https://unrelated.example.test"
			case "rp":
				rp = "unrelated.example.test"
			case "challenge":
				challenge = base64.RawURLEncoding.EncodeToString([]byte("wrong challenge"))
			case "verification":
				a.flags &^= flagUserVerified
			}
			site := cw.ref
			if attack == "cross_site" {
				site.SiteID = cw.second
			}
			request := &callrules.AccountRequest{Action: "register_finish", ChallengeID: begin.ChallengeID, Credential: a.Attest(challenge, rp, origin)}
			_, err = cw.svc.Account(context.Background(), site, "", request)
			require.Error(t, err)
			if attack != "cross_site" {
				_, err = cw.svc.Account(context.Background(), cw.ref, "", request)
				require.ErrorIs(t, err, callrules.ErrForbidden)
			}
			require.Empty(t, cw.rowsOf(credentialConcept))
		})
	}
}

func TestAccountsSingleUseChallengeAndCredentialUniquenessAcrossReplicas(t *testing.T) {
	cw := newCallWorld(t, "accounts-replay", 20)
	if cw == nil {
		return
	}
	a := newSoftwareAuthenticator(t)
	begin, err := cw.svc.Account(context.Background(), cw.ref, "", &callrules.AccountRequest{Action: "register_begin"})
	require.NoError(t, err)
	challenge, rp, _ := accountCreation(t, begin)
	req := callrules.AccountRequest{Action: "register_finish", ChallengeID: begin.ChallengeID, Credential: a.Attest(challenge, rp, "https://"+rp)}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other := *cw.svc
			_, e := other.Account(context.Background(), cw.ref, "", &req)
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	success := 0
	for e := range errs {
		if e == nil {
			success++
		}
	}
	require.Equal(t, 1, success)
	require.Len(t, cw.rowsOf(credentialConcept), 1)
	again, err := cw.svc.Account(context.Background(), cw.ref, "", &callrules.AccountRequest{Action: "register_begin"})
	require.NoError(t, err)
	challenge, rp, _ = accountCreation(t, again)
	_, err = cw.svc.Account(context.Background(), cw.ref, "", &callrules.AccountRequest{Action: "register_finish", ChallengeID: again.ChallengeID, Credential: a.Attest(challenge, rp, "https://"+rp)})
	require.ErrorIs(t, err, callrules.ErrForbidden)
	require.Len(t, cw.rowsOf(credentialConcept), 1)
}

func TestAccountsAddReauthenticateRevokeAndRefuseLastCredential(t *testing.T) {
	cw := newCallWorld(t, "accounts-manage", 20)
	if cw == nil {
		return
	}
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	ctx := context.Background()
	token := k.result.Session.Token
	_, err := cw.svc.Account(ctx, cw.ref, token, &callrules.AccountRequest{Action: "revoke", CredentialID: k.result.Credentials[0].ID})
	require.ErrorIs(t, err, callrules.ErrForbidden)
	cw.clock.Advance(6 * time.Minute)
	_, err = cw.svc.Account(ctx, cw.ref, token, &callrules.AccountRequest{Action: "add_begin"})
	require.ErrorIs(t, err, callrules.ErrForbidden)
	_, err = accountAssert(t, cw, k, token, "verify", "")
	require.NoError(t, err)
	begin, err := cw.svc.Account(ctx, cw.ref, token, &callrules.AccountRequest{Action: "add_begin"})
	require.NoError(t, err)
	challenge, rp, handle := accountCreation(t, begin)
	require.Equal(t, k.handle, handle)
	second := newSoftwareAuthenticator(t)
	added, err := cw.svc.Account(ctx, cw.ref, token, &callrules.AccountRequest{Action: "add_finish", ChallengeID: begin.ChallengeID, Label: "Spare", Credential: second.Attest(challenge, rp, "https://"+rp)})
	require.NoError(t, err)
	require.Len(t, added.Credentials, 2)
	secondKey := k
	secondKey.auth = second
	secondSession, err := accountAssert(t, cw, secondKey, "", "login", "")
	require.NoError(t, err)
	revoked, err := cw.svc.Account(ctx, cw.ref, secondSession.Session.Token, &callrules.AccountRequest{Action: "revoke", CredentialID: k.result.Credentials[0].ID})
	require.NoError(t, err)
	require.Len(t, revoked.Credentials, 1)
	_, err = cw.svc.Session(ctx, cw.ref, token)
	require.ErrorIs(t, err, callrules.ErrNoSession)
	_, err = accountAssert(t, cw, k, "", "login", "")
	require.ErrorIs(t, err, callrules.ErrForbidden)
	_, err = cw.svc.Account(ctx, cw.ref, secondSession.Session.Token, &callrules.AccountRequest{Action: "revoke", CredentialID: revoked.Credentials[0].ID})
	require.ErrorIs(t, err, callrules.ErrForbidden)
}

func TestAccountsReverificationCannotSwitchAccount(t *testing.T) {
	cw := newCallWorld(t, "accounts-binding", 20)
	if cw == nil {
		return
	}
	first := accountRegister(t, cw, newSoftwareAuthenticator(t))
	second := accountRegister(t, cw, newSoftwareAuthenticator(t))
	_, err := accountAssert(t, cw, second, first.result.Session.Token, "verify", "")
	require.ErrorIs(t, err, callrules.ErrForbidden)
	before, err := cw.svc.Account(context.Background(), cw.ref, first.result.Session.Token, nil)
	require.NoError(t, err)
	require.Equal(t, first.result.Account.KeyID, before.Account.KeyID)
}

func TestAccountsAddPasskeyRefusesChangedExpectedAccountBeforeChallenge(t *testing.T) {
	cw := newCallWorld(t, "accounts-add-expected", 20)
	if cw == nil {
		return
	}
	first := accountRegister(t, cw, newSoftwareAuthenticator(t))
	second := accountRegister(t, cw, newSoftwareAuthenticator(t))
	ctx := context.Background()
	challengeCount := func() int {
		var n int
		require.NoError(t, cw.svc.gateDB().QueryRowContext(ctx, "SELECT count(*) FROM identity_webauthn_challenges").Scan(&n))
		return n
	}
	before := challengeCount()
	// Both accounts have recent UV. A stale tab for A must not enroll into B
	// merely because another tab replaced its shared calls cookie with B's.
	out, err := cw.svc.Account(ctx, cw.ref, second.result.Session.Token, &callrules.AccountRequest{Action: "add_begin", AccountID: first.result.Account.AccountID})
	require.ErrorIs(t, err, callrules.ErrConflict)
	require.Equal(t, "account_changed", callrules.ReasonOf(err))
	require.Empty(t, out.ChallengeID)
	require.Empty(t, out.CreationOptions)
	require.Equal(t, before, challengeCount(), "a refused stale screen must not allocate a challenge")
	current, err := cw.svc.Account(ctx, cw.ref, second.result.Session.Token, nil)
	require.NoError(t, err)
	require.Len(t, current.Credentials, 1)
	begin, err := cw.svc.Account(ctx, cw.ref, second.result.Session.Token, &callrules.AccountRequest{Action: "add_begin", AccountID: second.result.Account.AccountID})
	require.NoError(t, err)
	_, _, handle := accountCreation(t, begin)
	require.Equal(t, second.handle, handle)
	require.Equal(t, before+1, challengeCount())
}

func TestAccountsSecuredHostStartPreservesKeyAndCredential(t *testing.T) {
	cw := newCallWorld(t, "accounts-secured-host", 20)
	if cw == nil {
		return
	}
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	// Test-only operator entitlement: no payment or balance credit is involved.
	cw.asOwnerMutate(keyConcept, k.result.Account.KeyID, "updateCallsKey", map[string]any{"keyId": k.result.Account.KeyID, "kind": "test"})
	verified, err := accountAssert(t, cw, k, k.result.Session.Token, "login", "")
	require.NoError(t, err)
	ctx := context.Background()
	started, err := cw.svc.Start(ctx, *verified.Session, callrules.StartBody{Name: "Secured host", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, started.Join.Outcome)
	sess, err := cw.svc.Session(ctx, cw.ref, verified.Session.Token)
	require.NoError(t, err)
	require.Equal(t, k.result.Account.KeyID, sess.KeyID)
	seat := cw.row(seatConcept, sess.SeatID)
	require.Equal(t, sess.KeyID, bare(seat["keyId"]), "the host seat must remain attributable to its key")
	require.Equal(t, k.result.Credentials[0].ID, bare(seat["credentialId"]), "credential revocation must still identify this host seat")
	storedSession, err := cw.svc.store.sessionByDigest(ctx, cw.owner, cw.site, callrules.Digest(cw.pepper(), []byte(sess.Token)))
	require.NoError(t, err)
	require.NotNil(t, storedSession)
	require.Equal(t, k.result.Credentials[0].ID, storedSession.CredentialID)
}

func TestAccountsRequiredGuestJoinAndLogoutDoNotGrantHostOrAbandonSeat(t *testing.T) {
	cw := newCallWorld(t, "accounts-join", 20)
	if cw == nil {
		return
	}
	host, start := cw.started("Host")
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	ctx := context.Background()
	body := callrules.JoinBody{Token: start.Call.GuestToken, Name: "Guest", Confirm: confirmed}
	_, unclaimed, err := cw.svc.Join(ctx, cw.ref, nil, body)
	require.NoError(t, err)
	require.NotEmpty(t, unclaimed.KeyID)
	require.NoError(t, cw.svc.Leave(ctx, unclaimed))
	policy := callrules.DefaultPolicy()
	policy.RequirePasskeys = true
	_, err = cw.svc.store.savePolicy(ctx, cw.owner, nil, cw.site, policy, []string{"requirePasskeys"}, cw.clock.Now())
	require.NoError(t, err)
	_, _, err = cw.svc.Join(ctx, cw.ref, nil, body)
	require.ErrorIs(t, err, callrules.ErrNoSession)
	_, _, err = cw.svc.Join(ctx, cw.ref, &unclaimed, body)
	require.ErrorIs(t, err, callrules.ErrNoSession, "a preexisting unclaimed guest key is not passkey authentication")
	_, _, err = cw.svc.Join(ctx, cw.ref, &host, body)
	require.ErrorIs(t, err, callrules.ErrNoSession, "a legacy key does not satisfy an explicit passkey requirement")
	joined, sess, err := cw.svc.Join(ctx, cw.ref, k.result.Session, body)
	require.NoError(t, err)
	require.False(t, joined.Host)
	require.Equal(t, k.result.Account.KeyID, sess.KeyID)
	_, err = cw.svc.Account(ctx, cw.ref, sess.Token, &callrules.AccountRequest{Action: "logout"})
	require.ErrorIs(t, err, callrules.ErrConflict)
	require.NoError(t, cw.svc.Leave(ctx, host))
	state, err := cw.svc.SessionCall(ctx, sess)
	require.NoError(t, err)
	require.False(t, state.Host)
	require.False(t, state.CountdownEndsAt.IsZero())
	require.NoError(t, cw.svc.Leave(ctx, sess))
	out, err := cw.svc.Account(ctx, cw.ref, sess.Token, &callrules.AccountRequest{Action: "logout"})
	require.NoError(t, err)
	require.False(t, out.Authenticated)
}

func TestAccountRequestRejectsExtraneousAuthorityAndUnboundedLabels(t *testing.T) {
	require.False(t, validAccountRequest(callrules.AccountRequest{Action: "add_begin", Key: "a key is not add authority"}))
	require.False(t, validAccountRequest(callrules.AccountRequest{Action: "logout", Credential: json.RawMessage(`{}`)}))
	require.False(t, validAccountRequest(callrules.AccountRequest{Action: "register_finish", ChallengeID: "id", Credential: json.RawMessage(`{}`), Label: "line\nbreak"}))
	require.True(t, validAccountRequest(callrules.AccountRequest{Action: "login_begin"}))
	require.True(t, errors.Is(accountRefusal("last_passkey"), callrules.ErrForbidden))
}

func TestAccountsSnapshotDoesNotDiscardALegacyGuestSeat(t *testing.T) {
	cw := newCallWorld(t, "accounts-legacy-seat", 20)
	if cw == nil {
		return
	}
	_, start := cw.started("Host")
	_, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	// Model a seat cookie issued before durable guest accounts existed.
	guest.Token, err = callrules.NewToken(cw.svc.random, tokenBytes)
	require.NoError(t, err)
	require.NoError(t, cw.svc.store.createSession(context.Background(), cw.owner, newSession{ID: "legacy-seat-cookie", SiteID: cw.site, TokenDigest: callrules.Digest(cw.pepper(), []byte(guest.Token)), Kind: callrules.SessionGuest, SeatID: guest.SeatID, RoomID: guest.RoomID, ExpiresAt: cw.clock.Now().Add(callrules.SessionLifetime)}, cw.clock.Now()))
	out, err := cw.svc.Account(context.Background(), cw.ref, guest.Token, nil)
	require.NoError(t, err)
	require.False(t, out.Authenticated)
	require.False(t, out.ClearSession)
	_, err = cw.svc.Session(context.Background(), cw.ref, guest.Token)
	require.NoError(t, err)
}

func TestAccountsAnonymousGuestClaimsTheSameKeyAfterLeaving(t *testing.T) {
	cw := newCallWorld(t, "accounts-claim", 20)
	if cw == nil {
		return
	}
	_, start := cw.started("Host")
	ctx := context.Background()
	_, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	require.NotEmpty(t, guest.KeyID)
	before, err := cw.svc.Account(ctx, cw.ref, guest.Token, nil)
	require.NoError(t, err)
	require.True(t, before.Authenticated)
	require.True(t, before.Account.Unclaimed)
	require.False(t, before.Account.Legacy)
	require.Zero(t, *before.Account.BalanceCents)
	require.False(t, before.Account.CanHost)
	_, err = cw.svc.Account(ctx, cw.ref, guest.Token, &callrules.AccountRequest{Action: "claim_begin"})
	require.ErrorIs(t, err, callrules.ErrConflict)
	require.NoError(t, cw.svc.Leave(ctx, guest))
	a := newSoftwareAuthenticator(t)
	begin, err := cw.svc.Account(ctx, cw.ref, guest.Token, &callrules.AccountRequest{Action: "claim_begin"})
	require.NoError(t, err)
	challenge, rp, handle := accountCreation(t, begin)
	claimed, err := cw.svc.Account(ctx, cw.ref, guest.Token, &callrules.AccountRequest{Action: "claim_finish", ChallengeID: begin.ChallengeID, Label: "Keep guest key", Credential: a.Attest(challenge, rp, "https://"+rp)})
	require.NoError(t, err)
	require.Equal(t, guest.KeyID, claimed.Account.KeyID)
	require.False(t, claimed.Account.Unclaimed)
	require.NotEmpty(t, claimed.Key)
	require.NotEqual(t, guest.Token, claimed.Session.Token)
	_, err = cw.svc.Session(ctx, cw.ref, guest.Token)
	require.ErrorIs(t, err, callrules.ErrNoSession)
	seat, err := cw.svc.store.seatByID(ctx, cw.owner, guest.SeatID)
	require.NoError(t, err)
	require.Equal(t, guest.KeyID, seat.KeyID)
	signed, err := accountAssert(t, cw, accountTestKey{auth: a, handle: handle, rp: rp, origin: "https://" + rp}, "", "login", "")
	require.NoError(t, err)
	require.Equal(t, guest.KeyID, signed.Account.KeyID)
	_, err = cw.svc.Unlock(ctx, cw.ref, claimed.Key)
	require.ErrorIs(t, err, callrules.ErrForbidden)
	_, err = cw.svc.Account(ctx, cw.ref, claimed.Session.Token, &callrules.AccountRequest{Action: "claim_begin"})
	require.ErrorIs(t, err, callrules.ErrForbidden)
}

func TestAccountsGuestClaimIsBoundToSessionAndSingleWinner(t *testing.T) {
	cw := newCallWorld(t, "accounts-claim-race", 20)
	if cw == nil {
		return
	}
	_, start := cw.started("Host")
	ctx := context.Background()
	_, first, err := cw.join(start.Call.GuestToken, "First")
	require.NoError(t, err)
	require.NoError(t, cw.svc.Leave(ctx, first))
	_, other, err := cw.join(start.Call.GuestToken, "Other")
	require.NoError(t, err)
	require.NoError(t, cw.svc.Leave(ctx, other))
	begin, err := cw.svc.Account(ctx, cw.ref, first.Token, &callrules.AccountRequest{Action: "claim_begin"})
	require.NoError(t, err)
	challenge, rp, _ := accountCreation(t, begin)
	_, err = cw.svc.Account(ctx, cw.ref, other.Token, &callrules.AccountRequest{Action: "claim_finish", ChallengeID: begin.ChallengeID, Credential: newSoftwareAuthenticator(t).Attest(challenge, rp, "https://"+rp)})
	require.ErrorIs(t, err, callrules.ErrForbidden)
	requests := make([]callrules.AccountRequest, 2)
	for i := range requests {
		begin, err := cw.svc.Account(ctx, cw.ref, first.Token, &callrules.AccountRequest{Action: "claim_begin"})
		require.NoError(t, err)
		challenge, rp, _ := accountCreation(t, begin)
		requests[i] = callrules.AccountRequest{Action: "claim_finish", ChallengeID: begin.ChallengeID, Credential: newSoftwareAuthenticator(t).Attest(challenge, rp, "https://"+rp)}
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(req callrules.AccountRequest) {
			defer wg.Done()
			_, err := cw.svc.Account(ctx, cw.ref, first.Token, &req)
			results <- err
		}(requests[i])
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	require.Equal(t, 1, success)
	credentials, err := cw.svc.store.credentialsForKey(ctx, cw.owner, first.KeyID)
	require.NoError(t, err)
	require.Len(t, credentials, 1)
	otherSnapshot, err := cw.svc.Account(ctx, cw.ref, other.Token, nil)
	require.NoError(t, err)
	require.True(t, otherSnapshot.Account.Unclaimed)
}

func TestAccountsSwitchReplacesOnlyAnIdleCookie(t *testing.T) {
	cw := newCallWorld(t, "accounts-switch", 20)
	if cw == nil {
		return
	}
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	idle := cw.host()
	signed, err := accountAssert(t, cw, k, idle.Token, "login", "")
	require.NoError(t, err)
	require.Equal(t, k.result.Account.KeyID, signed.Account.KeyID)
	_, err = cw.svc.Session(context.Background(), cw.ref, idle.Token)
	require.ErrorIs(t, err, callrules.ErrNoSession)
	host, _ := cw.started("Host")
	_, err = cw.svc.UnlockReplacing(context.Background(), cw.ref, cw.mintKey("active"), host.Token)
	require.ErrorIs(t, err, callrules.ErrConflict)
	for _, action := range []string{"register_begin", "login_begin", "logout"} {
		_, err = cw.svc.Account(context.Background(), cw.ref, host.Token, &callrules.AccountRequest{Action: action})
		require.ErrorIs(t, err, callrules.ErrConflict, action)
	}
}

func TestAccountsRevocationDisconnectsOnlyItsCredentialSeatsAndReplay(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		t.Run(map[bool]string{false: "connected", true: "waiting"}[waiting], func(t *testing.T) {
			cw := newCallWorld(t, "accounts-media", 20)
			if cw == nil {
				return
			}
			host, start := cw.started("Host")
			first := accountRegister(t, cw, newSoftwareAuthenticator(t))
			ctx := context.Background()
			begin, err := cw.svc.Account(ctx, cw.ref, first.result.Session.Token, &callrules.AccountRequest{Action: "add_begin"})
			require.NoError(t, err)
			challenge, rp, _ := accountCreation(t, begin)
			second := newSoftwareAuthenticator(t)
			_, err = cw.svc.Account(ctx, cw.ref, first.result.Session.Token, &callrules.AccountRequest{Action: "add_finish", ChallengeID: begin.ChallengeID, Credential: second.Attest(challenge, rp, "https://"+rp)})
			require.NoError(t, err)
			otherKey := first
			otherKey.auth = second
			other, err := accountAssert(t, cw, otherKey, "", "login", "")
			require.NoError(t, err)
			otherJoin, otherSession, err := cw.svc.Join(ctx, cw.ref, other.Session, callrules.JoinBody{Token: start.Call.GuestToken, Name: "Other device", Confirm: confirmed})
			require.NoError(t, err)
			if waiting {
				require.NoError(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: callrules.HostLock}))
			}
			firstJoin, firstSession, err := cw.svc.Join(ctx, cw.ref, first.result.Session, callrules.JoinBody{Token: start.Call.GuestToken, Name: "Revoked device", Confirm: confirmed})
			require.NoError(t, err)
			room, err := cw.svc.store.roomByID(ctx, cw.owner, start.Call.ID)
			require.NoError(t, err)
			people := []MediaParticipant{{Identity: start.Join.Identity}, {Identity: otherJoin.Identity}}
			if !waiting {
				people = append(people, MediaParticipant{Identity: firstJoin.Identity})
			}
			cw.media.setPresent(room.MediaRoom, people...)
			_, err = cw.svc.Account(ctx, cw.ref, otherSession.Token, &callrules.AccountRequest{Action: "revoke", CredentialID: first.result.Credentials[0].ID})
			require.NoError(t, err)
			_, err = cw.svc.Session(ctx, cw.ref, firstSession.Token)
			require.ErrorIs(t, err, callrules.ErrNoSession)
			seat, err := cw.svc.store.seatByID(ctx, cw.owner, firstSession.SeatID)
			require.NoError(t, err)
			require.Equal(t, seatLeft, seat.Status)
			kept, err := cw.svc.store.seatByID(ctx, cw.owner, otherSession.SeatID)
			require.NoError(t, err)
			require.True(t, kept.occupying())
			if !waiting {
				// A previously issued media JWT can reconnect while it remains valid.
				// The next reconciliation pass removes that identity again.
				cw.media.setPresent(room.MediaRoom, people...)
				require.NoError(t, cw.svc.Reconcile(ctx))
				cw.media.mu.Lock()
				present := append([]MediaParticipant(nil), cw.media.present[room.MediaRoom]...)
				cw.media.mu.Unlock()
				for _, p := range present {
					require.NotEqual(t, firstJoin.Identity, p.Identity)
				}
				require.Len(t, present, 2)
			}
		})
	}
}
