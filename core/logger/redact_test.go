package logger

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

func TestShouldRedact(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want bool
	}{
		// secretNamePattern hits.
		{"password literal", "password", true},
		{"camelCase password", "userPassword", true},
		{"token", "token", true},
		{"access_token snake", "access_token", true},
		{"api_key", "api_key", true},
		{"apiKey camel", "apiKey", true},
		{"secret", "secret", true},
		{"client_secret", "client_secret", true},
		{"credential", "credential", true},
		{"auth", "auth", true},
		// Explicit denylist hits (no secret-marker root).
		{"authorization header", "authorization", true},
		{"Authorization header (case-insensitive)", "Authorization", true},
		{"cookie", "cookie", true},
		{"set-cookie", "set-cookie", true},
		{"Set-Cookie", "Set-Cookie", true},
		{"bearer", "bearer", true},
		{"prompt (SI)", "prompt", true},
		{"completion (SI)", "completion", true},
		{"payload", "payload", true},
		{"body", "body", true},
		// Non-matches.
		{"user_id", "user_id", false},
		{"span_id", "span_id", false},
		{"duration_ms", "duration_ms", false},
		{"empty", "", false},
		{"random word", "elephant", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldRedact(tc.key); got != tc.want {
				t.Errorf("ShouldRedact(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

// drainJSONLog runs fn with a slog.Logger backed by a redacting
// handler around a JSON handler, then returns the captured JSON
// record(s) as decoded maps. Test helper for the integration-style
// assertions below.
func drainJSONLog(t *testing.T, fn func(l *slog.Logger)) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	l := slog.New(NewRedactingHandler(base))
	fn(l)
	out := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode JSON line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestRedactingHandler_RedactsDenylistedKey(t *testing.T) {
	recs := drainJSONLog(t, func(l *slog.Logger) {
		l.Info("login", "password", "hunter2", "user_id", "u-1")
	})
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if rec["password"] != RedactedPlaceholder {
		t.Errorf("password = %v, want %q", rec["password"], RedactedPlaceholder)
	}
	if rec["user_id"] != "u-1" {
		t.Errorf("user_id = %v, want preserved", rec["user_id"])
	}
}

func TestRedactingHandler_RedactsAuthorizationHeader(t *testing.T) {
	recs := drainJSONLog(t, func(l *slog.Logger) {
		l.Info("request", "authorization", "Bearer mql_pat_abc")
	})
	if recs[0]["authorization"] != RedactedPlaceholder {
		t.Errorf("authorization = %v, want %q", recs[0]["authorization"], RedactedPlaceholder)
	}
}

func TestRedactingHandler_NonDenylistedPassesThrough(t *testing.T) {
	recs := drainJSONLog(t, func(l *slog.Logger) {
		l.Info("status", "duration_ms", 42, "request_id", "req-7")
	})
	rec := recs[0]
	if rec["duration_ms"] != float64(42) { // JSON unmarshal turns ints into float64
		t.Errorf("duration_ms = %v, want 42", rec["duration_ms"])
	}
	if rec["request_id"] != "req-7" {
		t.Errorf("request_id = %v, want preserved", rec["request_id"])
	}
}

func TestRedactingHandler_WalksNestedGroups(t *testing.T) {
	recs := drainJSONLog(t, func(l *slog.Logger) {
		l.Info("session",
			slog.Group("session",
				slog.String("token", "wkr-secret"),
				slog.String("user_id", "u-2"),
			),
		)
	})
	rec := recs[0]
	session, ok := rec["session"].(map[string]any)
	if !ok {
		t.Fatalf("session group missing or wrong shape: %T %v", rec["session"], rec["session"])
	}
	if session["token"] != RedactedPlaceholder {
		t.Errorf("nested token = %v, want %q", session["token"], RedactedPlaceholder)
	}
	if session["user_id"] != "u-2" {
		t.Errorf("nested user_id = %v, want preserved", session["user_id"])
	}
}

func TestRedactingHandler_WithAttrsRedactsAtBindTime(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	l := slog.New(NewRedactingHandler(base)).With("api_key", "sk-abc", "request_id", "r-1")
	l.Info("hello")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec["api_key"] != RedactedPlaceholder {
		t.Errorf("api_key (via WithAttrs) = %v, want %q", rec["api_key"], RedactedPlaceholder)
	}
	if rec["request_id"] != "r-1" {
		t.Errorf("request_id (via WithAttrs) = %v, want preserved", rec["request_id"])
	}
}

// TestNew_MountsRedactingHandler is the lint test the issue calls
// for: confirms the wrapper is actually mounted on the root logger
// produced by logger.New. Without this, an accidental refactor
// that swaps the handler chain would silently drop redaction.
func TestNew_MountsRedactingHandler(t *testing.T) {
	var buf bytes.Buffer
	l := New(common.ComponentName("test"), &buf, slog.LevelDebug)
	l.Info("login", "password", "hunter2")

	// Find a line (the colorized writer prefixes output, so we
	// can't json-decode reliably -- substring match is enough for
	// this lint).
	out := buf.String()
	if !strings.Contains(out, RedactedPlaceholder) {
		t.Errorf("logger.New output missing %q -- redacting handler not mounted?\noutput:\n%s",
			RedactedPlaceholder, out)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("logger.New output leaked secret value 'hunter2':\n%s", out)
	}
}
