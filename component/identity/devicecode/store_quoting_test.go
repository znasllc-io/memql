package devicecode

// The quoting round-trip POST /device/code never had (memql#3611).
//
// createDeviceCode interpolated eleven caller-supplied values into a MemQL
// statement with Go's %q. The MemQL lexer implements the JSON escape set and
// ONLY that -- `" \ / b f n r t u` -- and returns `invalid escape character`
// for anything else. %q emits `\x00`, `\a`, `\v` and `\xNN`, none of which the
// lexer knows, so one control byte or one invalid UTF-8 byte anywhere in a
// value made the WHOLE mutation unparseable and the row was never written.
//
// This endpoint is the one that made that worst: RFC 8628's device
// authorization request is UNAUTHENTICATED, so `userAgent` and `clientId` are
// strings a caller chooses. Two bugs stacked -- truncateUserAgent also cut at
// byte 256 regardless of rune boundaries, so a long UA with an accent in it
// MANUFACTURED the invalid byte that then failed to parse.
//
// These tests run the REAL parser over the REAL emitted statement, so they fail
// against %q and pass against langparser.QuoteString. A unit test asserting
// "Create returned nil" would have passed throughout, because the store's own
// error path never sees the parse -- a fake engine that ignores its argument
// cannot notice that the argument is not a statement.

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/identity"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

type quotingCaptureEngine struct{ queries []string }

func (e *quotingCaptureEngine) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	e.queries = append(e.queries, query)
	return &memqlengine.ExecuteResult{}, nil
}

var _ identity.EngineExecutor = (*quotingCaptureEngine)(nil)

// hostileValues are the bytes the adversarial sweep in memql#3611 found. Each
// is something a real caller can put in a header:
//
//   - ESC / bell / vertical tab reach a UA string from any tool that pastes
//     terminal output into its version banner;
//   - NUL is what a truncating C client leaves behind;
//   - the split rune is what byte-truncation MAKES out of ordinary text, which
//     is why it is here and not merely hypothetical.
var hostileValues = []struct {
	name  string
	value string
}{
	{"ansi escape", "curl/8.5 \x1b[31mred\x1b[0m"},
	{"bell", "curl/8.5\a"},
	{"vertical tab", "curl/8.5\v1"},
	{"nul", "curl/8.5\x00"},
	{"invalid utf8 (split rune)", "curl/8.5 \xc3"},
	{"quote and backslash", `curl/8.5 "quoted" C:\path`},
	{"newline and tab", "curl/8.5\n\tcontinued"},
}

// parsedDeviceCodeArgs runs the production parser over an emitted statement and
// returns the arguments the engine would hand the mutation.
func parsedDeviceCodeArgs(t *testing.T, query string) map[string]any {
	t.Helper()
	expr, err := langparser.ParseExpression(query)
	if err != nil {
		t.Fatalf("the engine cannot parse the statement this store emits, so the row is "+
			"never written:\n  %s\n  %v", query, err)
	}
	call, ok := expr.(*langparser.FunctionCallExpr)
	if !ok {
		t.Fatalf("statement parsed as %T, want a mutation call:\n  %s", expr, query)
	}
	return call.Args
}

// TestCreateSurvivesHostileUserAgent is the defect in its most reachable form.
func TestCreateSurvivesHostileUserAgent(t *testing.T) {
	for _, tc := range hostileValues {
		t.Run(tc.name, func(t *testing.T) {
			engine := &quotingCaptureEngine{}
			store := &Store{Engine: engine}

			err := store.Create(context.Background(), CreateInput{
				Id:              "dc-1",
				ClientId:        "cockpit",
				DeviceCodeHash:  "abc123",
				UserCodeHash:    "def456",
				ExpiresAt:       time.Unix(1700000000, 0).UTC(),
				IntervalSeconds: 5,
				SourceIP:        "203.0.113.7",
				UserAgent:       tc.value,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if len(engine.queries) != 1 {
				t.Fatalf("expected exactly one engine call, got %d", len(engine.queries))
			}
			args := parsedDeviceCodeArgs(t, engine.queries[0])

			got, ok := args["userAgent"]
			if !ok {
				t.Fatalf("userAgent is ABSENT from the parsed args %#v", args)
			}
			// Valid UTF-8 must survive byte for byte. Invalid UTF-8 must not:
			// QuoteString runs the value through encoding/json, which replaces
			// an unpaired byte with U+FFFD. That is a deliberate trade recorded
			// at QuoteString -- a mangled diagnostic string beats an
			// unparseable statement and a request that fails outright -- and
			// asserting it here keeps it from being changed by accident.
			want := tc.value
			if !utf8.ValidString(want) {
				want = strings.ToValidUTF8(want, "\ufffd")
			}
			if got != want {
				t.Errorf("userAgent round-tripped as %q, want %q\n  emitted: %s", got, want, engine.queries[0])
			}
		})
	}
}

// TestCreateSurvivesHostileClientId covers the second unauthenticated input on
// the same request. clientId is not merely echoed back -- it selects the OAuth
// client -- so an unparseable statement here is the same lost write.
func TestCreateSurvivesHostileClientId(t *testing.T) {
	engine := &quotingCaptureEngine{}
	store := &Store{Engine: engine}

	if err := store.Create(context.Background(), CreateInput{
		Id:             "dc-2",
		ClientId:       "cockpit\x1b[2J",
		DeviceCodeHash: "abc123",
		UserCodeHash:   "def456",
		ExpiresAt:      time.Unix(1700000000, 0).UTC(),
		Scope:          "read\vwrite",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	args := parsedDeviceCodeArgs(t, engine.queries[0])
	if got := args["clientId"]; got != "cockpit\x1b[2J" {
		t.Errorf("clientId round-tripped as %q", got)
	}
	if got := args["scope"]; got != "read\vwrite" {
		t.Errorf("scope round-tripped as %q", got)
	}
}

// TestTruncateUserAgentCutsOnARuneBoundary is the other half of the defect.
//
// The bound is a BYTE bound and stays one -- what it protects is storage and
// render width. What changed is where the cut may land: never inside a rune, so
// the function cannot manufacture a string that is not valid UTF-8 out of input
// that was.
func TestTruncateUserAgentCutsOnARuneBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		ua   string
	}{
		// Each fills the buffer so the cut lands at a different offset within
		// the multi-byte rune -- the boundary case is which byte of the rune
		// byte 256 happens to be.
		{"2-byte rune spanning the cut", strings.Repeat("a", 255) + strings.Repeat("é", 40)},
		{"3-byte rune spanning the cut", strings.Repeat("a", 254) + strings.Repeat("→", 40)},
		{"4-byte rune spanning the cut", strings.Repeat("a", 254) + strings.Repeat("😀", 40)},
		{"all multi-byte", strings.Repeat("😀", 200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateUserAgent(tc.ua)
			if len(got) > userAgentMaxLen {
				t.Errorf("truncated to %d bytes, over the %d-byte bound", len(got), userAgentMaxLen)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncation produced invalid UTF-8: %q\n"+
					"A cut inside a rune leaves a lone continuation byte. That byte is what "+
					"%%q rendered as \\xNN, which the lexer rejects -- so this is how an "+
					"ordinary accented User-Agent took the endpoint down for its own request.", got)
			}
			if !strings.HasPrefix(tc.ua, got) {
				t.Errorf("truncation is not a prefix of the input: %q", got)
			}
		})
	}
}

// TestTruncateUserAgentLeavesShortValuesAlone pins the untouched path: the
// rune-boundary walk must not trim a value that was never over the bound.
func TestTruncateUserAgentLeavesShortValuesAlone(t *testing.T) {
	for _, ua := range []string{"", "curl/8.5", "  curl/8.5  ", "cøckpit/1.0 😀", strings.Repeat("é", 128)} {
		if got, want := truncateUserAgent(ua), strings.TrimSpace(ua); got != want {
			t.Errorf("truncateUserAgent(%q) = %q, want %q", ua, got, want)
		}
	}
}
