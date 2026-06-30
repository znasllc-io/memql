package identity

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// EngineDelegationResolver is the concrete auth.DelegationResolver
// implementation called by the gRPC stream interceptor (after JWT
// verification) and by the HTTP middleware variant. Looks up active
// delegation rows whose identitySubject matches the verified user
// id, enforces the four "must hold" constraints from memql#112
// (role ceiling, scope narrowing, lifetime, audit), and returns a
// fully-populated auth.DelegationContext on success.
//
// Constraints:
//
//   - **Role ceiling**: the delegation's roleCeiling MUST NOT exceed
//     the delegating user's own role. RoleLevel(ceiling) < RoleLevel
//     (user.Role) -> reject + blocked audit.
//   - **Lifetime**: the resolver re-checks expiresAt at request time
//     even though IsDelegationExpired is also called by the auth
//     middleware -- defense in depth, and lets us emit a typed audit
//     event the moment a stale row tries to ride through.
//   - **Scope narrowing**: scopes pass through to the
//     DelegationContext, but the universal "*" wildcard is dropped
//     for non-owner / non-admin delegators -- a "*" on a reader
//     delegator would paper over the role gate. Per-role scope
//     allowlists are a follow-up.
//   - **Audit**: every Resolve* outcome (admitted, rejected_expired,
//     rejected_ceiling, orphaned_identity) emits an AuditEvent under
//     category=authorization, action=delegation_*. Suppress is via
//     Auditor==nil (test paths).
//
// Reference: memql#112.
type EngineDelegationResolver struct {
	// Engine runs the DSL queries the resolver depends on. Required.
	// *memql.MemQLEngine satisfies EngineExecutor; tests pass a fake.
	Engine EngineExecutor
	// Auditor receives one event per Resolve outcome. nil = no
	// emit (dev / tests). Production wiring uses the SlogAuditLogger
	// already plumbed for the identity binary.
	Auditor AuditLogger
	// Logger receives a debug line per resolve with the matched row
	// id + outcome reason. nil = silent.
	Logger *slog.Logger
	// Now is a test-friendly clock. nil = time.Now().UTC.
	Now func() time.Time
}

// NewEngineDelegationResolver constructs a resolver and ensures the
// clock + logger fall back to safe defaults.
func NewEngineDelegationResolver(engine EngineExecutor, auditor AuditLogger, logger *slog.Logger) *EngineDelegationResolver {
	return &EngineDelegationResolver{
		Engine:  engine,
		Auditor: auditor,
		Logger:  logger,
		Now:     func() time.Time { return time.Now().UTC() },
	}
}

// ResolveActiveDelegation implements auth.DelegationResolver. Returns
// (nil, nil) when no active delegation applies to the subject -- the
// auth middleware treats that as "no delegation in play, run as the
// authenticated user directly." Returns (nil, err) only on a hard
// infrastructure failure (engine call errored). Every rejection on
// constraint grounds (expired, ceiling, orphan) returns (nil, nil)
// PLUS a blocked audit event so the trail is recoverable.
func (r *EngineDelegationResolver) ResolveActiveDelegation(ctx context.Context, subject string) (*auth.DelegationContext, error) {
	if r == nil || r.Engine == nil {
		return nil, nil
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return nil, nil
	}
	now := r.now()

	rows, err := r.lookupActiveBySubject(ctx, subject)
	if err != nil {
		return nil, fmt.Errorf("delegation resolver: lookup: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	// Pick the most-recently-created row when multiple active
	// delegations exist for the same subject. The frontend should
	// only allow one active grant per (subject, agent) but the
	// resolver picks deterministically when the data drifts.
	chosen := newestRow(rows)

	// Lifetime: re-check at request time (the auth.IsDelegationExpired
	// helper is the consumer-side gate; we want the EARLIER signal so
	// the row stops getting picked).
	if !chosen.ExpiresAt.IsZero() && !now.Before(chosen.ExpiresAt) {
		r.audit(ctx, subject, chosen, "delegation_rejected_expired", "delegation expired", AuditOutcomeBlocked)
		return nil, nil
	}

	// Resolve the delegating user's role. identityId points at the
	// v1:identity:identity row; that row's userId points at the user.
	user, err := r.lookupUserByIdentityId(ctx, chosen.IdentityId)
	if err != nil {
		return nil, fmt.Errorf("delegation resolver: lookup delegating user: %w", err)
	}
	if user == nil {
		r.audit(ctx, subject, chosen, "delegation_rejected_orphan", "delegating identity not resolvable", AuditOutcomeBlocked)
		return nil, nil
	}

	delegatorRole := auth.Role(strings.ToLower(strings.TrimSpace(user.Role)))
	if !auth.IsValidRole(delegatorRole) {
		delegatorRole = auth.RoleReader
	}

	// Role ceiling: lower RoleLevel = higher privilege. Reject if
	// the ceiling outranks the delegator.
	ceiling := auth.Role(strings.ToLower(strings.TrimSpace(chosen.RoleCeiling)))
	if !auth.IsValidRole(ceiling) {
		r.audit(ctx, subject, chosen, "delegation_rejected_ceiling", fmt.Sprintf("invalid roleCeiling %q", chosen.RoleCeiling), AuditOutcomeBlocked)
		return nil, nil
	}
	if auth.RoleLevel(ceiling) < auth.RoleLevel(delegatorRole) {
		r.audit(ctx, subject, chosen, "delegation_rejected_ceiling",
			fmt.Sprintf("ceiling %s exceeds delegator role %s", ceiling, delegatorRole),
			AuditOutcomeBlocked)
		return nil, nil
	}

	// Scope narrowing. See package doc.
	narrowed := narrowScopes(chosen.Scopes, delegatorRole)

	dc := &auth.DelegationContext{
		DelegationId: chosen.ID,
		IdentityId:   chosen.IdentityId,
		DelegatingIdentity: auth.UserIdentity{
			Subject:   chosen.IdentitySubject,
			Email:     user.PrimaryEmail,
			Role:      string(delegatorRole),
			FirstName: user.FirstName,
			LastName:  user.LastName,
		},
		IdentityType: chosen.IdentityType,
		AgentId:      chosen.AgentId,
		RoleCeiling:  ceiling,
		Scopes:       narrowed,
		ExpiresAt:    chosen.ExpiresAt,
	}

	r.audit(ctx, subject, chosen, "delegation_used", "", AuditOutcomeSuccess)
	if r.Logger != nil {
		r.Logger.DebugContext(ctx, "delegation resolved",
			"subject", subject,
			"delegation_id", chosen.ID,
			"agent_id", chosen.AgentId,
			"ceiling", string(ceiling),
			"delegator_role", string(delegatorRole),
			"expires_at", chosen.ExpiresAt,
		)
	}
	return dc, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (r *EngineDelegationResolver) now() time.Time {
	if r != nil && r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

// delegationRow is the in-memory shape projected from a delegationFull
// query. Mirrors the relevant payload fields.
type delegationRow struct {
	ID              string
	IdentityId      string
	IdentitySubject string
	IdentityType    string
	AgentId         string
	RoleCeiling     string
	Scopes          []string
	ExpiresAt       time.Time
	Active          bool
	RevokedAt       time.Time
	CreatedAt       time.Time
}

func (r *EngineDelegationResolver) lookupActiveBySubject(ctx context.Context, subject string) ([]*delegationRow, error) {
	query := fmt.Sprintf(`query activeDelegationsByIdentitySubject(identitySubject: %q)`, subject)
	result, err := r.Engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Bundle == nil || len(result.Bundle.Nodes) == 0 {
		return nil, nil
	}
	out := make([]*delegationRow, 0, len(result.Bundle.Nodes))
	for _, node := range result.Bundle.Nodes {
		row := delegationRowFromNode(node)
		if row == nil {
			continue
		}
		// Belt-and-suspenders: skip revoked rows even though the query's
		// isActiveRecord filter should have done this -- the trait
		// could drift, and a stale row riding through silently here would
		// be worse than an extra in-memory check.
		if !row.Active || !row.RevokedAt.IsZero() {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// lookupUserByIdentityId resolves the v1:identity:identity row that
// owns the given id, then loads the v1:identity:user behind it.
// Two-step because we don't have a single query that joins them.
func (r *EngineDelegationResolver) lookupUserByIdentityId(ctx context.Context, identityId string) (*UserRow, error) {
	identityId = strings.TrimSpace(identityId)
	if identityId == "" {
		return nil, nil
	}
	idQuery := fmt.Sprintf(`query queryIdentityById(identityId: %q)`, identityId)
	result, err := r.Engine.Execute(ctx, idQuery)
	if err != nil {
		return nil, fmt.Errorf("identity lookup: %w", err)
	}
	if result == nil || result.Bundle == nil || len(result.Bundle.Nodes) == 0 {
		return nil, nil
	}
	node := result.Bundle.Nodes[0]
	if node == nil || node.Payload == nil {
		return nil, nil
	}
	userId := strings.TrimSpace(node.Payload.GetFields()["userId"].GetStringValue())
	if userId == "" {
		return nil, nil
	}
	// MemQLEngine already satisfies EngineExecutor (same Execute
	// signature), so we can hand it directly to Store without an
	// adapter type.
	store := &Store{Engine: r.Engine, Logger: r.Logger}
	return store.LookupUserById(ctx, userId)
}

// newestRow picks the most-recently-created delegation from a slice.
// Stable on equal CreatedAt by preferring the first; the query
// already filters to active rows so this is just a deterministic
// tiebreak.
func newestRow(rows []*delegationRow) *delegationRow {
	if len(rows) == 0 {
		return nil
	}
	best := rows[0]
	for _, r := range rows[1:] {
		if r.CreatedAt.After(best.CreatedAt) {
			best = r
		}
	}
	return best
}

// narrowScopes drops scopes that would let a delegation paper over
// the role gate. Today that's just the universal "*" wildcard for
// non-owner / non-admin delegators -- a reader can't grant a
// delegation that admits everything. Per-role scope allowlists are
// a follow-up; this is the safe minimum for now.
func narrowScopes(raw []string, delegatorRole auth.Role) []string {
	if len(raw) == 0 {
		return nil
	}
	out := raw[:0:0] // fresh backing array; never share with the caller
	allowWildcard := delegatorRole == auth.RoleOwner || delegatorRole == auth.RoleAdmin
	for _, s := range raw {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			continue
		}
		if trimmed == "*" && !allowWildcard {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

// sanitizeLogValue strips CR, LF, and other control characters from a
// (potentially user-controlled) string before it reaches the audit log
// sink, so a crafted value cannot forge or split log entries
// (CWE-117 log injection).
func sanitizeLogValue(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1 // drop other control chars
		}
		return r
	}, s)
}

func (r *EngineDelegationResolver) audit(ctx context.Context, subject string, row *delegationRow, action, reason string, outcome AuditOutcome) {
	if r == nil || r.Auditor == nil || row == nil {
		return
	}
	reason = sanitizeLogValue(reason)
	detail := map[string]any{
		"delegation_id":      row.ID,
		"agent_id":           row.AgentId,
		"identity_id":        row.IdentityId,
		"role_ceiling":       row.RoleCeiling,
		"scopes":             row.Scopes,
		"delegating_subject": row.IdentitySubject,
	}
	if !row.ExpiresAt.IsZero() {
		detail["expires_at"] = row.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if reason != "" {
		detail["reason"] = reason
	}
	r.Auditor.Log(ctx, AuditEvent{
		OccurredAt:    r.now(),
		Category:      AuditCategoryAuthorization,
		Action:        action,
		ActorUserId:   subject,
		ActorIdentity: row.IdentityId,
		TargetType:    "delegation",
		TargetId:      row.ID,
		Outcome:       outcome,
		FailureReason: reason,
		Detail:        detail,
	})
}

// delegationRowFromNode projects a MemoryNode into delegationRow.
// Mirrors the shape of delegationFull. Empty / missing fields stay
// at their zero value -- callers (resolver) interpret zero as "not
// stamped".
func delegationRowFromNode(node *memqlv1.MemoryNode) *delegationRow {
	if node == nil || node.Payload == nil {
		return nil
	}
	fields := node.Payload.GetFields()
	getStr := func(k string) string {
		v, ok := fields[k]
		if !ok || v == nil {
			return ""
		}
		return strings.TrimSpace(v.GetStringValue())
	}
	getBool := func(k string) bool {
		v, ok := fields[k]
		if !ok || v == nil {
			return false
		}
		return v.GetBoolValue()
	}
	parseTime := func(k string) time.Time {
		s := getStr(k)
		if s == "" {
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}
		}
		return t
	}
	getStrings := func(k string) []string {
		v, ok := fields[k]
		if !ok || v == nil {
			return nil
		}
		list := v.GetListValue()
		if list == nil {
			return nil
		}
		out := make([]string, 0, len(list.Values))
		for _, item := range list.Values {
			if s := strings.TrimSpace(item.GetStringValue()); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	id := getStr("id")
	if id == "" {
		id = node.Id
	}
	return &delegationRow{
		ID:              id,
		IdentityId:      getStr("identityId"),
		IdentitySubject: getStr("identitySubject"),
		IdentityType:    getStr("identityType"),
		AgentId:         getStr("agentId"),
		RoleCeiling:     getStr("roleCeiling"),
		Scopes:          getStrings("scopes"),
		ExpiresAt:       parseTime("expiresAt"),
		Active:          getBool("active"),
		RevokedAt:       parseTime("revokedAt"),
		CreatedAt:       parseTime("createdAt"),
	}
}
