package workerpairing

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

// canonicalIdPrefix is the engine-stamped prefix on every pairing-
// code row's id.
const canonicalIdPrefix = "v1:identity:workerPairingCode:"

// Store wraps the engine with typed pairing-code operations. Mirrors
// the workertoken.Store and pat.Store patterns.
type Store struct {
	Engine identity.EngineExecutor
	Logger *slog.Logger
}

// Row is the Go projection of a v1:identity:workerPairingCode row.
type Row struct {
	ID             string
	OwnerUserId    string
	CodeHash       string
	ClusterURL     string
	ExpiresAt      time.Time
	RedeemedAt     time.Time
	RedeemedBy     string
	RedeemedFromIP string
	SourceIP       string
	CreatedAt      time.Time
}

// IsRedeemed reports whether the row has already been used.
func (r Row) IsRedeemed() bool { return !r.RedeemedAt.IsZero() }

// IsExpired reports whether the row has aged past expiresAt.
func (r Row) IsExpired(now time.Time) bool {
	return !r.ExpiresAt.IsZero() && now.After(r.ExpiresAt)
}

// NewId mints a fresh pairing-code identity slug ("wpc-<32 hex>").
func NewId() (string, error) { return identity.NewRandomId("") }

// CanonicalId converts a bare slug to the canonical id form.
func CanonicalId(slugOrFull string) string {
	if strings.HasPrefix(slugOrFull, canonicalIdPrefix) {
		return slugOrFull
	}
	return canonicalIdPrefix + slugOrFull
}

// BareSlug strips the canonical prefix off an id.
func BareSlug(id string) string {
	if strings.HasPrefix(id, canonicalIdPrefix) {
		return strings.TrimPrefix(id, canonicalIdPrefix)
	}
	return id
}

// Create persists a new pairing-code row.
func (s *Store) Create(ctx context.Context, pairingId, ownerUserId, codeHash, clusterURL string, expiresAt time.Time, sourceIP string) error {
	if s == nil || s.Engine == nil {
		return errors.New("workerpairing.Store: engine not wired")
	}
	if pairingId == "" || ownerUserId == "" || codeHash == "" || clusterURL == "" {
		return errors.New("workerpairing.Store.Create: pairingId, ownerUserId, codeHash, clusterURL all required")
	}
	q := fmt.Sprintf(
		`mutation createWorkerPairingCode(pairingId:%q,ownerUserId:%q,codeHash:%q,clusterURL:%q,expiresAt:%q,sourceIP:%q)`,
		pairingId, ownerUserId, codeHash, clusterURL,
		expiresAt.UTC().Format(time.RFC3339Nano), sourceIP,
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("workerpairing.Store.Create: %w", err)
	}
	return nil
}

// Redeem stamps redeemedAt + redeemedBy on a pairing-code row.
// Caller is responsible for first checking the row is unredeemed +
// unexpired (LookupByHash returns the row regardless of state); this
// mutation just persists the transition.
func (s *Store) Redeem(ctx context.Context, pairingId, redeemedBy, redeemedFromIP string, at time.Time) error {
	if s == nil || s.Engine == nil {
		return errors.New("workerpairing.Store: engine not wired")
	}
	q := fmt.Sprintf(
		`mutation redeemWorkerPairingCode(pairingId:%q,redeemedAt:%q,redeemedBy:%q,redeemedFromIP:%q)`,
		BareSlug(pairingId), at.UTC().Format(time.RFC3339Nano), redeemedBy, redeemedFromIP,
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("workerpairing.Store.Redeem: %w", err)
	}
	return nil
}

// LookupByHash is the gRPC interceptor hot-path. Returns the row
// (any state -- the caller decides whether expired/redeemed rows
// are acceptable) or nil when no code matches.
func (s *Store) LookupByHash(ctx context.Context, codeHash string) (*Row, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("workerpairing.Store: engine not wired")
	}
	if codeHash == "" {
		return nil, nil
	}
	q := fmt.Sprintf(`query workerPairingCodeByHash(codeHash:%q)`, codeHash)
	res, err := s.Engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("workerpairing.Store.LookupByHash: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	// workerPairingCodeByHash uses shape() so rows land in
	// res.OutputPayload (Data axis), not Bundle.Nodes. Same shape-axis
	// fall-through used by component/identity/store.go,
	// workertoken/store.go, and pat/store.go.
	if res.Bundle != nil && len(res.Bundle.Nodes) > 0 {
		return rowFromNode(res.Bundle.Nodes[0]), nil
	}
	_, data, err := res.ToAPIResult()
	if err != nil {
		return nil, fmt.Errorf("workerpairing.Store.LookupByHash: extract shape: %w", err)
	}
	if len(data) == 0 || data[0] == nil {
		return nil, nil
	}
	sv := data[0].GetStructValue()
	if sv == nil {
		return nil, nil
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
	return rowFromNode(node), nil
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
	parsedTime := func(k string) time.Time {
		raw := str(k)
		if raw == "" {
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			t, err = time.Parse(time.RFC3339, raw)
			if err != nil {
				return time.Time{}
			}
		}
		return t
	}
	return &Row{
		ID:             firstNonEmpty(str("id"), n.GetId()),
		OwnerUserId:    str("ownerUserId"),
		CodeHash:       str("codeHash"),
		ClusterURL:     str("clusterURL"),
		ExpiresAt:      parsedTime("expiresAt"),
		RedeemedAt:     parsedTime("redeemedAt"),
		RedeemedBy:     str("redeemedBy"),
		RedeemedFromIP: str("redeemedFromIP"),
		SourceIP:       str("sourceIP"),
		CreatedAt:      parsedTime("createdAt"),
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
