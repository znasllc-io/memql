package accounttoken

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

// canonicalIdentityIdPrefix mirrors pat / workertoken.
const canonicalIdentityIdPrefix = "v1:identity:identity:"

// Store wraps the memQL engine with typed account-token operations.
//
// EVERY METHOD RUNS AS THE CALLER. That is the difference from
// workertoken.Store, and it is the whole authorization story: this
// package stamps no internal origin and installs no system actor, so
// the reads resolve actor.userId to the authenticated operator and the
// `userId==actor.userId` conjunct in accountTokensForAccount /
// accountTokenById is a real gate rather than a comparison against a
// synthetic subject. component/identity/workertoken had to reach for
// internal origin because workerTokensForUser is @serverOnly and keys
// on a caller-supplied userId; nothing here is @serverOnly, precisely
// so nothing here needs that escape.
//
// The consequence for callers: pass the context whose AccessContext is
// the authenticated operator's (in the gRPC handler, the one built by
// auth.ContextWithAccess over the stream's resolved access). Passing a
// system-actor context would silently return zero rows rather than
// erroring, so the tests assert the caller scope directly.
type Store struct {
	Engine identity.EngineExecutor
	Logger *slog.Logger
}

// Row is the Go projection of an account_token identity row, in
// accountTokenSummary shape. There is deliberately no KeyHash field:
// the shape does not project the digest, so a reader cannot re-emit
// what it never received.
type Row struct {
	ID         string
	UserId     string
	AccountId  string
	Label      string
	MintedBy   string
	Active     bool
	ExpiresAt  time.Time
	LastUsedAt time.Time
	CreatedAt  time.Time
}

// NewId mints a new account-token identity slug.
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

// BareSlug is CanonicalId's inverse.
func BareSlug(id string) string {
	return strings.TrimPrefix(id, canonicalIdentityIdPrefix)
}

// Create persists a new account_token identity row.
//
// Caller flow:
//
//	plain, hash, err := accounttoken.Mint()
//	id, err := accounttoken.NewId()
//	err = store.Create(callerCtx, id, accountId, label, hash, expiresAt)
//	// return `plain` to the operator ONCE, then drop it.
//
// `plain` is never a parameter here. The mutation has no arg for it
// and this method has no field for it, so there is no path by which
// the plaintext could reach the engine, a log line or a payload.
//
// The subject (userId) and mintedBy are stamped by the mutation from
// actor.userId; they are not parameters for the same reason.
func (s *Store) Create(ctx context.Context, identityId, accountId, label, keyHash string, expiresAt time.Time) error {
	if s == nil || s.Engine == nil {
		return errors.New("accounttoken.Store: engine not wired")
	}
	if identityId == "" || accountId == "" || label == "" || keyHash == "" {
		return errors.New("accounttoken.Store.Create: identityId, accountId, label, keyHash all required")
	}
	expires := ""
	if !expiresAt.IsZero() {
		expires = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	q := fmt.Sprintf(
		`mutation createAccountTokenIdentity(identityId:%q,accountId:%q,label:%q,keyHash:%q,expiresAt:%q)`,
		identityId, accountId, label, keyHash, expires,
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("accounttoken.Store.Create: %w", err)
	}
	return nil
}

// Revoke flips active=false on the row. The CALLER is responsible for
// having established ownership first (see ByIdForCaller) -- the
// mutation carries no actor check, for the reason
// dsl/identity/mutations.memql states above it.
func (s *Store) Revoke(ctx context.Context, identityId string) error {
	if s == nil || s.Engine == nil {
		return errors.New("accounttoken.Store: engine not wired")
	}
	if strings.TrimSpace(identityId) == "" {
		return errors.New("accounttoken.Store.Revoke: identityId required")
	}
	q := fmt.Sprintf(`mutation revokeAccountTokenIdentity(identityId:%q)`, BareSlug(identityId))
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("accounttoken.Store.Revoke: %w", err)
	}
	return nil
}

// ListForAccount returns every account_token row the CALLER has issued
// for one account, active and revoked alike. Revoked rows are included
// so an operator can see a revocation took effect rather than watching
// a row vanish and having to trust that the right one went.
func (s *Store) ListForAccount(ctx context.Context, accountId string) ([]Row, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("accounttoken.Store: engine not wired")
	}
	if strings.TrimSpace(accountId) == "" {
		return nil, errors.New("accounttoken.Store.ListForAccount: accountId required")
	}
	q := fmt.Sprintf(`query accountTokensForAccount(accountId:%q)`, accountId)

	out := []Row{}
	cursor := ""
	// accountTokensForAccount is `paginate 50`, so one Execute is one
	// page. Callers need the complete set (the revoke ownership check
	// must find a token beyond page one), so walk the keyset cursor.
	// maxPageWalk is a runaway backstop, matching workertoken.
	for i := 0; i < maxPageWalk; i++ {
		nodes, next, err := s.executePage(ctx, q, cursor)
		if err != nil {
			return nil, fmt.Errorf("accounttoken.Store.ListForAccount: %w", err)
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

// ByIdForCaller resolves one account_token row by id, gated to the
// caller by accountTokenById's `userId==actor.userId` conjunct.
// Returns (nil, nil) when no row matches -- which, because the gate is
// part of the query, is the SAME answer for "no such row" and "not
// yours". That conflation is deliberate: distinguishing them would
// turn the revoke endpoint into an oracle for which credential ids
// exist on the cluster.
func (s *Store) ByIdForCaller(ctx context.Context, identityId string) (*Row, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("accounttoken.Store: engine not wired")
	}
	if strings.TrimSpace(identityId) == "" {
		return nil, errors.New("accounttoken.Store.ByIdForCaller: identityId required")
	}
	q := fmt.Sprintf(`query accountTokenById(identityId:%q)`, CanonicalId(identityId))
	nodes, _, err := s.executePage(ctx, q, "")
	if err != nil {
		return nil, fmt.Errorf("accounttoken.Store.ByIdForCaller: %w", err)
	}
	if len(nodes) == 0 {
		return nil, nil
	}
	return rowFromNode(nodes[0]), nil
}

// maxPageWalk bounds the keyset walk so a mis-paginated query cannot
// spin forever. 50 rows a page caps the walk at 50k tokens for one
// account, far beyond any real issuance.
const maxPageWalk = 1000

// executePage runs a shaped read and normalizes the two result shapes
// the engine can return -- a raw node bundle, or a shape projection in
// Data -- into memory nodes. Mirrors workertoken.Store.executeAndExtractPage;
// the shape path is the one that matters here, since both account-token
// reads are shaped.
func (s *Store) executePage(ctx context.Context, query, cursor string) ([]*memqlv1.MemoryNode, string, error) {
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
		return nil, "", fmt.Errorf("accounttoken.Store: extract shape result: %w", err)
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
		node := &memqlv1.MemoryNode{Payload: &structpb.Struct{Fields: sv.Fields}}
		if id, ok := sv.Fields["id"]; ok {
			node.Id = id.GetStringValue()
		}
		out = append(out, node)
	}
	return out, next, nil
}

// rowFromNode projects one accountTokenSummary result into a Row.
//
// The shape keys each projected path by its TERMINAL segment, so
// credentials.accountId arrives flat as `accountId` and
// credentials.mintedBy as `mintedBy`. There is no `credentials` struct
// to descend into and no keyHash anywhere in the result -- which is
// the property accountTokenSummary exists to give this reader.
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
		ID:         id,
		UserId:     str("userId"),
		AccountId:  str("accountId"),
		Label:      str("label"),
		MintedBy:   str("mintedBy"),
		Active:     boolean("active", true),
		ExpiresAt:  parsed("expiresAt"),
		LastUsedAt: parsed("lastUsedAt"),
		CreatedAt:  parsed("createdAt"),
	}
}
