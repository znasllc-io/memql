// Static guard: the release reaches the BINARY, and the stale-file stamp that
// used to stand in for it does not come back (znasllc-io/memql#3998).
//
// # The failure mode
//
// A memQL node could not state which release it was running. `ServerHello`
// carried the literal "v1" (the wire protocol version), and the version the
// engine reported everywhere else came from a checked-in `VERSION` file that
// had said `0.15.0` at every tag from v0.16.1 onward, which the image build
// then rewrote as `0.15.0-<epoch>` -- a form VERSIONING.md forbids outright.
//
// The cost was not "the version was missing". A missing version is honest and a
// client can say so. What shipped was release-SHAPED and wrong, freshly
// re-stamped on every build so it looked deliberate, and confidently comparable
// against a real release list. That is how an operator whose cluster is simply
// three releases behind their plugin gets `ERROR (validate): stream closed` and
// no hint at all -- the incident behind epic memql#3989.
//
// # Why a static guard and not a runtime one
//
// `-X` is silently ignored when its target does not resolve. Rename the
// package, move the module, misspell the variable, drop the flag in a
// "simplify the ldflags" cleanup -- the linker emits nothing, the build
// succeeds, and every release from then on reports `dev`. `dev` is SAFE (no
// client mistakes it for a release) which is exactly why nothing downstream
// would ever complain: the feature would quietly become inert and stay that
// way until someone noticed the upgrade banner never appears.
//
// So this composes the expected flag from the facts it must match -- the module
// path in `core/go.mod` and the actual `var release string` declaration -- and
// requires each Dockerfile's memql build line to carry precisely that. A test
// that merely grepped for "buildinfo" would pass on a typo, which is the one
// thing worth catching here.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// buildinfoPkgDir is the package whose link-time variable carries the release.
const (
	buildinfoModFile = "core/go.mod"
	buildinfoSrcFile = "core/buildinfo/buildinfo.go"
	buildinfoPkgPath = "buildinfo"
	buildinfoVarName = "release"
)

// releaseBuildArg is the docker build arg each Dockerfile threads into the
// linker flag, and the workflow supplies from the release it was dispatched
// with.
const releaseBuildArg = "MEMQL_RELEASE"

// dockerfilesThatBuildTheEngine maps a Dockerfile to the substring identifying
// the RUN line that links the memql binary. The healthcheck is built by a
// second `go build` in the same stage and deliberately takes no stamp -- it is
// a probe that never introduces itself to a client -- so the flag has to be
// asserted on the right line rather than anywhere in the file.
var dockerfilesThatBuildTheEngine = map[string]string{
	"Dockerfile":              "-o /app/bin/memql",
	"docker/memql.Dockerfile": "-o memql",
}

func TestEveryEngineImageLinksTheReleaseIntoTheBinary(t *testing.T) {
	wantFlag := fmt.Sprintf("-X %s/%s.%s=${%s}",
		modulePathOf(t, buildinfoModFile), buildinfoPkgPath, buildinfoVarName, releaseBuildArg)

	for path, buildMarker := range dockerfilesThatBuildTheEngine {
		t.Run(path, func(t *testing.T) {
			body := readRepoFile(t, path)

			if !declaresBuildArg(body, releaseBuildArg) {
				t.Errorf("%s does not declare `ARG %s`, so the release can never be passed in", path, releaseBuildArg)
			}

			line := lineContaining(body, buildMarker)
			if line == "" {
				t.Fatalf("%s has no line containing %q -- this guard's marker is stale, not the Dockerfile", path, buildMarker)
			}
			if !strings.Contains(line, wantFlag) {
				t.Errorf("%s links the engine binary without stamping the release.\n"+
					"  want the build line to contain: %s\n"+
					"  got line: %s\n"+
					"Without it every released node reports %q and no cluster can say which release it runs.",
					path, wantFlag, strings.TrimSpace(line), "dev")
			}
		})
	}
}

// TestTheReleaseStampTargetExists closes the other half of the silent -X
// no-op: the flag can be spelled perfectly and still hit nothing if the
// variable it names has been renamed or made a constant.
func TestTheReleaseStampTargetExists(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, buildinfoSrcFile, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", buildinfoSrcFile, err)
	}
	if got := file.Name.Name; got != buildinfoPkgPath {
		t.Fatalf("%s declares package %q, but the -X flag names %q", buildinfoSrcFile, got, buildinfoPkgPath)
	}

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range value.Names {
				if name.Name != buildinfoVarName {
					continue
				}
				// A string var with no initializer is the only shape the
				// linker can write to. `-X` refuses a non-string, and an
				// initialized var is a value the build silently keeps.
				if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != "string" {
					t.Fatalf("%s.%s must be a plain string var for -X to write it", buildinfoPkgPath, buildinfoVarName)
				}
				if len(value.Values) != 0 {
					t.Fatalf("%s.%s has an initializer; -X cannot stamp an initialized variable", buildinfoPkgPath, buildinfoVarName)
				}
				return
			}
		}
	}
	t.Fatalf("%s declares no package-level `var %s string` -- the -X target the image build names does not exist",
		buildinfoSrcFile, buildinfoVarName)
}

// TestTheReleaseBuildSuppliesTheRelease checks the one build that actually
// knows the release passes it. Everything else (a laptop `make dev`, a branch
// build) deliberately does not, and must keep not doing so.
func TestTheReleaseBuildSuppliesTheRelease(t *testing.T) {
	const workflow = ".github/workflows/build-engine-images.yml"
	body := readRepoFile(t, workflow)

	want := releaseBuildArg + "=${{ inputs.version }}"
	if !strings.Contains(body, want) {
		t.Errorf("%s does not pass %q as a build arg.\n"+
			"It is the only place that knows the release being cut; without this the images "+
			"carry the right tag and a binary that says %q.", workflow, want, "dev")
	}
}

// TestNoImageStampsAVersionFile is the regression half. The removed step read
// `VERSION`, cut everything after the first `-`, and wrote back
// `<prefix>-<epoch>`; the runtime stages then shipped that file. Nothing about
// it looked wrong in review, which is why it survived three releases, so the
// guard names the shape rather than trusting the next reader to recognise it.
func TestNoImageStampsAVersionFile(t *testing.T) {
	// A redirect into VERSION, however spelled: `> VERSION`, `>VERSION`,
	// `>> VERSION`, `> ./VERSION`.
	writesVersionFile := regexp.MustCompile(`>>?\s*\.?/?VERSION\b`)
	// A runtime stage taking the file along for the binary to read.
	copiesVersionFile := regexp.MustCompile(`COPY\s+--from=\S+\s+\S*VERSION\b`)

	for path := range dockerfilesThatBuildTheEngine {
		t.Run(path, func(t *testing.T) {
			for i, line := range strings.Split(readRepoFile(t, path), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue // the removed step is quoted in a comment on purpose
				}
				if writesVersionFile.MatchString(line) {
					t.Errorf("%s:%d writes the VERSION file: %s\n"+
						"The release is stamped into the binary by the linker (memql#3998); a file "+
						"rewritten at build time is how the engine came to report a version nobody set.",
						path, i+1, strings.TrimSpace(line))
				}
				if copiesVersionFile.MatchString(line) {
					t.Errorf("%s:%d copies a VERSION file into a runtime stage: %s\n"+
						"Nothing in the binary reads one; shipping it invites a future reader to wire it back up.",
						path, i+1, strings.TrimSpace(line))
				}
			}
		})
	}
}

// TestTheBinaryReadsNoVersionFile keeps the Go side matching the image side.
// The two halves went wrong together and would go wrong together again: a
// re-added file read makes the stale `VERSION` authoritative even on a
// correctly stamped release image, because it would run first or last but
// certainly somewhere.
func TestTheBinaryReadsNoVersionFile(t *testing.T) {
	body := readRepoFile(t, "main.go")
	// Deliberately narrow: main.go is the only place that ever resolved the
	// service version, and this is the literal it used.
	if strings.Contains(body, `os.ReadFile(versionFilePath)`) || strings.Contains(body, `"VERSION"`) {
		t.Error("main.go reads a VERSION file or env var again. The service version has one source, " +
			"the link-time stamp in core/buildinfo -- see resolveServiceVersion for why both of the " +
			"others were removed rather than demoted.")
	}
}

// modulePathOf reads the `module` line out of a go.mod.
func modulePathOf(t *testing.T, goMod string) string {
	t.Helper()
	for _, line := range strings.Split(readRepoFile(t, goMod), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("%s has no module line", goMod)
	return ""
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func declaresBuildArg(dockerfile, name string) bool {
	for _, line := range strings.Split(dockerfile, "\n") {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "ARG "); ok {
			if strings.HasPrefix(strings.TrimSpace(rest), name) {
				return true
			}
		}
	}
	return false
}

func lineContaining(body, marker string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, marker) && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			return line
		}
	}
	return ""
}
