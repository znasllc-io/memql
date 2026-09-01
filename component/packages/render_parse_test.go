package packages

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// render_parse_test.go -- every statement this package composes, run through
// the REAL MemQL front end (no database).
//
// It exists for the reason integrations/release/render_parse_test.go states at
// length and this package would have repeated: the rest of the suite drives a
// recording engine that parses NOTHING, so a call string the parser has never
// accepted stays green here forever and fails at parse on every real cluster.
// Five guest-invite mutations and the whole deploy-control surface shipped that
// way before anyone looked.
//
// This version is one step stronger than the precedent, and deliberately: it
// does not mirror the call sites in a table. It drives the REAL store methods
// against a recorder and parses whatever they actually produced -- so a
// renderer that changes cannot leave a stale fixture behind still passing.

// realEngine loads the embedded DSL tree and initialises an engine with no
// database, which is all parse + resolve needs.
//
// Built per test rather than once per package: a package-level fixture that
// mutates the concept registry leaks into every registry-walking test that
// runs after it.
func realEngine(t *testing.T) *memqlengine.MemQLEngine {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (the dsl/ tree): %v", err)
	}
	registry := concept.DefaultRegistry()
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

// awkwardText is what a build log or a GitHub message really looks like by the
// time it reaches a row -- remote text this package does not author.
//
// THE FOUR CONTROL BYTES ARE THE POINT and everything else is scenery.
// langparser.QuoteString and strconv.Quote agree on quotes, backslashes,
// newlines, tabs and non-ASCII, so a fixture built from the values you would
// naturally think of passes under BOTH and a renderer refactored to %q looks
// fine. They diverge on exactly four: NUL, BEL, VT and DEL, which %q renders
// as \x00 / \a / \v / \x7f and the MemQL lexer rejects with "invalid escape
// character". This is what makes the quoting choice CHECKED.
const awkwardText = "npm ERR! code ELIFECYCLE \"build\" \\ failed\nat line 2\x00 \a \v \x7f"

// captureStore drives every write and read the pipeline composes, and returns
// the statements they produced.
func captureStore(t *testing.T) []string {
	t.Helper()
	rec := &recordingEngine{}
	s := &store{engine: rec}
	ctx := context.Background()
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// Reads.
	_, _ = s.packageById(ctx, "v1:platform:package:abc")
	_, _ = s.deploymentById(ctx, "v1:platform:packageDeployment:def")
	_, _ = s.sitesForPackage(ctx, "v1:platform:package:abc")
	_, _ = s.siteById(ctx, "v1:platform:site:ghi")
	_, _ = s.packagesByRepoUrl(ctx, "https://github.com/acme/widget")
	_, _ = s.packagesTrackingRepos(ctx)

	// Writes.
	_ = s.openDeployment(ctx, deploymentSeed{
		DeploymentId:  "v1:platform:packageDeployment:def",
		PackageId:     "v1:platform:package:abc",
		OwnerUserId:   "v1:identity:user:someone",
		SourceVersion: "abcdef1234567890",
		RequestedBy:   "v1:identity:user:someone",
		StartedAt:     at,
	})
	for _, st := range stageOrder {
		_ = s.advance(ctx, "v1:platform:packageDeployment:def", st)
	}
	_ = s.recordReport(ctx, "v1:platform:packageDeployment:def", &Report{
		Name:          "acme",
		FormatVersion: 1,
		SourceVersion: "abcdef1234567890",
		Deployables: []DeployableReport{{
			Name: "storefront", Kind: KindStorefront, Path: "clients/web",
			BuildPlan: DefaultBuildCommand, Command: DefaultBuildCommand, Output: "dist",
			Binding: &ManifestBinding{StoreDomain: "acme.myshopify.com", StorefrontTokenRef: "acme-token"},
		}},
		DslDomains: []DslDomainReport{{Domain: "acme", Constructs: map[string]int{"concept": 3}, Files: 2}},
		GoPacks:    []GoPackReport{{Path: "bff", Module: "github.com/acme/bff", Note: awkwardText}},
		Problems:   []Problem{{Code: CodeGoPackNotDeployable, Message: awkwardText, Scope: "bff"}},
		OK:         true,
	}, "v1:library:artifact:snap")
	_ = s.closeDeployment(ctx, deploymentClose{
		DeploymentId: "v1:platform:packageDeployment:def",
		Status:       StatusFailed,
		Deployables: []DeployableOutcome{
			{Name: "storefront", SiteId: "v1:platform:site:ghi", Hostname: "shop.example.com", BundleRef: "blob://sites/ghi/v1/", Version: "v1", Created: true},
			{Name: "docs", Refusal: &Problem{Code: "deployable_build_failed", Message: awkwardText, Scope: "docs", Fatal: true}},
		},
		DslVersion:   "packages/acme/0123456789abcdef/",
		BuildLogTail: awkwardText,
		Error:        &Problem{Code: "deployable_build_failed", Message: awkwardText, Scope: "docs", Fatal: true},
		FinishedAt:   at,
	})
	_ = s.recordDeployedVersion(ctx, "v1:platform:package:abc", "abcdef1234567890", false)
	_ = s.recordPackageName(ctx, "v1:platform:package:abc", "acme")
	_ = s.recordUpstreamVersion(ctx, "v1:platform:package:abc", "fedcba0987654321", true)
	_ = s.bindSiteToPackage(ctx, "v1:platform:site:ghi", "v1:platform:package:abc", "storefront")
	_ = s.setPackageStatus(ctx, "v1:platform:package:abc", "archived")
	_ = s.setSiteStatus(ctx, "v1:platform:site:ghi", "archived")

	if len(rec.queries) == 0 {
		t.Fatal("no statements captured; this test would pass vacuously")
	}
	return rec.queries
}

func TestEveryRenderedStatementParsesAndResolves(t *testing.T) {
	eng := realEngine(t)
	for _, q := range captureStore(t) {
		if _, err := eng.Parse(q); err != nil {
			t.Errorf("does not parse through the real front end: %v\n  %s", err, q)
			continue
		}
		name := callName(q)
		if name == "" {
			t.Errorf("could not read a construct name out of: %s", q)
			continue
		}
		if fn, err := eng.Functions().Get(name); err != nil || fn == nil {
			t.Errorf("%s does not resolve in the function registry: %v", name, err)
		}
	}
}

// TestRenderedArgumentsAreDeclared is the half resolution does NOT cover.
// validateFunctionArgs iterates DECLARED fields and rejectUnknownArgs is gated
// behind the MCP boundary, so an argument this package invents is silently
// DROPPED rather than refused -- which is how revokeAuthSession came to be
// called with seven arguments against a two-argument declaration for its whole
// life (memql#4258).
func TestRenderedArgumentsAreDeclared(t *testing.T) {
	eng := realEngine(t)
	checked := 0
	for _, q := range captureStore(t) {
		name := callName(q)
		fn, err := eng.Functions().Get(name)
		if err != nil || fn == nil {
			continue // reported by the test above
		}
		passed := callArgNames(q)
		if len(passed) == 0 {
			// A no-argument call (packagesTrackingRepos) is a real call site
			// and its correctness is the parse+resolve test above, not this
			// one. Erroring here on the absent args block would be demanding a
			// declaration for arguments nobody sends.
			continue
		}
		declared := map[string]bool{}
		if fn.ArgsSchema != nil {
			for _, f := range fn.ArgsSchema.Fields {
				declared[f.Name] = true
			}
		}
		if len(declared) == 0 {
			t.Errorf("%s declares no args block, yet this package calls it with %d argument(s)", name, len(passed))
			continue
		}
		for _, arg := range passed {
			checked++
			if !declared[arg] {
				t.Errorf("%s is called with %q, which it does not declare. It is not refused -- "+
					"rejectUnknownArgs is gated behind the MCP boundary -- so the value is "+
					"silently discarded and the row never receives it (memql#3626, memql#4258).",
					name, arg)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no arguments checked; this test would pass vacuously")
	}
}

// callName reads the construct name out of "query foo(...)" / "mutation
// foo(...)".
func callName(q string) string {
	q = strings.TrimSpace(q)
	_, rest, ok := strings.Cut(q, " ")
	if !ok {
		return ""
	}
	name, _, ok := strings.Cut(strings.TrimSpace(rest), "(")
	if !ok {
		return ""
	}
	return strings.TrimSpace(name)
}

// callArgNames reads the argument NAMES out of a rendered call. Deliberately
// naive about values -- it only needs the identifiers before each top-level
// colon, and it skips anything inside a string, an object or a list.
func callArgNames(q string) []string {
	open := strings.IndexByte(q, '(')
	if open < 0 {
		return nil
	}
	body := q[open+1:]
	var (
		names   []string
		current strings.Builder
		depth   int
		inStr   bool
		esc     bool
	)
	for _, r := range body {
		switch {
		case esc:
			esc = false
		case inStr && r == '\\':
			esc = true
		case r == '"':
			inStr = !inStr
		case inStr:
			// skip
		case r == '{' || r == '[':
			depth++
		case r == '}' || r == ']':
			depth--
		case r == ')' && depth == 0:
			return names
		case r == ':' && depth == 0:
			if n := strings.TrimSpace(current.String()); n != "" {
				names = append(names, n)
			}
			current.Reset()
			// Everything up to the next top-level comma is the value.
			depth = -1
		case r == ',' && depth == -1:
			depth = 0
			current.Reset()
		case depth == 0:
			current.WriteRune(r)
		}
	}
	return names
}
