package release

import (
	"io"
	"log/slog"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
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
	// A refusal message really does reach the row's `error` field, and a
	// GitHub error message really can carry quotes. If the renderer got
	// quoting wrong, this is the value that would prove it.
	awkward := `tag_created_release_failed: GitHub said "Reference already exists" \ and then
stopped`
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
