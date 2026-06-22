package memql

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestIsRealtimeMirroredTool(t *testing.T) {
	mirrored := []string{
		"webSearch", "web_search",
		"knowledgeLookup", "knowledge_lookup",
		"domainLookup", "domain_lookup",
		"recentChat", "recent_chat",
		"  recentChat  ", // trimmed
	}
	for _, name := range mirrored {
		if !isRealtimeMirroredTool(name) {
			t.Errorf("expected %q to be a mirrored tool", name)
		}
	}
	notMirrored := []string{"uiClick", "canvas.publish", "createSpace", "", "agentUpdateSelf"}
	for _, name := range notMirrored {
		if isRealtimeMirroredTool(name) {
			t.Errorf("expected %q NOT to be a mirrored tool", name)
		}
	}
}

func TestFlattenToolResultContent(t *testing.T) {
	got := flattenToolResultContent([]*memqlv1.ToolResultContent{
		{Type: "text", Text: "line 1"},
		nil,
		{Type: "text", Text: "line 2"},
		{Type: "text", Text: ""},
	})
	if got != "line 1\nline 2" {
		t.Errorf("flatten = %q, want %q", got, "line 1\nline 2")
	}
	if flattenToolResultContent(nil) != "" {
		t.Error("flatten(nil) should be empty")
	}
}

func newCapturingSession(t *testing.T, voiceAgentScopeId string) (*streamSession, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return &streamSession{
		logger:            logger,
		identity:          auth.UserIdentity{Subject: "user-123"},
		voiceAgentScopeId: voiceAgentScopeId,
	}, &buf
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func TestMirrorRealtimeToolCall_EmitsForDirectLowRiskTool(t *testing.T) {
	sess, buf := newCapturingSession(t, "")
	sess.mirrorRealtimeToolCall("assistant", "knowledgeLookup", mustStruct(t, map[string]any{"q": "hi"}), "some result", false)

	out := buf.String()
	if !strings.Contains(out, "realtime.mcp.tool.call") {
		t.Fatalf("expected breadcrumb stage in log, got: %s", out)
	}
	for _, want := range []string{"knowledgeLookup", `\"q\":\"hi\"`, "some result", "user-123"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected log to contain %q, got: %s", want, out)
		}
	}
}

func TestMirrorRealtimeToolCall_SkipsVoiceAgentStream(t *testing.T) {
	// A voice-agent stream self-mirrors in-process; mirroring here would double-log.
	sess, buf := newCapturingSession(t, "v1:copresent:space:abc")
	sess.mirrorRealtimeToolCall("assistant", "knowledgeLookup", mustStruct(t, map[string]any{"q": "hi"}), "r", false)
	if buf.Len() != 0 {
		t.Errorf("expected no breadcrumb for a voice-agent stream, got: %s", buf.String())
	}
}

func TestMirrorRealtimeToolCall_SkipsNonAllowlistedTool(t *testing.T) {
	sess, buf := newCapturingSession(t, "")
	sess.mirrorRealtimeToolCall("assistant", "uiClick", mustStruct(t, map[string]any{"opId": "x"}), "r", false)
	if buf.Len() != 0 {
		t.Errorf("expected no breadcrumb for a non-allowlisted tool, got: %s", buf.String())
	}
}

func TestMirrorRealtimeToolCall_TruncatesLongResult(t *testing.T) {
	sess, buf := newCapturingSession(t, "")
	long := strings.Repeat("x", 800)
	sess.mirrorRealtimeToolCall("assistant", "recentChat", nil, long, false)
	out := buf.String()
	if strings.Contains(out, strings.Repeat("x", 600)) {
		t.Error("expected result to be truncated to 500 chars")
	}
	if !strings.Contains(out, "recentChat") {
		t.Errorf("expected breadcrumb for recentChat, got: %s", out)
	}
}
