package nodetoken

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"google.golang.org/protobuf/types/known/structpb"
)

// canonicalIdentityIdPrefix mirrors workertoken.canonicalIdentityIdPrefix.
const canonicalIdentityIdPrefix = "v1:identity:identity:"

// SystemBootstrapMintedBy is the sentinel `mintedBy` value used when
// /node/bootstrap creates a row -- distinguishes bootstrap-path rows
// from operator-CLI mints (which carry the operator's actual userId).
const SystemBootstrapMintedBy = "system:node_bootstrap"

// CanonicalIdentityIdFor returns the deterministic v1:identity:identity
// row id for a given (nodeType, nodeId). Used by /node/bootstrap to
// key the lookup-or-create flow against the same id the JWT subject
// claim already carries -- so the verifier's NodeRevocationCheck can
// pull the row using only the JWT, no separate (nodeType, nodeId)
// extraction.
func CanonicalIdentityIdFor(nodeType, nodeId string) string {
	return canonicalIdentityIdPrefix + "node:" + nodeType + ":" + nodeId
}

// Store wraps the memQL engine with typed node-token row operations.
// Mirrors workertoken.Store's shape; the major shape difference is
// that node tokens have NO Mint() (the JWT is minted by JWTIssuer,
// not here), and the per-call hot path is LookupByIdentityId not
// LookupByKeyHash (the JWT carries the row id as `sub`; the verifier
// reads it from the claim, not from a SHA-256 of the bearer).
type Store struct {
	Engine identity.EngineExecutor
	Logger *slog.Logger
}

// Row is the Go projection of a node-token identity row.
type Row struct {
	ID                 string
	UserId             string
	Active             bool
	NodeId             string
	NodeType           string
	KeyHash            string // The JWT `jti` of the most-recent mint, or empty for pre-mint rows.
	MintedBy           string
	ExpiresAt          time.Time
	LastConnectAt      time.Time
	BootstrappedAt     time.Time
	BootstrappedFrom   string
	LastBootstrappedAt time.Time
	RevokedAt          time.Time
	CreatedAt          time.Time
}

// IsRevoked is the verifier's hot-path check: a row is revoked when
// it's been explicitly stamped (RevokedAt non-zero) OR soft-deleted
// (Active=false). The verifier uses this to decide whether to reject
// an incoming NodeService.Stream open.
func (r *Row) IsRevoked() bool {
	if r == nil {
		return false
	}
	return !r.Active || !r.RevokedAt.IsZero()
}

// CreateInput is the per-call payload for Create. Field names match
// the DSL mutation's arg names for round-trip clarity.
type CreateInput struct {
	IdentityId         string
	UserId             string
	NodeId             string
	NodeType           string
	KeyHash            string
	MintedBy           string
	ExpiresAt          time.Time
	BootstrappedAt     time.Time
	BootstrappedFrom   string
	LastBootstrappedAt time.Time
}

// Create persists a new node-token identity row. Caller flow from
// /node/bootstrap is:
//
//	row, err := store.LookupByIdentityId(ctx, canonicalId)
//	if row == nil {
//	    err = store.Create(ctx, CreateInput{...})  // first bootstrap
//	} else {
//	    err = store.RecordBootstrap(ctx, RecordBootstrapInput{...})
//	}
//
// MintedBy is required; pass SystemBootstrapMintedBy on the bootstrap
// path. KeyHash should carry the JWT's jti claim (informational).
func (s *Store) Create(ctx context.Context, in CreateInput) error {
	if s == nil || s.Engine == nil {
		return errors.New("nodetoken.Store: engine not wired")
	}
	if in.IdentityId == "" || in.UserId == "" || in.NodeId == "" || in.NodeType == "" || in.MintedBy == "" {
		return errors.New("nodetoken.Store.Create: identityId, userId, nodeId, nodeType, mintedBy all required")
	}
	q := fmt.Sprintf(
		`mutationCreateNodeTokenIdentity({identityId:%q,userId:%q,nodeId:%q,nodeType:%q,keyHash:%q,mintedBy:%q,expiresAt:%q,bootstrappedAt:%q,bootstrappedFrom:%q,lastBootstrappedAt:%q})`,
		in.IdentityId,
		in.UserId,
		in.NodeId,
		in.NodeType,
		in.KeyHash,
		in.MintedBy,
		formatTime(in.ExpiresAt),
		formatTime(in.BootstrappedAt),
		in.BootstrappedFrom,
		formatTime(in.LastBootstrappedAt),
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("nodetoken.Store.Create: %w", err)
	}
	return nil
}

// RecordBootstrap updates an existing node-token row with the metadata
// of a fresh bootstrap mint. Called when LookupByIdentityId hit -- the
// row already exists from a prior bootstrap and the node is restarting.
//
// Takes the existing Row so the call site (the bootstrap handler, which
// already fetched the row in its pre-mint lookup) passes through the
// preserved-origin fields (bootstrappedAt + bootstrappedFrom) and the
// row-shape signals (nodeId / nodeType / mintedBy) without an extra
// store read. The DSL mutation does a whole-replace of the credentials
// object (the variant-discriminator validator rejects partials), so
// every field the row needs to carry forward has to be in the args --
// the Store assembles them from existing + the freshly-minted bits.
//
// Updates: keyHash (new JTI), expiresAt (new exp), lastBootstrappedAt
// (now).
// Preserves: bootstrappedAt + bootstrappedFrom + lastConnectAt +
// revokedAt (revokedAt should be empty here -- the bootstrap handler
// short-circuits revoked rows before this point, so we never overwrite
// a non-empty value).
func (s *Store) RecordBootstrap(ctx context.Context, existing *Row, keyHash string, expiresAt, lastBootstrappedAt time.Time) error {
	if s == nil || s.Engine == nil {
		return errors.New("nodetoken.Store: engine not wired")
	}
	if existing == nil {
		return errors.New("nodetoken.Store.RecordBootstrap: existing row required")
	}
	if existing.ID == "" {
		return errors.New("nodetoken.Store.RecordBootstrap: existing.ID required")
	}
	if lastBootstrappedAt.IsZero() {
		return errors.New("nodetoken.Store.RecordBootstrap: lastBootstrappedAt required")
	}
	q := fmt.Sprintf(
		`mutationRecordNodeTokenBootstrap({identityId:%q,nodeId:%q,nodeType:%q,mintedBy:%q,keyHash:%q,expiresAt:%q,bootstrappedAt:%q,bootstrappedFrom:%q,lastBootstrappedAt:%q,lastConnectAt:%q,revokedAt:%q})`,
		existing.ID,
		existing.NodeId,
		existing.NodeType,
		existing.MintedBy,
		keyHash,
		formatTime(expiresAt),
		formatTime(existing.BootstrappedAt),
		existing.BootstrappedFrom,
		formatTime(lastBootstrappedAt),
		formatTime(existing.LastConnectAt),
		formatTime(existing.RevokedAt),
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("nodetoken.Store.RecordBootstrap: %w", err)
	}
	return nil
}

// RevokeAt stamps the caller-supplied revokedAt + flips active=false
// on a node-token row. Same whole-replace pattern as RecordBootstrap:
// the row's credentials object has to be re-sent in full because the
// variant-discriminator validator rejects partials. Does an internal
// LookupByIdentityId first to pick up the fields the call site doesn't
// usually have (the admin handler has only an identityId; the
// preserved fields come from the row).
//
// Tests + future cron paths use this when they need to pin a specific
// timestamp; the /admin/tokens revoke button goes through Revoke
// (which substitutes time.Now()).
func (s *Store) RevokeAt(ctx context.Context, identityId string, when time.Time) error {
	if s == nil || s.Engine == nil {
		return errors.New("nodetoken.Store: engine not wired")
	}
	if identityId == "" {
		return errors.New("nodetoken.Store.RevokeAt: identityId required")
	}
	if when.IsZero() {
		when = time.Now().UTC()
	}
	existing, err := s.LookupByIdentityId(ctx, identityId)
	if err != nil {
		return fmt.Errorf("nodetoken.Store.RevokeAt: lookup: %w", err)
	}
	if existing == nil {
		return fmt.Errorf("nodetoken.Store.RevokeAt: no node_token row for %q", identityId)
	}
	q := fmt.Sprintf(
		`mutationRevokeNodeTokenIdentity({identityId:%q,nodeId:%q,nodeType:%q,mintedBy:%q,keyHash:%q,expiresAt:%q,bootstrappedAt:%q,bootstrappedFrom:%q,lastBootstrappedAt:%q,lastConnectAt:%q,revokedAt:%q})`,
		existing.ID,
		existing.NodeId,
		existing.NodeType,
		existing.MintedBy,
		existing.KeyHash,
		formatTime(existing.ExpiresAt),
		formatTime(existing.BootstrappedAt),
		existing.BootstrappedFrom,
		formatTime(existing.LastBootstrappedAt),
		formatTime(existing.LastConnectAt),
		formatTime(when),
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("nodetoken.Store.RevokeAt: %w", err)
	}
	return nil
}

// LookupByIdentityId is the hot path for both /node/bootstrap (row-
// dedup) and the verifier's NodeRevocationCheck (revocation gate).
// Returns nil when no row exists for the id.
//
// Returns rows even when active=false so the verifier can stamp a
// precise audit reason ("node token revoked") rather than a generic
// "not found".
func (s *Store) LookupByIdentityId(ctx context.Context, identityId string) (*Row, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("nodetoken.Store: engine not wired")
	}
	if identityId == "" {
		return nil, nil
	}
	q := fmt.Sprintf(`queryNodeTokenByIdentityId({identityId:%q})`, identityId)
	rows, err := s.executeAndExtract(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("nodetoken.Store.LookupByIdentityId: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rowFromNode(rows[0]), nil
}

// LookupById is the admin.NodeTokenAdapter port: same as
// LookupByIdentityId but named to match the adapter interface
// convention shared with pat.Store + workertoken.Store.
func (s *Store) LookupById(ctx context.Context, identityId string) (*Row, error) {
	return s.LookupByIdentityId(ctx, identityId)
}

// Revoke is the admin.NodeTokenAdapter port: stamps revokedAt =
// time.Now().UTC() and flips active=false on the named row. The
// /admin/tokens revoke button goes through this. Callers that need
// to pin a specific timestamp (tests, future cron paths) use
// RevokeAt instead.
//
// The verifier's NodeRevocationCheck reads the row on every
// NodeService.Stream open and rejects when revokedAt is non-empty;
// the revoke takes effect on the next dial (modulo the 5s cache TTL).
func (s *Store) Revoke(ctx context.Context, identityId string) error {
	return s.RevokeAt(ctx, identityId, time.Now().UTC())
}

// ListAll returns every node-token row in the cluster. Backs the
// /admin/tokens "node tokens" surface. Returns active + inactive.
func (s *Store) ListAll(ctx context.Context) ([]Row, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("nodetoken.Store: engine not wired")
	}
	rows, err := s.executeAndExtract(ctx, `queryAllNodeTokens({})`)
	if err != nil {
		return nil, fmt.Errorf("nodetoken.Store.ListAll: %w", err)
	}
	out := make([]Row, 0, len(rows))
	for _, n := range rows {
		r := rowFromNode(n)
		if r == nil {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

// formatTime is the canonical time encoding for DSL string fields.
// Empty time => empty string (the DSL coalesces to "" anyway, but
// avoiding "0001-01-01T..." in the wire payload keeps logs clean).
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// executeAndExtract handles the same Bundle-vs-Data result split that
// workertoken.Store.executeAndExtract handles (see that file's comment
// for the why). Shape-wrapped queries route through Data; raw queries
// route through Bundle. Both surfaces are synthesized into MemoryNode
// for downstream rowFromNode.
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
		return nil, fmt.Errorf("nodetoken.Store: extract shape result: %w", err)
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
	parsed := func(k string) time.Time {
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
		ID:        firstNonEmpty(str("id"), n.GetId()),
		UserId:    str("userId"),
		Active:    boolean("active", true),
		CreatedAt: parsed("createdAt"),
	}
	if creds, ok := fields["credentials"]; ok && creds != nil {
		if structVal, isStruct := creds.GetKind().(*structpb.Value_StructValue); isStruct {
			cf := structVal.StructValue.GetFields()
			row.NodeId = strFromStruct(cf, "nodeId")
			row.NodeType = strFromStruct(cf, "nodeType")
			row.KeyHash = strFromStruct(cf, "keyHash")
			row.MintedBy = strFromStruct(cf, "mintedBy")
			row.BootstrappedFrom = strFromStruct(cf, "bootstrappedFrom")
			row.ExpiresAt = parsedTimeFromStruct(cf, "expiresAt")
			row.LastConnectAt = parsedTimeFromStruct(cf, "lastConnectAt")
			row.BootstrappedAt = parsedTimeFromStruct(cf, "bootstrappedAt")
			row.LastBootstrappedAt = parsedTimeFromStruct(cf, "lastBootstrappedAt")
			row.RevokedAt = parsedTimeFromStruct(cf, "revokedAt")
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

func strFromStruct(cf map[string]*structpb.Value, k string) string {
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
	s := strFromStruct(cf, k)
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
