package enrolment

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

// canonicalIdPrefix is the engine-stamped prefix on every enrolment-token
// row's id.
const canonicalIdPrefix = "v1:identity:enrolmentToken:"

// State is the outcome of validating a presented token.
//
// FOUR REJECTIONS, NOT ONE. The acceptance criterion asks for distinct human
// messages, and that is not cosmetics: "this link expired", "this link was
// already used" and "this link was revoked" each tell the holder a different
// thing to do next, while "invalid" is the only one of the four that should
// ever leave a person guessing. Collapsing them would also hide a real signal
// from the audit trail -- a burst of `already-used` is a replay attempt, a
// burst of `invalid` is a scanner.
type State string

const (
	// StateValid means the token resolves to a live, unspent, unexpired,
	// unrevoked row.
	StateValid State = "valid"
	// StateInvalid means no row matched the presented hash -- a typo, a
	// truncated link, a fabricated token, or one from another cluster.
	StateInvalid State = "invalid"
	// StateExpired means the row aged past expiresAt before anyone used it.
	StateExpired State = "expired"
	// StateAlreadyUsed means consumedAt is stamped: the one registration this
	// token authorized already happened. This is the state a replay lands in.
	StateAlreadyUsed State = "already-used"
	// StateRevoked means the issuer killed the token before it was used.
	StateRevoked State = "revoked"
)

// Store wraps the engine with typed enrolment-token operations. Mirrors
// workerpairing.Store and workertoken.Store.
type Store struct {
	Engine identity.EngineExecutor
	Logger *slog.Logger
}

// Row is the Go projection of a v1:identity:enrolmentToken row.
//
// Note what is NOT here: the plaintext token. The row never carried one and
// this struct has nowhere to put one, which is the point -- there is no field
// a future edit could absent-mindedly persist it into.
type Row struct {
	ID             string
	UserId         string
	TokenHash      string
	IssuedBy       string
	ExpiresAt      time.Time
	ConsumedAt     time.Time
	ConsumedFromIP string
	RevokedAt      time.Time
	SourceIP       string
	CreatedAt      time.Time
}

// IsConsumed reports whether the token has already been spent.
func (r Row) IsConsumed() bool { return !r.ConsumedAt.IsZero() }

// IsRevoked reports whether the issuer killed the token.
func (r Row) IsRevoked() bool { return !r.RevokedAt.IsZero() }

// IsExpired reports whether the row aged past expiresAt.
func (r Row) IsExpired(now time.Time) bool {
	return !r.ExpiresAt.IsZero() && now.After(r.ExpiresAt)
}

// State classifies a row for the redeem path.
//
// ORDER MATTERS AND IS DELIBERATE: revoked beats consumed beats expired. A
// revoked token is a decision somebody made, and saying so is more useful than
// reporting the clock; a consumed token that has also since expired is still
// best described as used, because that is what actually ended it.
func (r Row) State(now time.Time) State {
	switch {
	case r.IsRevoked():
		return StateRevoked
	case r.IsConsumed():
		return StateAlreadyUsed
	case r.IsExpired(now):
		return StateExpired
	default:
		return StateValid
	}
}

// NewId mints a fresh enrolment-token row id.
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
	return strings.TrimPrefix(id, canonicalIdPrefix)
}

// Create persists a new enrolment-token row.
//
// Takes the HASH, never the plaintext. The signature is the enforcement: a
// caller cannot hand this function a plain token even by mistake, because
// there is no parameter for one.
func (s *Store) Create(ctx context.Context, enrolmentId, userId, tokenHash, issuedBy string, expiresAt time.Time, sourceIP string) error {
	if s == nil || s.Engine == nil {
		return errors.New("enrolment.Store: engine not wired")
	}
	if enrolmentId == "" || userId == "" || tokenHash == "" || issuedBy == "" {
		return errors.New("enrolment.Store.Create: enrolmentId, userId, tokenHash, issuedBy all required")
	}
	if expiresAt.IsZero() {
		return errors.New("enrolment.Store.Create: expiresAt required -- a token with no expiry is not a short-lived credential")
	}
	q := fmt.Sprintf(
		`mutation createEnrolmentToken(enrolmentId:%q,userId:%q,tokenHash:%q,issuedBy:%q,expiresAt:%q,sourceIP:%q)`,
		BareSlug(enrolmentId), userId, tokenHash, issuedBy,
		expiresAt.UTC().Format(time.RFC3339Nano), sourceIP,
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("enrolment.Store.Create: %w", err)
	}
	return nil
}

// Consume stamps consumedAt on a row. The caller is responsible for having
// checked State first; this just persists the transition.
func (s *Store) Consume(ctx context.Context, enrolmentId, consumedFromIP string, at time.Time) error {
	if s == nil || s.Engine == nil {
		return errors.New("enrolment.Store: engine not wired")
	}
	if strings.TrimSpace(enrolmentId) == "" {
		return errors.New("enrolment.Store.Consume: enrolmentId required")
	}
	q := fmt.Sprintf(
		`mutation consumeEnrolmentToken(enrolmentId:%q,consumedAt:%q,consumedFromIP:%q)`,
		BareSlug(enrolmentId), at.UTC().Format(time.RFC3339Nano), consumedFromIP,
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("enrolment.Store.Consume: %w", err)
	}
	return nil
}

// Revoke stamps revokedAt on a row -- the issuer's kill switch.
func (s *Store) Revoke(ctx context.Context, enrolmentId string, at time.Time) error {
	if s == nil || s.Engine == nil {
		return errors.New("enrolment.Store: engine not wired")
	}
	if strings.TrimSpace(enrolmentId) == "" {
		return errors.New("enrolment.Store.Revoke: enrolmentId required")
	}
	q := fmt.Sprintf(
		`mutation revokeEnrolmentToken(enrolmentId:%q,revokedAt:%q)`,
		BareSlug(enrolmentId), at.UTC().Format(time.RFC3339Nano),
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("enrolment.Store.Revoke: %w", err)
	}
	return nil
}

// LookupByHash resolves a presented token's hash to its row. Returns the row
// in EVERY state (consumed / expired / revoked included) so the caller can
// report which one; returns nil when nothing matches.
func (s *Store) LookupByHash(ctx context.Context, tokenHash string) (*Row, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("enrolment.Store: engine not wired")
	}
	if strings.TrimSpace(tokenHash) == "" {
		return nil, nil
	}
	q := fmt.Sprintf(`query enrolmentTokenByHash(tokenHash:%q)`, tokenHash)
	res, err := s.Engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("enrolment.Store.LookupByHash: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	// enrolmentTokenByHash projects through shape(), so rows land in the Data
	// axis rather than Bundle.Nodes. Same shape-axis fall-through used by
	// workerpairing/store.go, workertoken/store.go and pat/store.go.
	if res.Bundle != nil && len(res.Bundle.Nodes) > 0 {
		return rowFromNode(res.Bundle.Nodes[0]), nil
	}
	_, data, err := res.ToAPIResult()
	if err != nil {
		return nil, fmt.Errorf("enrolment.Store.LookupByHash: extract shape: %w", err)
	}
	if len(data) == 0 || data[0] == nil {
		return nil, nil
	}
	sv := data[0].GetStructValue()
	if sv == nil {
		return nil, nil
	}
	node := &memqlv1.MemoryNode{Payload: &structpb.Struct{Fields: sv.Fields}}
	if v, ok := sv.Fields["id"]; ok {
		node.Id = v.GetStringValue()
	}
	return rowFromNode(node), nil
}

// Resolve is the redeem path's one call: hash the presented plaintext, look
// the row up, and classify it.
//
// Returning (nil, StateInvalid, nil) for a miss rather than an error is what
// keeps a bad link from reading as a server fault at every call site.
func (s *Store) Resolve(ctx context.Context, plain string, now time.Time) (*Row, State, error) {
	hash := Hash(plain)
	if hash == "" || !IsEnrolmentToken(plain) {
		// Malformed input is not worth a database round trip, and refusing it
		// here means a scanner spraying garbage never reaches the engine.
		return nil, StateInvalid, nil
	}
	row, err := s.LookupByHash(ctx, hash)
	if err != nil {
		return nil, StateInvalid, err
	}
	if row == nil {
		return nil, StateInvalid, nil
	}
	return row, row.State(now), nil
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
	id := str("id")
	if id == "" {
		id = n.GetId()
	}
	return &Row{
		ID:             id,
		UserId:         str("userId"),
		TokenHash:      str("tokenHash"),
		IssuedBy:       str("issuedBy"),
		ExpiresAt:      parsedTime("expiresAt"),
		ConsumedAt:     parsedTime("consumedAt"),
		ConsumedFromIP: str("consumedFromIP"),
		RevokedAt:      parsedTime("revokedAt"),
		SourceIP:       str("sourceIP"),
		CreatedAt:      parsedTime("createdAt"),
	}
}
