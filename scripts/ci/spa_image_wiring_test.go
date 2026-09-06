// Static guard: the MemQL OS bundle actually reaches the EDGE IMAGE
// (znasllc-io/memql#3314, retargeted from the bff by #3711, and from the portal
// to the OS shell by epic memql#4984 -- the shell is a site row served by
// component/edge).
//
// # The failure this exists to prevent
//
// The shell is served from a directory, not //go:embed'ed, so nothing in the
// Go build knows whether the bundle is present. component/edge degrades
// deliberately -- a missing bundle 404s asset-by-asset rather than failing
// the node -- which is right for resilience and terrible for detection: an
// image built without the SPA stage boots green, passes every probe, and
// serves 404 for every request to a hostname that only a human ever visits.
//
// Three separate declarations have to agree for the bundle to be there, and
// they live in three files with nothing tying them together:
//
//	Dockerfile                              -- the runtime COPYs from `spa-dist`
//	scripts/lib/engine_build_args.sh        -- the LOCAL edge build selects spa-build
//	.github/workflows/build-engine-images.yml -- the RELEASE build's `edge` matrix
//	                                            entry (memql#3711 landed only the
//	                                            local path; memql#3714 added the
//	                                            release matrix entry)
//
// Any one of them silently omits the bundle. The release one would be the
// worst, because it is the path the cloud actually runs and it is not
// exercised by any pull-request lane -- which is exactly why this guard also
// asserts the NEGATIVE: nothing in the release matrix other than `edge` may
// claim `spa-build`, because a stray bff (or any other node) still carrying
// it would silently ship dead weight for a bundle nothing serves.
//
// # Scope
//
// This asserts WIRING, not that a build succeeds -- no PR lane builds an
// image. It reads the three files and checks they name each other correctly.
package ci

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The stage names the three files must agree on. Changing them is fine;
// changing them in only one file is what this catches.
const (
	spaDistStageArg  = "SPA_DIST_STAGE"
	spaBuildStage    = "spa-build"
	spaSkipStage     = "spa-skip"
	spaDistStageName = "spa-dist"
)

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(RepoRoot(), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// dockerfileStages returns the stage names declared by `FROM ... AS <name>`.
func dockerfileStages(t *testing.T, body string) map[string]bool {
	t.Helper()
	re := regexp.MustCompile(`(?mi)^FROM\s+\S+\s+AS\s+([A-Za-z0-9_.-]+)`)
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		out[strings.ToLower(m[1])] = true
	}
	if len(out) == 0 {
		t.Fatal("parsed no stages out of the Dockerfile; this guard cannot pass vacuously")
	}
	return out
}

// The selector and both of its alternatives must exist, and the global ARG
// must be declared BEFORE the first FROM -- that is the only scope a
// `FROM ${VAR}` line can read, and getting it wrong makes the variable expand
// to empty and the build fail with a confusing "invalid reference format".
func TestDockerfileDeclaresTheSpaStageSelector(t *testing.T) {
	body := readRepoFile(t, "Dockerfile")

	argIdx := strings.Index(body, "ARG "+spaDistStageArg)
	if argIdx < 0 {
		t.Fatalf("the Dockerfile declares no `ARG %s`", spaDistStageArg)
	}
	firstFrom := regexp.MustCompile(`(?mi)^FROM\s`).FindStringIndex(body)
	if firstFrom == nil {
		t.Fatal("the Dockerfile contains no FROM")
	}
	if argIdx > firstFrom[0] {
		t.Errorf("`ARG %s` is declared after the first FROM, so it is stage-scoped rather "+
			"than global and `FROM ${%s}` cannot read it", spaDistStageArg, spaDistStageArg)
	}

	stages := dockerfileStages(t, body)
	for _, stage := range []string{spaBuildStage, spaSkipStage, spaDistStageName} {
		if !stages[stage] {
			t.Errorf("the Dockerfile declares no `%s` stage (found %v)", stage, keysOf(stages))
		}
	}
}

// EVERY runtime stage must copy the bundle. A build that passes no --target
// resolves to the LAST stage -- so a copy present only in one runtime stage
// ships no bundle in the images that actually run, which is exactly the kind
// of asymmetry nobody notices in review.
func TestEveryRuntimeStageCopiesTheShell(t *testing.T) {
	body := readRepoFile(t, "Dockerfile")

	// Split the file into stages and inspect the ones that ENTRYPOINT the
	// memql binary -- that is what makes a stage a runtime rather than a
	// builder, and it survives a rename of any runtime stage.
	blocks := regexp.MustCompile(`(?mi)^FROM\s`).Split(body, -1)
	names := regexp.MustCompile(`(?mi)^FROM\s+(.*)$`).FindAllStringSubmatch(body, -1)

	runtimes := 0
	for i, block := range blocks[1:] {
		if !strings.Contains(block, `ENTRYPOINT ["./memql"]`) {
			continue
		}
		runtimes++
		if !strings.Contains(block, "--from="+spaDistStageName) {
			label := "stage " + string(rune('0'+i))
			if i < len(names) {
				label = strings.TrimSpace(names[i][1])
			}
			t.Errorf("runtime %q does not COPY --from=%s. A node built against this stage "+
				"boots green and serves 404 at os.<domain>, which no probe can see (memql#3314).",
				label, spaDistStageName)
		}
	}
	if runtimes == 0 {
		t.Fatal("found no runtime stage (none ENTRYPOINTs ./memql); this guard cannot pass " +
			"vacuously")
	}
}

// The LOCAL build path (make dev / the deploy.buildImage backend) must select
// the real stage for the edge -- the node that serves the OS shell
// (memql#4705).
func TestLocalEdgeBuildSelectsTheSpaStage(t *testing.T) {
	body := readRepoFile(t, "scripts/lib/engine_build_args.sh")
	if !strings.Contains(body, spaDistStageArg+"="+spaBuildStage) {
		t.Errorf("scripts/lib/engine_build_args.sh never passes %s=%s. `make dev NODE=edge` "+
			"would then build an edge image with no shell in it, and the local cluster "+
			"would 404 at os.<domain> while the cloud served it -- the environment "+
			"divergence the parity standard exists to prevent.", spaDistStageArg, spaBuildStage)
	}
	// ...and only for the edge. A blanket build-arg would put a Node stage in
	// front of every node image for bytes eight of them never serve.
	if !strings.Contains(body, `"$node" == "edge"`) && !strings.Contains(body, `"${node}" == "edge"`) {
		t.Errorf("scripts/lib/engine_build_args.sh does not gate %s on the edge node type; "+
			"every node image would build the SPA", spaDistStageArg)
	}
	// The bff must NOT still claim the stage -- it stopped serving the SPA in
	// the same change that made this test name the edge (memql#3711). A stray
	// `"$node" == "bff"` left in the OR alongside edge would silently build a
	// Node toolchain into an image nothing serves it from anymore.
	if strings.Contains(body, `"$node" == "bff"`) || strings.Contains(body, `"${node}" == "bff"`) {
		t.Errorf("scripts/lib/engine_build_args.sh still gates %s on the bff node type. "+
			"The bff serves no hosted site -- the edge does -- so this just pulls an "+
			"unused Node toolchain into the bff image.", spaDistStageArg)
	}
}

// The RELEASE build path must agree with the local one (memql#3714 wires the
// `edge` entry into the release image matrix; #3711 landed only the local
// build path and the DSL/serving side). The matrix must carry an `edge` entry
// selecting spa-build; every OTHER entry -- bff included -- must select
// spa-skip. That catches the three failure modes that matter: the bff
// silently keeping the stage it no longer serves, any node other than edge
// picking it up by accident, and the edge entry itself going missing or
// losing its spa-build selection on some future edit. This is the one no
// pull-request lane exercises, so a static assertion is the only signal
// before staging.
func TestReleaseBuildSelectsTheSpaStageOnlyForTheEdge(t *testing.T) {
	raw := readRepoFile(t, ".github/workflows/build-engine-images.yml")

	var doc struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []map[string]string `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []struct {
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("parse build-engine-images.yml: %v", err)
	}

	job, ok := doc.Jobs["build"]
	if !ok {
		t.Fatal("no `build` job in build-engine-images.yml; retarget this guard rather " +
			"than deleting it")
	}
	if len(job.Strategy.Matrix.Include) == 0 {
		t.Fatal("the build job declares an empty matrix; this guard cannot pass vacuously")
	}

	// The build step has to FORWARD the matrix value, or the per-entry
	// declarations below are decoration.
	forwarded := false
	for _, step := range job.Steps {
		if strings.Contains(step.With["build-args"], spaDistStageArg+"=") {
			forwarded = true
		}
	}
	if !forwarded {
		t.Errorf("no build step passes %s as a build-arg, so the matrix entries below are "+
			"never read and every image uses the Dockerfile default (no bundle)",
			spaDistStageArg)
	}

	sawEdge := false
	for _, entry := range job.Strategy.Matrix.Include {
		node, spa := entry["node"], entry["spa"]
		// An absent key renders as the empty string in `FROM ${VAR}`, which is
		// not a valid image reference -- the build fails rather than silently
		// skipping the bundle, but it fails for every node at once and reads
		// as a Dockerfile bug. Require the key explicitly.
		if spa == "" {
			t.Errorf("matrix entry %q declares no `spa:` stage. `FROM ${%s}` cannot take "+
				"an empty value, so this breaks the release build for that node type.",
				node, spaDistStageArg)
			continue
		}
		if node == "edge" {
			sawEdge = true
			if spa != spaBuildStage {
				t.Errorf("the edge matrix entry selects SPA stage %q, want %q. The edge is "+
					"the node that serves the OS shell (memql#4705); any other value "+
					"ships an image with no bundle to staging and production.",
					spa, spaBuildStage)
			}
			continue
		}
		if spa != spaSkipStage {
			t.Errorf("matrix entry %q selects SPA stage %q. Only the edge serves the "+
				"shell, so every other node type -- the bff included, since it stopped "+
				"serving one in memql#3711 -- must take %q, otherwise its image build "+
				"pulls a Node toolchain and compiles an SPA it never serves.",
				node, spa, spaSkipStage)
		}
	}
	// The release matrix MUST carry an `edge` entry (memql#3714): without one,
	// staging and production run every node type EXCEPT the one that serves
	// the shell, and nothing above would catch its quiet disappearance --
	// the loop just would not have anything to check.
	if !sawEdge {
		t.Error("the release matrix declares no `edge` entry. The edge is the node that " +
			"serves the OS shell (memql#4705) and cloud images must come from " +
			"the build server (CLAUDE.md); without this entry the edge cannot be deployed " +
			"above local.")
	}
}

// spaBuildStageBlock returns the body of the `spa-build` stage, header
// line included, up to the next FROM.
func spaBuildStageBlock(t *testing.T, body string) string {
	t.Helper()
	starts := regexp.MustCompile(`(?mi)^FROM\s`).FindAllStringIndex(body, -1)
	header := regexp.MustCompile(`(?i)\sAS\s+` + regexp.QuoteMeta(spaBuildStage) + `$`)
	for i, loc := range starts {
		end := len(body)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		block := body[loc[0]:end]
		first := block
		if nl := strings.IndexByte(block, '\n'); nl >= 0 {
			first = block[:nl]
		}
		if header.MatchString(strings.TrimSpace(first)) {
			return block
		}
	}
	t.Fatalf("no `%s` stage in the Dockerfile; retarget this guard rather than deleting it",
		spaBuildStage)
	return ""
}

// copySourcesIn returns the SOURCE arguments of every COPY in a stage body --
// every field but the last, with `--flags` dropped.
func copySourcesIn(block string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`(?mi)^\s*COPY\s+(.*)$`).FindAllStringSubmatch(block, -1) {
		var args []string
		for _, f := range strings.Fields(m[1]) {
			if strings.HasPrefix(f, "--") {
				continue
			}
			args = append(args, f)
		}
		if len(args) < 2 {
			continue
		}
		out = append(out, args[:len(args)-1]...)
	}
	return out
}

// The shell's stylesheet reaches OUTSIDE clients/os, and every tree it
// reaches into has to be in this stage's build context.
//
// # The failure this exists to prevent
//
// memql#4266 moved the type faces to a repo-root `brand/` tree so the client
// and the identity pages could not drift into two palettes, and had the client
// @import it by relative path: `../../../../brand/fonts.css`. That is four
// levels up out of clients/<name>/src/styles -- a source input of the
// spa-build stage that is invisible from clients/os itself.
//
// The stage copies four named trees and did not copy that one, so `vite build`
// failed to resolve the import and the edge image -- the ONLY node type that
// builds the SPA -- failed at the first release cut carrying the change, while
// every developer machine, `make os-build`, and every pull-request lane
// stayed green, because all of them build from a full checkout where the tree
// is simply there. The release build is the one path that does not, and no PR
// lane builds an image (see the file header), so a static assertion is the
// only signal before a cut goes red.
//
// It is not vacuous-pass-proof, and cannot be: zero escaping imports is a
// legitimate state, and the honest reading of it is "nothing to check".
func TestSpaBuildStageCopiesEveryTreeTheShellImports(t *testing.T) {
	root := RepoRoot()
	copied := copySourcesIn(spaBuildStageBlock(t, readRepoFile(t, "Dockerfile")))

	shellRel := filepath.Join("clients", "os")
	srcDir := filepath.Join(root, shellRel, "src")

	// Top-level tree -> the stylesheets that reach into it, for the message.
	escapes := map[string]map[string]bool{}

	importRe := regexp.MustCompile(`@import\s+["']([^"']+)["']`)
	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".css") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range importRe.FindAllStringSubmatch(string(raw), -1) {
			spec := m[1]
			// Only RELATIVE specifiers address the filesystem; a bare one is a
			// package name npm resolves out of node_modules.
			if !strings.HasPrefix(spec, ".") {
				continue
			}
			abs := filepath.Clean(filepath.Join(filepath.Dir(path), spec))
			rel, relErr := filepath.Rel(root, abs)
			if relErr != nil || strings.HasPrefix(rel, "..") {
				// Outside the repository entirely -- a different bug, and not
				// one a COPY could fix.
				continue
			}
			if rel == shellRel || strings.HasPrefix(rel, shellRel+string(filepath.Separator)) {
				continue
			}
			top := strings.Split(rel, string(filepath.Separator))[0]
			from, _ := filepath.Rel(root, path)
			if escapes[top] == nil {
				escapes[top] = map[string]bool{}
			}
			escapes[top][from] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", srcDir, err)
	}

	trees := make([]string, 0, len(escapes))
	for tree := range escapes {
		trees = append(trees, tree)
	}
	sort.Strings(trees)

	for _, tree := range trees {
		found := false
		for _, src := range copied {
			if src == tree || strings.HasPrefix(src, tree+"/") {
				found = true
				break
			}
		}
		if found {
			continue
		}
		importers := make([]string, 0, len(escapes[tree]))
		for f := range escapes[tree] {
			importers = append(importers, f)
		}
		sort.Strings(importers)
		t.Errorf("the shell @imports %s/ from %v, but the Dockerfile's `%s` stage never "+
			"COPYs it (it copies %v). The stage's build context would not contain the tree, "+
			"so `vite build` cannot resolve the import and the EDGE image fails to build -- "+
			"at a release cut, not in any pull-request lane. Add `COPY %s ./%s` to that stage.",
			tree, importers, spaBuildStage, copied, tree, tree)
	}
}

func TestEveryRuntimeStageCopiesTheOs(t *testing.T) {
	body := readRepoFile(t, "Dockerfile")
	blocks := regexp.MustCompile(`(?mi)^FROM\s`).Split(body, -1)
	names := regexp.MustCompile(`(?mi)^FROM\s+(.*)$`).FindAllStringSubmatch(body, -1)
	runtimes := 0
	for i, block := range blocks[1:] {
		if !strings.Contains(block, `ENTRYPOINT ["./memql"]`) {
			continue
		}
		runtimes++
		if !strings.Contains(block, "/os-dist") {
			label := "stage"
			if i < len(names) {
				label = strings.TrimSpace(names[i][1])
			}
			t.Errorf("runtime %q does not COPY the OS bundle from /os-dist. The edge would 404 at os.<domain> (memql#4705).", label)
		}
	}
	if runtimes == 0 {
		t.Fatal("found no runtime stage; this guard cannot pass vacuously")
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// The image references have to agree with each other, and nothing else checks it
// ---------------------------------------------------------------------------

// EVERY reference to the same image REPOSITORY must be byte-identical -- same
// tag, same digest -- whether it appears as `FROM` or as `COPY --from=`.
//
// # The failure this exists to prevent
//
// Dependabot cannot see `COPY --from=<image>`. It updates `FROM` lines and
// leaves those behind, so a base-image bump is PARTIAL BY CONSTRUCTION
// wherever a stage copies out of an image by name rather than by stage.
//
// This Dockerfile has exactly that shape: `spa-build` is `FROM node:...`, and
// `workbench-runtime` lifts the node binary and its modules with two
// `COPY --from=node:...` lines. A bump that moved the FROM and not the copies
// would ship one Node version building the bundle and a different one running
// somebody else's code -- in the image whose whole purpose is running somebody
// else's code -- while the comment above those two lines says they are the same
// image.
//
// It nearly happened twice on 2026-09-06: dependabot#5033 (node 22 -> 26) and
// #5032 (debian 12 -> 13) were both this, and both were caught by a human
// reading line numbers rather than by any lane.
//
// # Why the grouping key is the REPOSITORY and not the tag
//
// The obvious implementation groups by `node:22-bookworm-slim` and asserts one
// digest per group. That detects nothing here: dependabot's edit changes the
// TAG, so the drifted lines land in two different groups and each is
// internally consistent. The key has to be the part before the tag.
//
// A Dockerfile that genuinely needs two versions of one repository would fail
// this. That is deliberate -- it is rare, it is worth stating out loud, and a
// gate that permits it by default cannot see the case it exists for.
func TestEveryReferenceToAnImageAgreesWithTheOthers(t *testing.T) {
	body := readRepoFile(t, "Dockerfile")
	stages := dockerfileStages(t, body)

	// `FROM x [AS y]` and `COPY --from=x`. `x` is a STAGE when it names one
	// this file declares, or is an ARG expansion; anything else is an image.
	refRe := regexp.MustCompile(`(?mi)^(?:FROM\s+(\S+)|COPY\s+--from=(\S+))`)

	type ref struct {
		full string
		line int
	}
	byRepo := map[string][]ref{}
	for _, m := range refRe.FindAllStringSubmatchIndex(body, -1) {
		raw := ""
		for _, g := range [][2]int{{2, 3}, {4, 5}} {
			if m[g[0]] >= 0 {
				raw = body[m[g[0]]:m[g[1]]]
			}
		}
		if raw == "" || strings.HasPrefix(raw, "${") || stages[strings.ToLower(raw)] {
			continue // a stage reference, not an image
		}
		repo := raw
		if i := strings.Index(repo, "@"); i >= 0 {
			repo = repo[:i]
		}
		if i := strings.LastIndex(repo, ":"); i >= 0 {
			repo = repo[:i]
		}
		byRepo[repo] = append(byRepo[repo], ref{
			full: raw,
			line: strings.Count(body[:m[0]], "\n") + 1,
		})
	}

	if len(byRepo) == 0 {
		t.Fatal("no external image references found in the Dockerfile; this guard is scanning nothing")
	}

	for _, repo := range keysOf(func() map[string]bool {
		s := map[string]bool{}
		for k := range byRepo {
			s[k] = true
		}
		return s
	}()) {
		refs := byRepo[repo]

		// Every reference must carry a digest. Without one the agreement
		// below is satisfied by two unpinned copies of a moving tag.
		for _, r := range refs {
			if !strings.Contains(r.full, "@sha256:") {
				t.Errorf("Dockerfile:%d references %q with no @sha256 digest.\n"+
					"Every external image is digest-pinned here; an unpinned tag moves under the build.",
					r.line, r.full)
			}
		}

		distinct := map[string][]int{}
		for _, r := range refs {
			distinct[r.full] = append(distinct[r.full], r.line)
		}
		if len(distinct) <= 1 {
			continue
		}
		var b strings.Builder
		for _, form := range keysOf(func() map[string]bool {
			s := map[string]bool{}
			for k := range distinct {
				s[k] = true
			}
			return s
		}()) {
			fmt.Fprintf(&b, "\n    %s  (lines %v)", form, distinct[form])
		}
		t.Errorf("the Dockerfile references image repository %q in %d different forms:%s\n"+
			"Every FROM and COPY --from naming the same repository must be byte-identical.\n"+
			"Dependabot updates FROM lines and CANNOT SEE `COPY --from=<image>`, so this is\n"+
			"what a partially-applied base-image bump looks like -- and the stage that copies\n"+
			"out of the image by name ends up on a different version from the one that built it.\n"+
			"If two versions are genuinely required, this guard is the place to say why.",
			repo, len(distinct), b.String())
	}
}
