package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/webauthn"
)

func bootstrapToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	return strings.TrimSpace(token), ok && strings.EqualFold(scheme, identity.BootstrapScheme)
}

func (s *Server) bootstrapError(w http.ResponseWriter, err error) {
	s.logErr("ownership enrollment failed", err)
	writeJSON(w, http.StatusConflict, map[string]any{"error": "Setup could not finish. Retry this step, or return to setup to resume with your passkey or verified owner email.", "errorCode": "bootstrap_incomplete"})
}

func (s *Server) handleBootstrapRegister(w http.ResponseWriter, r *http.Request, token string, finish bool) {
	if !s.requireSecureRequest(w, r) {
		return
	}
	if s.Store == nil || s.Issuer == nil {
		s.bootstrapError(w, identity.ErrBootstrapEnrollment)
		return
	}
	if allowed, _ := passkeyLimiter(s).Allow(clientIP(r)); !allowed {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Too many enrollment attempts. Try again later."})
		return
	}
	ctx := r.Context()
	release, err := s.Store.AcquireBootstrapGate(ctx)
	if err != nil {
		s.bootstrapError(w, err)
		return
	}
	defer release()
	row, err := s.Store.BootstrapEnrollment(ctx, token, s.Cfg)
	if err != nil {
		s.bootstrapError(w, err)
		return
	}
	if row.Complete {
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "redirectTo": s.bootstrapLoginDestination(row)})
		return
	}
	reserved, err := s.Store.ReservedBootstrap(ctx)
	if err != nil || (reserved != nil && reserved.TokenHash != row.TokenHash) {
		s.bootstrapError(w, identity.ErrBootstrapEnrollment)
		return
	}
	claimed, err := s.Store.IsClusterBootstrappedE(ctx)
	if err != nil {
		s.bootstrapError(w, err)
		return
	}
	hasOwner, err := s.Store.HasOwnerUser(ctx)
	if err != nil {
		s.bootstrapError(w, err)
		return
	}
	if (claimed || hasOwner) && len(row.Proof) == 0 {
		s.bootstrapError(w, identity.ErrBootstrapEnrollment)
		return
	}
	ceremony, err := s.webauthnCeremonyFor(r)
	if err != nil {
		s.bootstrapError(w, err)
		return
	}
	if !finish {
		if len(row.Proof) > 0 {
			writeJSON(w, http.StatusOK, map[string]any{"success": true, "resume": true})
			return
		}
		challenge, err := ceremony.BeginRegistration(&webauthn.User{Id: row.UserID, Name: row.Settings.BootstrapEmail, DisplayName: strings.TrimSpace(row.Settings.BootstrapFirstName + " " + row.Settings.BootstrapLastName)})
		if err != nil {
			s.bootstrapError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, WebAuthnRegisterBeginResponse{Success: true, ChallengeId: challenge.ChallengeId, CreationOptions: challenge.Options, RelyingPartyId: ceremony.RPID()})
		return
	}
	if len(row.Proof) == 0 {
		var body WebAuthnRegisterFinishRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPasskeyRegisterBody)).Decode(&body); err != nil {
			s.bootstrapError(w, err)
			return
		}
		cred, err := ceremony.FinishRegistration(body.ChallengeId, row.UserID, bytes.NewReader(body.Credential))
		if err != nil {
			s.bootstrapError(w, err)
			return
		}
		existing, err := (&webauthn.Store{Engine: s.Store.Engine}).LookupByCredentialId(ctx, cred.CredentialId)
		if err != nil || existing != nil {
			s.bootstrapError(w, identity.ErrBootstrapEnrollment)
			return
		}
		row.Proof, err = json.Marshal(cred)
		if err != nil {
			s.bootstrapError(w, err)
			return
		}
		if err := s.Store.SaveBootstrapEnrollmentLocked(ctx, row); err != nil {
			s.bootstrapError(w, err)
			return
		}
	}
	if err := s.finishBootstrapLocked(ctx, row); err != nil {
		s.bootstrapError(w, err)
		return
	}
	if err := s.StartBrowserSessionFor(w, r, row.UserID, row.Settings.BootstrapEmail, "owner_passkey_setup_completed"); err != nil {
		s.bootstrapError(w, err)
		return
	}
	s.auditPasskey(r, "owner_passkey_setup_completed", row.UserID, row.IdentityID, identity.AuditOutcomeSuccess, "", map[string]any{"local": row.Local, "emailVerified": row.EmailVerified})
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "redirectTo": s.bootstrapLoginDestination(row)})
}

// Ordered, idempotent writes under the cluster claim lock. The immutable proof
// lands first; an owner can never appear without a persisted passkey. A retry
// uses the same user and credential IDs and never creates another credential.
func (s *Server) finishBootstrapLocked(ctx context.Context, row *identity.BootstrapEnrollment) error {
	if row.Local != s.Cfg.LocalPasskeyOnly() || (!row.Local && !row.EmailVerified) || len(row.Proof) == 0 {
		return identity.ErrBootstrapEnrollment
	}
	reserved, err := s.Store.ReservedBootstrap(ctx)
	if err != nil {
		return err
	}
	if reserved == nil || reserved.UserID != row.UserID {
		return identity.ErrBootstrapEnrollment
	}
	if reserved.Complete {
		return nil
	}
	row = reserved // A hosted re-verification may have rotated the grant before this lock.
	var cred webauthn.RegisteredCredential
	if err := json.Unmarshal(row.Proof, &cred); err != nil {
		return err
	}
	if cred.CredentialId == "" || cred.PublicKey == "" {
		return identity.ErrBootstrapEnrollment
	}
	owner, err := s.Store.LookupUserByEmail(ctx, row.Settings.BootstrapEmail)
	if err != nil {
		return err
	}
	hasOwner, err := s.Store.HasOwnerUser(ctx)
	if err != nil {
		return err
	}
	if hasOwner && owner == nil {
		return identity.ErrBootstrapEnrollment
	}
	if owner != nil && (strings.TrimPrefix(owner.ID, "v1:identity:user:") != row.UserID || owner.Role != "owner") {
		return identity.ErrBootstrapEnrollment
	}
	store := &webauthn.Store{Engine: s.Store.Engine, Logger: s.Logger}
	existing, err := store.LookupByCredentialId(ctx, cred.CredentialId)
	if err != nil {
		return err
	}
	if existing != nil && (strings.TrimPrefix(existing.UserId, "v1:identity:user:") != row.UserID || !existing.Active) {
		return identity.ErrBootstrapEnrollment
	}
	if existing == nil {
		if err := store.Create(identity.ContextWithSystemCredentialActor(ctx), row.IdentityID, row.UserID, "Owner passkey", &cred, row.UserID); err != nil {
			return err
		}
	}
	if err := s.Store.PersistClusterSettings(ctx, row.Settings); err != nil {
		return err
	}
	if owner == nil {
		cs := row.Settings
		if err := s.Store.CreateUserOnFirstLogin(ctx, row.UserID, strings.TrimSpace(cs.BootstrapFirstName+" "+cs.BootstrapLastName), cs.BootstrapEmail, "owner", true, identity.UserProfileSeed{
			FirstName: cs.BootstrapFirstName, LastName: cs.BootstrapLastName, Phone: cs.BootstrapPhone, PrimaryRole: cs.BootstrapPrimaryRole, Gender: cs.BootstrapGender, Birthdate: cs.BootstrapBirthdate,
		}); err != nil {
			return err
		}
	}
	if owner == nil && s.OnUserProvisioned != nil {
		s.OnUserProvisioned(ctx, row.UserID, row.Settings.BootstrapEmail, row.EmailVerified)
	}
	// Complete the existing self organization before sealing the claim or
	// issuing a session. All steps in this builtin are idempotent; a failure
	// resumes under the same claim lock and passkey proof on another replica.
	if err := s.Store.ConfigureBootstrapOrganization(ctx, row); err != nil {
		return err
	}
	if row.Local {
		if err := s.Store.SetUserSignInPolicy(ctx, row.UserID, identity.SignInPolicyPasskeyOnly); err != nil {
			return err
		}
	}
	// Only hosted email verification supplies mailbox ownership evidence.
	if row.EmailVerified {
		if err := s.Store.CreateIdentityMagicLink(ctx, "bootstrap-"+row.UserID, row.UserID, "Verified owner email"); err != nil {
			return err
		}
	}
	if err := s.Store.StampClusterBootstrapped(ctx); err != nil {
		return err
	}
	row.Complete = true
	return s.Store.SaveBootstrapEnrollmentLocked(ctx, row)
}

func (s *Server) bootstrapLoginDestination(row *identity.BootstrapEnrollment) string {
	q := url.Values{}
	if row.OAuth["client_id"] != "" {
		for k, v := range row.OAuth {
			q.Set(k, v)
		}
		q.Set("response_type", "code")
	}
	return strings.TrimRight(s.Cfg.BaseURL, "/") + "/login" + func() string {
		if len(q) > 0 {
			return "?" + q.Encode()
		}
		return ""
	}()
}

// A browser that lost its enrollment cookie can prove possession of the
// already-created passkey. Only the verified assertion may repair graph writes.
func (s *Server) pendingBootstrapCredential(ctx context.Context, credentialID string) (*webauthn.Row, error) {
	if s.Store.DirectDB == nil {
		return nil, nil
	}
	row, err := s.Store.ReservedBootstrap(ctx)
	if err != nil || row == nil || row.Complete {
		return nil, err
	}
	var cred webauthn.RegisteredCredential
	if err := json.Unmarshal(row.Proof, &cred); err != nil {
		return nil, nil
	}
	if cred.CredentialId != credentialID {
		return nil, nil
	}
	return &webauthn.Row{ID: row.IdentityID, UserId: row.UserID, Active: true, CredentialId: cred.CredentialId, PublicKey: cred.PublicKey, SignCount: cred.SignCount, AAGUID: cred.AAGUID, Transports: cred.Transports, BackupEligible: cred.BackupEligible, BackupState: cred.BackupState}, nil
}

func (s *Server) resumeBootstrapAfterAssertion(ctx context.Context, userID string) error {
	if s.Store.DirectDB == nil {
		return nil
	}
	row, err := s.Store.ReservedBootstrap(ctx)
	if err != nil {
		return err
	}
	if row == nil || row.Complete || row.UserID != strings.TrimPrefix(userID, "v1:identity:user:") {
		return nil
	}
	if len(row.Proof) == 0 {
		return errors.New("passkey setup is incomplete")
	}
	release, err := s.Store.AcquireBootstrapGate(ctx)
	if err != nil {
		return err
	}
	defer release()
	return s.finishBootstrapLocked(ctx, row)
}
