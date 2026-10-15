// Package roletest issues bounded sessions for real, separately owned test
// principals. It never borrows the operator's row ownership or login factors.
package roletest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/verifier"
	memql "github.com/znasllc-io/memql/component/memql"
)

const Lifetime = 30 * time.Minute
const TokenPrefix = "mql_test_"

var ErrDenied = errors.New("role testing is unavailable for this identity or role")
var ErrInvalid = errors.New("invalid or expired role-test session")

type Session struct {
	ID        string
	UserID    string
	Role      string
	ExpiresAt time.Time
	// Token is returned once by Start. It is never persisted or audited.
	Token string
}

type Service struct {
	Store *identity.Store
	Audit identity.AuditLogger
	Now   func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func (s *Service) db() (*sql.DB, error) {
	if s.Store == nil || s.Store.DirectDB == nil || s.Store.DirectDB() == nil || s.Audit == nil {
		return nil, errors.New("role testing is not configured")
	}
	return s.Store.DirectDB(), nil
}
func fresh(ctx context.Context) context.Context {
	return memql.ContextWithFreshRead(auth.ContextForRoleTestValidation(ctx))
}
func (s *Service) actor(ctx context.Context) (*identity.UserRow, error) {
	ac, ok := auth.AccessFromContext(ctx)
	claims, _ := auth.ClaimsFromContext(ctx)
	class, _ := claims["class"].(string)
	if claims["pat_id"] != nil {
		return nil, ErrDenied
	}
	if _, delegated := auth.DelegationFromContext(ctx); delegated {
		return nil, ErrDenied
	}
	if !ok || ac == nil || claims["sub"] != ac.UserId || ac.Synthetic || ac.Unranked || auth.IsRoleTestUser(ac.UserId) || (class != "" && class != verifier.ClassUser) {
		return nil, ErrDenied
	}
	u, err := s.Store.LookupUserById(fresh(ctx), ac.UserId)
	if err != nil || u == nil || !u.Active || (u.Role != string(auth.RoleOwner) && u.Role != string(auth.RoleDeveloper)) {
		return nil, ErrDenied
	}
	epoch := int64(0)
	if value, exists := claims["revocation_epoch"]; exists {
		epoch, err = strconv.ParseInt(fmt.Sprint(value), 10, 64)
		if err != nil {
			return nil, ErrDenied
		}
	}
	expires, err := strconv.ParseInt(fmt.Sprint(claims["exp"]), 10, 64)
	if err != nil || epoch != u.RevocationEpoch || !s.now().Before(time.Unix(expires, 0)) {
		return nil, ErrDenied
	}
	return u, nil
}

// Developer sessions cannot restore capabilities denied to the operator. Read
// the grant source explicitly so a failed read refuses instead of falling back
// to a role default. Personas start without any group or per-user grants.
func (s *Service) mayTest(ctx context.Context, operator *identity.UserRow, role string) bool {
	if !auth.MayTestRole(auth.Role(operator.Role), auth.Role(role)) {
		return false
	}
	if operator.Role == string(auth.RoleOwner) {
		return true
	}
	src := auth.InstalledGrantSource()
	if src == nil {
		return false
	}
	grants, err := src.ActiveGrantsForUser(fresh(ctx), operator.ID)
	if err != nil {
		return false
	}
	// A restriction on a developer is intentionally a reason to use their
	// real account, not to mint an unrestricted copy of their base role.
	for _, g := range grants {
		if !g.Inactive && g.Effect == auth.GrantDeny {
			return false
		}
	}
	ms := auth.InstalledMembershipSource()
	if ms != nil {
		checked, ok := ms.(auth.CheckedMembershipSource)
		if !ok {
			return false
		}
		groups, err := checked.ActiveGroupIdsForUserChecked(fresh(ctx), operator.ID)
		if err != nil {
			return false
		}
		groupGrants, err := src.ActiveGrantsForGroups(fresh(ctx), groups)
		if err != nil {
			return false
		}
		for _, g := range groupGrants {
			if !g.Inactive && g.Effect == auth.GrantDeny {
				return false
			}
		}
	}
	ctx = fresh(ctx)
	subject, ok := auth.SubjectFromContext(auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: operator.ID, Role: auth.Role(operator.Role)}))
	if !ok {
		return false
	}
	for _, d := range auth.EffectiveCapabilities(ctx, auth.Subject{Role: auth.Role(role)}) {
		// Viewer/Member can read their own groups; Developer has no such
		// catalog grant. A persona has no memberships, and verification
		// rejects one that acquires any, so this grants access to no rows.
		if d.Verb == auth.VerbRead && d.Resource == auth.ResourceGroup {
			continue
		}
		if d.Held && !auth.CapableFor(ctx, subject, d.Verb, d.Resource) {
			return false
		}
	}
	return true
}

func (s *Service) auditRoleTestSession(ctx context.Context, action, operator, target, role string, err error) {
	if s.Audit == nil {
		return
	}
	outcome := identity.AuditOutcomeSuccess
	reason := ""
	if err != nil {
		outcome = identity.AuditOutcomeFailure
		reason = "refused_or_failed"
	}
	s.Audit.Log(fresh(ctx), identity.AuditEvent{OccurredAt: s.now(), Category: identity.AuditCategoryAdmin, Action: action,
		ActorUserId: operator, TargetType: "session", TargetId: target,
		Detail: map[string]any{"role": role}, Outcome: outcome, FailureReason: reason})
}

func (s *Service) Start(ctx context.Context, role string) (out Session, err error) {
	var actorID string
	defer func() { s.auditRoleTestSession(ctx, "role_test_started", actorID, out.ID, role, err) }()
	db, err := s.db()
	if err != nil {
		return out, err
	}
	operator, err := s.actor(ctx)
	if err != nil {
		return out, err
	}
	actorID = operator.ID
	if !s.mayTest(ctx, operator, role) {
		return out, ErrDenied
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	// Serialize provisioning across BFF replicas without holding an engine
	// transaction open. A failed commit can leave an inert, unmapped user;
	// its reserved id cannot authenticate without a committed test session.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "role-test:"+actorID+":"+role); err != nil {
		return out, err
	}
	userID := "v1:identity:user:" + auth.RoleTestUserPrefix + uuid.NewString()
	if _, err = tx.ExecContext(ctx, `INSERT INTO identity_role_test_personas(operator_id,role,user_id) VALUES($1,$2,$3) ON CONFLICT(operator_id,role) DO NOTHING`, actorID, role, userID); err != nil {
		return out, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT user_id FROM identity_role_test_personas WHERE operator_id=$1 AND role=$2`, actorID, role).Scan(&userID); err != nil {
		return out, err
	}
	u, err := s.Store.LookupUserById(fresh(ctx), userID)
	if err != nil {
		return out, err
	}
	if u == nil {
		name := map[string]string{"reader": "Viewer", "writer": "Member", "developer": "Developer", "admin": "Admin"}[role]
		// The preceding actor/mayTest gates authorize exactly this bounded
		// persona. Its reserved ID is server-generated, the actor is unchanged,
		// and this inline stamp opens only the server-only user-creation write.
		// Never return the stamped context or accept a caller-selected user ID.
		err = s.Store.CreateUserOnFirstLogin(auth.ContextWithInternalOrigin(ctx), userID, "Test "+name,
			strings.TrimPrefix(userID, "v1:identity:user:")+"@role-test.invalid", role, false, identity.UserProfileSeed{})
		if err != nil {
			return out, err
		}
	} else if !u.Active || u.Role != role {
		return out, ErrDenied
	}
	var entropy [32]byte
	if _, err = rand.Read(entropy[:]); err != nil {
		return out, err
	}
	out = Session{ID: uuid.NewString(), UserID: userID, Role: role, ExpiresAt: s.now().Add(Lifetime), Token: TokenPrefix + hex.EncodeToString(entropy[:])}
	// One active run per operator/role. Restarting a test also ends any old
	// editor tabs for that persona instead of accumulating live credentials.
	if _, err = tx.ExecContext(ctx, `UPDATE identity_role_test_sessions SET revoked_at=$2 WHERE user_id=$1 AND revoked_at IS NULL`, userID, s.now()); err != nil {
		return Session{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO identity_role_test_sessions(id,token_hash,operator_id,operator_epoch,user_id,role,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, out.ID, hash(out.Token), actorID, operator.RevocationEpoch, userID, role, s.now(), out.ExpiresAt); err != nil {
		return Session{}, err
	}
	if err = tx.Commit(); err != nil {
		return Session{}, err
	}
	return out, nil
}

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) VerifyRoleTest(ctx context.Context, token string) (*verifier.VerifiedClaims, error) {
	if len(token) != len(TokenPrefix)+64 || !strings.HasPrefix(token, TokenPrefix) {
		return nil, ErrInvalid
	}
	db, err := s.db()
	if err != nil {
		return nil, err
	}
	var row Session
	var operatorID string
	var epoch int64
	err = db.QueryRowContext(ctx, `SELECT id,operator_id,operator_epoch,user_id,role,expires_at FROM identity_role_test_sessions WHERE token_hash=$1 AND revoked_at IS NULL AND expires_at>$2`, hash(token), s.now()).Scan(&row.ID, &operatorID, &epoch, &row.UserID, &row.Role, &row.ExpiresAt)
	if err != nil {
		return nil, ErrInvalid
	}
	u, err := s.validateRow(ctx, row, operatorID, epoch)
	if err != nil {
		return nil, err
	}
	cm := map[string]any{"sub": u.ID, "email": u.PrimaryEmail, "name": u.DisplayName, "role": row.Role, "class": auth.RoleTestClass, "exp": row.ExpiresAt.Unix(), "role_test_session": row.ID, "role_test_operator": operatorID}
	return &verifier.VerifiedClaims{UserId: u.ID, Email: u.PrimaryEmail, Name: u.DisplayName, Role: row.Role, Class: auth.RoleTestClass, ExpiresAt: row.ExpiresAt, Source: verifier.SourceRoleTest, ClaimsMap: cm}, nil
}

func (s *Service) End(ctx context.Context, sessionID string) (err error) {
	var actorID string
	defer func() { s.auditRoleTestSession(ctx, "role_test_ended", actorID, sessionID, "", err) }()
	db, err := s.db()
	if err != nil {
		return err
	}
	claims, _ := auth.ClaimsFromContext(ctx)
	if claims["class"] == auth.RoleTestClass {
		own, _ := claims["role_test_session"].(string)
		actorID, _ = claims["role_test_operator"].(string)
		if own == "" || sessionID != own {
			return ErrDenied
		}
	} else {
		u, e := s.actor(ctx)
		if e != nil {
			return e
		}
		actorID = u.ID
	}
	result, err := db.ExecContext(ctx, `UPDATE identity_role_test_sessions SET revoked_at=COALESCE(revoked_at,$3) WHERE id=$1 AND operator_id=$2`, sessionID, actorID, s.now())
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrDenied
	}
	return nil
}

func (s *Service) List(ctx context.Context) ([]Session, error) {
	db, err := s.db()
	if err != nil {
		return nil, err
	}
	u, err := s.actor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id,user_id,role,expires_at FROM identity_role_test_sessions WHERE operator_id=$1 AND revoked_at IS NULL AND expires_at>$2 ORDER BY created_at DESC`, u.ID, s.now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Session
	for rows.Next() {
		var r Session
		if err := rows.Scan(&r.ID, &r.UserID, &r.Role, &r.ExpiresAt); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (r Session) String() string { return fmt.Sprintf("role test %s (%s)", r.ID, r.Role) }

func (s *Service) validateRow(ctx context.Context, row Session, operatorID string, epoch int64) (*identity.UserRow, error) {
	operator, err := s.Store.LookupUserById(fresh(ctx), operatorID)
	if err != nil || operator == nil || !operator.Active || operator.RevocationEpoch != epoch || !s.mayTest(ctx, operator, row.Role) {
		return nil, ErrInvalid
	}
	u, err := s.Store.LookupUserById(fresh(ctx), row.UserID)
	if err != nil || u == nil || !u.Active || u.Role != row.Role || !auth.IsRoleTestUser(u.ID) {
		return nil, ErrInvalid
	}
	if src := auth.InstalledGrantSource(); src != nil {
		grants, err := src.ActiveGrantsForUser(fresh(ctx), u.ID)
		if err != nil {
			return nil, ErrInvalid
		}
		for _, g := range grants {
			if !g.Inactive {
				return nil, ErrInvalid
			}
		}
	}
	if ms := auth.InstalledMembershipSource(); ms != nil {
		checked, ok := ms.(auth.CheckedMembershipSource)
		if !ok {
			return nil, ErrInvalid
		}
		groups, err := checked.ActiveGroupIdsForUserChecked(fresh(ctx), u.ID)
		if err != nil || len(groups) != 0 {
			return nil, ErrInvalid
		}
	}
	return u, nil
}

func (s *Service) ValidateRoleTest(ctx context.Context, sessionID, subject string, role auth.Role) error {
	db, err := s.db()
	if err != nil {
		return err
	}
	var row Session
	var operatorID string
	var epoch int64
	err = db.QueryRowContext(ctx, `SELECT id,operator_id,operator_epoch,user_id,role,expires_at FROM identity_role_test_sessions WHERE id=$1 AND revoked_at IS NULL AND expires_at>$2`, sessionID, s.now()).Scan(&row.ID, &operatorID, &epoch, &row.UserID, &row.Role, &row.ExpiresAt)
	if err != nil || row.UserID != subject || row.Role != string(role) {
		return ErrInvalid
	}
	_, err = s.validateRow(ctx, row, operatorID, epoch)
	return err
}
