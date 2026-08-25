// Static guard: the COMMIT reaches the binary on every path that builds one,
// including the local ones (znasllc-io/memql#4574).
//
// # What was broken
//
// core/buildinfo has carried `commit` since memql#4486, and
// .github/workflows/build-engine-images.yml has passed it since. Nothing else
// did. scripts/lib/engine_build_args.sh -- THE nodeType -> build-args mapping
// shared by `make dev` (scripts/k3d/dev.sh, which is also the `k3d.dev`
// capability the editor's "Rebuild from checkout" runs) and by
// scripts/deploy/build-image.sh -- passed BUILD_TAGS, CGO_ENABLED and
// PORTAL_DIST_STAGE and no stamp at all.
//
// So a locally built node image carried NO commit. Not one nothing rendered:
// none. And the reason it could not be noticed by reading Commit() is that its
// second source looks like it should cover exactly this case -- the Go
// toolchain's own vcs.revision stamping, which handles `go build .` on a
// developer machine. It cannot fire inside an image build, because
// .dockerignore excludes .git. The build arg is the only source there.
//
// The visible symptom was a cluster that answered the bare word "dev" with no
// way to say which dev, on every surface: the portal's rail footer, the
// editor's cluster row, the boot log.
//
// # Why static, and why per-line
//
// The same reason release_stamp_test.go gives for its half, which this file
// deliberately mirrors rather than merges: `-X` is SILENTLY ignored when its
// target does not resolve, so a renamed variable or a dropped flag produces a
// successful build and an inert feature. And the healthcheck binary is built
// by a second `go build` in the same stage and takes no stamp on purpose, so
// the flag has to be asserted on the line that links memql rather than
// anywhere in the file.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// commitBuildArg is the docker build arg every engine build threads into the
// linker flag.
const (
	commitBuildArg  = "MEMQL_COMMIT"
	commitVarName   = "commit"
	engineBuildArgs = "scripts/lib/engine_build_args.sh"
)

func TestEveryEngineImageLinksTheCommitIntoTheBinary(t *testing.T) {
	wantFlag := fmt.Sprintf("-X %s/%s.%s=${%s}",
		modulePathOf(t, buildinfoModFile), buildinfoPkgPath, commitVarName, commitBuildArg)

	for path, buildMarker := range dockerfilesThatBuildTheEngine {
		t.Run(path, func(t *testing.T) {
			body := readRepoFile(t, path)

			if !declaresBuildArg(body, commitBuildArg) {
				t.Errorf("%s does not declare `ARG %s`, so the revision can never be passed in", path, commitBuildArg)
			}

			line := lineContaining(body, buildMarker)
			if line == "" {
				t.Fatalf("%s has no line containing %q -- this guard's marker is stale, not the Dockerfile", path, buildMarker)
			}
			if !strings.Contains(line, wantFlag) {
				t.Errorf("%s links the engine binary without stamping the revision.\n"+
					"  want the build line to contain: %s\n"+
					"  got line: %s\n"+
					"A Docker build context carries no .git, so this flag is the ONLY source a node has "+
					"for its own commit -- without it the image reports none and every version surface "+
					"falls back to the bare word %q.",
					path, wantFlag, strings.TrimSpace(line), "dev")
			}
		})
	}
}

// TestTheCommitStampTargetExists closes the other half of the silent -X no-op:
// the flag can be spelled perfectly and still write nothing if the variable it
// names has been renamed, initialised, or turned into a const.
func TestTheCommitStampTargetExists(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, buildinfoSrcFile, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", buildinfoSrcFile, err)
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
				if name.Name != commitVarName {
					continue
				}
				if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != "string" {
					t.Fatalf("%s.%s must be a plain string var for -X to write it", buildinfoPkgPath, commitVarName)
				}
				if len(value.Values) != 0 {
					t.Fatalf("%s.%s has an initializer; -X cannot stamp an initialized variable", buildinfoPkgPath, commitVarName)
				}
				return
			}
		}
	}
	t.Fatalf("%s declares no package-level `var %s string` -- the -X target the image builds name does not exist",
		buildinfoSrcFile, commitVarName)
}

// TestTheSharedBuildArgsHelperStampsTheCommit is the fix's own guard. Both
// local build paths go through this one file precisely so they cannot drift;
// what drifted instead was the file and the release workflow.
func TestTheSharedBuildArgsHelperStampsTheCommit(t *testing.T) {
	body := readRepoFile(t, engineBuildArgs)
	want := `--build-arg "` + commitBuildArg + `=`
	if !strings.Contains(body, want) {
		t.Errorf("%s does not append %s to ENGINE_BUILD_ARGS.\n"+
			"It is the shared mapping for `make dev` and deploy.buildImage, so without it "+
			"every locally built image is unstamped and every local cluster reports %q.",
			engineBuildArgs, commitBuildArg, "dev")
	}
	// The RELEASE must stay absent here. Neither caller is cutting one, and a
	// binary that was not cut from a release must not name one -- the property
	// core/buildinfo's package comment is built around.
	if strings.Contains(body, releaseBuildArg+"=") {
		t.Errorf("%s passes %s. Neither of its callers is cutting a release "+
			"(dev.sh builds a developer's checkout; build-image.sh is the local/replay surface), "+
			"so a stamp here would make a local build claim a release it was not cut from.",
			engineBuildArgs, releaseBuildArg)
	}
}

// TestBothLocalBuildPathsPassASourceDirectory pins the argument that makes the
// stamp possible.
//
// The helper reads the revision with `git -C <sourceDir>`, so a caller that
// passes only the node type produces an EMPTY commit -- an unstamped image and
// exactly the bug this epic fixed, arriving through a one-word omission. The
// helper takes the directory as a required positional under `set -u` so the
// omission fails at the call; this asserts that neither caller has quietly
// gone back to the one-argument form.
func TestBothLocalBuildPathsPassASourceDirectory(t *testing.T) {
	callers := map[string]string{
		"scripts/k3d/dev.sh":            `engine_build_args_for_node "$node" "$REPO_ROOT"`,
		"scripts/deploy/build-image.sh": `engine_build_args_for_node "$nodeType" "$workdir"`,
	}
	for path, want := range callers {
		t.Run(path, func(t *testing.T) {
			body := readRepoFile(t, path)
			if strings.Contains(body, want) {
				return
			}
			// Name what IS there. "expected X" over a file whose call is on the
			// next line down for an unrelated reason sends a reader hunting.
			got := lineContaining(body, "engine_build_args_for_node ")
			t.Errorf("%s does not call the shared helper with a source directory.\n"+
				"  want: %s\n"+
				"  got:  %s\n"+
				"Without the directory the helper has nothing to read the revision from, and the "+
				"image it builds carries no commit.", path, want, strings.TrimSpace(got))
		})
	}
}

// TestTheReleaseBuildSuppliesTheCommit -- the release workflow and the
// break-glass release script both know the revision they are cutting, and both
// must pass it. The workflow half already did; release.sh passed the release
// and not the revision, which is the same half-answer in the other direction.
func TestTheReleaseBuildSuppliesTheCommit(t *testing.T) {
	for path, want := range map[string]string{
		".github/workflows/build-engine-images.yml": commitBuildArg + "=${{ github.sha }}",
		"scripts/release/release.sh":                `--build-arg "` + commitBuildArg + `=${FULL_SHA}"`,
	} {
		t.Run(path, func(t *testing.T) {
			if !strings.Contains(readRepoFile(t, path), want) {
				t.Errorf("%s does not pass %q.\nA released image that cannot name its source commit "+
					"leaves an incident mapping an image digest back to a build by hand.", path, want)
			}
		})
	}
}
