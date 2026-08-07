// Differential tests for the paths-filter matcher (znasllc-io/memql#3222,
// memql#3223).
//
// # Provenance of the expectations
//
// Every `want` in this file was produced by running REAL picomatch 2.3.1 --
// the exact version `dorny/paths-filter@7b450fff` pins in its package-lock --
// under the action's own options:
//
//	const pm = require('picomatch');
//	const m = pm(pattern, {dot: true});
//	m(path);
//
// The full run was exhaustive rather than sampled: all 61 patterns then
// declared in `ci.yml` crossed with all 3085 tracked files -- 188,185 pairs --
// with CompilePattern required to agree on every one. It did, but only after
// the run caught two defects that reading the code had not:
//
//   - a trailing `/**` emitted its separator twice (`gen/(?:/.*)?`), so
//     `component/grpc/gen/**` matched NOTHING; and
//   - a segment starting with `*` dropped its wildcard, so `*.svg` matched
//     only the literal `.svg`.
//
// Both are represented below as named regression cases. That is the argument
// for keeping a differential oracle rather than hand-reasoning about globs:
// each bug produced a matcher that was quietly, plausibly wrong in the
// silent-green direction.
//
// The oracle is deliberately NOT wired into `go test` -- that would put node
// and an npm dependency on the critical path of a Go guard. Re-run it by hand
// when extending the grammar.
package ci

import "testing"

func TestCompilePatternMatchesPicomatch(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		// --- literal ---
		{"literal exact", "VERSION", "VERSION", true},
		{"literal is not a prefix", "VERSION", "VERSION.md", false},
		{"literal does not match a deeper copy", "go.mod", "component/memql/go.mod", false},
		{"nested literal path", "scripts/dev/proto-gen.sh", "scripts/dev/proto-gen.sh", true},

		// --- trailing globstar ---
		// REGRESSION: this matched nothing while the separator was emitted twice.
		{"trailing globstar matches a child", "component/grpc/gen/**", "component/grpc/gen/memql.pb.go", true},
		{"trailing globstar matches a deep child", "scripts/**", "scripts/ci/retry.sh", true},
		{"trailing globstar matches the directory itself", "scripts/**", "scripts", true},
		{"trailing globstar does not escape its prefix", "scripts/**", "sdk/ts/index.ts", false},
		{"trailing globstar is not a prefix match", "core/**", "corex/a.go", false},

		// --- leading globstar ---
		{"leading globstar matches at root", "**/*.go", "main.go", true},
		{"leading globstar matches nested", "**/*.go", "app/adapters.go", true},
		{"leading globstar matches deeply nested", "**/*.go", "component/memql/sense/hover.go", true},
		{"leading globstar respects the extension", "**/*.go", "docs/README.md", false},
		{"globstar literal basename at root", "**/go.mod", "go.mod", true},
		{"globstar literal basename nested", "**/go.mod", "component/memql/go.mod", true},

		// --- single star is within-segment only ---
		// REGRESSION: this matched only the literal ".svg" while the leading
		// wildcard was being dropped.
		{"star matches within a segment", "component/mcp/*.svg", "component/mcp/icon.svg", true},
		{"star does not cross a separator", "component/mcp/*.svg", "component/mcp/nested/icon.svg", false},
		{"star does not match an empty basename", "integrations/*.json", "integrations/sub/a.json", false},
		{"star as a whole segment", "examples/*/dsl/**", "examples/deploypack/dsl/a/b.memql", true},
		{"star as a whole segment spans exactly one", "examples/*/dsl/**", "examples/a/b/dsl/c.memql", false},

		// --- dot: true ---
		{"dotfile matches a star", "**/*.yml", ".github/dependabot.yml", true},
		{"dot directory matches a globstar", "**/*.yml", ".github/ISSUE_TEMPLATE/config.yml", true},

		// --- negation: the memql#3222 semantics ---
		// A `!` pattern is its own matcher meaning "anything that is NOT this".
		// Under the action's `.some()` combination that ADDS matches.
		{"negation excludes its own subtree", "!component/grpc/gen/**", "component/grpc/gen/memql.pb.go", false},
		{"negation admits a sibling", "!component/grpc/gen/**", "component/grpc/server.go", true},
		{"negation admits an unrelated file", "!component/grpc/gen/**", "VERSION", true},
		{"negation admits a doc", "!component/grpc/gen/**", "docs/README.md", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := CompilePattern(tc.pattern)
			if err != nil {
				t.Fatalf("CompilePattern(%q): %v", tc.pattern, err)
			}
			if got := m(tc.path); got != tc.want {
				t.Errorf("CompilePattern(%q)(%q) = %t, picomatch 2.3.1 says %t",
					tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

// The subset is only trustworthy because it refuses what it does not
// implement. A pattern scored by guesswork is worse than one that fails the
// build.
func TestCompilePatternRefusesUnsupportedGrammar(t *testing.T) {
	for _, pattern := range []string{
		"src/{a,b}/**",   // braces
		"src/file?.go",   // single-char wildcard
		"src/[abc].go",   // character class
		"src/+(a|b).go",  // extglob
		"src/@(a).go",    // extglob
		`src/a\*b.go`,    // escape
		"a/!(b)/**",      // negation away from position 0
		"component/a**b", // globstar fused into a segment
	} {
		if _, err := CompilePattern(pattern); err == nil {
			t.Errorf("CompilePattern(%q) succeeded; it must refuse grammar it has not been "+
				"differentially tested against rather than score it wrong", pattern)
		}
	}
}

// `.some()`, not `.every()` -- ci.yml sets no predicate-quantifier.
func TestBucketCombinesPatternsWithOr(t *testing.T) {
	b, err := CompileBucket("proto", []string{"**/*.proto", "component/grpc/gen/**"})
	if err != nil {
		t.Fatalf("CompileBucket: %v", err)
	}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"component/grpc/memql.proto", true},     // first pattern only
		{"component/grpc/gen/memql.pb.go", true}, // second pattern only
		{"docs/README.md", false},                // neither
	} {
		if got := b.Match(tc.path); got != tc.want {
			t.Errorf("bucket.Match(%q) = %t, want %t", tc.path, got, tc.want)
		}
	}
}
