package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// decode unmarshals a single JSON-RPC response line.
func decode(t *testing.T, line []byte) rpcResponse {
	t.Helper()
	var resp rpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("unmarshal response %q: %v", line, err)
	}
	if resp.JSONRPC != "2.0" {
		t.Fatalf("jsonrpc = %q, want 2.0", resp.JSONRPC)
	}
	return resp
}

// resultMap re-decodes the response result into a map for field assertions.
func resultMap(t *testing.T, resp rpcResponse) map[string]any {
	t.Helper()
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal result map: %v", err)
	}
	return m
}

func TestInitialize(t *testing.T) {
	s := NewServer(nil, "memql-mcp", "9.9.9", nil, Config{})
	resp := s.handleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{}}}`))
	if resp == nil {
		t.Fatal("initialize returned no response")
	}
	if resp.Error != nil {
		t.Fatalf("initialize error: %+v", resp.Error)
	}
	if string(resp.ID) != "1" {
		t.Fatalf("id = %s, want 1", resp.ID)
	}

	m := resultMap(t, *resp)
	// Server echoes the client's requested protocol version.
	if m["protocolVersion"] != "2025-03-26" {
		t.Fatalf("protocolVersion = %v, want 2025-03-26", m["protocolVersion"])
	}
	caps, ok := m["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities missing/!object: %v", m["capabilities"])
	}
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("capabilities.tools missing: %v", caps)
	}
	info, ok := m["serverInfo"].(map[string]any)
	if !ok || info["name"] != "memql-mcp" || info["version"] != "9.9.9" {
		t.Fatalf("serverInfo = %v, want name=memql-mcp version=9.9.9", m["serverInfo"])
	}
}

func TestInitializeDefaultsProtocolVersion(t *testing.T) {
	s := NewServer(nil, "memql-mcp", "0", nil, Config{})
	resp := s.handleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	m := resultMap(t, *resp)
	if m["protocolVersion"] != DefaultProtocolVersion {
		t.Fatalf("protocolVersion = %v, want default %s", m["protocolVersion"], DefaultProtocolVersion)
	}
}

func TestToolsListMetaToolsWithoutEngine(t *testing.T) {
	// With no engine connected, the reflected DSL tools are absent but the
	// generic dispatchers (run_query / run_mutation / run_automation) are
	// always present (Phase 1, #1531).
	s := NewServer(nil, "memql-mcp", "0", nil, Config{})
	resp := s.handleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":"abc","method":"tools/list"}`))
	if resp == nil || resp.Error != nil {
		t.Fatalf("tools/list unexpected: %+v", resp)
	}
	if string(resp.ID) != `"abc"` {
		t.Fatalf("id = %s, want \"abc\"", resp.ID)
	}
	m := resultMap(t, *resp)
	tools, ok := m["tools"].([]any)
	if !ok {
		t.Fatalf("tools not an array: %v", m["tools"])
	}
	names := map[string]bool{}
	for _, raw := range tools {
		if tm, ok := raw.(map[string]any); ok {
			if n, ok := tm["name"].(string); ok {
				names[n] = true
			}
		}
	}
	for _, want := range []string{toolRunQuery, toolRunMutation, toolRunAutomation} {
		if !names[want] {
			t.Errorf("tools/list missing meta-tool %q (got %v)", want, names)
		}
	}
	if len(tools) != 3 {
		t.Fatalf("no-engine tools/list should be the 3 meta-tools, got %d", len(tools))
	}
}

func TestPing(t *testing.T) {
	s := NewServer(nil, "memql-mcp", "0", nil, Config{})
	resp := s.handleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":7,"method":"ping"}`))
	if resp == nil || resp.Error != nil {
		t.Fatalf("ping unexpected: %+v", resp)
	}
}

func TestNotificationGetsNoResponse(t *testing.T) {
	s := NewServer(nil, "memql-mcp", "0", nil, Config{})
	if resp := s.handleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); resp != nil {
		t.Fatalf("notification should produce no response, got %+v", resp)
	}
}

func TestUnknownMethodErrors(t *testing.T) {
	s := NewServer(nil, "memql-mcp", "0", nil, Config{})
	resp := s.handleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"no/such/method"}`))
	if resp == nil || resp.Error == nil {
		t.Fatalf("unknown method should error, got %+v", resp)
	}
	if resp.Error.Code != codeMethodNotFnd {
		t.Fatalf("error code = %d, want %d", resp.Error.Code, codeMethodNotFnd)
	}
}

func TestParseErrorHasNullID(t *testing.T) {
	s := NewServer(nil, "memql-mcp", "0", nil, Config{})
	resp := s.handleMessage(context.Background(), []byte(`{not json`))
	if resp == nil || resp.Error == nil {
		t.Fatalf("malformed input should error, got %+v", resp)
	}
	if resp.Error.Code != codeParseError {
		t.Fatalf("error code = %d, want %d", resp.Error.Code, codeParseError)
	}
	if string(resp.ID) != "null" {
		t.Fatalf("parse-error id = %s, want null", resp.ID)
	}
}

// TestServeRoundTrip drives the full newline-delimited read/dispatch/write
// loop over in-memory streams: initialize -> initialized notification ->
// tools/list, asserting an empty tool surface comes back. This is the
// acceptance behaviour (memql#1530): an MCP client connects and receives an
// empty tools/list.
func TestServeRoundTrip(t *testing.T) {
	s := NewServer(nil, "memql-mcp", "1.2.3", nil, Config{})

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	lines := splitLines(out.Bytes())
	// initialize + tools/list reply; the notification produces nothing.
	if len(lines) != 2 {
		t.Fatalf("got %d response lines, want 2: %q", len(lines), out.String())
	}

	initResp := decode(t, lines[0])
	if initResp.Error != nil || string(initResp.ID) != "1" {
		t.Fatalf("initialize response wrong: %+v", initResp)
	}

	listResp := decode(t, lines[1])
	if listResp.Error != nil || string(listResp.ID) != "2" {
		t.Fatalf("tools/list response wrong: %+v", listResp)
	}
	// No engine on this server, so tools/list is exactly the 3 meta-tools.
	tools := resultMap(t, listResp)["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("no-engine tools/list should be the 3 meta-tools, got %d", len(tools))
	}
}

// TestInitializeAdvertisesIcon asserts the initialize serverInfo carries the
// branded title + icon (MCP Implementation.icons) as a data: URI, so connector
// UIs (e.g. Claude) render the memQL mark instead of a placeholder.
func TestInitializeAdvertisesIcon(t *testing.T) {
	s := NewServer(nil, "memql-mcp", "1.2.3", nil, Config{})
	res := s.initializeResult(nil)
	si, ok := res["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("serverInfo missing/wrong type: %#v", res["serverInfo"])
	}
	if si["title"] != "memQL" {
		t.Errorf("serverInfo.title = %v, want memQL", si["title"])
	}
	icons, ok := si["icons"].([]map[string]any)
	if !ok || len(icons) == 0 {
		t.Fatalf("serverInfo.icons missing/empty: %#v", si["icons"])
	}
	if src, _ := icons[0]["src"].(string); !strings.HasPrefix(src, "data:image/svg+xml;base64,") {
		t.Errorf("icon src is not a base64 svg data URI: %q", src)
	}
	if icons[0]["mimeType"] != "image/svg+xml" {
		t.Errorf("icon mimeType = %v, want image/svg+xml", icons[0]["mimeType"])
	}
}

// TestServeStopsAtEOF confirms Serve returns nil at EOF (no trailing newline).
func TestServeStopsAtEOF(t *testing.T) {
	s := NewServer(nil, "memql-mcp", "0", nil, Config{})
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`) // no newline
	var out bytes.Buffer
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve at EOF: %v", err)
	}
	if len(splitLines(out.Bytes())) != 1 {
		t.Fatalf("want 1 response before EOF, got %q", out.String())
	}
}

func splitLines(b []byte) [][]byte {
	var lines [][]byte
	for _, l := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			lines = append(lines, l)
		}
	}
	return lines
}
