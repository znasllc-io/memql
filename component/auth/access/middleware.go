// Package access enforces partition-level authorization on MemQL gRPC
// requests. It runs after the identity-verifier interceptor: claims
// are already validated and attached to the context. This package
// loads the caller's user + partition-access records from the engine
// and rejects requests that target partitions the caller has no grant
// for.
//
// See docs/auth/access-model.md for the full model.
package access

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/visionarys-io/memql/component/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SystemPartition is the reserved partition name for global-scoped
// concepts. Never acceptable on the wire -- the engine routes global
// writes there on its own.
const SystemPartition = "_system"

// DefaultPartition is the fallback when the envelope.partition field
// is empty.
const DefaultPartition = "default"

// QueryRunner is the narrow engine interface the loader needs. It
// executes a MemQL function call and returns the shaped output as a
// Go value. Production passes a thin wrapper around
// component/memql.MemQLEngine.Execute; tests pass a stub.
type QueryRunner interface {
	ExecuteShaped(ctx context.Context, query string) (any, error)
}

// Middleware bundles the loader configuration and the logger. One
// instance is shared across all streams.
type Middleware struct {
	Engine QueryRunner
	Logger *slog.Logger
}

// NewMiddleware constructs a Middleware ready to load ACLs via engine.
func NewMiddleware(engine QueryRunner, logger *slog.Logger) *Middleware {
	return &Middleware{Engine: engine, Logger: logger}
}

// ErrUserNotProvisioned means the caller has a valid JWT but no
// v1:identity:user record exists yet. Callers typically fall back to
// a claims-derived ACL so first-login requests racing the magic-link
// verifier's user-row insert don't fail with PermissionDenied.
var ErrUserNotProvisioned = errors.New("access: user not provisioned in database")

// LoadAccessFromClaims resolves the AccessContext for the current JWT
// claims. On success the returned AccessContext has UserId, Role,
// IdentityId, and PartitionACL populated.
//
// The lookup walks:
//   1. `sub` must already be a canonical v1:identity:user id (every
//      identity-service-issued JWT carries one). Anything else is
//      rejected with ErrUserNotProvisioned.
//   2. queryUserById(userId)                    -> user (for Role + email)
//   3. queryAccessForUser(userId)               -> grants -> PartitionACL
//
// If step 2 returns no rows, ErrUserNotProvisioned is returned so the
// caller can decide whether to short-circuit with a claims-based ACL.
// All other engine errors propagate.
func (m *Middleware) LoadAccessFromClaims(ctx context.Context, claims map[string]any) (*auth.AccessContext, error) {
	if m == nil || m.Engine == nil {
		return nil, errors.New("access middleware: engine not configured")
	}
	subject := stringClaim(claims, "sub")
	if subject == "" {
		return nil, errors.New("access middleware: missing subject claim")
	}

	// Identity-service-issued JWTs carry the canonical user id
	// directly as `sub`. There is no second-hop lookup -- the cluster
	// no longer issues tokens whose subject is an external identifier.
	if !strings.HasPrefix(subject, "_system:v1:identity:user:") &&
		!strings.HasPrefix(subject, "v1:identity:user:") {
		return nil, ErrUserNotProvisioned
	}
	identityId := ""
	userId := subject

	// 2. User by id.
	userQuery := fmt.Sprintf(`queryUserById({"userId": %s})`, quoteJSON(userId))
	user, err := m.Engine.ExecuteShaped(ctx, userQuery)
	if err != nil {
		return nil, fmt.Errorf("userById: %w", err)
	}
	userRow := firstRow(user)
	if userRow == nil {
		return nil, ErrUserNotProvisioned
	}
	role := stringField(userRow, "role")
	email := stringField(userRow, "primaryEmail")

	ac := &auth.AccessContext{
		UserId:       userId,
		PrimaryEmail: email,
		Role:         auth.Role(role),
		IdentityId:   identityId,
		PartitionACL: auth.PartitionACL{},
	}

	// 3. Access grants.
	accessQuery := fmt.Sprintf(`queryAccessForUser({"userId": %s})`, quoteJSON(userId))
	access, err := m.Engine.ExecuteShaped(ctx, accessQuery)
	if err != nil {
		return nil, fmt.Errorf("accessForUser: %w", err)
	}
	for _, row := range allRows(access) {
		// partitionName in the grant concept (partition is reserved as a
		// payload-level storage field in the engine, so we store the
		// grant's target partition under a distinct key).
		part := stringField(row, "partitionName")
		grantRole := stringField(row, "role")
		if part == "" || grantRole == "" {
			continue
		}
		ac.PartitionACL[part] = auth.Role(grantRole)
	}
	return ac, nil
}

// FallbackFromClaims returns a best-effort AccessContext derived solely
// from the JWT claims, used when LoadAccessFromClaims reports
// ErrUserNotProvisioned (typically the first request on a brand-new
// login, racing the magic-link verifier's user+identity+access
// inserts).
//
// The fallback grants the claims-derived role against the default
// partition only. Owner/admin from claims still lets the caller do
// cluster-wide owner things.
func FallbackFromClaims(claims map[string]any) *auth.AccessContext {
	subject := stringClaim(claims, "sub")
	email := firstNonEmptyClaim(claims, "email", "preferred_username")
	roleStr := stringClaim(claims, "role")
	role := auth.Role(strings.ToLower(strings.TrimSpace(roleStr)))
	if !auth.IsValidRole(role) {
		role = auth.RoleReader
	}
	acl := auth.PartitionACL{
		DefaultPartition: role,
	}
	return &auth.AccessContext{
		UserId:       subject, // placeholder until bootstrap lands
		PrimaryEmail: email,
		Role:         role,
		IdentityId:   "",
		PartitionACL: acl,
	}
}

// CheckPartition runs the per-message authorization check. Returns nil
// when the request may proceed, or a PermissionDenied gRPC status
// error otherwise. Also enforces the _system partition block.
//
// Cluster-wide owners bypass the ACL (they're the admin of last resort).
func (m *Middleware) CheckPartition(ctx context.Context, ac *auth.AccessContext, rawPartition string, messageId string) error {
	partition := strings.TrimSpace(rawPartition)
	if partition == "" {
		partition = DefaultPartition
	}

	// _system is always blocked on the wire. The engine routes global
	// concepts there automatically; clients should never need to set
	// envelope.partition to _system.
	if partition == SystemPartition {
		m.auditReject(ctx, ac, partition, messageId, "system_addressed")
		return status.Errorf(codes.PermissionDenied,
			"partition %q is reserved for system use and cannot be addressed directly", partition)
	}

	if ac == nil {
		// Should not happen: LoadAccessFromClaims is called before
		// CheckPartition. Fail closed if it somehow does.
		m.auditReject(ctx, nil, partition, messageId, "no_access_context")
		return status.Error(codes.Unauthenticated, "access context not available")
	}

	if ac.IsClusterOwner() {
		return nil
	}

	if _, ok := ac.RoleForPartition(partition); !ok {
		m.auditReject(ctx, ac, partition, messageId, "no_access")
		return status.Errorf(codes.PermissionDenied, "no access to partition %q", partition)
	}

	return nil
}

func (m *Middleware) auditReject(ctx context.Context, ac *auth.AccessContext, partition, messageId, reason string) {
	if m == nil || m.Logger == nil {
		return
	}
	attrs := []any{
		"partition", partition,
		"message_id", messageId,
		"reason", reason,
	}
	if ac != nil {
		attrs = append(attrs, "user_id", ac.UserId, "cluster_role", string(ac.Role))
	}
	if claims, ok := auth.ClaimsFromContext(ctx); ok {
		if sub, ok := claims["sub"].(string); ok && sub != "" {
			attrs = append(attrs, "subject", sub)
		}
	}
	m.Logger.Info("access middleware: rejected request", attrs...)
}

// --- result helpers ----------------------------------------------------

// allRows flattens the shaped output of a listing query into a slice of
// map[string]any. Accepts either a single row (returned as one-element
// slice) or a []any of rows.
func allRows(result any) []map[string]any {
	switch v := result.(type) {
	case nil:
		return nil
	case map[string]any:
		// Maybe a wrapped result envelope; check "data".
		if data, ok := v["data"]; ok {
			return allRows(data)
		}
		return []map[string]any{v}
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, row := range v {
			if m, ok := row.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case []map[string]any:
		return v
	case json.RawMessage:
		// Rare; try to decode.
		var decoded any
		if err := json.Unmarshal(v, &decoded); err == nil {
			return allRows(decoded)
		}
	}
	return nil
}

// firstRow returns the first row of a shaped result or nil if empty.
func firstRow(result any) map[string]any {
	rows := allRows(result)
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

// stringField reads a string field from a row, tolerating missing/nil.
func stringField(row map[string]any, key string) string {
	if row == nil {
		return ""
	}
	if v, ok := row[key].(string); ok {
		return v
	}
	return ""
}

func stringClaim(claims map[string]any, key string) string {
	if claims == nil {
		return ""
	}
	if v, ok := claims[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func firstNonEmptyClaim(claims map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := stringClaim(claims, k); v != "" {
			return v
		}
	}
	return ""
}

func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
