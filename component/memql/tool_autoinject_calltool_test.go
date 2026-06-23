package memql

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

// TestExecuteToolAppliesContextToolDefaults is the engine-side regression guard
// for memql#1503: the realtime-voice CallTool proxy hop dispatches straight
// through engine.ExecuteTool (component/grpc handleCallTool -> executeTool ->
// ExecuteTool) and does NOT pre-merge tool defaults the way the streaming tool
// loop (ai_tool_loop.go) does. produceArtifact's `@autoInjected` ownerUserId is
// therefore never stamped, and the central builtin validator rejects with
// "requires 'ownerUserId' field in argument".
//
// The fix makes ExecuteTool itself the auto-injection chokepoint: it merges the
// ToolDefaults carried on ctx over the (LLM-supplied) args before dispatch, so
// every path that reaches ExecuteTool gets the @autoInjected fields stamped.
//
// This test uses a webhook handler (no DB needed) carrying an @autoInjected
// ownerUserId field. With the ToolDefaults on ctx, the value reaches the request
// body. WITHOUT the ExecuteTool merge (pre-fix) the body would carry no
// ownerUserId -- the assertion below fails.
func TestExecuteToolAppliesContextToolDefaults(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	e := newTestEngineForWebhook(hostOf(srv))
	tool := &Tool{
		Name: "produceArtifactLike",
		// ownerUserId is server-stamped (@autoInjected) -- the model must not
		// supply it, the engine must.
		AutoInjectedFields: []string{"ownerUserId", "partitionId"},
		Handler: &ToolHandler{
			Type:   "webhook",
			URL:    srv.URL + "/produce",
			Method: "POST",
		},
	}

	// The model supplied ONLY goal (exactly the #1503 repro: arguments={"goal":...}).
	// The voice CallTool default-resolution layer puts ownerUserId + partitionId on
	// ctx; ExecuteTool must merge them in before dispatch.
	ctx := common.ContextWithToolDefaults(agentCtxForTest(), map[string]any{
		"ownerUserId": "user-jose",
		"partitionId":     "standard:v1:cognition:space:demo",
	})
	result, err := e.ExecuteTool(ctx, tool, map[string]any{"goal": "a markdown file of birds"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content[0].Text)
	}
	if !strings.Contains(gotBody, `"ownerUserId":"user-jose"`) {
		t.Fatalf("ExecuteTool did not auto-inject ownerUserId from ctx ToolDefaults; body=%s", gotBody)
	}
	if !strings.Contains(gotBody, `"partitionId":"standard:v1:cognition:space:demo"`) {
		t.Fatalf("ExecuteTool did not auto-inject partitionId from ctx ToolDefaults; body=%s", gotBody)
	}
	// goal (the LLM-supplied arg) must survive the merge.
	if !strings.Contains(gotBody, "a markdown file of birds") {
		t.Fatalf("ExecuteTool dropped the LLM-supplied goal; body=%s", gotBody)
	}
}

// TestExecuteToolAutoInjectedDropsForgedValue is the security half of #107 that
// the ExecuteTool chokepoint must also honor: when the LLM forges a value for an
// @autoInjected field and the server has NO default for it, the forged value is
// dropped (never trusted), not passed through.
func TestExecuteToolAutoInjectedDropsForgedValue(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	e := newTestEngineForWebhook(hostOf(srv))
	tool := &Tool{
		Name:               "forgeable",
		AutoInjectedFields: []string{"ownerUserId"},
		Handler: &ToolHandler{
			Type:   "webhook",
			URL:    srv.URL + "/x",
			Method: "POST",
		},
	}

	// No ToolDefaults on ctx, but the LLM forged an ownerUserId. It must be
	// dropped (the server has no default to stamp).
	result, err := e.ExecuteTool(agentCtxForTest(), tool, map[string]any{
		"goal":        "ok",
		"ownerUserId": "attacker",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content[0].Text)
	}
	if strings.Contains(gotBody, "attacker") {
		t.Fatalf("ExecuteTool passed a forged @autoInjected ownerUserId through; body=%s", gotBody)
	}
}
