// Static guard: the memQL Portal bundle actually reaches the bff IMAGE
// (znasllc-io/memql#3314).
//
// # The failure this exists to prevent
//
// The portal is served from a directory, not //go:embed'ed, so nothing in the
// Go build knows whether the bundle is present. component/portal degrades
// deliberately -- a missing directory logs one warning and answers 404 rather
// than failing the node -- which is right for resilience and terrible for
// detection: an image built without the portal stage boots green, passes
// every probe, and serves a 404 page that only a human clicking /portal ever
// sees.
//
// Three separate declarations have to agree for the bundle to be there, and
// they live in three files with nothing tying them together:
//
//	Dockerfile                              -- the runtime COPYs from `portal-dist`
//	scripts/lib/engine_build_args.sh        -- the LOCAL bff build selects portal-build
//	.github/workflows/build-engine-images.yml -- the RELEASE bff build selects it too
//
// Any one of them silently omits the bundle. The release one is the worst,
// because it is the path staging and production actually run, and it is not
// exercised by any pull-request lane.
//
// # Scope
//
// This asserts WIRING, not that a build succeeds -- no PR lane builds an
// image. It reads the three files and checks they name each other correctly.
package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The stage names the three files must agree on. Changing them is fine;
// changing them in only one file is what this catches.
const (
	portalDistStageArg  = "PORTAL_DIST_STAGE"
	portalBuildStage    = "portal-build"
	portalSkipStage     = "portal-skip"
	portalDistStageName = "portal-dist"
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
func TestDockerfileDeclaresThePortalStageSelector(t *testing.T) {
	body := readRepoFile(t, "Dockerfile")

	argIdx := strings.Index(body, "ARG "+portalDistStageArg)
	if argIdx < 0 {
		t.Fatalf("the Dockerfile declares no `ARG %s`", portalDistStageArg)
	}
	firstFrom := regexp.MustCompile(`(?mi)^FROM\s`).FindStringIndex(body)
	if firstFrom == nil {
		t.Fatal("the Dockerfile contains no FROM")
	}
	if argIdx > firstFrom[0] {
		t.Errorf("`ARG %s` is declared after the first FROM, so it is stage-scoped rather "+
			"than global and `FROM ${%s}` cannot read it", portalDistStageArg, portalDistStageArg)
	}

	stages := dockerfileStages(t, body)
	for _, stage := range []string{portalBuildStage, portalSkipStage, portalDistStageName} {
		if !stages[stage] {
			t.Errorf("the Dockerfile declares no `%s` stage (found %v)", stage, keysOf(stages))
		}
	}
}

// EVERY runtime stage must copy the bundle. The release build of the
// CGO-free node types passes no --target, which resolves to the LAST stage --
// so a copy present only in the distroless stage ships no portal in the
// images that actually run, which is exactly the kind of asymmetry nobody
// notices in review.
func TestEveryRuntimeStageCopiesThePortal(t *testing.T) {
	body := readRepoFile(t, "Dockerfile")

	// Split the file into stages and inspect the ones that ENTRYPOINT the
	// memql binary -- that is what makes a stage a runtime rather than a
	// builder, and it survives a rename of `runtime` / `voice-runtime`.
	blocks := regexp.MustCompile(`(?mi)^FROM\s`).Split(body, -1)
	names := regexp.MustCompile(`(?mi)^FROM\s+(.*)$`).FindAllStringSubmatch(body, -1)

	runtimes := 0
	for i, block := range blocks[1:] {
		if !strings.Contains(block, `ENTRYPOINT ["./memql"]`) {
			continue
		}
		runtimes++
		if !strings.Contains(block, "--from="+portalDistStageName) {
			label := "stage " + string(rune('0'+i))
			if i < len(names) {
				label = strings.TrimSpace(names[i][1])
			}
			t.Errorf("runtime %q does not COPY --from=%s. A node built against this stage "+
				"boots green and serves 404 at /portal, which no probe can see (memql#3314).",
				label, portalDistStageName)
		}
	}
	if runtimes == 0 {
		t.Fatal("found no runtime stage (none ENTRYPOINTs ./memql); this guard cannot pass " +
			"vacuously")
	}
}

// The LOCAL build path (make dev / the deploy.buildImage backend) must select
// the real stage for the bff.
func TestLocalBffBuildSelectsThePortalStage(t *testing.T) {
	body := readRepoFile(t, "scripts/lib/engine_build_args.sh")
	if !strings.Contains(body, portalDistStageArg+"="+portalBuildStage) {
		t.Errorf("scripts/lib/engine_build_args.sh never passes %s=%s. `make dev NODE=bff` "+
			"would then build a bff image with no portal in it, and the local cluster "+
			"would 404 at /portal while staging served it -- the environment divergence "+
			"the parity standard exists to prevent.", portalDistStageArg, portalBuildStage)
	}
	// ...and only for the bff. A blanket build-arg would put a Node stage in
	// front of every node image for bytes seven of them never serve.
	if !strings.Contains(body, `"$node" == "bff"`) && !strings.Contains(body, `"${node}" == "bff"`) {
		t.Errorf("scripts/lib/engine_build_args.sh does not gate %s on the bff node type; "+
			"every node image would build the SPA", portalDistStageArg)
	}
}

// The RELEASE build path must do the same. This is the one no pull-request
// lane exercises, so a static assertion is the only signal before staging.
func TestReleaseBffBuildSelectsThePortalStage(t *testing.T) {
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
		if strings.Contains(step.With["build-args"], portalDistStageArg+"=") {
			forwarded = true
		}
	}
	if !forwarded {
		t.Errorf("no build step passes %s as a build-arg, so the matrix entries below are "+
			"never read and every image uses the Dockerfile default (no portal)",
			portalDistStageArg)
	}

	sawBFF := false
	for _, entry := range job.Strategy.Matrix.Include {
		node, portal := entry["node"], entry["portal"]
		// An absent key renders as the empty string in `FROM ${VAR}`, which is
		// not a valid image reference -- the build fails rather than silently
		// skipping the portal, but it fails for every node at once and reads
		// as a Dockerfile bug. Require the key explicitly.
		if portal == "" {
			t.Errorf("matrix entry %q declares no `portal:` stage. `FROM ${%s}` cannot take "+
				"an empty value, so this breaks the release build for that node type.",
				node, portalDistStageArg)
			continue
		}
		if node == "bff" {
			sawBFF = true
			if portal != portalBuildStage {
				t.Errorf("the bff matrix entry selects portal stage %q, want %q. The bff is "+
					"the node that serves the portal; any other value ships an image with "+
					"no bundle to staging and production.", portal, portalBuildStage)
			}
			continue
		}
		if portal != portalSkipStage {
			t.Errorf("matrix entry %q selects portal stage %q. Only the bff serves the "+
				"portal, so every other node type must take %q -- otherwise its image "+
				"build pulls a Node toolchain and compiles an SPA it never serves.",
				node, portal, portalSkipStage)
		}
	}
	if !sawBFF {
		t.Error("the release matrix has no `bff` entry, so no image carries the portal " +
			"bundle at all")
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
