package calls

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	gowebauthn "github.com/go-webauthn/webauthn/webauthn"
	callrules "github.com/znasllc-io/memql/component/calls"
	passkeys "github.com/znasllc-io/memql/component/identity/webauthn"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

const accountVerificationAge = 5 * time.Minute
const maxAccountCredentials = 20

func accountRefusal(reason string) error {
	return callrules.Refuse(callrules.ErrForbidden, reason, "the account operation was refused")
}

// Account owns the protocol and authority boundary. Newly registered keys are
// persistent zero-balance guests. Real payments remain unavailable; a separate,
// operator-enabled local simulation can fund and activate bounded hosting passes.
func (s *Service) Account(ctx context.Context, site callrules.SiteRef, token string, request *callrules.AccountRequest) (callrules.AccountResult, error) {
	site, err := checkSite(site)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	if request == nil {
		return s.accountSnapshot(ctx, site, token)
	}
	req := *request
	if reportAction(req.Action) {
		if req.Report == nil || !reflect.DeepEqual(req, callrules.AccountRequest{Action: req.Action, Report: req.Report}) {
			return callrules.AccountResult{}, callrules.ErrInvalid
		}
		result, e := s.report(ctx, site, token, req.Action, *req.Report)
		if e != nil {
			return callrules.AccountResult{}, e
		}
		base, e := s.accountBase(ctx, site)
		base.Authenticated = true
		base.Report = &result
		return base, e
	}
	if req.Report != nil {
		return callrules.AccountResult{}, callrules.ErrInvalid
	}
	if req.Action != "logout" && callrules.TokenWellFormed(token) {
		row, _, e := s.session(ctx, site, token, s.now())
		if e != nil && !errors.Is(e, callrules.ErrNoSession) {
			return callrules.AccountResult{}, e
		}
		if row != nil {
			child, e := s.store.familyForSession(ctx, site.OwnerUserID, site.SiteID, row.ID)
			if e != nil {
				return callrules.AccountResult{}, e
			}
			if child != nil {
				return callrules.AccountResult{}, accountRefusal("family_child_account_unavailable")
			}
		}
	}
	if commerceAction(req.Action) {
		if !validCommerceRequest(req) {
			return callrules.AccountResult{}, callrules.ErrInvalid
		}
		return s.accountCommerce(ctx, site, token, req)
	}
	if !validAccountRequest(req) {
		return callrules.AccountResult{}, callrules.ErrInvalid
	}
	if req.Action == "quote_call" {
		return s.quoteCall(ctx, site, token, req)
	}
	if req.Action == "simulate_topup" || req.Action == "activate_pass" {
		if s.commerceConfig().enabled() {
			return callrules.AccountResult{}, accountRefusal("checkout_required")
		}
		return s.accountFunding(ctx, site, token, req)
	}
	if req.Action == "logout" {
		return s.accountLogout(ctx, site, token)
	}
	if req.Action == "revoke" {
		return s.accountRevoke(ctx, site, token, req.CredentialID)
	}
	if req.Action == "register_begin" {
		if gateErr := s.commerceRegistrationAllowed(ctx); gateErr != nil {
			return callrules.AccountResult{}, gateErr
		}
	}
	ceremony, err := s.accountCeremony(ctx, site)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	switch req.Action {
	case "claim_begin":
		return s.accountClaimBegin(ctx, site, token, ceremony)
	case "claim_finish":
		return s.accountClaimFinish(ctx, site, token, req, ceremony)
	case "register_begin", "add_begin":
		return s.accountRegisterBegin(ctx, site, token, req, ceremony)
	case "register_finish", "add_finish":
		return s.accountRegisterFinish(ctx, site, token, req, ceremony)
	case "login_begin", "verify_begin":
		return s.accountLoginBegin(ctx, site, token, req, ceremony)
	case "login_finish", "verify_finish":
		return s.accountLoginFinish(ctx, site, token, req, ceremony)
	default:
		return callrules.AccountResult{}, callrules.ErrInvalid
	}
}

func validAccountRequest(r callrules.AccountRequest) bool {
	if r.OperationID != "" || r.Outcome != "" || r.AdultDeclared || r.TermsAccepted || r.SaveMethod {
		return false
	}
	if r.Action == "quote_call" {
		return validQuoteRequest(r)
	}
	if r.ParticipantLimit != 0 || r.DurationMinutes != 0 || r.Insights || r.ParticipationMode != "" {
		return false
	}
	if r.Action == "simulate_topup" || r.Action == "activate_pass" {
		return validFundingRequest(r)
	}
	if (r.AccountID != "" && r.Action != "add_begin") || len(r.AccountID) > 128 || r.AmountCents != 0 || r.Plan != "" || r.IdempotencyKey != "" {
		return false
	}
	if len(r.Key) > 128 || len(r.ChallengeID) > 128 || len(r.CredentialID) > 128 || len(r.Credential) > 60*1024 || utf8.RuneCountInString(r.Label) > 64 || !utf8.ValidString(r.Label) {
		return false
	}
	for _, c := range r.Label {
		if unicode.IsControl(c) {
			return false
		}
	}
	switch r.Action {
	case "register_begin", "add_begin", "claim_begin", "verify_begin", "logout":
		return r.Key == "" && r.ChallengeID == "" && len(r.Credential) == 0 && r.CredentialID == "" && r.Label == ""
	case "login_begin":
		return r.ChallengeID == "" && len(r.Credential) == 0 && r.CredentialID == "" && r.Label == ""
	case "register_finish", "add_finish", "claim_finish":
		return r.Key == "" && r.ChallengeID != "" && len(r.Credential) > 0 && r.CredentialID == ""
	case "login_finish", "verify_finish":
		return r.Key == "" && r.ChallengeID != "" && len(r.Credential) > 0 && r.CredentialID == "" && r.Label == ""
	case "revoke":
		return r.Key == "" && r.ChallengeID == "" && len(r.Credential) == 0 && r.CredentialID != "" && r.Label == ""
	}
	return false
}

func (s *Service) accountBase(ctx context.Context, site callrules.SiteRef) (callrules.AccountResult, error) {
	policy, err := s.store.PolicyFor(ctx, site.OwnerUserID, site.SiteID)
	return callrules.AccountResult{Capabilities: callrules.AccountCapabilities{Reports: reportCapabilities(policy), FamilyMode: s.familyCapability(), Insights: policy.SimulatedFunding && policy.CallConfiguration && s.commerceConfig().admission(s.now()) == nil,
		Resources: policy.SimulatedFunding && policy.CallConfiguration && s.commerceConfig().admission(s.now()) == nil, Passkeys: true, RequirePasskeys: policy.RequirePasskeys, Payments: false, SimulatedFunding: policy.SimulatedFunding, CallConfiguration: policy.SimulatedFunding && policy.CallConfiguration, UsagePreview: policy.SimulatedFunding && policy.UsagePreview}, Credentials: []callrules.AccountCredential{}}, err
}
func (s *Service) accountSnapshot(ctx context.Context, site callrules.SiteRef, token string) (callrules.AccountResult, error) {
	out, err := s.accountBase(ctx, site)
	if err != nil {
		return out, err
	}
	if token == "" {
		return out, nil
	}
	// A valid legacy link-only seat is not an account, but its cookie still
	// belongs to the call. Merely opening the landing page must not revoke it.
	seat, _, err := s.session(ctx, site, token, s.now())
	if err == nil && seat.KeyID == "" {
		return out, nil
	}
	if err != nil && !errors.Is(err, callrules.ErrNoSession) {
		return out, err
	}
	h, err := s.authenticatedKey(ctx, callrules.Session{Site: site, Token: token})
	if errors.Is(err, callrules.ErrNoSession) {
		out.ClearSession = true
		return out, nil
	}
	if err != nil {
		return out, err
	}
	key, err := s.store.keyByID(ctx, site.OwnerUserID, h.row.KeyID)
	if err != nil {
		return out, err
	}
	rows, err := s.store.credentialsForKey(ctx, site.OwnerUserID, key.ID)
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		if !r.RevokedAt.IsZero() {
			continue
		}
		c := callrules.AccountCredential{ID: r.ID, Label: r.Label, CreatedAt: r.AddedAt}
		if !r.LastUsedAt.IsZero() {
			v := r.LastUsedAt
			c.LastUsedAt = &v
		}
		out.Credentials = append(out.Credentials, c)
	}
	sort.Slice(out.Credentials, func(i, j int) bool { return out.Credentials[i].CreatedAt.Before(out.Credentials[j].CreatedAt) })
	created := key.IssuedAt
	if created.IsZero() {
		created = key.Version
	}
	label := "Legacy key"
	if key.Kind == "guest" {
		label = "Guest key"
	}
	if key.KeySuffix != "" {
		label = "Key ending " + key.KeySuffix
	}
	out.Authenticated = true
	out.Account = &callrules.AccountInfo{AccountID: key.ID, KeyID: key.ID, KeyLabel: label, CreatedAt: created, CredentialCount: len(out.Credentials), Unclaimed: key.Kind == "guest" && len(out.Credentials) == 0, Legacy: key.Kind != "guest" && key.UserHandle == "", CanHost: keyActive(key.Status) && key.Kind != "guest"}
	if key.Kind == "guest" {
		zero := int64(0)
		out.Account.BalanceCents = &zero
	}
	if err := s.fundingSnapshot(ctx, site, &out, key); err != nil {
		return out, err
	}
	if err := s.commerceAccountSnapshot(ctx, site, &out, key); err != nil {
		return out, err
	}
	return out, nil
}

func (s *Service) accountCeremony(ctx context.Context, site callrules.SiteRef) (*passkeys.Ceremony, error) {
	host, err := s.store.publicHost(ctx, site.OwnerUserID, site.SiteID)
	if err != nil {
		return nil, err
	}
	// The live configured site hostname, never Host/Origin from a request. A
	// different custom domain is a different RP and cannot inherit credentials.
	return passkeys.New(passkeys.Config{BaseURL: "https://" + host, DisplayName: "Calls", Now: s.now, ChallengeBackend: passkeys.NewPostgresChallengeBackend(s.gateDB)})
}
func accountTag(site callrules.SiteRef, kind string) string {
	return "calls." + site.SiteID + "." + kind
}
func accountUser(k *Key, rows []credentialRow) *passkeys.User {
	u := &passkeys.User{Id: k.UserHandle, Name: "Calls key", DisplayName: "Calls key"}
	for _, r := range rows {
		if r.RevokedAt.IsZero() {
			u.Credentials = append(u.Credentials, r.Credential)
		}
	}
	return u
}
func (s *Service) recentlyVerified(ctx context.Context, site callrules.SiteRef, token string) (keyHolder, *Key, []credentialRow, error) {
	h, err := s.authenticatedKey(ctx, callrules.Session{Site: site, Token: token})
	if err != nil {
		return h, nil, nil, err
	}
	now := s.now()
	if h.row.CredentialID == "" || h.row.VerifiedAt.IsZero() || now.Before(h.row.VerifiedAt) || now.Sub(h.row.VerifiedAt) > accountVerificationAge {
		return h, nil, nil, accountRefusal("verification_required")
	}
	k, err := s.store.keyByID(ctx, site.OwnerUserID, h.row.KeyID)
	if err != nil {
		return h, nil, nil, err
	}
	rows, err := s.store.credentialsForKey(ctx, site.OwnerUserID, k.ID)
	return h, k, rows, err
}
func (s *Service) accountHasActiveSeat(ctx context.Context, site callrules.SiteRef, row *sessionRow) (bool, error) {
	if row == nil || row.SeatID == "" || row.RoomID == "" {
		return false, nil
	}
	room, err := s.store.roomByID(ctx, site.OwnerUserID, row.RoomID)
	if err != nil {
		return false, err
	}
	if room == nil || !room.open() {
		return false, nil
	}
	seat, err := s.store.seatByID(ctx, site.OwnerUserID, row.SeatID)
	return seat != nil && seat.placed(), err
}
func (s *Service) rejectAccountSwitchInCall(ctx context.Context, site callrules.SiteRef, token string) error {
	if token == "" {
		return nil
	}
	row, _, err := s.session(ctx, site, token, s.now())
	if errors.Is(err, callrules.ErrNoSession) {
		return nil
	}
	if err != nil {
		return err
	}
	active, err := s.accountHasActiveSeat(ctx, site, row)
	if err != nil {
		return err
	}
	if active {
		return callrules.Refuse(callrules.ErrConflict, "in_another_call", "leave the call before switching accounts")
	}
	return nil
}

func (s *Service) accountRegisterBegin(ctx context.Context, site callrules.SiteRef, token string, req callrules.AccountRequest, c *passkeys.Ceremony) (callrules.AccountResult, error) {
	out, err := s.accountBase(ctx, site)
	if err != nil {
		return out, err
	}
	var user *passkeys.User
	binding := id.NewShortId()
	tag := "register"
	if req.Action == "add_begin" {
		h, k, rows, e := s.recentlyVerified(ctx, site, token)
		if e != nil {
			return out, e
		}
		// Expected identity binds a newer client's already verified screen to
		// the cookie arriving now. Older bundles can omit it; the challenge is
		// still bound to the actual key and session, never chosen by the input.
		if req.AccountID != "" && req.AccountID != k.ID {
			return out, callrules.Refuse(callrules.ErrConflict, "account_changed", "the active account changed")
		}
		release, e := s.gates(ctx, accountGate(k.ID))
		if e != nil {
			return out, e
		}
		defer release()
		h, k, rows, e = s.recentlyVerified(memql.ContextWithFreshRead(ctx), site, token)
		if e != nil {
			return out, e
		}
		if req.AccountID != "" && req.AccountID != k.ID {
			return out, callrules.Refuse(callrules.ErrConflict, "account_changed", "the active account changed")
		}
		if len(rows) >= maxAccountCredentials {
			return out, accountRefusal("credential_limit")
		}
		user = accountUser(k, rows)
		binding = k.ID + "|" + h.row.ID
		tag = "add"
	} else {
		if err := s.rejectAccountSwitchInCall(ctx, site, token); err != nil {
			return out, err
		}
		handle, e := callrules.NewToken(s.random, 32)
		if e != nil {
			return out, e
		}
		user = &passkeys.User{Id: handle, Name: "Calls key", DisplayName: "Calls key"}
	}
	options, data, err := c.RP().BeginRegistration(user, gowebauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementRequired, RequireResidentKey: protocol.ResidentKeyRequired(), UserVerification: protocol.VerificationRequired}), gowebauthn.WithExclusions(gowebauthn.Credentials(user.Credentials).CredentialDescriptors()))
	if err != nil {
		return out, fmt.Errorf("calls: cannot begin passkey registration")
	}
	handle, expiry, err := c.Challenges().Put(binding, accountTag(site, tag), data, passkeys.OAuthContext{})
	if err != nil {
		return out, accountCeremonyError(err)
	}
	out.ChallengeID = handle
	out.ExpiresAt = &expiry
	out.CreationOptions, _ = json.Marshal(options.Response)
	return out, nil
}

func (s *Service) accountRegisterFinish(ctx context.Context, site callrules.SiteRef, token string, req callrules.AccountRequest, c *passkeys.Ceremony) (callrules.AccountResult, error) {
	out := callrules.AccountResult{}
	tag := "register"
	if req.Action == "add_finish" {
		tag = "add"
	}
	entry, err := c.Challenges().Take(req.ChallengeID, accountTag(site, tag))
	if err != nil {
		return out, accountCeremonyError(err)
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
	keyID := entry.UserId
	var h keyHolder
	if tag == "add" {
		var k *Key
		h, k, _, err = s.recentlyVerified(ctx, site, token)
		if err != nil {
			return out, err
		}
		if entry.UserId != k.ID+"|"+h.row.ID || k.UserHandle != user.Id {
			return out, accountRefusal("invalid_passkey")
		}
		keyID = k.ID
	}
	gateKeys := []string{accountGate(keyID), credentialGate(cred.ID)}
	if tag == "register" {
		gateKeys = append(gateKeys, commerceGlobalGate)
	}
	release, err := s.gates(ctx, gateKeys...)
	if err != nil {
		return out, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	prior, err := s.store.credentialByDigest(fresh, cred.ID)
	if err != nil {
		return out, err
	}
	if prior != nil {
		return out, accountRefusal("credential_exists")
	}
	if tag == "register" {
		if err := s.commerceRegistrationAllowed(fresh); err != nil {
			return out, err
		}
	}
	var keyText string
	if tag == "add" {
		h, _, rows, e := s.recentlyVerified(fresh, site, token)
		if e != nil {
			return out, e
		}
		if entry.UserId != h.row.KeyID+"|"+h.row.ID {
			return out, accountRefusal("invalid_passkey")
		}
		if len(rows) >= maxAccountCredentials {
			return out, accountRefusal("credential_limit")
		}
	} else {
		if err := s.rejectAccountSwitchInCall(fresh, site, token); err != nil {
			return out, err
		}
		raw, text, e := callrules.NewKey(s.random)
		if e != nil {
			return out, e
		}
		pepper, e := s.pepperOr(fresh, callrules.ErrNotFound)
		if e != nil {
			return out, e
		}
		keyText = text
		if err := s.store.createKey(fresh, site.OwnerUserID, newKey{ID: keyID, SiteID: site.SiteID, Digest: callrules.Digest(pepper, raw[:]), Kind: "guest", UserHandle: user.Id, KeySuffix: text[len(text)-4:], IssuedAt: s.now()}, s.now()); err != nil {
			return out, err
		}
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "Passkey"
	}
	row := credentialRow{ID: id.NewShortId(), SiteID: site.SiteID, KeyID: keyID, Label: label, Credential: *cred, AddedAt: s.now()}
	if err := s.store.createCredential(fresh, site.OwnerUserID, row, s.now()); err != nil {
		return out, err
	}
	if tag == "add" {
		return s.accountSnapshot(fresh, site, token)
	}
	sess, err := s.newAccountSession(fresh, site, keyID, row.ID)
	if err != nil {
		return out, err
	}
	if err := s.endReplacedAccountSession(fresh, site, token); err != nil {
		return out, err
	}
	out, err = s.accountSnapshot(fresh, site, sess.Token)
	out.Key = keyText
	out.Session = &sess
	return out, err
}

func (s *Service) accountLoginBegin(ctx context.Context, site callrules.SiteRef, token string, req callrules.AccountRequest, c *passkeys.Ceremony) (callrules.AccountResult, error) {
	out, err := s.accountBase(ctx, site)
	if err != nil {
		return out, err
	}
	binding := ""
	tag := "login"
	var k *Key
	if req.Action == "verify_begin" {
		h, e := s.authenticatedKey(ctx, callrules.Session{Site: site, Token: token})
		if e != nil {
			return out, e
		}
		k, err = s.store.keyByID(ctx, site.OwnerUserID, h.row.KeyID)
		if err != nil {
			return out, err
		}
		binding = k.ID + "|" + h.row.ID
		tag = "verify"
	} else {
		if err := s.rejectAccountSwitchInCall(ctx, site, token); err != nil {
			return out, err
		}
		if req.Key != "" {
			raw, e := callrules.ParseKey(req.Key)
			if e != nil {
				return out, callrules.ErrInvalid
			}
			pepper, e := s.pepperOr(ctx, callrules.ErrNotFound)
			if e != nil {
				return out, e
			}
			k, err = s.store.KeyByDigest(ctx, site.OwnerUserID, site.SiteID, callrules.Digest(pepper, raw[:]))
			if err != nil {
				return out, err
			}
			if k == nil {
				return out, accountRefusal("invalid_passkey")
			}
			binding = k.ID
		}
	}
	var options *protocol.CredentialAssertion
	var data *gowebauthn.SessionData
	if k != nil {
		if k.UserHandle == "" {
			if k.Kind == "guest" {
				return out, accountRefusal("invalid_passkey")
			}
			return out, accountRefusal("legacy_key")
		}
		rows, e := s.store.credentialsForKey(ctx, site.OwnerUserID, k.ID)
		if e != nil {
			return out, e
		}
		if len(rows) == 0 {
			return out, accountRefusal("invalid_passkey")
		}
		options, data, err = c.RP().BeginLogin(accountUser(k, rows), gowebauthn.WithUserVerification(protocol.VerificationRequired))
	} else {
		options, data, err = c.RP().BeginDiscoverableLogin(gowebauthn.WithUserVerification(protocol.VerificationRequired))
	}
	if err != nil {
		return out, fmt.Errorf("calls: cannot begin passkey assertion")
	}
	handle, expiry, err := c.Challenges().Put(binding, accountTag(site, tag), data, passkeys.OAuthContext{})
	if err != nil {
		return out, accountCeremonyError(err)
	}
	out.ChallengeID = handle
	out.ExpiresAt = &expiry
	out.RequestOptions, _ = json.Marshal(options.Response)
	return out, nil
}

func (s *Service) accountLoginFinish(ctx context.Context, site callrules.SiteRef, token string, req callrules.AccountRequest, c *passkeys.Ceremony) (callrules.AccountResult, error) {
	out := callrules.AccountResult{}
	tag := "login"
	if req.Action == "verify_finish" {
		tag = "verify"
	}
	entry, err := c.Challenges().Take(req.ChallengeID, accountTag(site, tag))
	if err != nil {
		return out, accountCeremonyError(err)
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(req.Credential))
	if err != nil {
		return out, accountRefusal("invalid_passkey")
	}
	hint, err := s.store.credentialByDigest(ctx, parsed.RawID)
	if err != nil {
		return out, err
	}
	if hint == nil || hint.SiteID != site.SiteID {
		return out, accountRefusal("invalid_passkey")
	}
	release, err := s.gates(ctx, accountGate(hint.KeyID), credentialGate(parsed.RawID))
	if err != nil {
		return out, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	row, err := s.store.credentialByDigest(fresh, parsed.RawID)
	if err != nil {
		return out, err
	}
	if row == nil || row.SiteID != site.SiteID || !row.RevokedAt.IsZero() {
		return out, accountRefusal("invalid_passkey")
	}
	k, err := s.store.keyByID(fresh, site.OwnerUserID, row.KeyID)
	if err != nil {
		return out, err
	}
	if k == nil || k.SiteID != site.SiteID || k.UserHandle == "" || k.Status == "banned" {
		return out, accountRefusal("invalid_passkey")
	}
	var current keyHolder
	if tag == "verify" {
		current, err = s.authenticatedKey(fresh, callrules.Session{Site: site, Token: token})
		if err != nil {
			return out, err
		}
		if entry.UserId != k.ID+"|"+current.row.ID || current.row.KeyID != k.ID {
			return out, accountRefusal("invalid_passkey")
		}
	} else if entry.UserId != "" && entry.UserId != k.ID {
		return out, accountRefusal("invalid_passkey")
	}
	user := accountUser(k, []credentialRow{*row})
	var verified *gowebauthn.Credential
	if len(entry.Session.UserID) > 0 {
		verified, err = c.RP().ValidateLogin(user, *entry.Session, parsed)
	} else {
		verified, err = c.RP().ValidateDiscoverableLogin(func(rawID, userHandle []byte) (gowebauthn.User, error) {
			if !bytes.Equal(rawID, row.Credential.ID) || !bytes.Equal(userHandle, []byte(k.UserHandle)) {
				return nil, errors.New("account mismatch")
			}
			return user, nil
		}, *entry.Session, parsed)
	}
	if err != nil || verified == nil || !parsed.Response.AuthenticatorData.Flags.UserVerified() || verified.Authenticator.CloneWarning {
		return out, accountRefusal("invalid_passkey")
	}
	data, err := json.Marshal(verified)
	if err != nil {
		return out, fmt.Errorf("calls: cannot encode verified public credential")
	}
	if err := s.store.updateCredential(fresh, site.OwnerUserID, *row, map[string]any{"credentialData": string(data), "lastUsedAt": versionStamp(s.now())}, s.now()); err != nil {
		return out, err
	}
	if tag == "verify" {
		if err := s.store.updateAccountSession(fresh, site.OwnerUserID, current.row, map[string]any{"verifiedAt": versionStamp(s.now())}, s.now()); err != nil {
			return out, err
		}
		return s.accountSnapshot(fresh, site, token)
	}
	if err := s.rejectAccountSwitchInCall(fresh, site, token); err != nil {
		return out, err
	}
	sess, err := s.newAccountSession(fresh, site, k.ID, row.ID)
	if err != nil {
		return out, err
	}
	if err := s.endReplacedAccountSession(fresh, site, token); err != nil {
		return out, err
	}
	out, err = s.accountSnapshot(fresh, site, sess.Token)
	out.Session = &sess
	return out, err
}

func (s *Service) newAccountSession(ctx context.Context, site callrules.SiteRef, keyID, credentialID string) (callrules.Session, error) {
	token, err := callrules.NewToken(s.random, tokenBytes)
	if err != nil {
		return callrules.Session{}, err
	}
	pepper, err := s.pepperOr(ctx, callrules.ErrNoSession)
	if err != nil {
		return callrules.Session{}, err
	}
	k, err := s.store.keyByID(ctx, site.OwnerUserID, keyID)
	if err != nil {
		return callrules.Session{}, err
	}
	if k == nil {
		return callrules.Session{}, callrules.ErrNoSession
	}
	kind := callrules.SessionGuest
	if k.Kind != "guest" {
		kind = callrules.SessionHost
	}
	now := s.now()
	row := newSession{ID: id.NewShortId(), SiteID: site.SiteID, TokenDigest: callrules.Digest(pepper, []byte(token)), Kind: kind, KeyID: keyID, CredentialID: credentialID, VerifiedAt: now, ExpiresAt: now.Add(callrules.SessionLifetime)}
	if err := s.store.createSession(ctx, site.OwnerUserID, row, now); err != nil {
		return callrules.Session{}, err
	}
	return callrules.Session{Token: token, Kind: kind, KeyID: keyID, ExpiresAt: row.ExpiresAt, Site: site}, nil
}
func accountCeremonyError(err error) error {
	if errors.Is(err, passkeys.ErrChallengeStorage) {
		return fmt.Errorf("calls: shared passkey challenge storage unavailable")
	}
	return accountRefusal("challenge_expired")
}

func (s *Service) accountLogout(ctx context.Context, site callrules.SiteRef, token string) (callrules.AccountResult, error) {
	out, err := s.accountBase(ctx, site)
	if err != nil {
		return out, err
	}
	if token == "" {
		out.ClearSession = true
		return out, nil
	}
	row, _, err := s.session(ctx, site, token, s.now())
	if errors.Is(err, callrules.ErrNoSession) {
		out.ClearSession = true
		return out, nil
	}
	if err != nil {
		return out, err
	}
	release, err := s.gates(ctx, accountGate(row.KeyID))
	if err != nil {
		return out, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	row, _, err = s.session(fresh, site, token, s.now())
	if err != nil {
		return out, err
	}
	if err := s.rejectAccountSwitchInCall(fresh, site, token); err != nil {
		return out, err
	}
	if err := s.store.updateAccountSession(fresh, site.OwnerUserID, row, map[string]any{"endedAt": versionStamp(s.now())}, s.now()); err != nil {
		return out, err
	}
	out.ClearSession = true
	return out, nil
}
func (s *Service) accountRevoke(ctx context.Context, site callrules.SiteRef, token, credentialID string) (callrules.AccountResult, error) {
	out := callrules.AccountResult{}
	h, _, _, err := s.recentlyVerified(ctx, site, token)
	if err != nil {
		return out, err
	}
	release, err := s.gates(ctx, accountGate(h.row.KeyID))
	if err != nil {
		return out, err
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	fresh := memql.ContextWithFreshRead(ctx)
	h, _, rows, err := s.recentlyVerified(fresh, site, token)
	if err != nil {
		return out, err
	}
	var target *credentialRow
	count := 0
	for i := range rows {
		r := &rows[i]
		if r.RevokedAt.IsZero() {
			count++
			if r.ID == credentialID {
				target = r
			}
		}
	}
	if target == nil {
		return out, callrules.ErrNotFound
	}
	if count <= 1 {
		return out, accountRefusal("last_passkey")
	}
	if target.ID == h.row.CredentialID {
		return out, accountRefusal("credential_in_use")
	}
	if err := s.store.updateCredential(fresh, site.OwnerUserID, *target, map[string]any{"revokedAt": versionStamp(s.now())}, s.now()); err != nil {
		return out, err
	}
	release()
	release = nil
	s.disconnectCredentialSeats(ctx, site, credentialID)
	return s.accountSnapshot(fresh, site, token)
}

// Rotation retires the old cookie before exposing its replacement. The session
// version fence also prevents a concurrent Start/Join from being orphaned: if
// it seated the old session after our check, this write fails and no cookie is
// replaced. No second account lock is acquired while holding the new account.
func (s *Service) endReplacedAccountSession(ctx context.Context, site callrules.SiteRef, token string) error {
	if token == "" {
		return nil
	}
	row, _, err := s.session(ctx, site, token, s.now())
	if errors.Is(err, callrules.ErrNoSession) {
		return nil
	}
	if err != nil {
		return err
	}
	active, err := s.accountHasActiveSeat(ctx, site, row)
	if err != nil {
		return err
	}
	if active {
		return callrules.Refuse(callrules.ErrConflict, "in_another_call", "leave the call before switching accounts")
	}
	return s.store.updateAccountSession(ctx, site.OwnerUserID, row, map[string]any{"endedAt": versionStamp(s.now())}, s.now())
}

// UnlockReplacing keeps the legacy operator-key route without allowing it to
// bypass the account switch boundary enforced by passkey sign-in.
func (s *Service) UnlockReplacing(ctx context.Context, site callrules.SiteRef, key, previousToken string) (callrules.Session, error) {
	if err := s.rejectAccountSwitchInCall(ctx, site, previousToken); err != nil {
		return callrules.Session{}, err
	}
	sess, err := s.Unlock(ctx, site, key)
	if err != nil {
		return callrules.Session{}, err
	}
	if err := s.endReplacedAccountSession(memql.ContextWithFreshRead(ctx), site, previousToken); err != nil {
		return callrules.Session{}, err
	}
	return sess, nil
}
