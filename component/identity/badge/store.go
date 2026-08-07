package badge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// Store wraps the memQL engine with typed badge operations. Mirrors
// the workertoken.Store pattern: create, revoke, lookup, list, touch.
type Store struct {
	Engine identity.EngineExecutor
	Logger *slog.Logger
}

// Row is the Go projection of a badge identity row.
type Row struct {
	ID           string
	UserId       string
	Label        string
	KeyHash      string
	RegisteredBy string
	Active       bool
	LastUsedAt   time.Time
	CreatedAt    time.Time
}

// Create persists a new badge identity row.
//
// Caller flow:
//
//	hash := badge.HashBadgeId(badgeId)   // plaintext never persisted
//	id, err := badge.NewId()
//	err = store.Create(ctx, id, userId, label, hash, registeredBy)
func (s *Store) Create(ctx context.Context, identityId, userId, label, keyHash, registeredBy string) error {
	if s == nil || s.Engine == nil {
		return errors.New("badge.Store: engine not wired")
	}
	if identityId == "" || userId == "" || keyHash == "" || label == "" {
		return errors.New("badge.Store.Create: identityId, userId, label, keyHash all required")
	}
	q := fmt.Sprintf(
		`mutation createBadgeIdentity(identityId:%q,userId:%q,label:%q,keyHash:%q,registeredBy:%q)`,
		identityId, userId, label, keyHash, registeredBy,
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("badge.Store.Create: %w", err)
	}
	return nil
}

// Revoke flips active=false on the row. Future grant exchanges are
// blocked immediately; outstanding short-TTL grant tokens expire on
// their own.
func (s *Store) Revoke(ctx context.Context, identityId string) error {
	if s == nil || s.Engine == nil {
		return errors.New("badge.Store: engine not wired")
	}
	q := fmt.Sprintf(`mutation revokeBadgeIdentity(identityId:%q)`, bareSlug(identityId))
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("badge.Store.Revoke: %w", err)
	}
	return nil
}

// LookupByKeyHash is the grant-exchange hot path. Returns the row
// (active OR inactive -- the handler decides what to do with revoked
// badges, so the audit reason is precise) or nil when no badge
// matches.
func (s *Store) LookupByKeyHash(ctx context.Context, keyHash string) (*Row, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("badge.Store: engine not wired")
	}
	if keyHash == "" {
		return nil, nil
	}
	q := fmt.Sprintf(`query badgeByKeyHash(keyHash:%q)`, keyHash)
	rows, err := s.executeAndExtract(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("badge.Store.LookupByKeyHash: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rowFromNode(rows[0]), nil
}

// ListForSelf returns every badge identity owned by the CALLER (active
// + inactive). badgesForSelf is `paginate 50`, so the keyset cursor
// is walked to assemble the complete set (mirrors workertoken).
//
// SELF-SCOPED (memql#3178). There is no userId parameter: the row set
// comes from `userId==actor.userId`, so this cannot be pointed at
// another user's badges. ctx MUST carry an AccessContext for the caller
// -- claims alone resolve actor.userId to "" and yield zero rows. The
// one production caller (component/grpc/badge_handlers.go) was already
// passing the caller's own id; it now passes the caller's own CONTEXT
// instead, which is the same intent expressed so the engine enforces it.
func (s *Store) ListForSelf(ctx context.Context) ([]Row, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("badge.Store: engine not wired")
	}
	const q = `query badgesForSelf()`
	out := []Row{}
	cursor := ""
	for i := 0; i < maxPageWalk; i++ {
		nodes, next, err := s.executeAndExtractPage(ctx, q, cursor)
		if err != nil {
			return nil, fmt.Errorf("badge.Store.ListForSelf: %w", err)
		}
		for _, n := range nodes {
			if r := rowFromNode(n); r != nil {
				out = append(out, *r)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// TouchLastUsed stamps credentials.lastUsedAt after a successful grant
// exchange. Best-effort: callers log failures and move on -- the grant
// must not fail on a bookkeeping write.
func (s *Store) TouchLastUsed(ctx context.Context, row *Row, at time.Time) error {
	if s == nil || s.Engine == nil {
		return errors.New("badge.Store: engine not wired")
	}
	if row == nil || row.ID == "" {
		return errors.New("badge.Store.TouchLastUsed: row required")
	}
	// Partial nested updates are not supported: merge the full
	// credentials object with only lastUsedAt changed.
	creds := fmt.Sprintf(`{keyHash:%q,registeredBy:%q,lastUsedAt:%q}`,
		row.KeyHash, row.RegisteredBy, at.UTC().Format(time.RFC3339Nano))
	q := fmt.Sprintf(`mutation touchBadgeLastUsed(identityId:%q,credentials:%s)`, bareSlug(row.ID), creds)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("badge.Store.TouchLastUsed: %w", err)
	}
	return nil
}

// TerminalBearerRevoked reports whether the terminal's bearer token
// belongs to a REVOKED auth session (memql#2513). The grant endpoint
// runs on the identity node, which does not carry the per-node
// session-revocation interceptor (that gates the bff/mesh gRPC plane),
// so a station whose session an admin revoked in response to a theft
// would otherwise keep minting operator grants until the bearer's
// natural exp. This closes that window by consulting the same
// authSession row the interceptor checks.
//
// Returns (revoked, err). A missing session row is NOT revoked
// (returns false): not every valid bearer has a session row (e.g.
// synthetic/dev tokens), and failing closed on absence would break
// those; the caller logs and proceeds on a lookup error.
func (s *Store) TerminalBearerRevoked(ctx context.Context, bearer string) (bool, error) {
	if s == nil || s.Engine == nil {
		return false, errors.New("badge.Store: engine not wired")
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(bearer)))
	tokenHash := hex.EncodeToString(sum[:])
	q := fmt.Sprintf(`query authSessionByTokenHash(tokenHash:%q)`, tokenHash)
	res, err := s.Engine.Execute(ctx, q)
	if err != nil {
		return false, fmt.Errorf("badge.Store.TerminalBearerRevoked: %w", err)
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return false, nil
	}
	fields := res.Bundle.Nodes[0].GetPayload().GetFields()
	if fields == nil {
		return false, nil
	}
	if v, ok := fields["revokedAt"]; ok && v != nil {
		return strings.TrimSpace(v.GetStringValue()) != "", nil
	}
	return false, nil
}

// maxPageWalk bounds the keyset walk (mirrors workertoken.maxPageWalk).
const maxPageWalk = 1000

// executeAndExtract / executeAndExtractPage mirror workertoken.Store:
// raw queries return Bundle nodes; shape-wrapped queries (identityFull)
// return Data structs that get synthesized into MemoryNodes so one
// rowFromNode covers both paths.
func (s *Store) executeAndExtract(ctx context.Context, query string) ([]*memqlv1.MemoryNode, error) {
	nodes, _, err := s.executeAndExtractPage(ctx, query, "")
	return nodes, err
}

func (s *Store) executeAndExtractPage(ctx context.Context, query, cursor string) ([]*memqlv1.MemoryNode, string, error) {
	res, err := s.Engine.Execute(memqlengine.ContextWithCursor(ctx, cursor), query)
	if err != nil {
		return nil, "", err
	}
	if res == nil {
		return nil, "", nil
	}
	next := ""
	if res.Meta != nil {
		next = res.Meta.Cursor
	}
	if res.Bundle != nil && len(res.Bundle.Nodes) > 0 {
		return res.Bundle.Nodes, next, nil
	}
	_, data, err := res.ToAPIResult()
	if err != nil {
		return nil, "", fmt.Errorf("badge.Store: extract shape result: %w", err)
	}
	if len(data) == 0 {
		return nil, next, nil
	}
	out := make([]*memqlv1.MemoryNode, 0, len(data))
	for _, v := range data {
		if v == nil {
			continue
		}
		sv := v.GetStructValue()
		if sv == nil {
			continue
		}
		node := &memqlv1.MemoryNode{
			Payload: &structpb.Struct{Fields: sv.Fields},
		}
		if id, ok := sv.Fields["id"]; ok {
			node.Id = id.GetStringValue()
		}
		out = append(out, node)
	}
	return out, next, nil
}

func rowFromNode(n *memqlv1.MemoryNode) *Row {
	if n == nil || n.Payload == nil {
		return nil
	}
	fields := n.Payload.GetFields()
	if fields == nil {
		return nil
	}
	str := func(k string) string {
		if v, ok := fields[k]; ok && v != nil {
			return strings.TrimSpace(v.GetStringValue())
		}
		return ""
	}
	boolean := func(k string, def bool) bool {
		v, ok := fields[k]
		if !ok || v == nil {
			return def
		}
		if _, isBool := v.GetKind().(*structpb.Value_BoolValue); isBool {
			return v.GetBoolValue()
		}
		return strings.EqualFold(strings.TrimSpace(v.GetStringValue()), "true")
	}
	parsedTime := func(s string) time.Time {
		if s == "" {
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t, err = time.Parse(time.RFC3339, s)
			if err != nil {
				return time.Time{}
			}
		}
		return t
	}
	row := &Row{
		ID:        firstNonEmpty(str("id"), n.GetId()),
		UserId:    str("userId"),
		Label:     str("label"),
		Active:    boolean("active", true),
		CreatedAt: parsedTime(str("createdAt")),
	}
	if creds, ok := fields["credentials"]; ok && creds != nil {
		if structVal, isStruct := creds.GetKind().(*structpb.Value_StructValue); isStruct {
			cf := structVal.StructValue.GetFields()
			get := func(k string) string {
				if v, ok := cf[k]; ok && v != nil {
					return strings.TrimSpace(v.GetStringValue())
				}
				return ""
			}
			row.KeyHash = get("keyHash")
			row.RegisteredBy = get("registeredBy")
			row.LastUsedAt = parsedTime(get("lastUsedAt"))
		}
	}
	return row
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
