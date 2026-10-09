package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// PendingOwnerSetup reads server-owned installation details, never a requested
// email/domain/provider. A completed claim or any previous sign-in credential
// closes this first-passkey path, even when that credential was later revoked.
// Callers that mint authority must hold AcquireBootstrapGate and re-read here.
func (s *Store) PendingOwnerSetup(ctx context.Context, cfg Config) (*BootstrapEnrollment, error) {
	ctx = memqlengine.ContextWithFreshRead(ctx)
	pending, err := s.ReservedBootstrap(ctx)
	if err != nil {
		return nil, err
	}
	if pending != nil {
		if pending.Complete || pending.Local != cfg.LocalPasskeyOnly() {
			return nil, nil
		}
		if len(pending.Proof) > 0 {
			return pending, nil
		}
	}
	settings, err := s.ReadClusterSettings(ctx)
	if err != nil {
		return nil, err
	}
	if pending != nil {
		settings = &pending.Settings
	}
	if settings == nil && cfg.Bootstrap.HasAllRequired() {
		b := cfg.Bootstrap
		settings = &ClusterSettingsRow{ClusterDomain: b.Domain, BrandName: b.OrgName, BootstrapEmail: b.OwnerEmail,
			BootstrapFirstName: b.OwnerFirstName, BootstrapLastName: b.OwnerLastName, BootstrapPhone: b.OwnerPhone,
			BootstrapPrimaryRole: b.OwnerPrimaryRole, BootstrapGender: b.OwnerGender, BootstrapBirthdate: b.OwnerBirthdate,
			RegistrationMode: b.RegistrationMode, RegistrationDomains: strings.Join(b.RegistrationDomains, ","),
			InternalDomains: strings.Join(b.InternalDomains, ","), InternalDefaultRole: b.InternalDefaultRole,
			AccessRequestNotifyEmails: strings.Join(b.NotifyEmails, ",")}
	}
	if settings == nil || strings.TrimSpace(settings.BootstrapEmail) == "" {
		return nil, nil
	}
	settings.BootstrapEmail = strings.TrimSpace(settings.BootstrapEmail)
	if settings.BrandName == "" {
		settings.BrandName = cfg.BrandName
	}
	owners, err := s.OwnerUserIds(ctx)
	if err != nil {
		return nil, err
	}
	owner, err := s.LookupUserByEmail(ctx, settings.BootstrapEmail)
	if err != nil {
		return nil, err
	}
	if len(owners) > 1 {
		return nil, nil
	}
	if owner != nil {
		if len(owners) != 1 || !owner.Active || owner.Role != "owner" || owner.RevocationEpoch != 0 ||
			strings.TrimPrefix(owners[0], "v1:identity:user:") != strings.TrimPrefix(owner.ID, "v1:identity:user:") {
			return nil, nil
		}
		history, err := s.executeAndExtract(auth.ContextWithInternalOrigin(ctx), fmt.Sprintf(`query signInIdentitiesForUser(userId: %s, includeHistory: true)`, dslJSONString(owner.ID)))
		if err != nil {
			return nil, err
		}
		if len(history) > 0 {
			return nil, nil
		}
	} else if len(owners) != 0 || settings.BootstrappedAt != "" {
		return nil, nil
	}
	if pending == nil {
		pending = &BootstrapEnrollment{Local: cfg.LocalPasskeyOnly(), Settings: *settings, Recovery: true}
	}
	if owner != nil {
		pending.UserID = strings.TrimPrefix(owner.ID, "v1:identity:user:")
	}
	return pending, nil
}

// ResumeOwnerSetupLocked authorizes only first-passkey enrollment. Hosted
// callers must arrive from the magic-link verifier. Local callers may match the
// installer's contact email only while no attestation or prior credential exists.
func (s *Store) ResumeOwnerSetupLocked(ctx context.Context, cfg Config, email string, oauth map[string]string, verified bool) (string, error) {
	if !cfg.LocalPasskeyOnly() && !verified {
		return "", ErrBootstrapEnrollment
	}
	row, err := s.PendingOwnerSetup(ctx, cfg)
	if err != nil {
		return "", err
	}
	if row == nil || !strings.EqualFold(strings.TrimSpace(email), row.Settings.BootstrapEmail) || (row.Local && len(row.Proof) > 0) {
		return "", ErrBootstrapEnrollment
	}
	row.Recovery = true
	row.EmailVerified = verified && !row.Local
	row.OAuth = oauth
	if row.UserID == "" {
		row.UserID, err = NewRandomId("")
		if err != nil {
			return "", err
		}
	}
	if row.IdentityID == "" {
		row.IdentityID, err = NewRandomId("")
		if err != nil {
			return "", err
		}
	}
	bytes := make([]byte, 32)
	if _, err = rand.Read(bytes); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(bytes)
	payload, err := json.Marshal(row)
	if err != nil {
		return "", err
	}
	db, err := s.bootstrapDB()
	if err != nil {
		return "", err
	}
	if row.TokenHash == "" {
		_, err = db.ExecContext(ctx, `INSERT INTO identity_bootstrap_enrollment(token_hash,payload,reserved,expires_at) VALUES($1,$2::jsonb,true,$3)`, bootstrapHash(token), string(payload), time.Now().Add(24*time.Hour))
	} else {
		_, err = db.ExecContext(ctx, `UPDATE identity_bootstrap_enrollment SET token_hash=$1,payload=$2::jsonb,expires_at=$3 WHERE token_hash=$4`, bootstrapHash(token), string(payload), time.Now().Add(24*time.Hour), row.TokenHash)
	}
	return token, err
}
