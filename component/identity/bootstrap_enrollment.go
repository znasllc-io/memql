package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"
)

// LocalPasskeyOnly is an operator declaration, never a property of an HTTP
// request, proposed domain or mail transport. Unknown targets require email.
func (c Config) LocalPasskeyOnly() bool { return c.DeployProvider == "docker-local" }

const BootstrapCookie = "memql_bootstrap"
const BootstrapScheme = "Bootstrap"

var ErrBootstrapEnrollment = errors.New("ownership enrollment is unavailable or belongs to another claim; resume with your passkey or verify the original owner email again")

// BootstrapEnrollment contains no login authority. Proof is the server-verified
// public credential, saved before any graph write so failures can be resumed.
type BootstrapEnrollment struct {
	TokenHash     string `json:"-"`
	UserID        string
	IdentityID    string
	Local         bool
	EmailVerified bool
	Settings      ClusterSettingsRow
	OAuth         map[string]string
	Proof         json.RawMessage `json:",omitempty"`
	Complete      bool
}

func bootstrapHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) bootstrapDB() (*sql.DB, error) {
	if s == nil || s.DirectDB == nil || s.DirectDB() == nil {
		return nil, ErrBootstrapEnrollment
	}
	return s.DirectDB(), nil
}

func (s *Store) readBootstrap(ctx context.Context, predicate string, arg any) (*BootstrapEnrollment, error) {
	db, err := s.bootstrapDB()
	if err != nil {
		return nil, err
	}
	var payload []byte
	var hash string
	err = db.QueryRowContext(ctx, `SELECT token_hash, payload FROM identity_bootstrap_enrollment WHERE `+predicate, arg).Scan(&hash, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var row BootstrapEnrollment
	if err = json.Unmarshal(payload, &row); err != nil {
		return nil, err
	}
	row.TokenHash = hash
	return &row, nil
}

func (s *Store) ReservedBootstrap(ctx context.Context) (*BootstrapEnrollment, error) {
	return s.readBootstrap(ctx, `reserved = $1`, true)
}

func (s *Store) BootstrapEnrollment(ctx context.Context, token string, cfg Config) (*BootstrapEnrollment, error) {
	if token == "" {
		return nil, ErrBootstrapEnrollment
	}
	row, err := s.readBootstrap(ctx, `token_hash = $1 AND expires_at > now()`, bootstrapHash(token))
	if err != nil {
		return nil, err
	}
	if row == nil || row.Local != cfg.LocalPasskeyOnly() || (!row.Local && !row.EmailVerified) {
		return nil, ErrBootstrapEnrollment
	}
	return row, nil
}

// BeginBootstrapEnrollmentLocked requires AcquireBootstrapGate. Hosted callers
// arrive only from the verified magic-link finisher. A retry of the same email
// rotates enrollment authority without replacing the reserved profile or proof.
func (s *Store) BeginBootstrapEnrollmentLocked(ctx context.Context, cfg Config, settings ClusterSettingsRow, oauth map[string]string, verified bool) (string, error) {
	email, emailErr := mail.ParseAddress(settings.BootstrapEmail)
	if emailErr != nil || email.Address != settings.BootstrapEmail || strings.TrimSpace(settings.BrandName) == "" || utf8.RuneCountInString(settings.BrandName) > 200 || settings.ClusterDomain == "" || settings.BootstrapFirstName == "" || settings.BootstrapLastName == "" {
		return "", errors.New("valid owner details, organization name and installation domain are required")
	}
	switch settings.RegistrationMode {
	case "", "open", "invite_only", "waitlist":
	case "domain_restricted":
		if strings.TrimSpace(settings.RegistrationDomains) == "" {
			return "", errors.New("approved email domains are required")
		}
	default:
		return "", errors.New("unknown registration mode")
	}
	if !cfg.LocalPasskeyOnly() && !verified {
		return "", ErrBootstrapEnrollment
	}
	reserved, err := s.ReservedBootstrap(ctx)
	if err != nil {
		return "", err
	}
	if reserved != nil && (reserved.Complete || reserved.Local || !verified || !strings.EqualFold(reserved.Settings.BootstrapEmail, settings.BootstrapEmail)) {
		return "", ErrBootstrapEnrollment
	}
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(bytes)
	uid, err := NewRandomId("")
	if err != nil {
		return "", err
	}
	iid, err := NewRandomId("")
	if err != nil {
		return "", err
	}
	row := &BootstrapEnrollment{UserID: uid, IdentityID: iid, Local: cfg.LocalPasskeyOnly(), EmailVerified: verified, Settings: settings, OAuth: oauth}
	db, err := s.bootstrapDB()
	if err != nil {
		return "", err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM identity_bootstrap_enrollment WHERE NOT reserved AND expires_at <= now()`); err != nil {
		return "", err
	}
	if reserved != nil {
		row = reserved
		payload, _ := json.Marshal(row)
		_, err = db.ExecContext(ctx, `UPDATE identity_bootstrap_enrollment SET token_hash=$1, payload=$2::jsonb, expires_at=$3 WHERE token_hash=$4`, bootstrapHash(token), string(payload), time.Now().Add(24*time.Hour), reserved.TokenHash)
	} else {
		payload, _ := json.Marshal(row)
		_, err = db.ExecContext(ctx, `INSERT INTO identity_bootstrap_enrollment(token_hash,payload,reserved,expires_at) VALUES($1,$2::jsonb,$3,$4)`, bootstrapHash(token), string(payload), !row.Local, time.Now().Add(24*time.Hour))
	}
	return token, err
}

// SaveBootstrapEnrollmentLocked durably reserves the one winning claim before
// graph writes. Once proof exists, another local browser cannot replace it.
func (s *Store) SaveBootstrapEnrollmentLocked(ctx context.Context, row *BootstrapEnrollment) error {
	db, err := s.bootstrapDB()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(row)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE identity_bootstrap_enrollment SET payload=$1::jsonb, reserved=true WHERE token_hash=$2`, string(payload), row.TokenHash)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrBootstrapEnrollment
	}
	return nil
}

// ConfigureBootstrapOrganization is the organization write in verified claim
// completion. The caller holds the shared claim lock and has persisted the
// attestation and real owner; the groups handler independently verifies that
// owner and performs resumable writes to the reserved self account.
func (s *Store) ConfigureBootstrapOrganization(ctx context.Context, row *BootstrapEnrollment) error {
	if row == nil || len(row.Proof) == 0 || row.UserID == "" || (!row.Local && !row.EmailVerified) {
		return ErrBootstrapEnrollment
	}
	ctx = auth.ContextWithInternalOrigin(ContextWithSystemCredentialActor(ctx))
	_, err := s.Engine.Execute(ctx, "builtin configureSelfAccount(name: "+langparser.QuoteString(row.Settings.BrandName)+", ownerUserId: "+langparser.QuoteString(row.UserID)+")")
	return err
}
