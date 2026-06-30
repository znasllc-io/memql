package workertoken

import (
	"context"
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

// canonicalIdentityIdPrefix mirrors pat.canonicalIdentityIdPrefix.
const canonicalIdentityIdPrefix = "v1:identity:identity:"

// Store wraps the memQL engine with typed worker-token operations.
// Mirrors the pat.Store pattern; lookup, create, revoke, list.
type Store struct {
	Engine identity.EngineExecutor
	Logger *slog.Logger
}

// Row is the Go projection of a worker-token identity row.
type Row struct {
	ID                     string
	UserId                 string
	Name                   string
	KeyHash                string
	RegisteredBy           string
	Active                 bool
	CapabilitiesAdvertised []string
	LabelsAdvertised       map[string]string
	ExpiresAt              time.Time
	LastUsedAt             time.Time
	CreatedAt              time.Time
}

// NewId mints a new worker-token identity slug ("wkr-<32 hex>").
func NewId() (string, error) {
	return identity.NewRandomId("")
}

// CanonicalId converts a bare slug to the canonical id form.
func CanonicalId(slugOrFull string) string {
	if strings.HasPrefix(slugOrFull, canonicalIdentityIdPrefix) {
		return slugOrFull
	}
	return canonicalIdentityIdPrefix + slugOrFull
}

// Create persists a new worker-token identity row.
//
// Caller flow:
//
//	plain, hash, err := workertoken.Mint()
//	id, err := workertoken.NewId()
//	err = store.Create(ctx, id, userId, name, hash, registeredBy, expiresAt)
//	// show plain to the user ONCE, then drop it.
//
// Pass `time.Time{}` for `expiresAt` to mint a non-expiring token;
// the in-stream rotation flow can stamp an expiry later.
func (s *Store) Create(ctx context.Context, identityId, userId, name, keyHash, registeredBy string, expiresAt time.Time) error {
	if s == nil || s.Engine == nil {
		return errors.New("workertoken.Store: engine not wired")
	}
	if identityId == "" || userId == "" || keyHash == "" || name == "" {
		return errors.New("workertoken.Store.Create: identityId, userId, name, keyHash all required")
	}
	expires := ""
	if !expiresAt.IsZero() {
		expires = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	q := fmt.Sprintf(
		`mutation createWorkerTokenIdentity(identityId:%q,userId:%q,name:%q,keyHash:%q,registeredBy:%q,expiresAt:%q)`,
		identityId, userId, name, keyHash, registeredBy, expires,
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("workertoken.Store.Create: %w", err)
	}
	return nil
}

// Revoke flips active=false on the row.
func (s *Store) Revoke(ctx context.Context, identityId string) error {
	if s == nil || s.Engine == nil {
		return errors.New("workertoken.Store: engine not wired")
	}
	q := fmt.Sprintf(`mutation revokeWorkerTokenIdentity(identityId:%q)`, bareSlug(identityId))
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("workertoken.Store.Revoke: %w", err)
	}
	return nil
}

// LookupByKeyHash is the gRPC interceptor hot-path. Returns the
// row (active OR inactive -- the interceptor decides what to do
// with revoked tokens) or nil when no token matches.
func (s *Store) LookupByKeyHash(ctx context.Context, keyHash string) (*Row, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("workertoken.Store: engine not wired")
	}
	if keyHash == "" {
		return nil, nil
	}
	q := fmt.Sprintf(`query workerTokenByKeyHash(keyHash:%q)`, keyHash)
	rows, err := s.executeAndExtract(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("workertoken.Store.LookupByKeyHash: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rowFromNode(rows[0]), nil
}

// ListForUser returns every worker-token identity owned by the
// user (active + inactive). The keyHash field is populated but
// callers should never re-emit it -- it's the lookup key, not
// data to surface.
func (s *Store) ListForUser(ctx context.Context, userId string) ([]Row, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("workertoken.Store: engine not wired")
	}
	q := fmt.Sprintf(`query workerTokensForUser(userId:%q)`, userId)
	out := []Row{}
	cursor := ""
	// workerTokensForUser is `paginate 50` (5.2 / epic #1964), so a
	// single Execute returns only the first page. The callers of this
	// method (the per-user revoke ownership check) need the COMPLETE set
	// -- a token beyond the first page must still be found-as-owned --
	// so walk the keyset cursor to assemble the full list. maxPageWalk
	// is a runaway backstop.
	for i := 0; i < maxPageWalk; i++ {
		nodes, next, err := s.executeAndExtractPage(ctx, q, cursor)
		if err != nil {
			return nil, fmt.Errorf("workertoken.Store.ListForUser: %w", err)
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

// maxPageWalk bounds the keyset walk so a mis-paginated query can never
// spin forever. workerTokensForUser pages 50 at a time, capping the
// complete-set walk at 50k rows -- far beyond any real holder count.
const maxPageWalk = 1000

func bareSlug(id string) string {
	if strings.HasPrefix(id, canonicalIdentityIdPrefix) {
		return strings.TrimPrefix(id, canonicalIdentityIdPrefix)
	}
	return id
}

// executeAndExtract runs a worker-token query and pulls out a
// memory-node-shaped result list. Two output paths to handle:
//
//  1. Raw queries (concept==X; ...) -> Bundle populated with
//     []*memqlv1.MemoryNode. Pass-through.
//  2. Shape-wrapped queries (`shape(...)` -> identityFull) ->
//     Data populated with []*structpb.Value, each a flat struct
//     of the projected fields. The fieldGetter downstream only
//     reads MemoryNode.Payload.Fields, so synthesize MemoryNodes
//     from each Data entry.
//
// Without (2), the gRPC interceptor's hot-path
// workerTokenByKeyHash silently returned zero matches even
// when the worker_token row existed -- the interceptor logged
// "worker token not found", every cockpit reconnect was rejected,
// the in-memory worker registry stayed empty, and Sofia's
// workerStatus tool kept replying "unconfigured" with the green
// header pill clearly contradicting it. Mirrors the same fix
// already applied to component/identity/store.go.
func (s *Store) executeAndExtract(ctx context.Context, query string) ([]*memqlv1.MemoryNode, error) {
	nodes, _, err := s.executeAndExtractPage(ctx, query, "")
	return nodes, err
}

// executeAndExtractPage is the keyset-aware sibling of executeAndExtract:
// it threads an inbound continuation cursor onto the context (5.12 /
// epic #1964 -- workerTokensForUser is `paginate 50` now) and hands
// back the engine's nextCursor. Empty inbound cursor = first page; empty
// returned cursor = set exhausted.
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
		return nil, "", fmt.Errorf("workertoken.Store: extract shape result: %w", err)
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
		if cb, ok := sv.Fields["createdBy"]; ok {
			node.CreatedBy = cb.GetStringValue()
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
	row := &Row{
		ID:         firstNonEmpty(str("id"), n.GetId()),
		UserId:     str("userId"),
		Active:     boolean("active", true),
		LastUsedAt: parsedTime("lastUsedAt"),
		CreatedAt:  parsedTime("createdAt"),
	}
	// credentials is a struct under identityFull -- name, keyHash,
	// registeredBy, expiresAt, capabilitiesAdvertised, labelsAdvertised.
	if creds, ok := fields["credentials"]; ok && creds != nil {
		if structVal, isStruct := creds.GetKind().(*structpb.Value_StructValue); isStruct {
			cf := structVal.StructValue.GetFields()
			row.Name = stringFromStruct(cf, "name")
			row.KeyHash = stringFromStruct(cf, "keyHash")
			row.RegisteredBy = stringFromStruct(cf, "registeredBy")
			row.ExpiresAt = parsedTimeFromStruct(cf, "expiresAt")
			row.CapabilitiesAdvertised = stringListFromStruct(cf, "capabilitiesAdvertised")
			row.LabelsAdvertised = stringMapFromStruct(cf, "labelsAdvertised")
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

func stringFromStruct(cf map[string]*structpb.Value, k string) string {
	if cf == nil {
		return ""
	}
	v, ok := cf[k]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(v.GetStringValue())
}

func parsedTimeFromStruct(cf map[string]*structpb.Value, k string) time.Time {
	s := stringFromStruct(cf, k)
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

func stringListFromStruct(cf map[string]*structpb.Value, k string) []string {
	if cf == nil {
		return nil
	}
	v, ok := cf[k]
	if !ok || v == nil {
		return nil
	}
	list := v.GetListValue()
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list.Values))
	for _, item := range list.Values {
		if item == nil {
			continue
		}
		out = append(out, item.GetStringValue())
	}
	return out
}

func stringMapFromStruct(cf map[string]*structpb.Value, k string) map[string]string {
	if cf == nil {
		return nil
	}
	v, ok := cf[k]
	if !ok || v == nil {
		return nil
	}
	st := v.GetStructValue()
	if st == nil {
		return nil
	}
	out := make(map[string]string, len(st.Fields))
	for kk, vv := range st.Fields {
		if vv == nil {
			continue
		}
		out[kk] = vv.GetStringValue()
	}
	return out
}
