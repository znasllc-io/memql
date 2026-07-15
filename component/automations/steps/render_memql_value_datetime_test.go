package steps

// render_memql_value_datetime_test.go -- regression guards for memql#2543.
//
// A logic call's resolved args are stringified into query text by
// renderMemQLValue and re-parsed by engine.Execute. The type switch had no
// datetime cases, so a row's createdAt (a live time.Time off the DB model,
// component/database/memory-nodes/models.go) fell through to the reflect
// tail's fmt.Sprintf("%v") and rendered in Go's default format
// ("2006-01-02 15:04:05 -0700 MST") -- which the langparser rejects with
// `expected '}', got "-07"`, crashing the automation. The proto twin
// (*timestamppb.Timestamp, the shape-projection createdAt representation)
// leaked proto text ("seconds:... nanos:...") the same way, and
// time.Duration leaked "1h30m0s".
//
// The contract pinned here: every datetime-ish scalar renders as a QUOTED
// RFC3339 UTC string (durations as their quoted Go string), so the emitted
// query text stays parseable and the receiving logic reads a value the
// runtime date helpers (parseDate / time.Parse(RFC3339)) accept.

import (
	"strings"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRenderMemQLValue_DatetimeScalars(t *testing.T) {
	cet := time.FixedZone("CET", 3600)
	ts := time.Date(2026, 7, 14, 9, 30, 0, 0, cet)
	utc := time.Date(2026, 7, 14, 8, 30, 0, 0, time.UTC)

	cases := []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "time.Time renders quoted RFC3339 in UTC",
			value: ts,
			want:  `"2026-07-14T08:30:00Z"`,
		},
		{
			name:  "time.Time already UTC",
			value: utc,
			want:  `"2026-07-14T08:30:00Z"`,
		},
		{
			name:  "*time.Time renders like time.Time",
			value: &ts,
			want:  `"2026-07-14T08:30:00Z"`,
		},
		{
			name:  "nil *time.Time renders null",
			value: (*time.Time)(nil),
			want:  "null",
		},
		{
			name:  "*timestamppb.Timestamp renders quoted RFC3339 in UTC",
			value: timestamppb.New(ts),
			want:  `"2026-07-14T08:30:00Z"`,
		},
		{
			name:  "nil *timestamppb.Timestamp renders null",
			value: (*timestamppb.Timestamp)(nil),
			want:  "null",
		},
		{
			name:  "time.Duration renders quoted",
			value: 90 * time.Minute,
			want:  `"1h30m0s"`,
		},
		{
			// The exact #2543 row shape: a query-result row map whose
			// createdAt is a live time.Time next to ordinary payload fields.
			name: "row map with createdAt",
			value: map[string]any{
				"id":        "v1:ship:scan:1",
				"createdAt": ts,
				"count":     3,
			},
			want: `{"count": 3, "createdAt": "2026-07-14T08:30:00Z", "id": "v1:ship:scan:1"}`,
		},
		{
			name: "row collection with createdAt",
			value: []any{
				map[string]any{"createdAt": ts},
				map[string]any{"createdAt": utc},
			},
			want: `[{"createdAt": "2026-07-14T08:30:00Z"}, {"createdAt": "2026-07-14T08:30:00Z"}]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderMemQLValue(tc.value)
			if got != tc.want {
				t.Errorf("renderMemQLValue:\n  got:  %s\n  want: %s", got, tc.want)
			}
			// The load-bearing invariant #2543 violated: no Go default
			// datetime / proto-text markers may leak into query text.
			for _, bad := range []string{" MST", " CET", "+0100", "seconds:", "m=+"} {
				if strings.Contains(got, bad) {
					t.Errorf("renderMemQLValue leaked Go-format datetime marker %q: %s", bad, got)
				}
			}
		})
	}
}

// TestRenderFunctionArgs_DatetimeRowsParse pins the serialize->re-parse
// boundary itself: the query text a function step builds for a logic call
// whose arg carries datetime rows must PARSE through the langparser (the
// engine's sole runtime parser, parseViaLangparser). Before the fix this
// was the #2543 crash: `expected '}', got "-07"`.
func TestRenderFunctionArgs_DatetimeRowsParse(t *testing.T) {
	ts := time.Date(2026, 7, 14, 9, 30, 0, 0, time.FixedZone("CET", 3600))
	args := map[string]any{
		"rows": []any{
			map[string]any{"id": "v1:ship:scan:1", "createdAt": ts, "count": 3},
		},
		"day": "2026-07-14",
	}

	query := "logicDayRollup(" + renderFunctionArgs(args) + ")"
	parsed, err := langparser.ParseExpression(query)
	if err != nil {
		t.Fatalf("re-parse of the rendered logic call failed (the #2543 crash): %v\nquery: %s", err, query)
	}
	call, ok := parsed.(*langparser.FunctionCallExpr)
	if !ok {
		t.Fatalf("expected *FunctionCallExpr, got %T", parsed)
	}
	rows, ok := call.Args["rows"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("rows arg did not round-trip as a collection: %#v", call.Args["rows"])
	}
	row, ok := rows[0].(map[string]any)
	if !ok {
		t.Fatalf("row did not round-trip as an object: %#v", rows[0])
	}
	if row["createdAt"] != "2026-07-14T08:30:00Z" {
		t.Errorf("createdAt round-tripped as %#v (%T), want the RFC3339 UTC string", row["createdAt"], row["createdAt"])
	}
	if row["count"] != int64(3) {
		t.Errorf("count round-tripped as %#v (%T), want int64(3)", row["count"], row["count"])
	}
}
