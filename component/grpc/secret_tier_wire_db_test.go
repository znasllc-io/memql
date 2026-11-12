package memql

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/provenance"
	"github.com/znasllc-io/memql/component/secret"
)

// THE SECRET CONCEPTS' TIER, THROUGH THE REQUEST HANDLER.
//
// component/memql holds the tier against the engine. This drives the same
// questions through handleExecuteQuery, which is where a client's
// ExecuteQueryMsg lands: the raw query string a concept browser sends, the
// named mutation the generated SDK exposes, and the raw insert literal. Two
// gates stand on this path and the test reads which one answered, because they
// refuse different people:
//
//   - the coarse data-plane gate refuses a reader's write before the engine
//     sees it, whatever the concept;
//   - the row tier refuses a writer's, and decides every read.
//
// A cluster owner's stream -- which is also what the operator credential
// admits, and so what scripts/secrets seeds through -- reads and writes.
//
// Skips (via openWireTestDB -> dbtest.Unreachable) when no Postgres is
// reachable; CI's db-tests lane sets MEMQL_REQUIRE_DB=1.
func TestSecretRowsOverTheStreamFollowTheClusterOwnerTier(t *testing.T) {
	db := openWireTestDB(t)
	t.Setenv(secret.EnvMasterKey, strings.Repeat("b7", 32))
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memqlengine.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(concept.DefaultRegistry()))

	suffix := fmt.Sprintf("wire-%d", time.Now().UnixNano())
	seal := func(plaintext string) (string, string) {
		ciphertext, fingerprint, err := secret.Encrypt(plaintext)
		require.NoError(t, err)
		return ciphertext, fingerprint
	}
	// Every value rides through the parser's own quoter, never %q: the two
	// disagree about control bytes, and a statement the lexer cannot read is a
	// write that silently does not happen (dsl_call_quoting_test.go).
	quote := langparser.QuoteString
	setSecret := func(id, name, ciphertext, fingerprint string) string {
		return fmt.Sprintf(`mutation setGlobalSecret(id: %s, name: %s, encryptedValue: %s, fingerprint: %s)`,
			quote(id), quote(name), quote(ciphertext), quote(fingerprint))
	}

	// The row every stream below asks for, sealed by a cluster owner.
	ownerCtx := provenance.ContextWithProvenance(context.Background(), provenance.Direct("test:secret-tier-wire"))
	ownerCtx = auth.ContextWithAccess(ownerCtx, &auth.AccessContext{UserId: "secret-wire-owner", Role: auth.RoleOwner})
	ownerCtx = auth.ContextWithToken(ownerCtx, &auth.TokenInfo{Subject: "secret-wire-owner"})
	seedName := "SECRET_WIRE_SEED_" + strings.ToUpper(strings.ReplaceAll(suffix, "-", "_"))
	seedCiphertext, seedFingerprint := seal("sealed-before-any-stream-opened")
	_, err = eng.Execute(ownerCtx, setSecret("secret-wire-seed-"+suffix, seedName, seedCiphertext, seedFingerprint))
	require.NoError(t, err, "seed the row")
	readSeed := fmt.Sprintf(`concept==v1:platform:globalSecret && payload.name==%s`, quote(seedName))

	newSession := func(role auth.Role) (*streamSession, *captureStream) {
		logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		who := "secret-wire-" + string(role)
		cs := newCaptureStream(t)
		// The stream interceptor stamps client origin on every request-derived
		// context; a test that left it off would be measuring a different caller.
		cs.ctx = auth.ContextWithClientOrigin(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: who}))
		return &streamSession{
			service: &service{engine: eng, logger: logger}, stream: cs, logger: logger,
			access:       &auth.AccessContext{UserId: who, Role: role},
			accessLoaded: true,
		}, cs
	}
	// outcome waits for a request's answer: the nodes it returned, or the
	// refusal it got.
	outcome := func(cs *captureStream, requestId string) (nodes int, payload map[string]any, refusal string) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			cs.mu.Lock()
			for _, m := range cs.sent {
				if qe := m.GetQueryError(); qe != nil && qe.GetRequestId() == requestId {
					cs.mu.Unlock()
					return 0, nil, qe.GetError().GetMessage()
				}
				if qr := m.GetQueryResult(); qr != nil && qr.GetRequestId() == requestId && qr.GetDone() {
					cs.mu.Unlock()
					got := qr.GetResult().GetBundle().GetNodes()
					if len(got) > 0 {
						payload = got[0].GetPayload().AsMap()
					}
					return len(got), payload, ""
				}
			}
			cs.mu.Unlock()
			time.Sleep(15 * time.Millisecond)
		}
		t.Fatalf("no answer for request %q within 8s", requestId)
		return 0, nil, ""
	}
	stored := func(id string) bool {
		n, err := db.NewSelect().Model((*concept.MemoryNode)(nil)).
			Where("id = ?", "v1:platform:globalSecret:"+id).Count(context.Background())
		require.NoError(t, err)
		return n > 0
	}

	for _, tc := range []struct {
		role        auth.Role
		refusedBy   string // what a write's refusal names; "" when the write lands
		readsTheRow bool
	}{
		{auth.RoleReader, "data plane", false},
		{auth.RoleWriter, "row-authz", false},
		{auth.RoleDeveloper, "row-authz", false},
		{auth.RoleOwner, "", true},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			s, cs := newSession(tc.role)

			driveGateQuery(t, s, "read", readSeed)
			nodes, payload, refusal := outcome(cs, "read")
			if tc.readsTheRow {
				require.Empty(t, refusal)
				require.Equal(t, 1, nodes, "a cluster owner's stream did not read the row")
				require.Equal(t, seedFingerprint, payload["fingerprint"])
			} else {
				require.Zero(t, nodes, "a %s's stream was handed a secret row: %v", tc.role, payload)
			}

			ciphertext, fingerprint := seal("written over the stream by " + string(tc.role))
			writes := map[string]string{
				"named": setSecret("secret-wire-named-"+string(tc.role)+"-"+suffix, "SECRET_WIRE_NAMED_"+strings.ToUpper(string(tc.role)), ciphertext, fingerprint),
				"raw": fmt.Sprintf(`insert("v1:platform:globalSecret", id=%s, payload={"name":%s,"encryptedValue":%s,"fingerprint":%s,"active":true})`,
					quote("secret-wire-raw-"+string(tc.role)+"-"+suffix), quote("SECRET_WIRE_RAW_"+strings.ToUpper(string(tc.role))), quote(ciphertext), quote(fingerprint)),
			}
			for kind, query := range writes {
				id := "secret-wire-" + kind + "-" + string(tc.role) + "-" + suffix
				driveGateQuery(t, s, kind, query)
				nodes, _, refusal := outcome(cs, kind)
				if tc.refusedBy == "" {
					require.Empty(t, refusal, "a cluster owner's %s write was refused", kind)
					require.Equal(t, 1, nodes)
					require.True(t, stored(id), "a cluster owner's %s write did not land", kind)
					continue
				}
				require.Contains(t, refusal, tc.refusedBy, "a %s's %s write was not refused by the gate that owns it", tc.role, kind)
				require.False(t, stored(id), "a %s's refused %s write reached the table", tc.role, kind)
			}
		})
	}
}
