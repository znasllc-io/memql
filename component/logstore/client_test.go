package logstore

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func lineObj(kv ...any) map[string]any {
	m := map[string]any{"level": "warn", "message": "something happened"}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func TestValidateSession(t *testing.T) {
	for _, ok := range []string{"os-abcd", "ABCD", strings.Repeat("a", 64), "a_b-C9"} {
		if err := ValidateSession(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "abc", strings.Repeat("a", 65), "has space", "semi;colon", "dot.ted"} {
		if err := ValidateSession(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Every cap refuses the WHOLE call with a sentence naming the line and the
// cap, so the shell can see what it did.
func TestParseClientLinesRefusesEachCapNamingTheLine(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	tooMany := make([]any, 51)
	for i := range tooMany {
		tooMany[i] = lineObj()
	}
	if _, err := ParseClientLines(tooMany, now); err == nil || !strings.Contains(err.Error(), "51 lines is past the cap of 50") {
		t.Errorf("51 lines: %v", err)
	}

	cases := []struct {
		name  string
		lines []any
		want  string
	}{
		{"empty", nil, "lines is empty"},
		{"not an object", []any{lineObj(), "x"}, "line 1 is not an object"},
		{"message too long", []any{lineObj(), lineObj("message", strings.Repeat("m", 4097))}, "line 1: message is 4097 bytes; the cap is 4096"},
		{"message missing", []any{lineObj("message", "  ")}, "line 0: message is required"},
		{"bad level", []any{lineObj("level", "fatal")}, `line 0: level "fatal" is not one of debug, info, warn, error`},
		{"bad app", []any{lineObj(), lineObj(), lineObj("app", "Files")}, `line 2: app "Files" is not an app id`},
		{"bad component", []any{lineObj("component", "OS.Files")}, `line 0: component "OS.Files" is not a component name`},
		{"attributes too big", []any{lineObj("attributes", map[string]any{"blob": strings.Repeat("x", 9000)})}, "line 0: attributes serialize to"},
		{"attributes not object", []any{lineObj("attributes", []any{1})}, "line 0: attributes is not an object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseClientLines(tc.lines, now)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
	if _, err := ParseClientLines([]any{lineObj("attributes", map[string]any{"blob": strings.Repeat("x", 9000)})}, now); err == nil || !strings.Contains(err.Error(), "the cap is 8192") {
		t.Errorf("attributes cap sentence: %v", err)
	}
}

func TestParseClientLinesDerivesComponentAndKeepsAtWithinSkew(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	inside := now.Add(-2 * time.Minute).Format(time.RFC3339)
	outside := now.Add(-3 * time.Hour).Format(time.RFC3339)
	lines, err := ParseClientLines([]any{
		lineObj("app", "files", "at", inside, "attributes", map[string]any{"repeat": float64(3)}, "subject", "f-1", "subjectConcept", "v1:library:file"),
		lineObj("at", outside, "level", "ERROR"),
		lineObj("app", "deployables", "component", "deployables.build"),
	}, now)
	if err != nil {
		t.Fatalf("ParseClientLines: %v", err)
	}
	if lines[0].Component != "os.files" || lines[0].App != "files" {
		t.Errorf("component derived as %q for app %q, want os.files", lines[0].Component, lines[0].App)
	}
	if !lines[0].At.Equal(now.Add(-2 * time.Minute)) {
		t.Errorf("an `at` inside the skew was replaced: %v", lines[0].At)
	}
	if lines[0].Subject != "f-1" || lines[0].SubjectConcept != "v1:library:file" || lines[0].Attributes["repeat"] != float64(3) {
		t.Errorf("subject / attributes lost: %+v", lines[0])
	}
	if lines[1].Component != "os.shell" || lines[1].Level != slog.LevelError {
		t.Errorf("shell default / level: %+v", lines[1])
	}
	if !lines[1].At.Equal(now) {
		t.Errorf("an `at` three hours out must be replaced by now, got %v", lines[1].At)
	}
	if lines[2].Component != "deployables.build" {
		t.Errorf("an explicit component must be kept, got %q", lines[2].Component)
	}
}

func TestClientLimiterRefusesBeyond120AndRefills(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	l := NewClientLimiter()
	for i := 0; i < 120; i++ {
		if !l.Allow("u1", "os-a", 1, now) {
			t.Fatalf("line %d refused inside the 120 capacity", i)
		}
	}
	if l.Allow("u1", "os-a", 1, now) {
		t.Fatal("the 121st line was admitted")
	}
	// Another session of the same user has its own bucket; so does another user.
	if !l.Allow("u1", "os-b", 1, now) || !l.Allow("u2", "os-a", 1, now) {
		t.Fatal("buckets must be per (user, session)")
	}
	// Two per second: after one second, two lines fit and a third does not.
	later := now.Add(time.Second)
	if !l.Allow("u1", "os-a", 2, later) {
		t.Fatal("two tokens should have refilled after a second")
	}
	if l.Allow("u1", "os-a", 1, later) {
		t.Fatal("a third line was admitted after a one-second refill")
	}
	// A call for more lines than the bucket holds is refused WHOLE and
	// consumes nothing: the next smaller call still fits.
	later = later.Add(10 * time.Second) // 20 tokens
	if l.Allow("u1", "os-a", 50, later) {
		t.Fatal("a 50-line call was admitted with 20 tokens")
	}
	if !l.Allow("u1", "os-a", 20, later) {
		t.Fatal("the refused call consumed tokens; a refusal must consume nothing")
	}
	if l.Snapshot() != 3 {
		t.Errorf("bucket count %d, want 3", l.Snapshot())
	}
}
