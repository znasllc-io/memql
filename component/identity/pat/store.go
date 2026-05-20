package pat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"google.golang.org/protobuf/types/known/structpb"
)

// canonicalIdentityIdPrefix is what the engine prepends to a bare PAT
// slug when persisting a v1:identity:identity row. Identity is
// @scope("global"), so the partition is always _system regardless of
// the request envelope.
const canonicalIdentityIdPrefix = "default:v1:identity:identity:"

// Store wraps the memQL engine with typed PAT operations: Create,
// Revoke, Lookup, BumpLastUsed, ListForUser. Mirrors the existing
// identity.Store pattern -- thin wrappers around mutations / queries
// that parse the returned graph bundles into typed Go structs.
type Store struct {
	Engine identity.EngineExecutor
	Logger *slog.Logger
}

// PATRow is the Go projection of a PAT identity row used by the
// web UI listings and by the gRPC interceptor.
//
// KeyHash is populated only by lookups that hit the identityFull-
// shaped query (LookupByKeyHash, LookupById). The list paths use
// patSummary which deliberately excludes credentials.
type PATRow struct {
	ID             string // canonical id ("default:v1:identity:identity:pat-...")
	UserId         string
	Label          string
	KeyHash        string
	Active         bool
	UsableByAgents bool
	LastUsedAt     time.Time
	CreatedAt      time.Time
}

// NewId mints a new PAT identity slug ("pat-<32 hex>"). The caller
// passes this to Create; the engine stamps the canonical
// "default:v1:identity:identity:" prefix on persist.
func NewId() (string, error) {
	return identity.NewRandomId("")
}

// CanonicalId converts a bare slug to the canonical id form. Useful
// when reading a slug from a UI form ("pat-abc...") and needing the
// fully-qualified id for an id== filter.
func CanonicalId(slugOrFull string) string {
	if strings.HasPrefix(slugOrFull, canonicalIdentityIdPrefix) {
		return slugOrFull
	}
	return canonicalIdentityIdPrefix + slugOrFull
}

// Create persists a new PAT identity row.
//
// Caller flow:
//
//	plain, hash, err := pat.Mint()
//	id, err := pat.NewId()
//	err = store.Create(ctx, id, userId, label, hash)
//	// show plain to the user ONCE, then drop it.
func (s *Store) Create(ctx context.Context, identityId, userId, label, keyHash string) error {
	if s == nil || s.Engine == nil {
		return errors.New("pat.Store: engine not wired")
	}
	if identityId == "" || userId == "" || keyHash == "" {
		return errors.New("pat.Store.Create: identityId, userId, keyHash all required")
	}
	q := fmt.Sprintf(
		`mutationCreatePATIdentity({identityId:%q,userId:%q,label:%q,keyHash:%q})`,
		identityId, userId, label, keyHash,
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("pat.Store.Create: %w", err)
	}
	return nil
}

// Revoke flips the row's active flag to false. Looks the row up first
// so the new version preserves the discriminator + keyHash; the
// concept's JSON-Schema validator rejects partial rows.
func (s *Store) Revoke(ctx context.Context, identityId string) error {
	if s == nil || s.Engine == nil {
		return errors.New("pat.Store: engine not wired")
	}
	row, err := s.LookupById(ctx, identityId)
	if err != nil {
		return fmt.Errorf("pat.Store.Revoke: %w", err)
	}
	if row == nil {
		return errors.New("pat.Store.Revoke: row not found")
	}
	q := fmt.Sprintf(
		`mutationRevokePATIdentity({identityId:%q,userId:%q,label:%q,keyHash:%q,usableByAgents:%t})`,
		bareSlug(row.ID), row.UserId, row.Label, row.KeyHash, row.UsableByAgents,
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("pat.Store.Revoke: %w", err)
	}
	return nil
}

// LookupByKeyHash is the gRPC-interceptor hot path. Returns the row
// (active OR inactive -- the interceptor handles the active check)
// or nil when no PAT matches.
func (s *Store) LookupByKeyHash(ctx context.Context, keyHash string) (*PATRow, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("pat.Store: engine not wired")
	}
	if keyHash == "" {
		return nil, nil
	}
	q := fmt.Sprintf(`queryPatIdentityByKeyHash({keyHash:%q})`, keyHash)
	rows, err := s.executeAndExtract(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("pat.Store.LookupByKeyHash: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rowFromNode(rows[0]), nil
}

// LookupById returns the PAT identity row by its canonical id (or
// bare slug -- the helper canonicalizes either form).
func (s *Store) LookupById(ctx context.Context, identityId string) (*PATRow, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("pat.Store: engine not wired")
	}
	q := fmt.Sprintf(`queryPatIdentityById({identityId:%q})`, CanonicalId(identityId))
	rows, err := s.executeAndExtract(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("pat.Store.LookupById: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rowFromNode(rows[0]), nil
}

// BumpLastUsed re-stamps lastUsedAt on a PAT row. Best-effort: the
// caller must NOT fail the request when this returns an error. The
// gRPC interceptor logs at warn and proceeds.
//
// Looks the row up first so the new version preserves every other
// field (the engine's append-only model means each version row must
// carry the full discriminator). A missed bump is harmless -- the
// next request will retry.
func (s *Store) BumpLastUsed(ctx context.Context, identityId string) error {
	if s == nil || s.Engine == nil {
		return errors.New("pat.Store: engine not wired")
	}
	row, err := s.LookupById(ctx, identityId)
	if err != nil {
		return fmt.Errorf("pat.Store.BumpLastUsed: %w", err)
	}
	if row == nil {
		return errors.New("pat.Store.BumpLastUsed: row not found")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	q := fmt.Sprintf(
		`mutationBumpPATLastUsedAt({identityId:%q,userId:%q,label:%q,keyHash:%q,active:%t,usableByAgents:%t,lastUsedAt:%q})`,
		bareSlug(row.ID), row.UserId, row.Label, row.KeyHash, row.Active, row.UsableByAgents, now,
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("pat.Store.BumpLastUsed: %w", err)
	}
	return nil
}

// ListForUser returns every PAT identity (active + inactive) owned by
// the given user, in a UI-safe projection (no keyHash). Used by the
// /me/tokens listing.
func (s *Store) ListForUser(ctx context.Context, userId string) ([]PATRow, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("pat.Store: engine not wired")
	}
	q := fmt.Sprintf(`queryPatIdentitiesForUser({userId:%q})`, userId)
	rows, err := s.executeAndExtract(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("pat.Store.ListForUser: %w", err)
	}
	out := make([]PATRow, 0, len(rows))
	for _, n := range rows {
		r := rowFromNode(n)
		if r == nil {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

// ListAll returns every PAT identity (active + inactive) in the
// cluster. Used by the /admin/tokens listing for security-incident
// response. Walks the active-users list -- there's no per-concept
// "list all PATs" query because the patIdentitiesForUser shape
// already scopes by user.
func (s *Store) ListAll(ctx context.Context) ([]PATRow, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("pat.Store: engine not wired")
	}
	users, err := s.queryActiveUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("pat.Store.ListAll: list users: %w", err)
	}
	out := []PATRow{}
	for _, u := range users {
		ofUser, err := s.ListForUser(ctx, u)
		if err != nil {
			s.warn("pat.Store.ListAll: user fetch failed", "userId", u, "error", err)
			continue
		}
		out = append(out, ofUser...)
	}
	return out, nil
}

// queryActiveUsers returns the set of v1:identity:user ids known to
// the cluster. Driven by the Phase-3 queryActiveUsers function.
func (s *Store) queryActiveUsers(ctx context.Context) ([]string, error) {
	res, err := s.Engine.Execute(ctx, `queryActiveUsers({})`)
	if err != nil {
		return nil, err
	}
	out := []string{}
	if res == nil || res.Bundle == nil {
		return out, nil
	}
	for _, n := range res.Bundle.Nodes {
		if n == nil {
			continue
		}
		out = append(out, n.GetId())
	}
	return out, nil
}

// rowFromNode parses a memory node (either identityFull or patSummary
// shape) into a PATRow. Both shapes share enough keys that one parser
// works for both; KeyHash stays empty for patSummary rows.
func rowFromNode(n *memqlv1.MemoryNode) *PATRow {
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
		// Some shapes encode bools as their natural Value type; others
		// (legacy paths) round-trip through strings. Accept both.
		if _, isBool := v.GetKind().(*structpb.Value_BoolValue); isBool {
			return v.GetBoolValue()
		}
		return strings.EqualFold(strings.TrimSpace(v.GetStringValue()), "true")
	}
	parsedTime := func(k string) time.Time {
		s := str(k)
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
	row := &PATRow{
		ID:             firstNonEmpty(str("id"), n.GetId()),
		UserId:         str("userId"),
		Label:          str("label"),
		Active:         boolean("active", true),
		UsableByAgents: boolean("usableByAgents", false),
		LastUsedAt:     parsedTime("lastUsedAt"),
		CreatedAt:      parsedTime("createdAt"),
	}
	// credentials is a struct under identityFull; not present on
	// patSummary. When present, project keyHash for the caller.
	if creds, ok := fields["credentials"]; ok && creds != nil {
		if structVal, isStruct := creds.GetKind().(*structpb.Value_StructValue); isStruct {
			if cf := structVal.StructValue.GetFields(); cf != nil {
				if v, ok := cf["keyHash"]; ok && v != nil {
					row.KeyHash = strings.TrimSpace(v.GetStringValue())
				}
			}
		} else if structAsString := creds.GetStringValue(); structAsString != "" {
			// Some shapes serialize sub-structs as JSON strings; tolerate.
			var parsed map[string]any
			if err := json.Unmarshal([]byte(structAsString), &parsed); err == nil {
				if h, ok := parsed["keyHash"].(string); ok {
					row.KeyHash = strings.TrimSpace(h)
				}
			}
		}
	}
	return row
}

// bareSlug strips the canonical "default:v1:identity:identity:"
// prefix off an id, returning the per-instance slug. The mutations
// expect the slug rather than the full id (the engine re-prepends).
func bareSlug(id string) string {
	if strings.HasPrefix(id, canonicalIdentityIdPrefix) {
		return strings.TrimPrefix(id, canonicalIdentityIdPrefix)
	}
	return id
}

// executeAndExtract runs a PAT query and pulls out a memory-node-
// shaped result list. Same shape-axis fall-through used by
// component/identity/store.go and component/identity/workertoken/store.go
// -- shape() queries land in res.OutputPayload (Data axis), not
// Bundle.Nodes, and the original implementation only read the latter.
// That silently returned zero matches for every PAT lookup
// (queryPatIdentityByKeyHash on the gRPC interceptor hot-path,
// queryPatIdentityById, queryPatIdentitiesForUser for the /me/pats
// page), with no log -- "PAT not found" was indistinguishable from
// "PAT actually missing." Mirroring the workertoken fix here.
func (s *Store) executeAndExtract(ctx context.Context, query string) ([]*memqlv1.MemoryNode, error) {
	res, err := s.Engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	if res.Bundle != nil && len(res.Bundle.Nodes) > 0 {
		return res.Bundle.Nodes, nil
	}
	_, data, err := res.ToAPIResult()
	if err != nil {
		return nil, fmt.Errorf("pat.Store: extract shape result: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
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
		if cb, ok := sv.Fields["createdBy"]; ok {
			node.CreatedBy = cb.GetStringValue()
		}
		out = append(out, node)
	}
	return out, nil
}

func (s *Store) warn(msg string, args ...any) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Warn(msg, args...)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
