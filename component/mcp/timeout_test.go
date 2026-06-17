package mcp

// Tests for the per-call execute timeout + structured request logging added in
// #1594: a blocked tools/call must turn into a JSON-RPC timeout error (not an
// indefinite hang), the session must stay usable for the next call, and every
// request must emit one structured log line.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// hangEngine wraps the test fakeEngine but makes ExecuteAuthored (the path
// run_query takes when a session is active) block until the per-call deadline
// cancels the context -- the staging "tools/call hangs" shape (#1595),
// reproduced in a unit test. It respects ctx so the orphaned goroutine drains
// once the timeout fires.
type hangEngine struct {
	*fakeEngine
}

func newHangEngine() *hangEngine { return &hangEngine{fakeEngine: newFakeEngine()} }

func (e *hangEngine) ExecuteAuthored(ctx context.Context, query, owner string, reg *memql.AuthoredRuntimeRegistry) (*memql.ExecuteResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

const toolsCallCurrentUser = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_query","arguments":{"name":"currentUser"}}}`

// TestToolsCallTimeoutReturnsError: a tools/call whose engine execution blocks
// returns a JSON-RPC timeout error (codeToolTimeout) within roughly the
// configured deadline, rather than hanging forever.
func TestToolsCallTimeoutReturnsError(t *testing.T) {
	eng := newHangEngine()
	s := NewServer(nil, "memql-mcp", "0", eng, Config{
		Tier:        TierAuthoring,
		ActingUser:  "v1:identity:user:tester",
		ActingRole:  "owner",
		ToolTimeout: 75 * time.Millisecond,
	})

	start := time.Now()
	raw, hasReply := s.Dispatch(context.Background(), []byte(toolsCallCurrentUser))
	elapsed := time.Since(start)
	if !hasReply {
		t.Fatal("expected a reply for a request id")
	}
	if elapsed > time.Second {
		t.Fatalf("tools/call took %s; the timeout did not fire", elapsed)
	}

	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error == nil {
		t.Fatalf("expected a JSON-RPC error, got: %s", raw)
	}
	if resp.Error.Code != codeToolTimeout {
		t.Errorf("error code = %d, want %d (codeToolTimeout)", resp.Error.Code, codeToolTimeout)
	}
	if !strings.Contains(resp.Error.Message, "timed out") {
		t.Errorf("error message = %q, want it to mention the timeout", resp.Error.Message)
	}
}

// TestToolsCallTimeoutReleasesSession drives the full HTTP path: a blocked
// tools/call times out, and a subsequent request on the SAME session is served
// promptly -- proving the per-session mutex was released (the hung call no
// longer wedges the session).
func TestToolsCallTimeoutReleasesSession(t *testing.T) {
	h := newTestHandlerCfg(t, HTTPConfig{
		BaseConfig: Config{Tier: TierAuthoring, ActingUser: "v1:identity:user:tester", ActingRole: "owner", ToolTimeout: 75 * time.Millisecond},
		NewServer: func(c Config) *Server {
			return NewServer(nil, "memql-mcp", "9.9.9", newHangEngine(), c)
		},
	})

	// initialize -> session id
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize status = %d", rec.Code)
	}
	sid := rec.Header().Get(sessionHeader)
	if sid == "" {
		t.Fatal("no session id assigned")
	}

	// A tools/call that blocks in the engine -> times out (codeToolTimeout).
	rec = post(t, h, toolsCallCurrentUser, sid, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "timed out") {
		t.Fatalf("expected a timeout error, got: %s", rec.Body.String())
	}

	// The session must still be usable: tools/list returns promptly.
	done := make(chan *struct{}, 1)
	go func() {
		rec2 := post(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, sid, true)
		if rec2.Code == http.StatusOK && strings.Contains(rec2.Body.String(), `"tools"`) {
			done <- &struct{}{}
		} else {
			done <- nil
		}
	}()
	select {
	case got := <-done:
		if got == nil {
			t.Fatal("tools/list after a timed-out call did not return a tool list")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tools/list wedged after a timed-out call -- session mutex not released")
	}
}

// TestRequestLoggingEmitsStructuredLine asserts every dispatched request emits
// one "mcp request" line carrying method + outcome, and that tools/call lines
// carry the tool name (but never the arguments).
func TestRequestLoggingEmitsStructuredLine(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	s := NewServer(logger, "memql-mcp", "0", nil, Config{Tier: TierAuthoring})

	// ping is a trivial, always-ok request.
	s.Dispatch(context.Background(), []byte(`{"jsonrpc":"2.0","id":7,"method":"ping"}`))

	logs := buf.String()
	if !strings.Contains(logs, `"msg":"mcp request"`) {
		t.Fatalf("no structured request log line emitted: %s", logs)
	}
	if !strings.Contains(logs, `"method":"ping"`) {
		t.Errorf("request log missing method: %s", logs)
	}
	if !strings.Contains(logs, `"outcome":"ok"`) {
		t.Errorf("request log missing ok outcome: %s", logs)
	}

	// A tools/call against the nil engine fails in-band (isError) -> outcome
	// error, and the line names the tool but not the args.
	buf.Reset()
	s.Dispatch(context.Background(), []byte(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"run_query","arguments":{"secret":"do-not-log"}}}`))
	logs = buf.String()
	if !strings.Contains(logs, `"tool":"run_query"`) {
		t.Errorf("tools/call log missing tool name: %s", logs)
	}
	if strings.Contains(logs, "do-not-log") {
		t.Errorf("tools/call log leaked argument values: %s", logs)
	}
}
