package mcp

// Tests for the MCP actor-identity wiring + unknown-tool error semantics
// surfaced by the live staging smoke test:
//   - a reflected query tool must run with the session's authenticated actor
//     on the context (so self-scoped reads like currentUser resolve
//     actor.userId instead of failing "id cannot be empty") -- #1595;
//   - a tools/call for a tool that does not exist must be a JSON-RPC protocol
//     error, not a 200 response carrying isError:true.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// dispatchToolsCall is a small helper: run a tools/call and return the decoded
// response envelope.
func dispatchToolsCall(t *testing.T, s *Server, name string, args map[string]any) struct {
	Result *json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
} {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": json.RawMessage(params)})
	raw, ok := s.Dispatch(context.Background(), req)
	if !ok {
		t.Fatal("expected a reply")
	}
	var resp struct {
		Result *json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp
}

// TestReflectedToolGetsActorEnvelope: a reflected tool call runs with the
// session's acting user/role stamped as an auth.AccessContext, so a self-scoped
// query resolves actor.userId. This is the fix for the staging "id cannot be
// empty" failure on currentUser (#1595).
func TestReflectedToolGetsActorEnvelope(t *testing.T) {
	eng := newFakeEngine()
	const user = "v1:identity:user:96377b35"
	s := NewServer(nil, "memql-mcp", "0", eng, Config{
		Tier:       TierAuthoring,
		ActingUser: user,
		ActingRole: "owner",
	})

	resp := dispatchToolsCall(t, s, "openTool", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %+v", resp.Error)
	}
	if eng.actorUserSeen != user {
		t.Errorf("actor.userId on the engine context = %q, want %q (the session's acting user)", eng.actorUserSeen, user)
	}
	if eng.actorRoleSeen != "owner" {
		t.Errorf("actor.role = %q, want %q", eng.actorRoleSeen, "owner")
	}
}

// TestActorEnvelopeNotClobbered: when the inbound context already carries a
// real authenticated AccessContext (e.g. a richer envelope from the transport),
// the server does not overwrite it with the coarse cfg-derived one.
func TestActorEnvelopeNotClobbered(t *testing.T) {
	eng := newFakeEngine()
	s := NewServer(nil, "memql-mcp", "0", eng, Config{Tier: TierAuthoring, ActingUser: "cfg-user", ActingRole: "owner"})

	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "real-user", Role: auth.RoleWriter})
	params, _ := json.Marshal(map[string]any{"name": "openTool", "arguments": map[string]any{}})
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": json.RawMessage(params)})
	s.Dispatch(ctx, req)
	if eng.actorUserSeen != "real-user" {
		t.Errorf("actor.userId = %q, want %q (existing envelope must not be clobbered)", eng.actorUserSeen, "real-user")
	}
}

// TestUnknownToolIsProtocolError: a tools/call for a tool that does not exist
// returns a JSON-RPC error (codeMethodNotFnd), NOT a 200 response with
// isError:true -- so a nonexistent tool is never dressed up as a successful call.
func TestUnknownToolIsProtocolError(t *testing.T) {
	eng := newFakeEngine() // knows openTool / gaTool, plus the meta-tools
	s := NewServer(nil, "memql-mcp", "0", eng, Config{Tier: TierAuthoring, ActingUser: "u", ActingRole: "owner"})

	resp := dispatchToolsCall(t, s, "currentUser", map[string]any{}) // not "currentUser"
	if resp.Error == nil {
		t.Fatalf("expected a JSON-RPC error for an unknown tool, got result: %v", resp.Result)
	}
	if resp.Error.Code != codeMethodNotFnd {
		t.Errorf("error code = %d, want %d (method/tool not found)", resp.Error.Code, codeMethodNotFnd)
	}
	if resp.Result != nil {
		t.Errorf("an unknown tool must not return a result envelope; got %s", *resp.Result)
	}
}

// TestKnownToolStillDispatches: a real reflected tool + the generic meta-tools
// remain dispatchable (the unknown-tool guard must not reject valid names).
func TestKnownToolStillDispatches(t *testing.T) {
	eng := newFakeEngine()
	s := NewServer(nil, "memql-mcp", "0", eng, Config{Tier: TierAuthoring, ActingUser: "u", ActingRole: "owner"})

	for _, name := range []string{"openTool", toolRunQuery, toolRunMutation} {
		resp := dispatchToolsCall(t, s, name, map[string]any{"name": "someConstruct"})
		if resp.Error != nil {
			t.Errorf("tool %q: unexpected JSON-RPC error %+v (should dispatch, even if it fails in-band)", name, resp.Error)
		}
	}
}
