package webauthn

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
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// canonicalIdentityIdPrefix mirrors badge / workertoken / pat.
const canonicalIdentityIdPrefix = "v1:identity:identity:"

// maxPageWalk bounds the keyset walk (mirrors badge / workertoken).
const maxPageWalk = 1000

// Store wraps the memQL engine with typed passkey operations. Mirrors
// badge.Store: create, lookup, list.
//
// Revocation is deliberately absent -- it belongs to the passkey
// management surface (memql#3409) alongside the rename and the
// last-credential guard, and adding a bare revoke here would invite a
// caller that removes a user's only passkey with nothing checking
// whether they still have a way in.
type Store struct {
	Engine identity.EngineExecutor
	Logger *slog.Logger
}

// Row is the Go projection of a passkey identity row.
type Row struct {
	ID             string
	UserId         string
	Label          string
	Active         bool
	CredentialId   string
	PublicKey      string
	SignCount      uint32
	AAGUID         string
	Transports     []string
	BackupEligible bool
	BackupState    bool
	RegisteredBy   string
	LastUsedAt     time.Time
	CreatedAt      time.Time
}

// NewId mints a new passkey identity slug.
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

func bareSlug(id string) string {
	return strings.TrimPrefix(id, canonicalIdentityIdPrefix)
}

// Create persists a new passkey identity row.
//
// ctx MUST carry the system credential actor
// (identity.ContextWithSystemCredentialActor): a passkey row is gated by
// the memql#2513 credential-actor guard, and the handler's own bearer is
// a user actor the guard rejects. The verified attestation is the
// authorization; the system actor is how that authorization reaches the
// write.
//
// Uniqueness of cred.CredentialId is the CALLER's obligation -- run
// LookupByCredentialId first. Stated here because this function cannot
// enforce it: the engine has no unique index to lean on, so the check
// and the write are two steps and only the handler can sequence them.
func (s *Store) Create(ctx context.Context, identityId, userId, label string, cred *RegisteredCredential, registeredBy string) error {
	if s == nil || s.Engine == nil {
		return errors.New("webauthn.Store: engine not wired")
	}
	if cred == nil {
		return errors.New("webauthn.Store.Create: credential required")
	}
	if identityId == "" || userId == "" || label == "" || cred.CredentialId == "" || cred.PublicKey == "" {
		return errors.New("webauthn.Store.Create: identityId, userId, label, credentialId, publicKey all required")
	}
	transports, err := json.Marshal(cred.Transports)
	if err != nil {
		return fmt.Errorf("webauthn.Store.Create: encode transports: %w", err)
	}
	if len(cred.Transports) == 0 {
		transports = []byte("[]")
	}
	q := fmt.Sprintf(
		`mutation createPasskeyIdentity(identityId:%s,userId:%s,label:%s,credentialId:%s,publicKey:%s,signCount:%d,aaguid:%s,transports:%s,backupEligible:%t,backupState:%t,registeredBy:%s)`,
		dslString(identityId), dslString(userId), dslString(label),
		dslString(cred.CredentialId), dslString(cred.PublicKey),
		cred.SignCount, dslString(cred.AAGUID), string(transports),
		cred.BackupEligible, cred.BackupState, dslString(registeredBy),
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("webauthn.Store.Create: %w", err)
	}
	return nil
}

// LookupByCredentialId resolves a base64url credential id to its row, or
// nil when nothing matches.
//
// Returns revoked (active=false) rows too. Both callers need that:
// register/finish must refuse to re-register a credential id even when
// the previous row was revoked (the id is not free again -- the same
// authenticator still holds the private key), and the login ceremony
// wants to say "revoked" rather than "unknown" in its audit trail.
func (s *Store) LookupByCredentialId(ctx context.Context, credentialId string) (*Row, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("webauthn.Store: engine not wired")
	}
	credentialId = strings.TrimSpace(credentialId)
	if credentialId == "" {
		return nil, nil
	}
	q := fmt.Sprintf(`query passkeyByCredentialId(credentialId:%s)`, dslString(credentialId))
	rows, err := s.executeAndExtract(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("webauthn.Store.LookupByCredentialId: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rowFromNode(rows[0]), nil
}

// ListForSelf returns every passkey owned by the CALLER (active +
// inactive). passkeysForSelf is `paginate 50`, so the keyset cursor is
// walked to assemble the complete set (mirrors badge / workertoken).
//
// SELF-SCOPED: there is no userId parameter, and there deliberately is
// not one. The row set comes from `userId==actor.userId`, so this cannot
// be pointed at another user's authenticators; ctx MUST carry an
// AccessContext for the caller or the query resolves actor.userId to ""
// and yields zero rows -- which, at register/begin, would silently mean
// "no exclusions" rather than an error.
func (s *Store) ListForSelf(ctx context.Context) ([]Row, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("webauthn.Store: engine not wired")
	}
	const q = `query passkeysForSelf()`
	out := []Row{}
	cursor := ""
	for i := 0; i < maxPageWalk; i++ {
		nodes, next, err := s.executeAndExtractPage(ctx, q, cursor)
		if err != nil {
			return nil, fmt.Errorf("webauthn.Store.ListForSelf: %w", err)
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

// RecordAssertion stamps the post-login verifier state on a passkey row:
// the new signature counter, the current backup state, and lastUsedAt.
//
// BEST-EFFORT. Callers log a failure and let the login stand, matching
// the badge variant's precedent -- the assertion has already been
// verified, and refusing a login because a bookkeeping write failed
// would lock a user out over a database hiccup. The cost of the miss is
// bounded: a lastUsedAt that lags, and a counter that stays where it was
// (so the NEXT assertion from the same authenticator compares against an
// older value, which is conservative rather than permissive -- it can
// only produce a false regression, never suppress a real one).
//
// ctx MUST carry the system credential actor, for the same reason
// Create does: a passkey row is gated by the memql#2513 credential-actor
// guard and the login path has no authenticated actor at all yet.
//
// The write is a WHOLESALE credentials merge because partial nested
// updates are unsupported, so every field that is not changing is
// re-passed FROM THE STORED ROW. That matters most for backupEligible,
// which the spec fixes for the credential's lifetime: it is echoed back
// unchanged rather than re-read from the assertion, so this path cannot
// rewrite it even by accident.
func (s *Store) RecordAssertion(ctx context.Context, row *Row, signCount uint32, backupState bool, at time.Time) error {
	if s == nil || s.Engine == nil {
		return errors.New("webauthn.Store: engine not wired")
	}
	if row == nil || row.ID == "" {
		return errors.New("webauthn.Store.RecordAssertion: row required")
	}
	transports, err := json.Marshal(row.Transports)
	if err != nil {
		return fmt.Errorf("webauthn.Store.RecordAssertion: encode transports: %w", err)
	}
	if len(row.Transports) == 0 {
		transports = []byte("[]")
	}
	q := fmt.Sprintf(
		`mutation recordPasskeyAssertion(identityId:%s,credentialId:%s,publicKey:%s,signCount:%d,aaguid:%s,transports:%s,backupEligible:%t,backupState:%t,registeredBy:%s,lastUsedAt:%s)`,
		dslString(bareSlug(row.ID)), dslString(row.CredentialId), dslString(row.PublicKey),
		signCount, dslString(row.AAGUID), string(transports),
		row.BackupEligible, backupState, dslString(row.RegisteredBy),
		dslString(at.UTC().Format(time.RFC3339Nano)),
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("webauthn.Store.RecordAssertion: %w", err)
	}
	return nil
}

// dslString JSON-encodes a value into a DSL string literal. encoding/json's
// quoting keeps a value containing a double quote from breaking out of its
// enclosing literal, mirroring identity.Store's dslJSONString.
func dslString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// executeAndExtract / executeAndExtractPage mirror badge.Store: raw
// queries return Bundle nodes; shape-wrapped queries return Data structs
// that get synthesized into MemoryNodes so one rowFromNode covers both.
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
		return nil, "", fmt.Errorf("webauthn.Store: extract shape result: %w", err)
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
		node := &memqlv1.MemoryNode{Payload: &structpb.Struct{Fields: sv.Fields}}
		if id, ok := sv.Fields["id"]; ok {
			node.Id = id.GetStringValue()
		}
		out = append(out, node)
	}
	return out, next, nil
}

// rowFromNode projects a node into a Row. It reads the passkey leaves
// from the nested credentials object when present (identityFull) AND
// from the top level (passkeySummary, whose shape keys each projected
// path by its terminal segment), so one projector covers both queries.
func rowFromNode(n *memqlv1.MemoryNode) *Row {
	if n == nil || n.Payload == nil {
		return nil
	}
	fields := n.Payload.GetFields()
	if fields == nil {
		return nil
	}
	creds := map[string]*structpb.Value{}
	if v, ok := fields["credentials"]; ok && v != nil {
		if sv, isStruct := v.GetKind().(*structpb.Value_StructValue); isStruct {
			creds = sv.StructValue.GetFields()
		}
	}
	// leaf reads the credentials object first, then the flattened
	// top-level key the summary shape produces.
	leaf := func(k string) *structpb.Value {
		if v, ok := creds[k]; ok && v != nil {
			return v
		}
		if v, ok := fields[k]; ok && v != nil {
			return v
		}
		return nil
	}
	str := func(v *structpb.Value) string {
		if v == nil {
			return ""
		}
		return strings.TrimSpace(v.GetStringValue())
	}
	boolean := func(v *structpb.Value, def bool) bool {
		if v == nil {
			return def
		}
		if _, isBool := v.GetKind().(*structpb.Value_BoolValue); isBool {
			return v.GetBoolValue()
		}
		return strings.EqualFold(strings.TrimSpace(v.GetStringValue()), "true")
	}
	row := &Row{
		ID:             firstNonEmpty(str(fields["id"]), n.GetId()),
		UserId:         str(fields["userId"]),
		Label:          str(fields["label"]),
		Active:         boolean(fields["active"], true),
		CredentialId:   str(leaf("credentialId")),
		PublicKey:      str(leaf("publicKey")),
		SignCount:      uint32(leaf("signCount").GetNumberValue()),
		AAGUID:         str(leaf("aaguid")),
		BackupEligible: boolean(leaf("backupEligible"), false),
		BackupState:    boolean(leaf("backupState"), false),
		RegisteredBy:   str(leaf("registeredBy")),
		LastUsedAt:     parseStamp(str(leaf("lastUsedAt"))),
		CreatedAt:      parseStamp(str(fields["createdAt"])),
	}
	if v := leaf("transports"); v != nil {
		if lv, isList := v.GetKind().(*structpb.Value_ListValue); isList {
			for _, item := range lv.ListValue.GetValues() {
				if s := strings.TrimSpace(item.GetStringValue()); s != "" {
					row.Transports = append(row.Transports, s)
				}
			}
		}
	}
	return row
}

func parseStamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
