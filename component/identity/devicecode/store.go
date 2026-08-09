package devicecode

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

// canonicalIdPrefix is the engine-stamped prefix on every
// device-authorization row's id.
const canonicalIdPrefix = "v1:identity:deviceCode:"

// Status values, mirroring the concept's enum. The state machine is
// pending -> approved -> redeemed, or pending -> denied. Expiry is
// orthogonal: it is read off ExpiresAt, never written into Status, so
// an expired row still reports whether the human had answered.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusDenied   = "denied"
	StatusRedeemed = "redeemed"
)

// Store wraps the engine with typed device-authorization operations.
// Mirrors the workerpairing.Store / workertoken.Store patterns.
type Store struct {
	Engine identity.EngineExecutor
	Logger *slog.Logger
}

// Row is the Go projection of a v1:identity:deviceCode row.
type Row struct {
	ID                  string
	ClientId            string
	DeviceCodeHash      string
	UserCodeHash        string
	Status              string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
	ExpiresAt           time.Time
	IntervalSeconds     int
	LastPolledAt        time.Time
	ApprovedByUserId    string
	ApprovedAt          time.Time
	DeniedAt            time.Time
	RedeemedAt          time.Time
	SourceIP            string
	UserAgent           string
	CreatedAt           time.Time
}

// IsExpired reports whether the row has aged past expiresAt.
func (r Row) IsExpired(now time.Time) bool {
	return !r.ExpiresAt.IsZero() && now.After(r.ExpiresAt)
}

// Interval returns the row's effective poll floor, defaulted and
// clamped. A row written before the field existed, or one carrying a
// nonsense value, still yields a usable clock.
func (r Row) Interval() int {
	if r.IntervalSeconds <= 0 {
		return DefaultIntervalSeconds
	}
	if r.IntervalSeconds > MaxIntervalSeconds {
		return MaxIntervalSeconds
	}
	return r.IntervalSeconds
}

// NewId mints a fresh device-authorization slug.
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

// CreateInput carries everything POST /device/code stamps onto a fresh
// row. Both hashes arrive already digested -- neither plaintext is a
// parameter of this package's persistence surface at all, which is the
// structural version of "never persisted".
type CreateInput struct {
	Id                  string
	ClientId            string
	DeviceCodeHash      string
	UserCodeHash        string
	ExpiresAt           time.Time
	IntervalSeconds     int
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
	SourceIP            string
	UserAgent           string
}

// Create persists a new device-authorization row in status=pending.
func (s *Store) Create(ctx context.Context, in CreateInput) error {
	if s == nil || s.Engine == nil {
		return errors.New("devicecode.Store: engine not wired")
	}
	if in.Id == "" || in.ClientId == "" || in.DeviceCodeHash == "" || in.UserCodeHash == "" {
		return errors.New("devicecode.Store.Create: id, clientId, deviceCodeHash, userCodeHash all required")
	}
	interval := in.IntervalSeconds
	if interval <= 0 {
		interval = DefaultIntervalSeconds
	}
	q := fmt.Sprintf(
		`mutation createDeviceCode(deviceCodeId:%q,clientId:%q,deviceCodeHash:%q,userCodeHash:%q,expiresAt:%q,intervalSeconds:%d,scope:%q,codeChallenge:%q,codeChallengeMethod:%q,sourceIP:%q,userAgent:%q)`,
		BareSlug(in.Id), in.ClientId, in.DeviceCodeHash, in.UserCodeHash,
		in.ExpiresAt.UTC().Format(time.RFC3339Nano), interval,
		in.Scope, in.CodeChallenge, in.CodeChallengeMethod,
		in.SourceIP, truncateUserAgent(in.UserAgent),
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("devicecode.Store.Create: %w", err)
	}
	return nil
}

// TouchPoll stamps the poll clock. Called on EVERY poll -- accepted or
// throttled -- because the next poll's slow_down decision is made
// against it, and a rejected poll that did not move the clock would let
// a client hammer the endpoint indefinitely on one stale timestamp.
func (s *Store) TouchPoll(ctx context.Context, id string, at time.Time, intervalSeconds int) error {
	if s == nil || s.Engine == nil {
		return errors.New("devicecode.Store: engine not wired")
	}
	if intervalSeconds <= 0 {
		intervalSeconds = DefaultIntervalSeconds
	}
	q := fmt.Sprintf(
		`mutation touchDeviceCodePoll(deviceCodeId:%q,lastPolledAt:%q,intervalSeconds:%d)`,
		BareSlug(id), at.UTC().Format(time.RFC3339Nano), intervalSeconds,
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("devicecode.Store.TouchPoll: %w", err)
	}
	return nil
}

// Approve flips a pending row to approved. The approver is stamped from
// the actor on ctx by the mutation itself, so the caller cannot name
// somebody else; callers must run this under the signed-in user's actor
// context.
func (s *Store) Approve(ctx context.Context, id string) error {
	return s.transition(ctx, "approveDeviceCode", id)
}

// Deny flips a pending row to denied. Terminal.
func (s *Store) Deny(ctx context.Context, id string) error {
	return s.transition(ctx, "denyDeviceCode", id)
}

func (s *Store) transition(ctx context.Context, mutation, id string) error {
	if s == nil || s.Engine == nil {
		return errors.New("devicecode.Store: engine not wired")
	}
	q := fmt.Sprintf(`mutation %s(deviceCodeId:%q)`, mutation, BareSlug(id))
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("devicecode.Store.%s: %w", mutation, err)
	}
	return nil
}

// Redeem flips an approved row to redeemed. The token handler calls
// this BEFORE minting, so a concurrent second poll reads status=
// redeemed and is refused -- the same ordering /oauth/token uses for
// the authorization_code grant.
func (s *Store) Redeem(ctx context.Context, id string, at time.Time) error {
	if s == nil || s.Engine == nil {
		return errors.New("devicecode.Store: engine not wired")
	}
	q := fmt.Sprintf(
		`mutation redeemDeviceCode(deviceCodeId:%q,redeemedAt:%q)`,
		BareSlug(id), at.UTC().Format(time.RFC3339Nano),
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("devicecode.Store.Redeem: %w", err)
	}
	return nil
}

// LookupByDeviceCodeHash is the polling hot path. Returns the row in
// whatever state it is in -- the caller decides which RFC 8628 error
// each state maps to -- or nil when nothing matches.
func (s *Store) LookupByDeviceCodeHash(ctx context.Context, hash string) (*Row, error) {
	return s.lookup(ctx, "deviceCodeByDeviceCodeHash", "deviceCodeHash", hash)
}

// LookupByUserCodeHash backs the verification page.
func (s *Store) LookupByUserCodeHash(ctx context.Context, hash string) (*Row, error) {
	return s.lookup(ctx, "deviceCodeByUserCodeHash", "userCodeHash", hash)
}

func (s *Store) lookup(ctx context.Context, query, arg, hash string) (*Row, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("devicecode.Store: engine not wired")
	}
	if hash == "" {
		return nil, nil
	}
	res, err := s.Engine.Execute(ctx, fmt.Sprintf(`query %s(%s:%q)`, query, arg, hash))
	if err != nil {
		return nil, fmt.Errorf("devicecode.Store.%s: %w", query, err)
	}
	if res == nil {
		return nil, nil
	}
	// Shaped queries land in res.OutputPayload (the Data axis) rather
	// than Bundle.Nodes; the same fall-through the sibling credential
	// stores use.
	if res.Bundle != nil && len(res.Bundle.Nodes) > 0 {
		return rowFromNode(res.Bundle.Nodes[0]), nil
	}
	_, data, err := res.ToAPIResult()
	if err != nil {
		return nil, fmt.Errorf("devicecode.Store.%s: extract shape: %w", query, err)
	}
	if len(data) == 0 || data[0] == nil {
		return nil, nil
	}
	sv := data[0].GetStructValue()
	if sv == nil {
		return nil, nil
	}
	node := &memqlv1.MemoryNode{Payload: &structpb.Struct{Fields: sv.Fields}}
	if id, ok := sv.Fields["id"]; ok {
		node.Id = id.GetStringValue()
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
	num := func(k string) int {
		if v, ok := fields[k]; ok && v != nil {
			return int(v.GetNumberValue())
		}
		return 0
	}
	stamp := func(k string) time.Time {
		raw := str(k)
		if raw == "" {
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return t
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t
		}
		return time.Time{}
	}
	id := str("id")
	if id == "" {
		id = n.GetId()
	}
	return &Row{
		ID:                  id,
		ClientId:            str("clientId"),
		DeviceCodeHash:      str("deviceCodeHash"),
		UserCodeHash:        str("userCodeHash"),
		Status:              str("status"),
		Scope:               str("scope"),
		CodeChallenge:       str("codeChallenge"),
		CodeChallengeMethod: str("codeChallengeMethod"),
		ExpiresAt:           stamp("expiresAt"),
		IntervalSeconds:     num("intervalSeconds"),
		LastPolledAt:        stamp("lastPolledAt"),
		ApprovedByUserId:    str("approvedByUserId"),
		ApprovedAt:          stamp("approvedAt"),
		DeniedAt:            stamp("deniedAt"),
		RedeemedAt:          stamp("redeemedAt"),
		SourceIP:            str("sourceIP"),
		UserAgent:           str("userAgent"),
		CreatedAt:           stamp("createdAt"),
	}
}

// userAgentMaxLen bounds what a device can write into a row that a
// human will later be shown. The approval page renders this string, so
// an unbounded one is both a storage cost and a place to hide a wall of
// text that pushes the Approve / Deny buttons off the screen.
const userAgentMaxLen = 256

func truncateUserAgent(ua string) string {
	ua = strings.TrimSpace(ua)
	if len(ua) > userAgentMaxLen {
		return ua[:userAgentMaxLen]
	}
	return ua
}
