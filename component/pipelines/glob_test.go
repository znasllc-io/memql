package pipelines

import "testing"

// The cases scripts/ci/pathsfilter_test.go holds against real picomatch 2.3.1
// (dorny/paths-filter's matcher, {dot: true}), restated here because this
// matcher is a port of that one: a bucket moved from ci.yml into a manifest
// must mean what it meant. The root module's parity test (Task 3) runs both
// matchers over one shared list; this is the leaf's own floor.
func TestCompileGlobAgreesWithTheBridgesOracleCases(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{"literal exact", "VERSION", "VERSION", true},
		{"literal is not a prefix", "VERSION", "VERSION.md", false},
		{"literal does not match a deeper copy", "go.mod", "component/memql/go.mod", false},
		{"nested literal path", "scripts/dev/proto-gen.sh", "scripts/dev/proto-gen.sh", true},

		{"trailing globstar matches a child", "component/grpc/gen/**", "component/grpc/gen/memql.pb.go", true},
		{"trailing globstar matches a deep child", "scripts/**", "scripts/ci/retry.sh", true},
		{"trailing globstar matches the directory itself", "scripts/**", "scripts", true},
		{"trailing globstar does not escape its prefix", "scripts/**", "sdk/ts/index.ts", false},
		{"trailing globstar is not a prefix match", "core/**", "corex/a.go", false},

		{"leading globstar matches at root", "**/*.go", "main.go", true},
		{"leading globstar matches nested", "**/*.go", "app/adapters.go", true},
		{"leading globstar matches deeply nested", "**/*.go", "component/memql/sense/hover.go", true},
		{"leading globstar respects the extension", "**/*.go", "docs/README.md", false},
		{"globstar literal basename at root", "**/go.mod", "go.mod", true},
		{"globstar literal basename nested", "**/go.mod", "component/memql/go.mod", true},

		{"star matches within a segment", "component/mcp/*.svg", "component/mcp/icon.svg", true},
		{"star does not cross a separator", "component/mcp/*.svg", "component/mcp/nested/icon.svg", false},
		{"star does not match an empty basename", "integrations/*.json", "integrations/sub/a.json", false},
		{"star as a whole segment", "examples/*/dsl/**", "examples/deploypack/dsl/a/b.memql", true},
		{"star as a whole segment spans exactly one", "examples/*/dsl/**", "examples/a/b/dsl/c.memql", false},

		{"dotfile matches a star", "**/*.yml", ".github/dependabot.yml", true},
		{"dot directory matches a globstar", "**/*.yml", ".github/ISSUE_TEMPLATE/config.yml", true},

		{"negation excludes its own subtree", "!component/grpc/gen/**", "component/grpc/gen/memql.pb.go", false},
		{"negation admits a sibling", "!component/grpc/gen/**", "component/grpc/server.go", true},
		{"negation admits an unrelated file", "!component/grpc/gen/**", "VERSION", true},
		{"negation admits a doc", "!component/grpc/gen/**", "docs/README.md", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			match, err := CompileGlob(tc.pattern)
			if err != nil {
				t.Fatalf("CompileGlob(%q): %v", tc.pattern, err)
			}
			if got := match(tc.path); got != tc.want {
				t.Errorf("CompileGlob(%q)(%q) = %t, want %t", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

// The grammar the plan names: `**` crosses directories, `*` and `?` stay in
// one segment, a leading `!` negates, everything is anchored at the root.
func TestCompileGlobGrammar(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/*.md", "a.md", true},
		{"**/*.md", "x/y/a.md", true},
		{"**/*.md", "x/y/a.mdx", false},
		{"dsl/**", "dsl/a/b.memql", true},
		{"dsl/**", "dslx/a", false},
		{"dsl/**", "x/dsl/a", false},
		{"*.go", "a.go", true},
		{"*.go", "a/b.go", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/y/bc", false},
		{"**", "anything/at/all", true},

		// `?` is exactly one rune that is not a separator.
		{"file?.go", "file1.go", true},
		{"file?.go", "file\u00e9.go", true}, // one rune, two bytes
		{"file?.go", "file.go", false},
		{"file?.go", "file12.go", false},
		{"a?b", "a/b", false},
		{"?", ".", true},
		{"src/?/x", "src/a/x", true},
		{"src/?/x", "src/ab/x", false},

		{"!docs/**", "docs/a.md", false},
		{"!docs/**", "src/a.go", true},
		{"!*.md", "a.md", false},
		{"!*.md", "x/a.md", true},

		// Anchored at the repository root, never a substring search.
		{"clients/**", "x/clients/a.ts", false},
		{"a.go", "x/a.go", false},
	}
	for _, tc := range cases {
		match, err := CompileGlob(tc.pattern)
		if err != nil {
			t.Errorf("CompileGlob(%q): %v", tc.pattern, err)
			continue
		}
		if got := match(tc.path); got != tc.want {
			t.Errorf("CompileGlob(%q)(%q) = %t, want %t", tc.pattern, tc.path, got, tc.want)
		}
	}
}

// Fail-closed: a pattern outside the grammar is an error, never a guess that
// scores a path wrong.
func TestCompileGlobRefusesWhatItDoesNotImplement(t *testing.T) {
	for _, pattern := range []string{
		"",               // nothing to match
		"!",              // negates nothing
		"src/[abc].go",   // character class
		"src/a].go",      // stray class bracket
		"src/{a,b}/**",   // braces
		"src/+(a|b).go",  // extglob
		"src/@(a).go",    // extglob
		"src/(a).go",     // group
		`src/a\*b.go`,    // escape
		"a/!(b)/**",      // negation away from position 0
		"component/a**b", // globstar fused into a segment
		"a/***/b",        // globstar fused with a star
		"docs/\xff.md",   // invalid UTF-8, refused as the original refuses it
	} {
		if _, err := CompileGlob(pattern); err == nil {
			t.Errorf("CompileGlob(%q) succeeded; it must refuse grammar it does not implement", pattern)
		}
	}
}
