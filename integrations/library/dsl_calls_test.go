package library

// dsl_calls_test.go -- memql#4342.
//
// The stub in analysis_test.go records query strings and PARSES NOTHING.
// That is the failure mode memql#4256 and memql#4032 both name: a handler
// suite that never runs its statements through the real front end stays
// green while every one of them fails at parse in production. Five guest
// mutations shipped exactly that way.
//
// So this file takes the statements the three new capabilities ACTUALLY
// emit -- collected by driving the real code paths, never hand-copied --
// and runs each of them through the real engine with the whole embedded
// DSL loaded. Two levels, because they catch different things:
//
//	Parse    -- the text is well formed AND the construct name resolves.
//	Declared -- every argument name is a field the construct declares.
//
// The second is not implied by the first, and assuming it was would leave
// half the bug uncovered: validateFunctionArgs iterates DECLARED fields
// and rejectUnknownArgs is gated behind the MCP boundary, so an argument
// a Go call site invents is silently DISCARDED rather than refused. The
// row simply never receives it, and the call site keeps claiming it did.

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	conceptreg "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// collectEmittedCalls drives every new code path against the stub and
// returns every statement they handed the engine. Driving the real paths
// rather than listing statements by hand is the point: a call site added
// later is covered without anybody remembering to add it here.
func collectEmittedCalls(t *testing.T) []string {
	t.Helper()
	s := newLibStub()
	s.summaryText = "Two fish-eating birds and how they hunt."

	// 1. the analysis pass, all the way through.
	i := newAnalysisIntegration(s, birdsText())
	i.SetDomainWriteAuthorizer(&recordingAuthorizer{allow: true})
	s.seedFile("file-birds", "user-a", "birds.txt", "text/plain", "text")
	s.seedPromotedArtifact("file-birds", "user-a", []string{"nature"})
	analyzedFile(t, s, i, "file-birds", "user-a")

	// 2. the failure path, which renders setLibraryFileStatus differently
	//    (status + failureReason rather than status + summary + embedding).
	s.seedFile("file-locked", "user-a", "locked.pdf", "application/pdf", "pdf")
	s.seedPromotedArtifact("file-locked", "user-a", nil)
	failing := NewIntegration(s)
	failing.SetExtractor(fixedExtractor{text: ""})
	failing.SetArtifactPoll(1, 0)
	_ = failing.AnalyzeFile(context.Background(), AnalyzeFileParams{
		FileId: "file-locked", OwnerUserId: "user-a", Name: "locked.pdf", MimeType: "application/pdf",
	})

	// 3. search, both entry shapes.
	ctx := ownerContext("user-a")
	if _, err := i.handleSimilarArtifacts(ctx, map[string]any{"text": "kingfisher tunnels"}, 0); err != nil {
		t.Fatalf("collect: search by text: %v", err)
	}
	s.artifacts[libStubArtifactId(conceptFile+":file-birds")]["summary"] = "kingfishers and herons"
	if _, err := i.handleSimilarArtifacts(ctx, map[string]any{
		"artifactId": libStubArtifactId(conceptFile + ":file-birds"),
	}, 0); err != nil {
		t.Fatalf("collect: search by artifactId: %v", err)
	}

	// 4. train -- ingest, the trained-domain append and the audit event.
	if _, err := i.handleTrainFile(ctx, map[string]any{
		"fileId": "file-birds", "domainId": "domain-birds",
	}, 0); err != nil {
		t.Fatalf("collect: train: %v", err)
	}
	return s.calls
}

// callName pulls the construct name off a statement, stripping the
// `query ` / `mutation ` prefix the way the engine's own dispatch does.
func callName(stmt string) string {
	open := strings.IndexByte(stmt, '(')
	if open <= 0 {
		return ""
	}
	name := strings.TrimSpace(stmt[:open])
	if fields := strings.Fields(name); len(fields) > 0 {
		return fields[len(fields)-1]
	}
	return ""
}

// expectedCallNames is every construct the three capabilities reach. A
// statement naming something NOT in this set fails the test rather than
// being skipped: an unlisted name is either a typo or a new dependency
// that nobody reviewed.
var expectedCallNames = map[string]bool{
	"setLibraryFileStatus":              true,
	"createLibraryFileChunk":            true,
	"libraryEmbedChunk":                 true,
	"libraryFileById":                   true,
	"libraryFileChunksForFile":          true,
	"libraryArtifactById":               true,
	"libraryArtifactBySourceConceptRef": true,
	"appendLibraryFileTrainedDomain":    true,
	"createArtifact":                    true,
	"createAuditEvent":                  true,
	"similarTo":                         true,
	"knowledgeIngest":                   true,
}

// TestEmittedCallsCoverEveryConstruct is the guard on the guard: a
// collector that silently stopped driving one of the paths would make
// every test below vacuously green.
func TestEmittedCallsCoverEveryConstruct(t *testing.T) {
	seen := map[string]bool{}
	for _, stmt := range collectEmittedCalls(t) {
		seen[callName(stmt)] = true
	}
	for name := range expectedCallNames {
		if !seen[name] {
			t.Errorf("no statement naming %q was emitted; either the capability stopped calling "+
				"it or collectEmittedCalls stopped driving that path", name)
		}
	}
	for name := range seen {
		if !expectedCallNames[name] {
			t.Errorf("an unlisted construct %q was called. Add it to expectedCallNames if it is "+
				"deliberate -- an unreviewed dependency is exactly what this list exists to "+
				"surface.", name)
		}
	}
}

// TestEmittedCallsAreSyntacticallyValid -- syntax only, so it reports a
// malformed statement plainly rather than as a resolution failure.
func TestEmittedCallsAreSyntacticallyValid(t *testing.T) {
	for _, stmt := range collectEmittedCalls(t) {
		if _, err := langparser.ParseExpression(stmt); err != nil {
			t.Errorf("the parser refused a statement this integration emits:\n  %s\n  --> %v", stmt, err)
		}
	}
}

// newLibraryDSLEngine boots a real engine with the whole embedded DSL
// loaded and no database -- the parser and name resolution are live,
// which is all these two tests need.
func newLibraryDSLEngine(t *testing.T) *memqlengine.MemQLEngine {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := conceptreg.DefaultRegistry()
	if registry == nil {
		t.Fatalf("concept registry is nil")
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

// TestEmittedCallsResolve runs every statement through the whole front
// end, so the construct NAME is checked too. A capability calling a
// mutation that exists in no .memql file parses fine and fails at execute
// -- which is how the guest-invite writes were broken for their whole
// life (memql#4258).
func TestEmittedCallsResolve(t *testing.T) {
	eng := newLibraryDSLEngine(t)
	checked := 0
	for _, stmt := range collectEmittedCalls(t) {
		checked++
		if _, err := eng.Parse(stmt); err != nil {
			t.Errorf("the engine refused a statement this integration emits:\n  %s\n  --> %v", stmt, err)
		}
	}
	if checked == 0 {
		t.Fatalf("no statements were checked")
	}
}

// namedArgRe pulls the argument NAMES out of a named-argument call
// (`mutation x(a: 1, b: "two")`). Only top-level names matter, so a
// nested object literal's keys are dropped below.
var namedArgRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*:`)

// topLevelArgNames returns the argument names a named-argument statement
// passes, ignoring anything inside a nested `{ }` (createAuditEvent's
// `detail` object carries its own keys, which are payload, not arguments).
func topLevelArgNames(stmt string) []string {
	open := strings.IndexByte(stmt, '(')
	if open < 0 {
		return nil
	}
	body := stmt[open+1:]
	// Blank out nested braces so their keys cannot be mistaken for args.
	var b strings.Builder
	depth, inString, escaped := 0, false, false
	for _, r := range body {
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case r == '\\':
				escaped = true
			case r == '"':
				inString = false
			}
			b.WriteRune(' ')
			continue
		case r == '"':
			inString = true
			b.WriteRune(' ')
			continue
		case r == '{':
			depth++
			b.WriteRune(' ')
			continue
		case r == '}':
			depth--
			b.WriteRune(' ')
			continue
		}
		if depth > 0 {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	var names []string
	for _, m := range namedArgRe.FindAllStringSubmatch(b.String(), -1) {
		names = append(names, m[1])
	}
	return names
}

// TestEmittedArgumentsAreDeclared is memql#3626's other direction at the
// Go boundary: an argument name the caller invents is not refused, it is
// DROPPED -- so the call site believes it wrote a field and the row never
// receives it. Resolution cannot catch this; only the declaration can.
//
// Scope: the named-argument statements. The object-profile builtin calls
// (similarTo / knowledgeIngest / libraryEmbedChunk) carry a JSON body
// rather than named arguments, and their field lists are asserted against
// the DSL by TestObjectProfileBuiltinArgumentsAreDeclared below.
func TestEmittedArgumentsAreDeclared(t *testing.T) {
	eng := newLibraryDSLEngine(t)
	checked := 0
	for _, stmt := range collectEmittedCalls(t) {
		name := callName(stmt)
		if objectProfileCalls[name] {
			continue
		}
		fn, err := eng.Functions().Get(name)
		if err != nil || fn == nil {
			t.Errorf("%s: not in the function registry: %v", name, err)
			continue
		}
		declared := map[string]bool{}
		if fn.ArgsSchema != nil {
			for _, f := range fn.ArgsSchema.Fields {
				declared[f.Name] = true
			}
		}
		if len(declared) == 0 {
			t.Errorf("%s declares no args block, but the call site passes arguments:\n  %s", name, stmt)
			continue
		}
		checked++
		for _, arg := range topLevelArgNames(stmt) {
			if declared[arg] {
				continue
			}
			t.Errorf("%s: this integration passes %q, which the construct does not declare. "+
				"It is not refused -- rejectUnknownArgs is gated behind the MCP boundary -- so "+
				"the value is silently discarded and the row never receives it (memql#3626).\n  %s",
				name, arg, stmt)
		}
	}
	if checked == 0 {
		t.Fatalf("no named-argument call sites were checked")
	}
}

// objectProfileCalls names the builtins this integration calls with an
// @args(profile="object") JSON body rather than named arguments.
var objectProfileCalls = map[string]bool{
	"similarTo":         true,
	"knowledgeIngest":   true,
	"libraryEmbedChunk": true,
}

// TestObjectProfileBuiltinArgumentsAreDeclared covers the other three.
// Their bodies are JSON, so the check is on the KEYS: every key must be a
// field the builtin declares in dsl/. The same silent-discard hazard
// applies -- more so, because an object-profile builtin hands its whole
// map to a Go handler that reads the keys it knows and ignores the rest.
func TestObjectProfileBuiltinArgumentsAreDeclared(t *testing.T) {
	eng := newLibraryDSLEngine(t)
	seen := map[string]bool{}
	for _, stmt := range collectEmittedCalls(t) {
		name := callName(stmt)
		if !objectProfileCalls[name] {
			continue
		}
		seen[name] = true
		fn, err := eng.Functions().Get(name)
		if err != nil || fn == nil {
			t.Errorf("%s: not in the function registry: %v", name, err)
			continue
		}
		// A builtin's fields live on BuiltinArgs, not ArgsSchema -- the
		// body IS the schema for a builtin, and the loader lands it in the
		// arg CONTRACT. Reading ArgsSchema here would find nothing and
		// flag every argument, which is a test that fails for the wrong
		// reason rather than one that passes for the right one.
		declared := map[string]bool{}
		if fn.BuiltinArgs != nil {
			for prop := range fn.BuiltinArgs.Properties {
				declared[prop] = true
			}
		}
		if len(declared) == 0 {
			t.Errorf("%s: the builtin declares no fields in dsl/", name)
			continue
		}
		_, args := parseLibCall(stmt)
		if len(args) == 0 {
			t.Errorf("%s: the call body parsed to no arguments:\n  %s", name, stmt)
			continue
		}
		for key := range args {
			if declared[key] {
				continue
			}
			t.Errorf("%s: this integration passes %q, which the builtin does not declare in "+
				"dsl/. The handler reads the keys it knows and ignores the rest, so the value "+
				"is silently dropped.\n  %s", name, key, stmt)
		}
	}
	for name := range objectProfileCalls {
		if !seen[name] {
			t.Errorf("no call to the object-profile builtin %q was collected", name)
		}
	}
}
