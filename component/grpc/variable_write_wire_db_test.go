package memql

import (
	"context"
	"encoding/json"
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
)

// TWO WRITE RULES OF THE PLATFORM'S CONFIGURATION ROWS, THROUGH THE REQUEST
// HANDLER.
//
// component/memql holds both rules against the engine. This drives them through
// handleExecuteQuery, which is where a client's ExecuteQueryMsg lands: the named
// mutation the generated SDK exposes and the raw insert literal a concept
// browser can send, under client origin, as each role's stream.
//
//   - A CONFIGURATION VARIABLE's row is written by a cluster owner or by stamped
//     server code, and read by everyone signed in. Two gates stand on the write
//     and the test reads which one answered: the coarse data-plane gate refuses
//     a reader's write before the engine sees it, and the variable write rule
//     refuses everyone else below the cluster owner.
//   - A SOURCE CREDENTIAL's row is server-written: a person's own stream may
//     revoke their credential and nothing else.
//
// Skips (via openWireTestDB -> dbtest.Unreachable) when no Postgres is
// reachable; CI's db-tests lane sets MEMQL_REQUIRE_DB=1.

// wireConfigHarness is one engine and the session plumbing both tests share.
type wireConfigHarness struct {
	t      *testing.T
	eng    *memqlengine.MemQLEngine
	suffix string
	stored func(conceptName, id string) (versions int, payload map[string]any)
}

func newWireConfigHarness(t *testing.T) *wireConfigHarness {
	t.Helper()
	db := openWireTestDB(t)
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memqlengine.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(concept.DefaultRegistry()))
	return &wireConfigHarness{
		t: t, eng: eng, suffix: fmt.Sprintf("wire-%d", time.Now().UnixNano()),
		stored: func(conceptName, id string) (int, map[string]any) {
			t.Helper()
			var nodes []concept.MemoryNode
			err := db.NewSelect().Model(&nodes).Where("id = ?", conceptName+":"+id).Order("createdAt DESC").Scan(context.Background())
			require.NoError(t, err)
			if len(nodes) == 0 {
				return 0, nil
			}
			payload := map[string]any{}
			require.NoError(t, json.Unmarshal(nodes[0].Payload, &payload))
			return len(nodes), payload
		},
	}
}

// actor is a signed-in caller acting on the engine directly: the fixture's
// writer, never a stream under test.
func (h *wireConfigHarness) actor(who string, role auth.Role) context.Context {
	ctx := provenance.ContextWithProvenance(context.Background(), provenance.Direct("test:config-write-wire"))
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: who, Role: role})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: who})
}

// session is one role's stream, as the interceptor presents it: client origin
// on every request-derived context.
func (h *wireConfigHarness) session(who string, role auth.Role) (*streamSession, *captureStream) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	cs := newCaptureStream(h.t)
	cs.ctx = auth.ContextWithClientOrigin(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: who}))
	return &streamSession{
		service: &service{engine: h.eng, logger: logger}, stream: cs, logger: logger,
		access:       &auth.AccessContext{UserId: who, Role: role},
		accessLoaded: true,
	}, cs
}

// outcome waits for a request's answer: the nodes it returned, or the refusal
// it got.
func (h *wireConfigHarness) outcome(cs *captureStream, requestId string) (nodes int, payload map[string]any, refusal string) {
	h.t.Helper()
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
	h.t.Fatalf("no answer for request %q within 8s", requestId)
	return 0, nil, ""
}

func TestVariableRowsOverTheStreamAreWrittenByTheClusterOwner(t *testing.T) {
	h := newWireConfigHarness(t)
	// Every value rides through the parser's own quoter, never %q
	// (dsl_call_quoting_test.go).
	quote := langparser.QuoteString
	const variable = "v1:platform:globalVariable"
	setVariable := func(id, name, value string) string {
		return fmt.Sprintf(`mutation setGlobalVariable(id: %s, name: %s, value: %s)`, quote(id), quote(name), quote(value))
	}

	// The row every stream below asks for, written by a cluster owner.
	seedId := "variable-wire-seed-" + h.suffix
	seedName := "VARIABLE_WIRE_SEED_" + strings.ToUpper(strings.ReplaceAll(h.suffix, "-", "_"))
	_, err := h.eng.Execute(h.actor("variable-wire-owner", auth.RoleOwner), setVariable(seedId, seedName, "set before any stream opened"))
	require.NoError(t, err, "seed the row")
	// The raw form, which answers a bundle of nodes this harness can read; the
	// named query's reach is component/memql's to hold.
	readSeed := fmt.Sprintf(`concept==%s && payload.name==%s`, variable, quote(seedName))

	for _, tc := range []struct {
		role      auth.Role
		refusedBy string // what a write's refusal names; "" when the write lands
	}{
		{auth.RoleReader, "data plane"},
		{auth.RoleWriter, "configuration variable"},
		{auth.RoleAdmin, "configuration variable"},
		{auth.RoleDeveloper, "configuration variable"},
		{auth.RoleOwner, ""},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			s, cs := h.session("variable-wire-"+string(tc.role), tc.role)

			// READS ARE NOT THIS RULE'S: every role's stream reads the row.
			driveGateQuery(t, s, "read", readSeed)
			nodes, payload, refusal := h.outcome(cs, "read")
			require.Empty(t, refusal, "a %s's stream could not read a variable", tc.role)
			require.Equal(t, 1, nodes, "a %s's stream did not read the variable row", tc.role)
			require.Equal(t, "set before any stream opened", payload["value"])

			role := string(tc.role)
			writes := []struct{ kind, id, query string }{
				{"named", "variable-wire-named-" + role + "-" + h.suffix,
					setVariable("variable-wire-named-"+role+"-"+h.suffix, "VARIABLE_WIRE_NAMED_"+strings.ToUpper(role), "written over the stream")},
				{"raw", "variable-wire-raw-" + role + "-" + h.suffix,
					fmt.Sprintf(`insert(%s, id=%s, payload={"name":%s,"value":"written over the stream","active":true})`,
						quote(variable), quote("variable-wire-raw-"+role+"-"+h.suffix), quote("VARIABLE_WIRE_RAW_"+strings.ToUpper(role)))},
				// Over the stored row: the write that would change what a name means.
				{"overwrite", seedId, setVariable(seedId, seedName, "overwritten by a "+role)},
			}
			for _, w := range writes {
				before, _ := h.stored(variable, w.id)
				driveGateQuery(t, s, w.kind, w.query)
				nodes, _, refusal := h.outcome(cs, w.kind)
				after, stored := h.stored(variable, w.id)
				if tc.refusedBy == "" {
					require.Empty(t, refusal, "a cluster owner's %s write was refused", w.kind)
					require.Equal(t, 1, nodes)
					require.Greater(t, after, before, "a cluster owner's %s write did not land", w.kind)
					continue
				}
				require.Contains(t, refusal, tc.refusedBy, "a %s's %s write was not refused by the gate that owns it", tc.role, w.kind)
				require.Equal(t, before, after, "a %s's refused %s write reached the table", tc.role, w.kind)
				if w.kind == "overwrite" {
					require.Equal(t, "set before any stream opened", stored["value"], "a %s's refused write changed the stored value", tc.role)
				}
			}
		})
	}
}

func TestSourceCredentialRowsOverTheStreamAreServerWritten(t *testing.T) {
	h := newWireConfigHarness(t)
	quote := langparser.QuoteString
	const credential = "v1:platform:sourceCredential"
	person := "credential-wire-person-" + h.suffix
	credentialId := "credential-wire-" + h.suffix

	// The row server code lands for a person: their own actor, internal origin
	// stamped, which is how sourceCredentialCreate writes it.
	_, err := h.eng.Execute(auth.ContextWithInternalOrigin(h.actor(person, auth.RoleWriter)), fmt.Sprintf(
		`mutation createSourceCredential(credentialId: %s, host: "github.com", label: "pasted token", encryptedValue: "sealed-by-the-server", fingerprint: "...rver")`,
		quote(credentialId)))
	require.NoError(t, err, "seed the credential")
	versions, row := h.stored(credential, credentialId)
	require.Equal(t, 1, versions)
	require.Equal(t, "active", row["status"])

	s, cs := h.session(person, auth.RoleWriter)

	// A fresh row, composed by the person.
	freshId := "credential-wire-fresh-" + h.suffix
	driveGateQuery(t, s, "create", fmt.Sprintf(
		`insert(%s, id=%s, payload={"host":"github.com","label":"composed","encryptedValue":"composed-by-the-caller","fingerprint":"...ller","status":"active"})`,
		quote(credential), quote(freshId)))
	_, _, refusal := h.outcome(cs, "create")
	require.Contains(t, refusal, "server-written", "the person's stream created a source credential row")
	versions, _ = h.stored(credential, freshId)
	require.Zero(t, versions, "the refused create reached the table")

	// Their own stored row: the sealed value, then where it may be presented.
	for kind, delta := range map[string]string{
		"value": `{"encryptedValue":"composed-by-the-caller"}`,
		"host":  `{"host":"example.test"}`,
	} {
		driveGateQuery(t, s, kind, fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, quote(credential), quote(credentialId), delta))
		_, _, refusal := h.outcome(cs, kind)
		require.Contains(t, refusal, "server-written", "the person's stream wrote its own credential's %s", kind)
	}
	versions, row = h.stored(credential, credentialId)
	require.Equal(t, 1, versions, "a refused write reached the table")
	require.Equal(t, "sealed-by-the-server", row["encryptedValue"])
	require.Equal(t, "github.com", row["host"])

	// The one write that is the person's own.
	driveGateQuery(t, s, "revoke", fmt.Sprintf(`mutation revokeSourceCredential(credentialId: %s)`, quote(credentialId)))
	nodes, _, refusal := h.outcome(cs, "revoke")
	require.Empty(t, refusal, "the person's stream could not revoke their own credential")
	require.Equal(t, 1, nodes)
	_, row = h.stored(credential, credentialId)
	require.Equal(t, "revoked", row["status"])
	require.Equal(t, "sealed-by-the-server", row["encryptedValue"], "the revoke changed the sealed value")
}
