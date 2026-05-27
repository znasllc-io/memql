package nodetoken

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// recordingEngine is a stub EngineExecutor that captures every query
// the Store hands to it + returns a caller-controlled response. Lets
// us assert the exact MemQL text the Store produced without spinning
// up a real engine, and lets us drive the lookup paths through
// canned bundle / shape responses without touching the database.
type recordingEngine struct {
	calls []string
	resp  *memql.ExecuteResult
	err   error
}

func (e *recordingEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) { //nolint:revive // ctx required by interface
	_ = ctx
	e.calls = append(e.calls, query)
	if e.err != nil {
		return nil, e.err
	}
	return e.resp, nil
}

func TestCanonicalIdentityIdFor(t *testing.T) {
	got := CanonicalIdentityIdFor("bff", "bff-local")
	want := "v1:identity:identity:node:bff:bff-local"
	if got != want {
		t.Fatalf("CanonicalIdentityIdFor = %q, want %q", got, want)
	}
}

// TestCreate_HappyPath asserts the Create mutation text carries every
// arg the DSL declares. Round-tripping the produced query through a
// recordingEngine catches typos / argument-name drift that would
// silently land an `mutationCreateNodeTokenIdentity not found` error
// at runtime (the same gotcha that bit memql#344's mutationCreateSkill).
func TestCreate_HappyPath(t *testing.T) {
	eng := &recordingEngine{}
	s := &Store{Engine: eng}
	bootAt := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	expiresAt := bootAt.AddDate(0, 0, 30)
	err := s.Create(context.Background(), CreateInput{
		IdentityId:         "v1:identity:identity:node:bff:bff-local",
		UserId:             "v1:identity:user:operator-1",
		NodeId:             "bff-local",
		NodeType:           "bff",
		KeyHash:            "jti-abc123",
		MintedBy:           SystemBootstrapMintedBy,
		ExpiresAt:          expiresAt,
		BootstrappedAt:     bootAt,
		BootstrappedFrom:   "10.0.0.42",
		LastBootstrappedAt: bootAt,
	})
	require.NoError(t, err)
	require.Len(t, eng.calls, 1, "expected one Execute call")
	q := eng.calls[0]
	assert.True(t, strings.HasPrefix(q, "mutationCreateNodeTokenIdentity({"), "wrong mutation name: %s", q)
	for _, fragment := range []string{
		`identityId:"v1:identity:identity:node:bff:bff-local"`,
		`userId:"v1:identity:user:operator-1"`,
		`nodeId:"bff-local"`,
		`nodeType:"bff"`,
		`keyHash:"jti-abc123"`,
		`mintedBy:"system:node_bootstrap"`,
		`bootstrappedFrom:"10.0.0.42"`,
		`bootstrappedAt:"2026-05-26T10:00:00Z"`,
		`lastBootstrappedAt:"2026-05-26T10:00:00Z"`,
		`expiresAt:"2026-06-25T10:00:00Z"`,
	} {
		assert.Contains(t, q, fragment, "fragment %q missing", fragment)
	}
}

// TestCreate_RejectsMissingRequired asserts the validation gate runs
// BEFORE the engine call, so a malformed CreateInput doesn't leak a
// half-formed mutation into the engine + database.
func TestCreate_RejectsMissingRequired(t *testing.T) {
	eng := &recordingEngine{}
	s := &Store{Engine: eng}
	cases := []struct {
		name string
		in   CreateInput
	}{
		{"missing identityId", CreateInput{UserId: "u", NodeId: "n", NodeType: "bff", MintedBy: "m"}},
		{"missing userId", CreateInput{IdentityId: "i", NodeId: "n", NodeType: "bff", MintedBy: "m"}},
		{"missing nodeId", CreateInput{IdentityId: "i", UserId: "u", NodeType: "bff", MintedBy: "m"}},
		{"missing nodeType", CreateInput{IdentityId: "i", UserId: "u", NodeId: "n", MintedBy: "m"}},
		{"missing mintedBy", CreateInput{IdentityId: "i", UserId: "u", NodeId: "n", NodeType: "bff"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.Create(context.Background(), c.in)
			require.Error(t, err)
			assert.Empty(t, eng.calls, "engine should not have been called when input is invalid")
		})
	}
}

// TestRecordBootstrap_HappyPath asserts the re-bootstrap update path
// carries the right fields. Whole-replace of credentials means the
// mutation MUST re-send every field the row needs to validate against
// the variant discriminator -- so the existing row is passed through
// for its preserved-origin signals (bootstrappedAt + bootstrappedFrom)
// and shape signals (nodeId / nodeType / mintedBy).
func TestRecordBootstrap_HappyPath(t *testing.T) {
	eng := &recordingEngine{}
	s := &Store{Engine: eng}
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	bootAt := now.Add(-7 * 24 * time.Hour)
	existing := &Row{
		ID:               "v1:identity:identity:node:bff:bff-local",
		NodeId:           "bff-local",
		NodeType:         "bff",
		MintedBy:         SystemBootstrapMintedBy,
		BootstrappedAt:   bootAt,
		BootstrappedFrom: "10.0.0.42",
	}
	require.NoError(t, s.RecordBootstrap(context.Background(), existing, "jti-new", now.AddDate(0, 0, 30), now))
	require.Len(t, eng.calls, 1)
	q := eng.calls[0]
	assert.True(t, strings.HasPrefix(q, "mutationRecordNodeTokenBootstrap({"))
	// Updated fields
	assert.Contains(t, q, `keyHash:"jti-new"`)
	assert.Contains(t, q, `lastBootstrappedAt:"2026-05-26T10:00:00Z"`)
	// Preserved-origin fields MUST appear (the bug fix #343 makes
	// these required for the variant validator to accept the update).
	assert.Contains(t, q, `bootstrappedAt:"2026-05-19T10:00:00Z"`)
	assert.Contains(t, q, `bootstrappedFrom:"10.0.0.42"`)
	// Shape signals
	assert.Contains(t, q, `nodeId:"bff-local"`)
	assert.Contains(t, q, `nodeType:"bff"`)
	assert.Contains(t, q, `mintedBy:"system:node_bootstrap"`)
}

// TestRevoke_StampsTimestamp asserts the revoke surface stamps the
// caller-supplied timestamp (vs. picking its own internal time) so
// the audit timestamp matches the operator-action time the admin
// page records. Exercises RevokeAt (caller-supplied timestamp); the
// admin path goes through Revoke (which stamps time.Now() and is a
// thin wrapper around RevokeAt). RevokeAt does its own
// LookupByIdentityId first (caller usually only has the id), so the
// fixture stages a row response for the lookup call.
func TestRevokeAt_StampsTimestamp(t *testing.T) {
	bootAt := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	creds := mustStruct(map[string]any{
		"nodeId":           "bff-local",
		"nodeType":         "bff",
		"mintedBy":         SystemBootstrapMintedBy,
		"bootstrappedAt":   bootAt.Format(time.RFC3339Nano),
		"bootstrappedFrom": "10.0.0.42",
	})
	payload := &structpb.Struct{Fields: map[string]*structpb.Value{
		"userId":      structpb.NewStringValue("system:node_bootstrap"),
		"active":      structpb.NewBoolValue(true),
		"credentials": structpb.NewStructValue(creds),
	}}
	eng := &recordingEngine{
		resp: &memql.ExecuteResult{
			Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
				{Id: "v1:identity:identity:node:bff:bff-local", Payload: payload},
			}},
		},
	}
	s := &Store{Engine: eng}
	when := time.Date(2026, 5, 26, 12, 30, 0, 0, time.UTC)
	require.NoError(t, s.RevokeAt(context.Background(), "v1:identity:identity:node:bff:bff-local", when))
	// Two calls expected: the lookup + the revoke mutation.
	require.Len(t, eng.calls, 2)
	assert.True(t, strings.HasPrefix(eng.calls[0], "queryNodeTokenByIdentityId"))
	revoke := eng.calls[1]
	assert.True(t, strings.HasPrefix(revoke, "mutationRevokeNodeTokenIdentity"))
	assert.Contains(t, revoke, `revokedAt:"2026-05-26T12:30:00Z"`)
	// Preserved origin fields pulled from the lookup-staged row.
	assert.Contains(t, revoke, `bootstrappedFrom:"10.0.0.42"`)
	assert.Contains(t, revoke, `nodeType:"bff"`)
}

// TestRevokeAt_RejectsEmptyId catches the "easy to forget" failure mode
// where a caller passes an empty id and gets a generic engine error
// later in the path instead of a clear validation error.
func TestRevokeAt_RejectsEmptyId(t *testing.T) {
	eng := &recordingEngine{}
	s := &Store{Engine: eng}
	err := s.RevokeAt(context.Background(), "", time.Now())
	require.Error(t, err)
	assert.Empty(t, eng.calls)
}

// TestRevokeAt_RejectsMissingRow asserts that revoking a non-existent
// id surfaces a clear error rather than silently producing a "ghost"
// row. The lookup branch returns nil + nil; the store must short-
// circuit on nil-row.
func TestRevokeAt_RejectsMissingRow(t *testing.T) {
	eng := &recordingEngine{resp: &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}}
	s := &Store{Engine: eng}
	err := s.RevokeAt(context.Background(), "v1:identity:identity:node:bff:doesnotexist", time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no node_token row")
	// One call (the lookup); no mutation issued.
	require.Len(t, eng.calls, 1)
}

// TestRevoke_StampsCurrentTime asserts the no-arg Revoke wrapper
// (the admin-path entry point) records a timestamp. The internal
// lookup needs a row to be staged so the revoke path runs to
// completion.
func TestRevoke_StampsCurrentTime(t *testing.T) {
	creds := mustStruct(map[string]any{
		"nodeId":   "bff-local",
		"nodeType": "bff",
		"mintedBy": SystemBootstrapMintedBy,
	})
	payload := &structpb.Struct{Fields: map[string]*structpb.Value{
		"active":      structpb.NewBoolValue(true),
		"credentials": structpb.NewStructValue(creds),
	}}
	eng := &recordingEngine{
		resp: &memql.ExecuteResult{
			Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
				{Id: "v1:identity:identity:node:bff:bff-local", Payload: payload},
			}},
		},
	}
	s := &Store{Engine: eng}
	require.NoError(t, s.Revoke(context.Background(), "v1:identity:identity:node:bff:bff-local"))
	require.Len(t, eng.calls, 2)
	// Crude assertion: there's an RFC3339Nano-shaped revokedAt in the
	// revoke mutation. Full parsing would re-encode + risk drift.
	assert.Contains(t, eng.calls[1], `revokedAt:"`)
}

// TestLookupByIdentityId_ExtractsRowFromBundle asserts the LookupByIdentityId
// path correctly projects a row out of the Bundle-flavored engine
// response (raw concept query result). This is the variant the
// verifier hot path observes.
func TestLookupByIdentityId_ExtractsRowFromBundle(t *testing.T) {
	bootAt := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	creds := mustStruct(map[string]any{
		"nodeId":           "bff-local",
		"nodeType":         "bff",
		"keyHash":          "jti-abc",
		"mintedBy":         SystemBootstrapMintedBy,
		"expiresAt":        bootAt.AddDate(0, 0, 30).Format(time.RFC3339Nano),
		"bootstrappedAt":   bootAt.Format(time.RFC3339Nano),
		"bootstrappedFrom": "10.0.0.42",
		"revokedAt":        "",
	})
	payload := &structpb.Struct{Fields: map[string]*structpb.Value{
		"userId":      structpb.NewStringValue("v1:identity:user:operator-1"),
		"active":      structpb.NewBoolValue(true),
		"credentials": structpb.NewStructValue(creds),
	}}
	eng := &recordingEngine{
		resp: &memql.ExecuteResult{
			Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
				{Id: "v1:identity:identity:node:bff:bff-local", Payload: payload},
			}},
		},
	}
	s := &Store{Engine: eng}
	row, err := s.LookupByIdentityId(context.Background(), "v1:identity:identity:node:bff:bff-local")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "v1:identity:identity:node:bff:bff-local", row.ID)
	assert.Equal(t, "v1:identity:user:operator-1", row.UserId)
	assert.True(t, row.Active)
	assert.Equal(t, "bff", row.NodeType)
	assert.Equal(t, "bff-local", row.NodeId)
	assert.Equal(t, "jti-abc", row.KeyHash)
	assert.Equal(t, "10.0.0.42", row.BootstrappedFrom)
	assert.False(t, row.BootstrappedAt.IsZero())
	assert.False(t, row.IsRevoked(), "active row with empty revokedAt must not be revoked")
}

// TestLookupByIdentityId_DetectsRevoked asserts the IsRevoked
// helper flips when either signal (active=false OR revokedAt set)
// fires. The verifier relies on this for its hot-path gate.
func TestLookupByIdentityId_DetectsRevoked(t *testing.T) {
	cases := []struct {
		name   string
		active bool
		stamp  string
		want   bool
	}{
		{"active + unstamped", true, "", false},
		{"inactive + unstamped", false, "", true},
		{"active + stamped", true, "2026-05-26T12:00:00Z", true},
		{"inactive + stamped", false, "2026-05-26T12:00:00Z", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			creds := mustStruct(map[string]any{"revokedAt": c.stamp})
			payload := &structpb.Struct{Fields: map[string]*structpb.Value{
				"active":      structpb.NewBoolValue(c.active),
				"credentials": structpb.NewStructValue(creds),
			}}
			eng := &recordingEngine{resp: &memql.ExecuteResult{
				Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{Payload: payload}}},
			}}
			s := &Store{Engine: eng}
			row, err := s.LookupByIdentityId(context.Background(), "id")
			require.NoError(t, err)
			require.NotNil(t, row)
			assert.Equal(t, c.want, row.IsRevoked())
		})
	}
}

// TestLookupByIdentityId_NoMatch returns nil-row + nil-error when the
// engine returns an empty bundle. The verifier interprets nil as
// "no row -- treat as not-yet-persisted operator-CLI mint" rather
// than "revoked", so the nil/empty distinction is meaningful.
func TestLookupByIdentityId_NoMatch(t *testing.T) {
	eng := &recordingEngine{resp: &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}}
	s := &Store{Engine: eng}
	row, err := s.LookupByIdentityId(context.Background(), "v1:identity:identity:node:bff:bff-local")
	require.NoError(t, err)
	assert.Nil(t, row)
}

// TestNilStore_Errors covers the "Store wasn't wired" failure path so
// callers see a clean error message rather than a nil-deref panic.
func TestNilStore_Errors(t *testing.T) {
	var s *Store
	err := s.Create(context.Background(), CreateInput{})
	require.Error(t, err)
	_, err = s.LookupByIdentityId(context.Background(), "any")
	require.Error(t, err)
}

// EngineExecutor compile-time check: the recordingEngine satisfies the
// same interface the production Store consumes. Catches a future
// signature drift in identity.EngineExecutor that would silently break
// the test harness.
var _ identity.EngineExecutor = (*recordingEngine)(nil)

func mustStruct(m map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}
	return s
}
