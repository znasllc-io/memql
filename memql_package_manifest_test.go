package main

import (
	"errors"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"

	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/pipelines"
)

// memql_package_manifest_test.go -- the engine repository's own pipeline (epic
// memql#5478, issue memql#5496).
//
// memql-package.yaml at the repository root makes this repository a source with
// a pipeline and no apps, with no special case (design record D12). Two readers
// take the file: Deployables' source probe and every pipeline run read it
// through component/packages' one strict decoder, and a run compiles its
// pipeline: block with component/pipelines. These tests read it the same way --
// compileEngineOpening is component/pipelinerun's readPlan, step for step, over
// the committed tree -- so a manifest that would fail its own run fails here
// first, on the pull request that broke it, rather than as the first red check
// on main.
//
// Beyond "it compiles", they hold three things a reader of the manifest has to
// take on trust otherwise:
//
//   - Both images by digest. The Postgres is ALWAYS asserted, and it is the
//     image ci.yml's db-tests lane runs, so the pipeline and that lane test
//     against one image. The toolchain digest exists only once
//     build-toolchain-image.yml has run on main, so that test skips while the
//     manifest names the first tag, and fails on any other reference that is
//     not a digest. Nothing here asserts an invented digest.
//   - The copies the manifest has to carry stay copies. select.dbGated is
//     scripts/ci/db-gated-packages.sh's set: a tree missing from it would run
//     its database tests in go-tests, with no database, where they skip --
//     green over tests that never ran. The gate-inputs step runs the packages
//     ci.yml's planner adds for a change that is not Go source.
//   - The db step reaches the service it declares, and fails rather than skips
//     when it cannot.

// The images the manifest names. Each repository is a contract: the toolchain
// is what build-toolchain-image.yml publishes, the Postgres is the mirror
// ci.yml's db-tests lane runs.
const (
	manifestToolchainRepository = "ghcr.io/znasllc-io/memql-toolchain"
	// manifestToolchainFirstTag is the one reference the toolchain may carry
	// that is not a digest: the tag build-toolchain-image.yml's first dispatch
	// publishes, named until that digest exists.
	manifestToolchainFirstTag  = manifestToolchainRepository + ":1.0.0"
	manifestPostgresRepository = "ghcr.io/znasllc-io/ci-timescaledb"
	manifestPostgresService    = "postgres"
)

// The stages and steps of memql-package.yaml, by name. Each step mirrors a lane
// of .github/workflows/ci.yml; the manifest's comments say which.
const (
	manifestStageChecks     = "checks"
	manifestStageTests      = "tests"
	manifestStepGoChecks    = "go-checks"
	manifestStepPathRouting = "path-routing"
	manifestStepGateInputs  = "gate-inputs"
	manifestStepGoTests     = "go-tests"
	manifestStepDBTests     = "db-tests"
	manifestStepFuzz        = "fuzz"
	manifestStepOSChecks    = "os-checks"
)

// manifestImageByDigest is an immutable image reference, the repository in its
// first group.
var manifestImageByDigest = regexp.MustCompile(`^([^@\s]+)@sha256:[0-9a-f]{64}$`)

// engineManifest reads memql-package.yaml through the reader Deployables' source
// probe and a pipeline run both use: packages.ReadManifest, the strict decoder
// ParseManifest wraps.
func engineManifest(t *testing.T) *packages.Manifest {
	t.Helper()
	m, err := packages.ReadManifest(os.DirFS("."))
	if err != nil {
		t.Fatalf("%s does not read the way a source probe and a pipeline run read it: %v", packages.ManifestName, err)
	}
	if m.Pipeline == nil {
		t.Fatalf("%s declares no pipeline: block, so every run of this repository would fail %s",
			packages.ManifestName, pipelines.CodeNotDeclared)
	}
	return m
}

// engineDeclaredStep is the step of the manifest named name, in any stage.
func engineDeclaredStep(spec *pipelines.Spec, name string) (pipelines.StepSpec, bool) {
	for _, stage := range spec.Stages {
		for _, step := range stage.Steps {
			if step.Name == name {
				return step, true
			}
		}
	}
	return pipelines.StepSpec{}, false
}

// engineCommittedGoTree is the Go tree a run reads: the tracked go.mod, go.work
// and .go files, which is what component/pipelinerun's keepForRun keeps of a
// commit (less the manifest, which engineManifest reads). Listed by `git
// ls-files`, so an untracked file in a working tree cannot change the graph;
// read from the working tree, so an edit to a tracked file can. It also answers
// the whole tracked set, which the openings check their changed paths against.
func engineCommittedGoTree(t *testing.T) (fstest.MapFS, map[string]bool) {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	tree := fstest.MapFS{}
	tracked := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		tracked[p] = true
		base := path.Base(p)
		if base != "go.mod" && base != "go.work" && !strings.HasSuffix(base, ".go") {
			continue
		}
		data, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted in the working tree, not yet in a commit
		}
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		tree[p] = &fstest.MapFile{Data: data}
	}
	if len(tree) == 0 {
		t.Fatal("git ls-files listed no Go source: the import graph below would be empty and every opening would prove nothing")
	}
	return tree, tracked
}

// engineImportGraph is the graph a run selects packages from, read from source
// with pipelines.ScanGoTree as component/pipelinerun reads it.
func engineImportGraph(t *testing.T) (*pipelines.Graph, map[string]bool) {
	t.Helper()
	tree, tracked := engineCommittedGoTree(t)
	graph, err := pipelines.ScanGoTree(tree)
	if err != nil {
		t.Fatalf("pipelines.ScanGoTree over the committed tree: %v", err)
	}
	// A file the graph cannot read makes every affected selection Full, and the
	// pull-request openings below would then pass without compiling the
	// narrowed plan they exist to compile.
	if unread := graph.Incomplete(); len(unread) > 0 {
		t.Fatalf("the import graph could not read %v, so every pull request would select everything", unread)
	}
	if n := len(graph.Packages()); n < 100 {
		t.Fatalf("the import graph holds %d packages: too few to be this repository's", n)
	}
	return graph, tracked
}

// compileEngineOpening compiles the pipeline for one run the way
// component/pipelinerun's readPlan does: the block validated before anything is
// computed from it, the mode the event decides, the change list a pull
// request's compare answers, the import graph selected with Affected whenever a
// step selects packages, and Compile.
//
// Compute is cluster, the default a pipeline row carries. The engine's pipeline
// consents to no fleet machine, so a step that named a need would refuse the
// whole run here exactly as it would there.
func compileEngineOpening(spec *pipelines.Spec, graph *pipelines.Graph, event pipelines.Event, changed []string) (pipelines.Plan, *pipelines.Refusal) {
	if refusal := pipelines.Validate(spec); refusal != nil {
		return pipelines.Plan{}, refusal
	}
	mode, ok := pipelines.ModeFor(event)
	if !ok {
		return pipelines.Plan{}, pipelines.Refuse(pipelines.CodeEventUnknown, "", "no mode is decided for event %q", event)
	}
	in := pipelines.CompileInput{Mode: mode, Event: event, Compute: pipelines.ComputeCluster}
	if mode == pipelines.ModeAffected {
		in.Changed, in.ChangedKnown = changed, true
	}
	if pipelines.NeedsSelector(spec) {
		var selected, full []string
		if in.ChangedKnown {
			selected = in.Changed
		}
		if spec.Select != nil {
			full = spec.Select.Full
		}
		selection, err := pipelines.Affected(graph, selected, full)
		if err != nil {
			return pipelines.Plan{}, pipelines.Refuse(pipelines.CodeSelectInvalid, "select/full", "%v", err)
		}
		in.Selector = pipelines.GraphSelector(graph, selection)
	}
	return pipelines.Compile(spec, in)
}

// Every event that opens a run compiles with no refusal, into the two stages in
// the order written, every step in the toolchain image on the cluster, the db
// step beside its Postgres; and each opening runs and skips what the manifest
// says it does.
func TestEngineManifestCompilesForEveryOpening(t *testing.T) {
	spec := engineManifest(t).Pipeline
	graph, tracked := engineImportGraph(t)
	postgres, ok := spec.Services[manifestPostgresService]
	if !ok {
		t.Fatalf("the pipeline declares no %q service, so the db-gated trees have nothing to test against", manifestPostgresService)
	}

	every := []string{manifestStepGoChecks, manifestStepPathRouting, manifestStepGateInputs,
		manifestStepGoTests, manifestStepDBTests, manifestStepFuzz, manifestStepOSChecks}
	always := []string{manifestStepGoChecks, manifestStepPathRouting, manifestStepFuzz}
	openings := []struct {
		name    string
		event   pipelines.Event
		changed []string
		// runs must be planned and not skipped; skips must be planned and
		// skipped. A step in neither depends on what the import graph says.
		runs, skips []string
	}{
		{name: "a push to the default branch", event: pipelines.EventPush, runs: every},
		{name: "a merge group", event: pipelines.EventMergeGroup, runs: every},
		// The compiler of this very manifest: Go source only, and the db-gated
		// driver (component/pipelinerun) imports it, so the change reaches both
		// test steps and no bucket.
		{
			name: "a pull request changing Go", event: pipelines.EventPullRequest,
			changed: []string{"component/pipelines/compile.go"},
			runs:    append(slices.Clone(always), manifestStepGoTests, manifestStepDBTests),
			skips:   []string{manifestStepGateInputs, manifestStepOSChecks},
		},
		// Not Go source, so the gate packages run; nothing the OS shell reads.
		{
			name: "a pull request changing only docs", event: pipelines.EventPullRequest,
			changed: []string{"docs/public/overview/quickstart.md"},
			runs:    append(slices.Clone(always), manifestStepGateInputs),
			skips:   []string{manifestStepOSChecks},
		},
	}

	for _, o := range openings {
		t.Run(o.name, func(t *testing.T) {
			for _, p := range o.changed {
				if !tracked[p] {
					t.Fatalf("%s is not a tracked file, so this opening no longer changes what it says it does; pick another path of the same kind", p)
				}
			}
			plan, refusal := compileEngineOpening(spec, graph, o.event, o.changed)
			if refusal != nil {
				t.Fatalf("the run would fail before any step started: %v", refusal)
			}

			var stages []string
			for _, stage := range plan.Stages {
				stages = append(stages, stage.Name)
			}
			if want := []string{manifestStageChecks, manifestStageTests}; !slices.Equal(stages, want) {
				t.Errorf("the plan's stages are %v, want %v: checks first, so a change that does not build starts no test", stages, want)
			}

			byName := map[string][]pipelines.Step{}
			for _, step := range plan.Steps() {
				byName[step.Name] = append(byName[step.Name], step)
				if step.Kind != pipelines.StepCommand || step.Image != spec.Image {
					t.Errorf("step %s is a %s step in %q; every step is a command in the toolchain image %q",
						step.Key, step.Kind, step.Image, spec.Image)
				}
				if len(step.Needs) > 0 {
					t.Errorf("step %s names needs %v, which send it to a fleet machine; the engine's pipeline runs on the cluster alone", step.Key, step.Needs)
				}
			}

			for _, step := range byName[manifestStepDBTests] {
				got, ok := step.Services[manifestPostgresService]
				if len(step.Services) != 1 || !ok || got.Image != postgres.Image || got.Ready != postgres.Ready || !maps.Equal(got.Env, postgres.Env) {
					t.Errorf("step %s carries services %v, want only %q (%s): the db-gated trees test against that sidecar",
						step.Key, step.Services, manifestPostgresService, postgres.Image)
				}
			}
			for _, name := range o.runs {
				steps := byName[name]
				if len(steps) == 0 {
					t.Errorf("step %s is not planned; the manifest no longer runs what this opening expects", name)
				}
				for _, step := range steps {
					if step.Skip != nil {
						t.Errorf("step %s is skipped (%s: %s), and this opening must run it", step.Key, step.Skip.Code, step.Skip.Reason)
					}
				}
			}
			for _, name := range o.skips {
				steps := byName[name]
				if len(steps) == 0 {
					t.Errorf("step %s is not planned; a skipped step is still planned, so the manifest lost it", name)
				}
				for _, step := range steps {
					if step.Skip == nil {
						t.Errorf("step %s runs, and this opening must skip it: nothing it reads changed", step.Key)
					}
				}
			}

			if mode, _ := pipelines.ModeFor(o.event); mode == pipelines.ModeFull {
				checkEngineTestsPartitionEveryPackage(t, spec, graph, byName)
			}
		})
	}
}

// checkEngineTestsPartitionEveryPackage holds a full run's two test steps to
// every package of the graph between them, each exactly once, both split into
// shards. Which side a package lands on is the steps' `only:`;
// TestEngineManifestDBStepReachesItsService holds db-tests to the db-gated one.
func checkEngineTestsPartitionEveryPackage(t *testing.T, spec *pipelines.Spec, graph *pipelines.Graph, byName map[string][]pipelines.Step) {
	t.Helper()
	seen := map[string]string{}
	for _, name := range []string{manifestStepGoTests, manifestStepDBTests} {
		declared, _ := engineDeclaredStep(spec, name)
		if n := len(byName[name]); n < 2 || n > declared.Shards {
			t.Errorf("a full run plans %s as %d step(s), want 2 to %d shards", name, n, declared.Shards)
		}
		for _, step := range byName[name] {
			for _, pkg := range step.Packages {
				if other, dup := seen[pkg]; dup {
					t.Errorf("%s runs in both %s and %s", pkg, other, step.Key)
				}
				seen[pkg] = step.Key
			}
		}
	}
	var missing []string
	for _, p := range graph.Packages() {
		if _, ok := seen[p.ImportPath]; !ok {
			missing = append(missing, p.ImportPath)
		}
	}
	if len(missing) > 0 {
		t.Errorf("a full run tests %d package(s) in neither go-tests nor db-tests, first %s", len(missing), missing[0])
	}
}

// The Postgres sidecar is pinned by digest, always, and is the very image, user,
// password and database ci.yml's db-tests lane runs: the pipeline and the GitHub
// lane test against one image until ci.yml retires.
func TestEngineManifestPostgresServiceIsPinnedByDigest(t *testing.T) {
	spec := engineManifest(t).Pipeline
	postgres, ok := spec.Services[manifestPostgresService]
	if !ok {
		t.Fatalf("the pipeline declares no %q service", manifestPostgresService)
	}
	m := manifestImageByDigest.FindStringSubmatch(postgres.Image)
	if m == nil || m[1] != manifestPostgresRepository {
		t.Fatalf("services.%s.image is %q; it must be %s@sha256:<digest>. A tag moves under every run that names it.",
			manifestPostgresService, postgres.Image, manifestPostgresRepository)
	}
	if strings.TrimSpace(postgres.Ready) == "" {
		t.Errorf("services.%s declares no ready probe, so a step could start before its database accepts a connection", manifestPostgresService)
	}

	lane, ok := readManifestCIWorkflow(t).Jobs["db-tests"].Services["postgres"]
	if !ok {
		t.Fatal("ci.yml's db-tests job declares no postgres service; if the lane moved, point this test at it rather than deleting it")
	}
	if postgres.Image != lane.Image {
		t.Errorf("the pipeline's Postgres is %s and ci.yml's db-tests lane runs %s; bump both in one change, or the two test against different databases",
			postgres.Image, lane.Image)
	}
	for _, key := range []string{"POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB"} {
		if postgres.Env[key] == "" || postgres.Env[key] != lane.Env[key] {
			t.Errorf("services.%s.env.%s is %q and ci.yml's db-tests lane sets %q", manifestPostgresService, key, postgres.Env[key], lane.Env[key])
		}
	}
}

// The toolchain image is pinned by digest. Until build-toolchain-image.yml has
// run on main there is no digest to pin, and the manifest names the first tag;
// that one reference skips, and every other one that is not a digest fails.
func TestEngineManifestToolchainImageIsPinnedByDigest(t *testing.T) {
	image := engineManifest(t).Pipeline.Image
	if image == manifestToolchainFirstTag {
		t.Skip("memql-toolchain is named by tag until build-toolchain-image.yml has run on main; pin its digest (follow-up to epic memql#5478)")
	}
	m := manifestImageByDigest.FindStringSubmatch(image)
	if m == nil || m[1] != manifestToolchainRepository {
		t.Fatalf("pipeline.image is %q; it must be %s@sha256:<digest>, the digest build-toolchain-image.yml's run summary prints. "+
			"Only the first tag, %s, may stand in for it, and only until that digest exists.",
			image, manifestToolchainRepository, manifestToolchainFirstTag)
	}
}

// select.dbGated is scripts/ci/db-gated-packages.sh's set, asked of the script
// itself: the one canonical list, which ci.yml's planner and scripts/cidb read
// too. A tree it gains that the manifest lacks runs in go-tests with no
// database, where its tests skip.
func TestEngineManifestDBGatedTreesAreTheScriptsTrees(t *testing.T) {
	spec := engineManifest(t).Pipeline
	out, err := exec.Command("bash", path.Join("scripts", "ci", "db-gated-packages.sh"), "--trees").Output()
	if err != nil {
		t.Fatalf("scripts/ci/db-gated-packages.sh --trees: %v", err)
	}
	want := strings.Fields(string(out))
	if len(want) == 0 {
		t.Fatal("scripts/ci/db-gated-packages.sh --trees printed nothing, so this comparison would prove nothing")
	}
	var got []string
	if spec.Select != nil {
		got = slices.Clone(spec.Select.DBGated)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("select.dbGated is not scripts/ci/db-gated-packages.sh --trees:\n  missing from the manifest: %v\n  not in the script:         %v\n"+
			"Copy the script's list into select.dbGated; the script is the canonical set.",
			engineSetMinus(want, got), engineSetMinus(got, want))
	}
}

// The gate-inputs step runs the packages ci.yml's planner adds whenever a change
// is not Go source (the plan step's GATE_PACKAGES, held to go-checks' own
// gate-inputs step by scripts/dev/gate_inputs_lane_scope_test.go): their tests
// read repository files by path, which no import graph can see.
func TestEngineManifestGateStepRunsThePlannersGatePackages(t *testing.T) {
	spec := engineManifest(t).Pipeline
	step, ok := engineDeclaredStep(spec, manifestStepGateInputs)
	if !ok {
		t.Fatalf("the pipeline has no %s step, so a pull request changing only docs or manifests runs no gate", manifestStepGateInputs)
	}
	var got []string
	for _, line := range strings.Split(step.Run, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "go" || fields[1] != "test" {
			continue
		}
		for _, f := range fields[2:] {
			if strings.HasPrefix(f, "./") {
				got = append(got, f)
			}
		}
	}
	var want []string
	for _, s := range readManifestCIWorkflow(t).Jobs["plan"].Steps {
		if s.ID == "plan" {
			want = strings.Fields(s.Env["GATE_PACKAGES"])
		}
	}
	if len(want) == 0 || len(got) == 0 {
		t.Fatalf("could not read both lists (ci.yml's GATE_PACKAGES %v, the %s step's %v); this comparison cannot pass over nothing",
			want, manifestStepGateInputs, got)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the %s step runs %v and ci.yml's planner adds %v; make them one list, or a gate runs on a pull request in one and not the other",
			manifestStepGateInputs, got, want)
	}
}

// The db step's DSN names the sidecar it declares -- the pod's own localhost,
// the service's user, password and database -- and MEMQL_REQUIRE_DB turns a
// database it cannot reach into a failure rather than a step of skips.
func TestEngineManifestDBStepReachesItsService(t *testing.T) {
	spec := engineManifest(t).Pipeline
	step, ok := engineDeclaredStep(spec, manifestStepDBTests)
	if !ok {
		t.Fatalf("the pipeline has no %s step, so no db-gated package runs with a database", manifestStepDBTests)
	}
	if step.Only != pipelines.OnlyDBGated || !slices.Contains(step.Services, manifestPostgresService) {
		t.Errorf("step %s is only %q with services %v; it must take the db-gated packages (only: %s) beside %q",
			manifestStepDBTests, step.Only, step.Services, pipelines.OnlyDBGated, manifestPostgresService)
	}
	postgres := spec.Services[manifestPostgresService]

	assignment := regexp.MustCompile(`MEMQL_DATABASE_DSN=['"]?([^'"\s]+)`).FindStringSubmatch(step.Run)
	if assignment == nil {
		t.Fatalf("step %s sets no MEMQL_DATABASE_DSN, so its tests look for whatever database the default names", manifestStepDBTests)
	}
	dsn, err := url.Parse(assignment[1])
	if err != nil {
		t.Fatalf("step %s's MEMQL_DATABASE_DSN does not parse: %v", manifestStepDBTests, err)
	}
	password, _ := dsn.User.Password()
	switch {
	case dsn.Hostname() != "localhost" && dsn.Hostname() != "127.0.0.1":
		t.Errorf("the DSN names host %q; a sidecar shares the step's pod network, so its database is on localhost", dsn.Hostname())
	case dsn.Port() != "" && dsn.Port() != "5432":
		t.Errorf("the DSN names port %s; the %s sidecar listens on 5432", dsn.Port(), manifestPostgresService)
	case dsn.User.Username() != postgres.Env["POSTGRES_USER"]:
		t.Errorf("the DSN's user %q is not the %s service's POSTGRES_USER %q",
			dsn.User.Username(), manifestPostgresService, postgres.Env["POSTGRES_USER"])
	case password != postgres.Env["POSTGRES_PASSWORD"]:
		t.Errorf("the DSN's password is not the %s service's POSTGRES_PASSWORD", manifestPostgresService)
	case strings.TrimPrefix(dsn.Path, "/") != postgres.Env["POSTGRES_DB"]:
		t.Errorf("the DSN names database %q and the %s service creates %q", strings.TrimPrefix(dsn.Path, "/"), manifestPostgresService, postgres.Env["POSTGRES_DB"])
	}
	if !regexp.MustCompile(`(^|\s)MEMQL_REQUIRE_DB=1(\s|$)`).MatchString(step.Run) {
		t.Errorf("step %s does not set MEMQL_REQUIRE_DB=1, so a database it cannot reach turns every db-gated test into a skip and the step green",
			manifestStepDBTests)
	}
}

// manifestCIWorkflow is the slice of .github/workflows/ci.yml the manifest
// copies: the db-tests lane's service and the plan step's environment.
type manifestCIWorkflow struct {
	Jobs map[string]struct {
		Services map[string]struct {
			Image string            `yaml:"image"`
			Env   map[string]string `yaml:"env"`
		} `yaml:"services"`
		Steps []struct {
			ID  string            `yaml:"id"`
			Env map[string]string `yaml:"env"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readManifestCIWorkflow(t *testing.T) manifestCIWorkflow {
	t.Helper()
	raw, err := os.ReadFile(path.Join(".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	var wf manifestCIWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	return wf
}

// engineSetMinus is a's entries that b lacks.
func engineSetMinus(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}
