package recoverykey

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// canonicalIdPrefix is the engine-stamped prefix on every identity row's id.
// A recovery key is a VARIANT of v1:identity:identity rather than a concept of
// its own (memql#3964): the credential machinery -- the union, the audit
// event's targetType enum, the soft-delete flag -- already fits it, and a new
// concept would have had to widen that enum for nothing.
const canonicalIdPrefix = "v1:identity:identity:"

// State is the outcome of validating a presented key.
//
// FOUR OUTCOMES, NOT TWO, for the reason enrolment.State records: each tells
// the holder a different thing to do next, and each is a different signal in
// the audit trail. A burst of `already-redeemed` is a replay; a burst of
// `invalid` is somebody spraying guesses at the endpoint.
//
// NOTE WHAT IS ABSENT: there is no `expired`. A recovery key does not expire,
// deliberately -- see the package comment. Single-use plus revocable is what
// bounds it instead.
type State string

const (
	// StateValid means the key resolves to a live, unredeemed, active row.
	StateValid State = "valid"
	// StateInvalid means no row matched the presented hash -- a typo, a
	// truncated key, a fabricated one, or one from another cluster.
	StateInvalid State = "invalid"
	// StateAlreadyRedeemed means redeemedAt is stamped: the one passkey
	// registration this key authorized already happened. This is where a
	// replay lands, and it is the state most worth auditing loudly.
	StateAlreadyRedeemed State = "already-redeemed"
	// StateDeactivated means the row was retired without being spent --
	// rotated out by an owner (memql#3970), or superseded.
	StateDeactivated State = "deactivated"
)

// Store wraps the engine with typed recovery-key operations. Mirrors
// enrolment.Store.
type Store struct {
	Engine identity.EngineExecutor
	Logger *slog.Logger
}

// Row is the Go projection of a v1:identity:identity row of the recovery_key
// variant.
//
// Note what is NOT here: the plaintext key. The row never carried one and this
// struct has nowhere to put one, which is the point -- there is no field a
// future edit could absent-mindedly persist it into.
type Row struct {
	ID               string
	UserId           string
	Label            string
	Active           bool
	KeyHash          string
	BoundOwnerUserId string
	MintedBy         string
	ClaimedAt        time.Time
	ClaimedFromIP    string
	RedeemedAt       time.Time
	RedeemedFromIP   string
	RotatedFrom      string
	CreatedAt        time.Time
}

// IsClaimed reports whether the plaintext has been revealed to an operator.
//
// An UNCLAIMED key is re-mintable: nobody holds it, so replacing it costs
// nothing and a lost claim output is not a lockout. Once claimed, somebody has
// it, and replacing it silently would strand whatever they wrote down.
func (r Row) IsClaimed() bool { return !r.ClaimedAt.IsZero() }

// IsRedeemed reports whether the key has been spent.
func (r Row) IsRedeemed() bool { return !r.RedeemedAt.IsZero() }

// State classifies a row for the redeem path.
//
// ORDER MATTERS AND IS DELIBERATE: redeemed beats deactivated. Redemption
// deactivates the row in the same write, so a spent key is ALWAYS also
// inactive -- and "you already used this" is the true and useful answer, while
// "this was retired" would be technically true and actively misleading about
// what happened.
func (r Row) State() State {
	switch {
	case r.IsRedeemed():
		return StateAlreadyRedeemed
	case !r.Active:
		return StateDeactivated
	default:
		return StateValid
	}
}

// NewId mints a fresh identity-row id for a recovery key.
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

// DefaultLabel is what a minted row carries when no caller names one. It shows
// up in credential listings, so it says what the row is for rather than what
// it is.
const DefaultLabel = "Cluster recovery key"

// Create persists a newly-minted recovery key.
//
// Takes the HASH, never the plaintext. The signature is the enforcement: a
// caller cannot hand this function a plain key even by mistake, because there
// is no parameter for one.
//
// rotatedFrom is empty on a first mint and carries the predecessor's id when
// this row replaces one -- by owner rotation, or as the successor the redeem
// path's invariant produces.
func (s *Store) Create(ctx context.Context, identityId, ownerUserId, keyHash, mintedBy, rotatedFrom, label string) error {
	if s == nil || s.Engine == nil {
		return errors.New("recoverykey.Store: engine not wired")
	}
	if identityId == "" || ownerUserId == "" || keyHash == "" || mintedBy == "" {
		return errors.New("recoverykey.Store.Create: identityId, ownerUserId, keyHash, mintedBy all required")
	}
	if strings.TrimSpace(label) == "" {
		label = DefaultLabel
	}
	q := fmt.Sprintf(
		`mutation createRecoveryKeyIdentity(identityId:%s,userId:%s,label:%s,keyHash:%s,boundOwnerUserId:%s,mintedBy:%s,rotatedFrom:%s)`,
		langparser.QuoteString(BareSlug(identityId)),
		langparser.QuoteString(ownerUserId),
		langparser.QuoteString(label),
		langparser.QuoteString(keyHash),
		// boundOwnerUserId is the SAME value as userId for every row that
		// exists, and is written explicitly anyway: the break-glass gate reads
		// it pre-actor, and a field the gate depends on should not be a
		// derivation somebody could later change the meaning of.
		langparser.QuoteString(ownerUserId),
		langparser.QuoteString(mintedBy),
		langparser.QuoteString(rotatedFrom),
	)
	if _, err := s.executeServerOnly(ctx, q); err != nil {
		return fmt.Errorf("recoverykey.Store.Create: %w", err)
	}
	return nil
}

// Claim stamps claimedAt / claimedFromIP -- the moment the plaintext was
// revealed to an operator. The key stays usable; claiming is not spending.
func (s *Store) Claim(ctx context.Context, identityId, claimedFromIP string, at time.Time) error {
	if s == nil || s.Engine == nil {
		return errors.New("recoverykey.Store: engine not wired")
	}
	if strings.TrimSpace(identityId) == "" {
		return errors.New("recoverykey.Store.Claim: identityId required")
	}
	q := fmt.Sprintf(
		`mutation claimRecoveryKey(identityId:%s,claimedAt:%s,claimedFromIP:%s)`,
		langparser.QuoteString(BareSlug(identityId)),
		langparser.QuoteString(at.UTC().Format(time.RFC3339Nano)),
		langparser.QuoteString(claimedFromIP),
	)
	if _, err := s.executeServerOnly(ctx, q); err != nil {
		return fmt.Errorf("recoverykey.Store.Claim: %w", err)
	}
	return nil
}

// Redeem spends the key: stamps the redemption AND deactivates the row, in one
// engine call because the mutation does both in one write.
//
// The caller is responsible for having checked State first; this persists the
// transition. It does NOT mint the successor -- that is the standing invariant
// (memql#3965), so there is exactly one place in the tree that decides a
// recovery key should exist.
func (s *Store) Redeem(ctx context.Context, identityId, redeemedFromIP string, at time.Time) error {
	if s == nil || s.Engine == nil {
		return errors.New("recoverykey.Store: engine not wired")
	}
	if strings.TrimSpace(identityId) == "" {
		return errors.New("recoverykey.Store.Redeem: identityId required")
	}
	q := fmt.Sprintf(
		`mutation redeemRecoveryKey(identityId:%s,redeemedAt:%s,redeemedFromIP:%s)`,
		langparser.QuoteString(BareSlug(identityId)),
		langparser.QuoteString(at.UTC().Format(time.RFC3339Nano)),
		langparser.QuoteString(redeemedFromIP),
	)
	if _, err := s.executeServerOnly(ctx, q); err != nil {
		return fmt.Errorf("recoverykey.Store.Redeem: %w", err)
	}
	return nil
}

// Deactivate retires a key WITHOUT spending it -- the predecessor half of an
// owner-driven rotation (memql#3970).
//
// Distinct from Redeem because the two are different events with different
// audit meanings: a redeemed key was USED, a deactivated one was REPLACED
// having never been used, and an audit trail that could not tell them apart
// could not tell a break-glass event from routine hygiene.
func (s *Store) Deactivate(ctx context.Context, identityId string) error {
	if s == nil || s.Engine == nil {
		return errors.New("recoverykey.Store: engine not wired")
	}
	if strings.TrimSpace(identityId) == "" {
		return errors.New("recoverykey.Store.Deactivate: identityId required")
	}
	q := fmt.Sprintf(`mutation deactivateRecoveryKey(identityId:%s)`, langparser.QuoteString(BareSlug(identityId)))
	if _, err := s.executeServerOnly(ctx, q); err != nil {
		return fmt.Errorf("recoverykey.Store.Deactivate: %w", err)
	}
	return nil
}

// LookupByHash resolves a presented key's hash to its row. Returns the row in
// EVERY state (redeemed / deactivated included) so the caller can report which
// one; returns nil when nothing matches.
func (s *Store) LookupByHash(ctx context.Context, keyHash string) (*Row, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("recoverykey.Store: engine not wired")
	}
	if strings.TrimSpace(keyHash) == "" {
		return nil, nil
	}
	q := fmt.Sprintf(`query recoveryKeyByHash(keyHash:%s)`, langparser.QuoteString(keyHash))
	rows, err := s.queryRows(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("recoverykey.Store.LookupByHash: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// ActiveForUser returns the live recovery keys bound to one user. Backs the
// mint invariant: an empty result means the cluster has no break-glass route
// and one must be minted.
func (s *Store) ActiveForUser(ctx context.Context, ownerUserId string) ([]*Row, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("recoverykey.Store: engine not wired")
	}
	if strings.TrimSpace(ownerUserId) == "" {
		return nil, errors.New("recoverykey.Store.ActiveForUser: ownerUserId required")
	}
	q := fmt.Sprintf(`query activeRecoveryKeys(userId:%s)`, langparser.QuoteString(ownerUserId))
	rows, err := s.queryRows(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("recoverykey.Store.ActiveForUser: %w", err)
	}
	// A redeemed row is deactivated in the same write, so `isActiveRecord`
	// already excludes it. Filtering again here is belt-and-braces against a
	// future edit that splits those two writes apart: the invariant must never
	// treat a spent key as a live break-glass route.
	live := rows[:0]
	for _, r := range rows {
		if r != nil && r.Active && !r.IsRedeemed() {
			live = append(live, r)
		}
	}
	return live, nil
}

// Resolve is the redeem path's one call: hash the presented plaintext, look
// the row up, and classify it.
//
// Returning (nil, StateInvalid, nil) for a miss rather than an error is what
// keeps a bad key from reading as a server fault at every call site.
func (s *Store) Resolve(ctx context.Context, plain string) (*Row, State, error) {
	hash := Hash(plain)
	if hash == "" || !IsRecoveryKey(plain) {
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
	return row, row.State(), nil
}

// executeServerOnly runs one construct on a context stamped server-initiated.
//
// EVERY construct this store issues is @serverOnly -- both reads
// (activeRecoveryKeys, recoveryKeyByHash) and all four writes
// (createRecoveryKeyIdentity, claimRecoveryKey, redeemRecoveryKey,
// deactivateRecoveryKey). The engine refuses a @serverOnly construct unless
// auth.OriginFromContext says the call is server-initiated
// (component/memql/engine.go), so this is the one thing standing between the
// annotation and a feature that cannot run at all.
//
// It was missing, and the cost was the whole credential rather than one call:
// the boot invariant could not take its read, so no cluster minted an owner
// recovery key; `memql recovery-key claim` exited 1; owner rotation failed;
// and the redeem path could not resolve a presented key. Every affected
// cluster booted with no break-glass route for its owner, degraded to a WARN
// that nothing surfaces.
//
// # Why a helper rather than six inline stamps
//
// call_origin.go prefers the stamp INLINE at the one Execute that needs it, so
// the marked context dies at that call. This keeps that property and adds
// one: the helper returns a RESULT, never a context, so there is no value a
// later frame could inherit the mark from. A context-returning wrapper would
// be the laundering shape call_origin_conformance_test.go names as the gap it
// cannot see; this cannot be used that way, because there is nothing to hold.
//
// It also makes a partial stamp unrepresentable. Six copies of the stamp is
// six chances for the seventh operation to be written without one, which is
// the failure mode that arrives later and reads as a typo.
//
// # Why this store may stamp at all, including from the wire
//
// Two of the three call sites are plainly server-initiated: the boot invariant
// (invariant.go) and `memql recovery-key claim`, neither of which has a
// request in scope. The third does: component/identity/http/webauthn_recovery.go
// calls Resolve on an UNAUTHENTICATED request context, which is exactly the
// shape call_origin.go warns about.
//
// What earns it is the argument. recoveryKeyByHash filters on a DIGEST of a
// secret the caller had to present, computed by Resolve rather than supplied
// by it -- so naming a row is a possession proof, not an identifier a caller
// can choose, and there is nothing to enumerate. That is the opposite of
// workerTokensForUser, whose caller-supplied userId is why THAT allowlist
// entry needs a caller-scope test and this one does not.
//
// activeRecoveryKeys does take a caller-supplied userId, and no wire path
// reaches it: its three callers resolve the owner themselves (the invariant,
// the CLI) or sit downstream of the owner/admin gate asserted by
// component/identity/adminops/gate_test.go. The rows it returns carry no
// directory PII -- a recovery-key row is a hash, a binding and some
// timestamps, and never the plaintext, which this package has nowhere to put.
//
// Pinned by store_internal_origin_test.go, which drives all six operations
// with a client-origin context and asserts the engine saw internal.
func (s *Store) executeServerOnly(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	return s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), q)
}

// queryRows runs a projected query and converts every returned row.
//
// Both shapes are handled for the reason enrolment.Store.LookupByHash records:
// a query projecting through shape() lands rows in the Data axis rather than
// Bundle.Nodes, and which axis is populated depends on the projection rather
// than on the caller.
func (s *Store) queryRows(ctx context.Context, q string) ([]*Row, error) {
	res, err := s.executeServerOnly(ctx, q)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	if res.Bundle != nil && len(res.Bundle.Nodes) > 0 {
		out := make([]*Row, 0, len(res.Bundle.Nodes))
		for _, n := range res.Bundle.Nodes {
			if r := rowFromNode(n); r != nil {
				out = append(out, r)
			}
		}
		return out, nil
	}
	_, data, err := res.ToAPIResult()
	if err != nil {
		return nil, fmt.Errorf("extract shape: %w", err)
	}
	out := make([]*Row, 0, len(data))
	for _, d := range data {
		if d == nil {
			continue
		}
		sv := d.GetStructValue()
		if sv == nil {
			continue
		}
		node := &memqlv1.MemoryNode{Payload: &structpb.Struct{Fields: sv.Fields}}
		if v, ok := sv.Fields["id"]; ok {
			node.Id = v.GetStringValue()
		}
		if r := rowFromNode(node); r != nil {
			out = append(out, r)
		}
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
	// credentials is the variant object; every recovery-key field lives inside
	// it rather than at the top level.
	cred := map[string]*structpb.Value{}
	if v, ok := fields["credentials"]; ok && v != nil {
		if sv := v.GetStructValue(); sv != nil {
			cred = sv.GetFields()
		}
	}
	credStr := func(k string) string {
		if v, ok := cred[k]; ok && v != nil {
			return strings.TrimSpace(v.GetStringValue())
		}
		return ""
	}
	parseTime := func(raw string) time.Time {
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
	active := true
	if v, ok := fields["active"]; ok && v != nil {
		if _, isBool := v.GetKind().(*structpb.Value_BoolValue); isBool {
			active = v.GetBoolValue()
		}
	}
	id := str("id")
	if id == "" {
		id = n.GetId()
	}
	return &Row{
		ID:               id,
		UserId:           str("userId"),
		Label:            str("label"),
		Active:           active,
		KeyHash:          credStr("keyHash"),
		BoundOwnerUserId: credStr("boundOwnerUserId"),
		MintedBy:         credStr("mintedBy"),
		ClaimedAt:        parseTime(credStr("claimedAt")),
		ClaimedFromIP:    credStr("claimedFromIP"),
		RedeemedAt:       parseTime(credStr("redeemedAt")),
		RedeemedFromIP:   credStr("redeemedFromIP"),
		RotatedFrom:      credStr("rotatedFrom"),
		CreatedAt:        parseTime(str("createdAt")),
	}
}
