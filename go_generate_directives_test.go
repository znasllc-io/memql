// Static guard: every `//go:generate` directive names inputs that exist
// (znasllc-io/memql#3215).
//
// # The failure mode
//
// `component/server/doc.go` carried two directives whose every referenced path
// was absent from the tree -- `component/schemas/open_api.yaml`,
// `scripts/postprocess_server_gen/main.go`, and the `server.gen.go` they
// claimed to produce. `make generate` had been broken for some time and nobody
// ran it, because there is no `go generate` step in CI.
//
// The cost was not the broken target. It was that the directives asserted
// `component/server` is generated from an OpenAPI spec, sitting in the
// package's own `doc.go`, where a reader trusts it. The package is
// hand-written.
//
// Nothing verified the claim, so nothing could report it had stopped being
// true. This guard is what makes the next one fail loudly instead.
//
// # Why it refuses rather than guesses
//
// Deciding which tokens of an arbitrary command line are paths is not
// decidable in general: `-o server.gen.go` names an output that must NOT
// exist yet, `github.com/oapi-codegen/...@v2.5.1` looks like a path and is a
// module, and `sh -c "cd ../../.. && make x"` is an opaque shell payload.
//
// A guard that guesses wrong in the permissive direction reports a broken
// directive as clean, which is the defect it exists to catch. So this
// implements a deliberately small set of directive shapes and FAILS on
// anything outside it, asking the author to extend the guard. That is the
// same fail-closed posture as CompilePattern in scripts/ci/pathsfilter.go,
// for the same reason.
package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// generateDirective is one `//go:generate` line and where it was found.
type generateDirective struct {
	file string // repo-relative, slash-separated
	dir  string // absolute directory the directive resolves paths against
	line int
	text string // everything after the `//go:generate ` prefix
}

// isNestedCheckout reports whether dir is the root of a git checkout that is
// not this one -- a worktree (whose `.git` is a FILE) or a clone (whose `.git`
// is a DIRECTORY). Both carry a full second copy of the tree.
//
// A nested copy is not part of this repository (memql#3346). This repo's own
// .gitignore and CLAUDE.md put worktrees under `.claude/worktrees/`, so a
// developer following the documented layout has one, and every walk that does
// not skip it inspects that copy's files as though they were this checkout's.
// For the directive walks below the effect is only duplicate reporting; for
// TestEveryProtoPackageDelegatesToThePinnedGenerator it is a false failure,
// because a STALE worktree still holding a since-deleted proto dir would be
// reported against the current tree, and the suggested remediation would be to
// edit a directory the current tree does not contain.
//
// CI never sees any of it: a CI checkout has no nested worktrees.
func isNestedCheckout(root, dir string) bool {
	if dir == root {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// collectGenerateDirectives finds every `//go:generate` line in the tree.
func collectGenerateDirectives(t *testing.T, root string) []generateDirective {
	t.Helper()

	var out []generateDirective
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return fs.SkipDir
			}
			if isNestedCheckout(root, path) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		for i, line := range strings.Split(string(raw), "\n") {
			// Must be column 0 to be a directive; an indented one is a comment.
			if !strings.HasPrefix(line, "//go:generate ") {
				continue
			}
			out = append(out, generateDirective{
				file: filepath.ToSlash(rel),
				dir:  filepath.Dir(path),
				line: i + 1,
				text: strings.TrimSpace(strings.TrimPrefix(line, "//go:generate ")),
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository: %v", err)
	}
	return out
}

// splitArgs tokenizes on whitespace, keeping a double-quoted run as one token.
// Enough for the shapes below; anything needing more is refused upstream.
func splitArgs(s string) []string {
	var (
		args []string
		cur  strings.Builder
		inQ  bool
	)
	flush := func() {
		if cur.Len() > 0 {
			args = append(args, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
		case (r == ' ' || r == '\t') && !inQ:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return args
}

// isRemoteModule reports whether a `go run` target is a module path rather
// than a path on disk: `example.com/x@v1` or `example.com/x`.
//
// The leading-relative check comes first and is not merely a shortcut. Go
// spells a local target `./x` or `../x`, and those first segments contain a
// dot -- so the dotted-first-segment heuristic alone classifies every relative
// path as remote and silently checks nothing. That is exactly the permissive
// misfire this guard must not make.
func isRemoteModule(arg string) bool {
	if strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../") || strings.HasPrefix(arg, "/") {
		return false
	}
	if strings.Contains(arg, "@") {
		return true
	}
	// A module path's first segment is a domain, which must contain a dot.
	first, _, _ := strings.Cut(arg, "/")
	return strings.Contains(first, ".")
}

// requiredInputs returns the paths a directive requires to already exist,
// relative to the directive's own directory. The bool is false when the
// directive's shape is not one this guard understands.
func requiredInputs(args []string) (paths []string, understood bool) {
	if len(args) == 0 {
		return nil, false
	}
	switch args[0] {
	case "sh", "bash":
		// `sh -c "<script>"` is an opaque payload: its contents are a shell
		// program, not a path list, and this guard does not parse shell.
		// Accepted with no claims, but only in exactly that form.
		return nil, len(args) >= 3 && args[1] == "-c"

	case "protoc":
		// No directive uses this shape any more -- they all delegate to
		// `make proto-gen` (memql#3251). The branch stays so that a
		// reintroduced one is understood here and fails in
		// TestGoGenerateDirectivesDoNotInvokeBareProtoc with the reason it is
		// wrong, rather than failing here with "extend requiredInputs", which
		// is the one piece of advice that would be actively misleading.
		for _, a := range args[1:] {
			switch {
			case strings.HasPrefix(a, "--proto_path="):
				paths = append(paths, strings.TrimPrefix(a, "--proto_path="))
			case strings.HasSuffix(a, ".proto"):
				paths = append(paths, a)
			case strings.HasPrefix(a, "-"):
				// Output dirs (`--go_out=gen`) and codegen options
				// (`--go_opt=paths=source_relative`) make no input claim.
			default:
				return nil, false // an unrecognised positional
			}
		}
		return paths, true

	case "go":
		if len(args) < 3 || args[1] != "run" {
			return nil, false
		}
		if !isRemoteModule(args[2]) {
			paths = append(paths, args[2])
		}
		for _, a := range args[3:] {
			// A bare filename is ambiguous -- `-o server.gen.go` names an
			// output. Only a token carrying a separator is read as an input
			// path, which is the conservative direction.
			if !strings.HasPrefix(a, "-") && strings.Contains(a, "/") {
				paths = append(paths, a)
			}
		}
		return paths, true
	}
	return nil, false
}

// TestGoGenerateDirectivesReferenceExistingPaths is the guard.
func TestGoGenerateDirectivesReferenceExistingPaths(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	directives := collectGenerateDirectives(t, root)
	if len(directives) == 0 {
		t.Fatal("found no //go:generate directives anywhere -- either they were " +
			"all removed or this guard's discovery is broken. Failing closed " +
			"rather than reporting a vacuous pass")
	}

	for _, d := range directives {
		args := splitArgs(d.text)
		inputs, understood := requiredInputs(args)
		if !understood {
			t.Errorf("%s:%d: this guard does not understand the directive %q.\n"+
				"It refuses unfamiliar shapes rather than guessing which tokens "+
				"are paths, because guessing permissively would report a broken "+
				"directive as clean. Extend requiredInputs in %s to describe this "+
				"command, then this directive is checked like the others.",
				d.file, d.line, d.text, "go_generate_directives_test.go")
			continue
		}
		for _, p := range inputs {
			if _, err := os.Stat(filepath.Join(d.dir, filepath.FromSlash(p))); err != nil {
				t.Errorf("%s:%d: directive references %q, which does not exist "+
					"(resolved against %s).\n"+
					"`go generate ./...` fails on this, and there is no go generate "+
					"step in CI -- so the directive also asserts something false "+
					"about how this package is maintained, in the package's own "+
					"source, with nothing to report that it stopped being true.",
					d.file, d.line, p, filepath.ToSlash(filepath.Dir(d.file)))
			}
		}
	}
}

// bareProtocInvocation matches `protoc` used as a command word.
//
// It is applied to the directive's raw text rather than to splitArgs' first
// token on purpose: `sh -c "protoc ..."` is an opaque shell payload this file
// deliberately does not parse, and a first-token check would wave it straight
// through -- reopening the hole one level down. Scanning the text closes the
// `sh -c` route too.
//
// Requiring whitespace-or-end AFTER the word is what keeps the legitimate
// neighbours out: `protoc-gen-go` and `--proto_path=` do not match, and neither
// does `make proto-gen`, which has no `c` after `proto`.
var bareProtocInvocation = regexp.MustCompile(`(^|[\s;&|"'(])protoc(\s|$)`)

// TestGoGenerateDirectivesDoNotInvokeBareProtoc forbids the second generation
// path (znasllc-io/memql#3251).
//
// # What went wrong
//
// `scripts/dev/proto-gen.sh` pins protoc -- PROTOC_VERSION, provisioned into
// bin/tools/ and used even when a system protoc exists (memql#2774). The
// //go:generate directives in component/{grpc,node,bus} did not go through it;
// they ran whatever `protoc` was on PATH. `make generate` and `make proto-gen`
// therefore generated the same nine files by two different routes, and on a
// machine whose system protoc differed the former rewrote the
// `// protoc vX.Y.Z` stamp in every one of them. A contributor who ran the
// obvious-sounding target got a nine-file diff they had not authored.
//
// # Why this is the assertion worth making
//
// Neither target was wrong in isolation. The defect was that there were two and
// nothing named which was authoritative -- and the unpinned one was the one
// with the friendlier name. Pointing the directives at `make proto-gen` fixes
// today's tree; this test is what makes the property hold tomorrow, because a
// bare `protoc` directive is an easy and natural thing to reach for and its
// damage is invisible until someone with a different protoc runs it.
//
// The converse -- that every proto package HAS a delegating directive -- is
// TestEveryProtoPackageDelegatesToThePinnedGenerator below. It was left unwritten
// when this guard landed because the tree then held an orphaned
// component/polyphon/proto alongside the vendored google stubs, so asserting it
// needed a hand-maintained exclusion list: the drift-prone parallel list
// memql#3251 had just removed from proto-gen.sh, re-added in Go. Deleting the
// orphan (memql#3289) left only the google stubs, which are excluded by a
// structural predicate rather than by name.
func TestGoGenerateDirectivesDoNotInvokeBareProtoc(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	directives := collectGenerateDirectives(t, root)
	if len(directives) == 0 {
		t.Fatal("found no //go:generate directives anywhere -- either they were " +
			"all removed or this guard's discovery is broken. Failing closed " +
			"rather than reporting a vacuous pass")
	}

	for _, d := range directives {
		if !bareProtocInvocation.MatchString(d.text) {
			continue
		}
		t.Errorf("%s:%d: directive invokes protoc directly: %q.\n"+
			"protoc is pinned in scripts/dev/proto-gen.sh (PROTOC_VERSION), which "+
			"prefers its own provisioned copy over whatever is on PATH. A directive "+
			"that calls protoc itself bypasses that pin, so `make generate` "+
			"regenerates with the local protoc and rewrites the `// protoc vX.Y.Z` "+
			"stamp in every generated file -- a diff the author did not write and "+
			"cannot tell apart from a real regeneration at a glance (memql#3251, "+
			"memql#2774).\n"+
			"Delegate to the pinned path instead, scoped to this package so "+
			"`go generate` over the tree does not regenerate every proto dir once "+
			"per proto package:\n"+
			"\t//go:generate sh -c \"cd ../.. && make proto-gen PROTO_GEN_ONLY=<this dir>\"",
			d.file, d.line, d.text)
	}
}

// pinnedGeneratorDelegation matches a directive that routes generation through
// `make proto-gen`, the single pinned path (memql#3251). It is the positive
// counterpart to bareProtocInvocation: that one names the route a directive
// must not take, this one names the route it must.
var pinnedGeneratorDelegation = regexp.MustCompile(`\bproto-gen\b`)

// isVendoredProtoStub reports whether a directory of `.proto` files is a
// vendored copy of a third-party import rather than a generation target of
// this repository.
//
// The predicate is the proto IMPORT PATH, not a list of directories. A source
// that writes `import "google/protobuf/timestamp.proto"` has protoc resolve
// that exact string against --proto_path, so a vendored copy is only findable
// at `<proto_path>/google/protobuf/timestamp.proto`. The `google/` segment is
// forced by the import statement, which is what makes keying on it structural
// rather than a naming convention someone could quietly break. protoc-gen-go
// maps those files to the well-known types already compiled into
// google.golang.org/protobuf, so nothing in this repository is generated from
// them and a //go:generate directive over them would be a lie.
//
// A hand-maintained exclusion list is what this exists to avoid: the guard
// below is only worth having if adding a proto dir cannot also add a line to
// the guard that waves it through.
func isVendoredProtoStub(dir string) bool {
	for _, seg := range strings.Split(dir, "/") {
		if seg == "google" {
			return true
		}
	}
	return false
}

// protoDirs returns every directory holding a `.proto`, repo-relative and
// slash-separated, mapped to one example file in it for the failure message.
func protoDirs(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return fs.SkipDir
			}
			if isNestedCheckout(root, path) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".proto") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		dir := filepath.ToSlash(filepath.Dir(rel))
		if _, seen := out[dir]; !seen {
			out[dir] = rel
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository: %v", err)
	}
	return out
}

// TestEveryProtoPackageDelegatesToThePinnedGenerator is the converse of
// TestGoGenerateDirectivesDoNotInvokeBareProtoc (znasllc-io/memql#3289).
//
// # What it catches
//
// A `.proto` that generates nothing. `component/polyphon/proto/polyphon.proto`
// was one for the whole life of this repository: two gRPC services and
// seventeen messages describing a memQL-to-Bridge-Agent contract, no `.pb.go`
// ever produced from it in any commit, no Go symbol naming any of its types.
// It still matched the `proto` bucket in ci.yml, so editing it scheduled the
// drift gate over three trees it could not affect -- and, worse, it read as a
// wire contract. A `.proto` in a directory called `proto/` is the strongest
// available signal that a file is load-bearing. Someone changing Polyphon
// behaviour there got a green CI and no effect.
//
// That is the same shape as the defect the sibling guard above exists for
// (memql#3251: two generation paths, nothing naming which was authoritative)
// and as memql#3215 (directives whose every referenced path was absent): an
// artifact asserting something about how a package is maintained, with nothing
// able to report that it stopped being true.
//
// # Why this can be asserted now and could not be before
//
// The obstacle was never the property, it was the exclusion list. Deciding
// which proto dirs are generation targets meant enumerating the ones that are
// not -- and a guard carrying a hand-maintained list of things it does not
// check is one edit away from being talked out of checking anything.
//
// With the orphan deleted, every remaining non-target is a vendored google
// stub, excluded by isVendoredProtoStub on the import path protoc itself
// forces. Nothing here is a name somebody chose.
func TestEveryProtoPackageDelegatesToThePinnedGenerator(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	dirs := protoDirs(t, root)
	if len(dirs) == 0 {
		t.Fatal("found no .proto anywhere -- either the wire sources moved or " +
			"this guard's discovery is broken. Failing closed rather than " +
			"reporting a vacuous pass")
	}

	delegating := map[string]bool{}
	for _, d := range collectGenerateDirectives(t, root) {
		if pinnedGeneratorDelegation.MatchString(d.text) {
			delegating[filepath.ToSlash(filepath.Dir(d.file))] = true
		}
	}
	if len(delegating) == 0 {
		t.Fatal("no //go:generate directive anywhere delegates to `make " +
			"proto-gen` -- the pinned generation path has no entry point, or " +
			"this guard's discovery is broken. Failing closed")
	}

	sorted := make([]string, 0, len(dirs))
	for dir := range dirs {
		sorted = append(sorted, dir)
	}
	sort.Strings(sorted)

	for _, dir := range sorted {
		if isVendoredProtoStub(dir) || delegating[dir] {
			continue
		}
		t.Errorf("%s holds %q but no //go:generate directive that reaches the "+
			"pinned generator, so nothing in this repository is generated from "+
			"it.\n"+
			"A .proto reads as a wire contract -- that is the whole reason to "+
			"write one -- so an orphan is worse than a missing file: editing it "+
			"to change behaviour produces a green CI and no effect, and it "+
			"matches the `proto` bucket in ci.yml, scheduling the drift gate "+
			"over trees it cannot affect (memql#3289).\n"+
			"Resolve it in one of two directions:\n"+
			"\t- it should generate: add a generate.go in %s carrying\n"+
			"\t  //go:generate sh -c \"cd <repo root> && make proto-gen PROTO_GEN_ONLY=%s\",\n"+
			"\t  add %s to PROTO_TARGETS in scripts/dev/proto-gen.sh, and add its\n"+
			"\t  output tree to the `proto` bucket in .github/workflows/ci.yml;\n"+
			"\t- it should not: delete it.\n"+
			"Vendored third-party stubs are exempt, but only via the import path "+
			"protoc forces (a `google/` segment) -- this guard deliberately "+
			"carries no list of names to add yourself to.",
			dir, dirs[dir], dir, dir, dir)
	}
}
