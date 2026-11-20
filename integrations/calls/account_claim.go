package calls

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	gowebauthn "github.com/go-webauthn/webauthn/webauthn"
	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	passkeys "github.com/znasllc-io/memql/component/identity/webauthn"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// The unclaimed HttpOnly session is the authority to keep this guest key.
// Neither a visible key nor an invitation can claim somebody else's account.
// A partial write before the first credential can be retried by that same
// session; once any usable credential exists, every unclaimed session closes.
func (s *Service) unclaimedAccount(ctx context.Context, site callrules.SiteRef, token string) (keyHolder, *Key, error) {
	h, err := s.authenticatedKey(ctx, callrules.Session{Site: site, Token: token})
	if err != nil {
		return h, nil, err
	}
	k, err := s.store.keyByID(ctx, site.OwnerUserID, h.row.KeyID)
	if err != nil {
		return h, nil, err
	}
	if k.Kind != "guest" || h.row.CredentialID != "" || k.Status == "banned" {
		return h, nil, accountRefusal("already_claimed")
	}
	rows, err := s.store.credentialsForKey(ctx, site.OwnerUserID, k.ID)
	if err != nil {
		return h, nil, err
	}
	if len(rows) > 0 {
		return h, nil, accountRefusal("already_claimed")
	}
	if err := s.rejectAccountSwitchInCall(ctx, site, token); err != nil {
		return h, nil, err
	}
	return h, k, nil
}

func (s *Service) accountClaimBegin(ctx context.Context, site callrules.SiteRef, token string, c *passkeys.Ceremony) (callrules.AccountResult, error) {
	out, err := s.accountBase(ctx, site)
	if err != nil {
		return out, err
	}
	h, k, err := s.unclaimedAccount(ctx, site, token)
	if err != nil {
		return out, err
	}
	handle, err := callrules.NewToken(s.random, 32)
	if err != nil {
		return out, err
	}
	user := &passkeys.User{Id: handle, Name: "Calls key", DisplayName: "Calls key"}
	options, data, err := c.RP().BeginRegistration(user, gowebauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementRequired, RequireResidentKey: protocol.ResidentKeyRequired(), UserVerification: protocol.VerificationRequired}))
	if err != nil {
		return out, fmt.Errorf("calls: cannot begin guest key claim")
	}
	challenge, expiry, err := c.Challenges().Put(k.ID+"|"+h.row.ID, accountTag(site, "claim"), data, passkeys.OAuthContext{})
	if err != nil {
		return out, accountCeremonyError(err)
	}
	out.ChallengeID = challenge
	out.ExpiresAt = &expiry
	out.CreationOptions, _ = json.Marshal(options.Response)
	return out, nil
}

func (s *Service) accountClaimFinish(ctx context.Context, site callrules.SiteRef, token string, req callrules.AccountRequest, c *passkeys.Ceremony) (callrules.AccountResult, error) {
	out := callrules.AccountResult{}
	entry, err := c.Challenges().Take(req.ChallengeID, accountTag(site, "claim"))
	if err != nil {
		return out, accountCeremonyError(err)
	}
	h, k, err := s.unclaimedAccount(ctx, site, token)
	if err != nil {
		return out, err
	}
	if entry.UserId != k.ID+"|"+h.row.ID {
		return out, accountRefusal("invalid_passkey")
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(req.Credential))
	if err != nil {
		return out, accountRefusal("invalid_passkey")
	}
	user := &passkeys.User{Id: string(entry.Session.UserID)}
	cred, err := c.RP().CreateCredential(user, *entry.Session, parsed)
	if err != nil || !cred.Flags.UserVerified {
		return out, accountRefusal("invalid_passkey")
	}
	release, err := s.gates(ctx, accountGate(k.ID), credentialGate(cred.ID))
	if err != nil {
		return out, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	h, k, err = s.unclaimedAccount(fresh, site, token)
	if err != nil {
		return out, err
	}
	if entry.UserId != k.ID+"|"+h.row.ID {
		return out, accountRefusal("invalid_passkey")
	}
	prior, err := s.store.credentialByDigest(fresh, cred.ID)
	if err != nil {
		return out, err
	}
	if prior != nil {
		return out, accountRefusal("credential_exists")
	}
	raw, keyText, err := callrules.NewKey(s.random)
	if err != nil {
		return out, err
	}
	call, err := langparser.RenderCall("claimCallsKey", map[string]any{"keyId": k.ID, "digest": callrules.Digest(h.pepper, raw[:]), "userHandle": user.Id, "keySuffix": keyText[len(keyText)-4:], "versionTime": versionStamp(memql.VersionTimeAfter(k.Version, s.now()))})
	if err := s.store.writeAs(auth.ContextWithInternalOrigin(fresh), site.OwnerUserID, at(keyConcept, k.ID, k.Version), "the guest key claim", call, err); err != nil {
		return out, err
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "Passkey"
	}
	row := credentialRow{ID: id.NewShortId(), SiteID: site.SiteID, KeyID: k.ID, Label: label, Credential: *cred, AddedAt: s.now()}
	if err := s.store.createCredential(fresh, site.OwnerUserID, row, s.now()); err != nil {
		return out, err
	}
	// Credential persistence is the first-claim commit. Old unclaimed cookies
	// now fail closed; a lost response recovers through discoverable sign-in.
	sess, err := s.newAccountSession(fresh, site, k.ID, row.ID)
	if err != nil {
		return out, err
	}
	out, err = s.accountSnapshot(fresh, site, sess.Token)
	out.Key = keyText
	out.Session = &sess
	return out, err
}
