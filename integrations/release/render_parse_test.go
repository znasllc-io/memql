package release

import (
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// render_parse_test.go -- every statement this package composes, run through
// the REAL MemQL front end (no database).
//
// ===========================================================================
// WHY THIS EXISTS, AND WHY THE REST OF THE SUITE DOES NOT COVER IT
// ===========================================================================
// Every other test in this package drives a fake engine that RECORDS call
// strings and parses nothing. That is the right shape for asserting what a cut
// wrote -- and it is exactly the failure mode memql#4256 and memql#4209
// document: a handler covered only against a recording engine can ship a call
// string the parser has never accepted, fail at parse on every real cluster,
// and stay green here forever. Five guest-invite mutations and the whole
// deploy-control surface each shipped that way.
//
// So this is the second level, and it checks two DIFFERENT things:
//
//   PARSE + RESOLVE -- the string is syntactically legal AND the function name
//   resolves in the registry loaded from the embedded dsl/ tree. Syntax alone
//   would have passed while every write failed at execute, which is what was
//   actually happening in #4258.
//
//   ARGUMENTS ARE DECLARED -- resolution is NOT argument checking, and
//   assuming it is leaves half the bug uncovered. validateFunctionArgs
//   iterates DECLARED fields and rejectUnknownArgs is gated behind the MCP
//   boundary, so an argument this package invents is silently DROPPED rather
//   than refused. That is how revokeAuthSession came to be called with seven
//   arguments against a two-argument declaration for its whole life.
//
// The tables below mirror the production call sites in store.go. A new call
// site that is not here is not covered -- which is the same contract
// component/grpc/render_query_args_parse_test.go states for its handlers.

// realEngine loads the embedded DSL tree and initialises the engine with no
// database, which is all that is needed to parse and resolve.
func realEngine(t *testing.T) *memqlengine.MemQLEngine {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (the dsl/ tree): %v", err)
	}
	registry := concept.DefaultRegistry()
	if registry == nil {
		t.Fatal("no concept registry")
	}
	eng, err := memqlengine.New(nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine init: %v", err)
	}
	return eng
}

// callSite is one production statement, with the argument map that builds it.
type callSite struct {
	fn   string
	args map[string]any
}

// productionCallSites mirrors store.go. The values are shaped like the real
// ones -- an RFC3339 timestamp, a detail object, a message carrying quotes and
// a newline -- because the renderer's job is to survive them.
func productionCallSites() []callSite {
	// A refusal message really does reach the row's `error` field via
	// describeRefusal, and it carries GitHub's own message -- remote text this
	// package does not author.
	//
	// THE FOUR CONTROL BYTES ARE THE POINT, and everything else here is
	// scenery. langparser.QuoteString and strconv.Quote agree on quotes,
	// backslashes, newlines, tabs, non-ASCII and base64 -- so a fixture built
	// from the values you would naturally think of passes under BOTH, and a
	// renderer refactored to %q looks fine. They diverge on exactly four:
	// NUL, BEL (\a), VT (\v) and DEL. %q renders those as \x00 / \a / \v /
	// \x7f, and the MemQL lexer rejects every one with "invalid escape
	// character"; QuoteString emits the \u form the lexer decodes.
	//
	// So this fixture is what makes the quoting choice CHECKED rather than
	// merely correct today.
	awkward := "tag_created_release_failed: GitHub said \"Reference already exists\" \\ and then\n" +
		"stopped\x00 \a \v \x7f"
	return []callSite{
		// Store.WriteCut -- the happy path, every field populated.
		{fn: "createReleaseCut", args: map[string]any{
			"version":          "v1.2.3",
			"bump":             "patch",
			"baseSha":          "abcdef1234567890",
			"requestedBy":      "v1:identity:user:someone",
			"requestedByEmail": "op@example.test",
			"status":           "dispatched",
			"releaseUrl":       "https://github.test/acme/widget/releases/tag/v1.2.3",
			"tagName":          "v1.2.3",
			"error":            "",
			"pinBumpPrUrl":     "https://github.test/acme/widget/pull/42",
			"pinBumpNote":      "",
			"dispatchedAt":     "2026-08-24T00:00:00Z",
		}},
		// Store.WriteCut -- the half-done path, which is the one that
		// carries an error string.
		{fn: "createReleaseCut", args: map[string]any{
			"version":     "v1.2.3",
			"bump":        "minor",
			"baseSha":     "abcdef1234567890",
			"requestedBy": "v1:identity:user:someone",
			"status":      "tag_created_release_failed",
			"tagName":     "v1.2.3",
			"error":       awkward,
		}},
		// Store.UpdateStatus.
		{fn: "updateReleaseCutStatus", args: map[string]any{
			"version":   "v1.2.3",
			"status":    "images_available",
			"error":     "",
			"checkedAt": "2026-08-24T00:05:00Z",
		}},
		// Store.WriteAudit. The detail object is the only nested value
		// this package renders.
		{fn: "createAuditEvent", args: map[string]any{
			"eventId":     "v1:identity:auditEvent:abc123",
			"occurredAt":  "2026-08-24T00:00:00Z",
			"category":    "admin",
			"action":      "release_cut",
			"actorUserId": "v1:identity:user:someone",
			"actorEmail":  "op@example.test",
			"actorRole":   "owner",
			"targetType":  "releaseCut",
			"targetId":    "v1.2.3",
			"detail": map[string]any{
				"version": "v1.2.3",
				"bump":    "patch",
				"baseSha": "abcdef1234567890",
				"status":  "dispatched",
				"error":   awkward,
			},
			"outcome": "success",
		}},
	}
}

func TestEveryRenderedStatementParsesAndResolves(t *testing.T) {
	eng := realEngine(t)
	sites := productionCallSites()
	for _, site := range sites {
		call := "mutation " + renderCall(site.fn, site.args)
		if _, err := eng.Parse(call); err != nil {
			t.Errorf("%s does not parse through the real front end: %v\n  %s", site.fn, err, call)
		}
	}
	// The READ this package composes. It is built by renderCall like the
	// writes, so it goes through the same two levels.
	read := "query " + renderCall("releaseCutByVersion", map[string]any{"version": "v1.2.3"})
	if _, err := eng.Parse(read); err != nil {
		t.Errorf("the releaseCutByVersion read does not parse: %v\n  %s", err, read)
	}
	if fn, err := eng.Functions().Get("releaseCutByVersion"); err != nil || fn == nil {
		t.Errorf("releaseCutByVersion is not in the function registry: %v", err)
	}
	if len(sites) == 0 {
		t.Fatal("no call sites; this test would pass vacuously")
	}
}

func TestEveryRenderedArgumentIsDeclared(t *testing.T) {
	eng := realEngine(t)
	checked := 0
	for _, site := range productionCallSites() {
		fn, err := eng.Functions().Get(site.fn)
		if err != nil || fn == nil {
			t.Errorf("%s is not in the function registry: %v. A call to a function no .memql "+
				"file declares fails at EXECUTE, which no fake-engine test can see.", site.fn, err)
			continue
		}
		declared := map[string]bool{}
		if fn.ArgsSchema != nil {
			for _, f := range fn.ArgsSchema.Fields {
				declared[f.Name] = true
			}
		}
		if len(declared) == 0 {
			t.Errorf("%s declares no args block, but the call site passes %d argument(s)",
				site.fn, len(site.args))
			continue
		}
		checked++
		for name, value := range site.args {
			// A blank string is dropped by renderCall before it
			// reaches the wire, so it is not an argument at all.
			if s, ok := value.(string); ok && s == "" {
				continue
			}
			if declared[name] {
				continue
			}
			t.Errorf("%s: this package passes %q, which the mutation does not declare. "+
				"It is NOT refused -- validateFunctionArgs iterates declared fields and "+
				"rejectUnknownArgs is gated behind the MCP boundary -- so the value is "+
				"silently discarded and the row never receives it (memql#3626, memql#4258).",
				site.fn, name)
		}
	}
	if want := len(productionCallSites()); checked != want {
		t.Fatalf("checked %d call sites, want %d", checked, want)
	}
}

// TestTheParseGateCanActuallyFail is the reachable positive control.
//
// Both tests above assert that nothing is wrong, and an assertion of that
// shape is worth exactly as much as the instrument behind it. If renderCall
// emitted the retired `name({k: v})` object-literal wrapper -- the shape that
// broke five guest mutations in memql#4258 -- would this notice? It has to be
// demonstrated, not assumed.
func TestTheParseGateCanActuallyFail(t *testing.T) {
	eng := realEngine(t)
	// The retired wrapper the parser has rejected since memql#2335.
	if _, err := eng.Parse(`mutation createReleaseCut({version: "v1.2.3"})`); err == nil {
		t.Fatal("the front end ACCEPTED the retired object-literal call form, so " +
			"TestEveryRenderedStatementParsesAndResolves proves nothing about call shape")
	}
	// And a function nothing declares must fail to resolve, or the
	// registry half of the gate is equally hollow.
	if fn, err := eng.Functions().Get("createReleaseCutThatDoesNotExist"); err == nil && fn != nil {
		t.Fatal("the registry resolved a function nothing declares, so " +
			"TestEveryRenderedArgumentIsDeclared proves nothing about resolution")
	}
}

// TestTheResultNodeSurvivesTheWIREEncoding closes the last gap on the data
// path: what the portal actually receives.
//
// A capability returns ONE synthetic MemoryNode whose Payload is JSON. The
// engine encodes that as a protobuf Struct, and the TS SDK's flattenNode
// spreads its entries onto the row -- so every field the Releases card reads
// (`version`, `status`, `checkError`, and the nested `images` LIST OF OBJECTS)
// has to survive protobuf's value model, which has no arrays of arbitrary Go
// types, only ListValue of Value.
//
// The portal tests cannot see this: they hand the card a plain object and
// never encode anything. That is the same shape as a page being fully green
// against jsdom and rendering blank in a browser -- the assertion is about the
// fake, and the encoding is where the real answer lives.
//
// `images` is the field at risk. A flat payload (the dataOrigins precedent)
// proves nothing about a nested list.
func TestTheResultNodeSurvivesTheWIREEncoding(t *testing.T) {
	out := StatusOutcome{
		Version: "v1.2.3", BareVersion: "1.2.3", Status: "dispatched",
		Age: "4 minutes ago", CheckError: "", Repository: "acme/widget",
		Images: []ImageDetail{
			{Repository: "acme/widget-identity", Present: true},
			{Repository: "acme/widget-agent", Present: false},
		},
	}
	nodes, err := resultNode("status", out.Version, out)
	if err != nil {
		t.Fatalf("resultNode: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("a capability returns ONE node; got %d", len(nodes))
	}

	// The engine's own encoding step: payload JSON -> protobuf Struct.
	var decoded map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &decoded); err != nil {
		t.Fatalf("the payload is not JSON: %v", err)
	}
	wire, err := structpb.NewStruct(decoded)
	if err != nil {
		t.Fatalf("the payload does not encode as a protobuf Struct, so it cannot reach a "+
			"client at all: %v", err)
	}
	row := wire.AsMap()

	// The scalar fields the card reads.
	for key, want := range map[string]string{
		"version": "v1.2.3", "bareVersion": "1.2.3", "status": "dispatched",
		"age": "4 minutes ago", "repository": "acme/widget",
	} {
		if got, _ := row[key].(string); got != want {
			t.Errorf("row[%q] = %v, want %q -- the card reads this key verbatim", key, row[key], want)
		}
	}

	// THE ONE AT RISK. flattenNode spreads payload entries onto the row, so
	// the card indexes row["images"] and expects a list of objects.
	images, ok := row["images"].([]any)
	if !ok {
		t.Fatalf("row[\"images\"] came back as %T, not a list. The card renders 'Not published yet: "+
			"...' from it, so this is the difference between naming the missing builds and "+
			"rendering nothing.", row["images"])
	}
	if len(images) != 2 {
		t.Fatalf("images has %d entries, want 2", len(images))
	}
	first, ok := images[0].(map[string]any)
	if !ok {
		t.Fatalf("images[0] is %T, not an object", images[0])
	}
	if repo, _ := first["repository"].(string); repo != "acme/widget-identity" {
		t.Errorf("images[0].repository = %v, want acme/widget-identity", first["repository"])
	}
	if present, _ := first["present"].(bool); !present {
		t.Error("images[0].present did not survive as a BOOLEAN; the card filters on !present, " +
			"so a string \"true\" here would report a published image as missing")
	}
	// And the false one, because a bool that degrades to a truthy value
	// would make every image look published.
	second, _ := images[1].(map[string]any)
	if present, isBool := second["present"].(bool); !isBool || present {
		t.Errorf("images[1].present = %v (%T), want the boolean false", second["present"], second["present"])
	}

	// checkError is OMITTED when empty (`omitempty`), which the card reads as
	// "the check did not error". Asserted so the tag is not dropped as tidying.
	if _, present := row["checkError"]; present {
		t.Error("checkError is present on a clean check; the card branches on it being empty, " +
			"and an empty-string key is fine -- but a NON-empty one here would render " +
			"'the check could not tell' over a perfectly good result")
	}
}

// TestQuoteStringHandlesTheBytesGoQuotingBreaksOn pins the quoting choice
// against the four bytes that distinguish it.
//
// This package renders every call-string value through
// langparser.QuoteString. That is not a style preference: %q emits Go escapes
// the MemQL lexer rejects, so ONE control byte turns a statement into a parse
// failure at execute time -- with the tests green, because a recording fake
// never parses. A neighbouring session shipped exactly that and caught it
// only by testing the two functions against each other.
//
// The reachable path here is `error`, which carries GitHub's own message
// through describeRefusal -- remote text this package does not author.
func TestQuoteStringHandlesTheBytesGoQuotingBreaksOn(t *testing.T) {
	eng := realEngine(t)
	// The four, individually, so a failure names which one.
	for _, tc := range []struct{ name, value string }{
		{"NUL", "a\x00b"},
		{"BEL", "a\ab"},
		{"VT", "a\vb"},
		{"DEL", "a\x7fb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := "mutation " + renderCall("updateReleaseCutStatus", map[string]any{
				"version": "v1.2.3", "status": "failed", "error": tc.value,
			})
			if _, err := eng.Parse(call); err != nil {
				t.Fatalf("a %s in the error message made the statement unparseable: %v\n  %s",
					tc.name, err, call)
			}

			// THE CONTROL, and it has to be here or the case above is
			// compatible with a lexer that accepts everything. Go's own
			// quoting of the same byte must NOT parse -- that divergence
			// is the whole reason renderValue calls QuoteString.
			goQuoted := `mutation updateReleaseCutStatus(version:"v1.2.3",status:"failed",error:` +
				strconv.Quote(tc.value) + `)`
			if _, err := eng.Parse(goQuoted); err == nil {
				t.Fatalf("the lexer ACCEPTED Go-quoted %s (%s), so this test would pass against "+
					"a renderer using %%q and proves nothing about the quoting choice",
					tc.name, strconv.Quote(tc.value))
			}
		})
	}
}
