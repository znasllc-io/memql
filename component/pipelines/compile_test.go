package pipelines

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// selectorStub is a Selector with fixed answers, standing in for the
// GraphSelector Task 3 builds over a graph read from source, so Compile is
// tested on values alone.
type selectorStub struct {
	all      []string
	affected Selection
	dirs     map[string]string
}

func (s selectorStub) All() []string                  { return s.all }
func (s selectorStub) Affected() Selection            { return s.affected }
func (s selectorStub) DirOf(importPath string) string { return s.dirs[importPath] }

// The repository the record's example runs against: eight packages, two of
// them under the db-gated trees, one (databasex) beside a db-gated tree's name
// without being under it, and one (sense) new enough to be unmeasured.
const (
	d7Root      = "example.test/app"
	d7Tool      = "example.test/app/cmd/tool"
	d7Database  = "example.test/app/component/database"
	d7DatabaseX = "example.test/app/component/databasex"
	d7GRPC      = "example.test/app/component/grpc"
	d7Memql     = "example.test/app/component/memql"
	d7Sense     = "example.test/app/component/memql/sense"
	d7Env       = "example.test/app/core/env"

	d7Image    = "ghcr.io/znasllc-io/memql-toolchain@sha256:..."
	d7Postgres = "ghcr.io/znasllc-io/timescaledb-pgvector@sha256:..."
)

var d7Dirs = map[string]string{
	d7Root:      ".",
	d7Tool:      "cmd/tool",
	d7Database:  "component/database",
	d7DatabaseX: "component/databasex",
	d7GRPC:      "component/grpc",
	d7Memql:     "component/memql",
	d7Sense:     "component/memql/sense",
	d7Env:       "core/env",
}

var d7Timings = map[string]float64{
	d7Root:      111.40,
	d7Tool:      445.63,
	d7Database:  20,
	d7DatabaseX: 60,
	d7GRPC:      119.09,
	d7Memql:     496,
	d7Env:       3,
}

func d7Selector(affected Selection) selectorStub {
	return selectorStub{all: slices.Sorted(maps.Keys(d7Dirs)), affected: affected, dirs: d7Dirs}
}

func testPackagePolicies(spec *Spec, mode Mode, selector Selector) map[string]PackagePolicy {
	if !NeedsSelector(spec) || selector == nil {
		return nil
	}
	policies := map[string]PackagePolicy{}
	affected := selector.Affected()
	for _, stage := range spec.Stages {
		for _, step := range stage.Steps {
			if step.Packages == "" {
				continue
			}
			coverage := PackageCoverageAffected
			if mode != ModeAffected || step.Packages == PackagesAll || affected.Full {
				coverage = PackageCoverageAll
			}
			filter := step.Only
			if filter == "" {
				filter = PackageFilterAll
			}
			policies[StepKey(stage.Name, step.Name)] = PackagePolicy{Coverage: coverage, Filter: filter}
		}
	}
	return policies
}

// d7Command is a command step of the record's example as Compile renders it
// before any packages are chosen.
func d7Command(stage, name, run string, dependsOn ...string) Step {
	return Step{
		Execution: ExecutionContainer, Placement: PlacementCluster,
		Key: StepKey(stage, name), Stage: stage, Name: name, Kind: StepCommand, Run: run,
		Image: d7Image, Caches: []string{"go", "npm"},
		TimeoutSeconds: int(DefaultStepTimeout.Seconds()), DependsOn: dependsOn,
	}
}

func d7Shard(step Step, index, count int, packages ...string) Step {
	step.Key = fmt.Sprintf("%s#%d", step.Key, index)
	step.Shard = ShardRef{Index: index, Count: count}
	step.Packages = packages
	return step
}

var (
	d7BuildVet = d7Command("checks", "build-vet", "go build ./... && go vet ./...")
	d7GoTests  = d7Command("tests", "go-tests", "go test $MEMQL_PACKAGES", "checks.build-vet")
	d7DBTests  = func() Step {
		s := d7Command("tests", "db-tests", "MEMQL_REQUIRE_DB=1 go test $MEMQL_PACKAGES", "checks.build-vet")
		s.Services = map[string]Service{"postgres": {Image: d7Postgres}}
		return s
	}()
	d7OSChecks = func() Step {
		s := d7Command("tests", "os-checks", "make os-typecheck os-test os-build", "checks.build-vet")
		s.Placement = PlacementFleet
		s.Platform = "linux/amd64"
		return s
	}()
)

func mustCompilePlan(t *testing.T, spec *Spec, in CompileInput) Plan {
	t.Helper()
	if NeedsSelector(spec) && in.PackagePolicies == nil && in.Selector != nil {
		// Most compiler tests exercise graph mechanics rather than DSL policy.
		// workflow_test.go runs the pinned selectors against policy cases.
		in.PackagePolicies = testPackagePolicies(spec, in.Mode, in.Selector)
	}
	if in.StageSelection == nil {
		// Most compiler tests exercise step mechanics, not DSL stage policy.
		// The dedicated stage-selection test supplies the policy result.
		selection := StageSelection{Included: make([]string, 0, len(spec.Stages))}
		for _, stage := range spec.Stages {
			selection.Included = append(selection.Included, stage.Name)
		}
		in.StageSelection = &selection
	}
	if in.BucketSelection == nil && NeedsBucketSelection(spec) {
		included := []string{}
		if spec.Select != nil {
			included = slices.Sorted(maps.Keys(spec.Select.Buckets))
		}
		in.BucketSelection = &BucketSelection{Included: included}
	}
	plan, r := Compile(spec, in)
	if r != nil {
		t.Fatalf("Compile refused: %v", r)
	}
	return plan
}

func planStageNames(p Plan) []string {
	var out []string
	for _, s := range p.Stages {
		out = append(out, s.Name)
	}
	return out
}

func planStepByKey(t *testing.T, p Plan, key string) Step {
	t.Helper()
	for _, s := range p.Steps() {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("the plan has no step %q; it has %v", key, planStepKeys(p.Steps()))
	return Step{}
}

func planStepKeys(steps []Step) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Key)
	}
	return out
}

func diffPlanSteps(t *testing.T, got, want []Step) {
	t.Helper()
	if !reflect.DeepEqual(planStepKeys(got), planStepKeys(want)) {
		t.Fatalf("step keys = %v\nwant        %v", planStepKeys(got), planStepKeys(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("step %s =\n%+v\nwant\n%+v", want[i].Key, got[i], want[i])
		}
	}
}

// (a) A pull request: affected mode. The push-only stages are absent; go-tests
// is split by the timing table, db-tests narrowed to the db-gated trees; the
// os bucket gates os-checks on what changed.
func TestCompileThePullRequestRunOfTheRecordsExample(t *testing.T) {
	affected := Selection{
		Reason:   "2 packages changed",
		Seeds:    []string{d7Memql, d7GRPC},
		Packages: []string{d7DatabaseX, d7GRPC, d7Memql, d7Sense, d7Env},
	}
	in := CompileInput{
		Mode: ModeAffected, Event: EventPullRequest, Compute: ComputeClusterAndFleet,
		Selector: d7Selector(affected), Timings: d7Timings,
		BucketSelection: &BucketSelection{},
		StageSelection:  &StageSelection{Included: []string{"checks", "tests"}},
	}
	plan := mustCompilePlan(t, d7ExampleSpec(), in)

	if plan.Mode != ModeAffected || plan.Event != EventPullRequest {
		t.Errorf("plan is %s/%s, want affected/pull_request", plan.Mode, plan.Event)
	}
	if got, want := planStageNames(plan), []string{"checks", "tests"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stages = %v, want %v: deploy and notify run on push only", got, want)
	}
	if !reflect.DeepEqual(plan.Stages[1].Needs, []string{"checks"}) {
		t.Errorf("tests needs = %v, want the stage's needs as written", plan.Stages[1].Needs)
	}

	osSkipped := d7OSChecks
	osSkipped.Skip = &Skip{Code: CodeNotAffected, Reason: "No change under bucket os."}
	diffPlanSteps(t, plan.Steps(), []Step{
		d7BuildVet,
		// memql (496), grpc (119.09), databasex (60) and env (3) open the four
		// shards; sense, unmeasured, joins the lightest, env's.
		d7Shard(d7GoTests, 1, 4, d7Memql),
		d7Shard(d7GoTests, 2, 4, d7GRPC),
		d7Shard(d7GoTests, 3, 4, d7DatabaseX),
		d7Shard(d7GoTests, 4, 4, d7Sense, d7Env),
		// Two affected packages lie under the db-gated trees; databasex only
		// shares a prefix with component/database. Two packages, two shards.
		d7Shard(d7DBTests, 1, 2, d7Memql),
		d7Shard(d7DBTests, 2, 2, d7Sense),
		osSkipped,
	})

	// A bucket's `!` glob means "except": a change only under the excluded
	// tree leaves os-checks skipped, where paths-filter's OR would run it.
	excluding := d7ExampleSpec()
	excluding.Select.Buckets["os"] = append(excluding.Select.Buckets["os"], "!clients/vendor/**")
	vendorOnly := in
	vendorOnly.BucketSelection = &BucketSelection{}
	if got := planStepByKey(t, mustCompilePlan(t, excluding, vendorOnly), "tests.os-checks"); got.Skip == nil {
		t.Errorf("os-checks ran on a change only under an excluded path: %+v", got)
	}

	// The same run with a change under clients/: os-checks runs.
	in.BucketSelection = &BucketSelection{Included: []string{"os"}}
	plan = mustCompilePlan(t, d7ExampleSpec(), in)
	if got := planStepByKey(t, plan, "tests.os-checks"); !reflect.DeepEqual(got, d7OSChecks) {
		t.Errorf("os-checks with a change under clients/ =\n%+v\nwant it to run:\n%+v", got, d7OSChecks)
	}
}

// (b) A push to the default branch: full mode. Every stage is present, the
// packages are every package (the affected selection is not consulted), the
// bucket gates nothing, and notify compiles to one notify step.
func TestCompileThePushRunOfTheRecordsExample(t *testing.T) {
	in := CompileInput{
		Mode: ModeFull, Event: EventPush, Compute: ComputeClusterAndFleet,
		// A selection that would answer differently, to show full mode never reads it.
		Selector: d7Selector(Selection{Packages: []string{d7Env}}), Timings: d7Timings,
		BucketSelection: &BucketSelection{Included: []string{"os"}},
	}
	plan := mustCompilePlan(t, d7ExampleSpec(), in)

	if got, want := planStageNames(plan), []string{"checks", "tests", "deploy", "notify"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stages = %v, want %v", got, want)
	}
	testsKeys := []string{
		"tests.go-tests#1", "tests.go-tests#2", "tests.go-tests#3", "tests.go-tests#4",
		"tests.db-tests#1", "tests.db-tests#2", "tests.db-tests#3", "tests.os-checks",
	}
	verify := d7Command("deploy", "verify-rollout",
		"memql-verify --target=https://api.<domain> --version=$MEMQL_VERSION", testsKeys...)
	notify := Step{
		Key: "notify.notify", Stage: "notify", Name: "notify", Kind: StepNotify,
		Channel: "znas-instance", TimeoutSeconds: int(DefaultStepTimeout.Seconds()),
		DependsOn: []string{"deploy.verify-rollout"},
	}
	diffPlanSteps(t, plan.Steps(), []Step{
		d7BuildVet,
		// memql, tool, grpc and root open the shards; databasex joins root's,
		// then database, env and the unmeasured sense each join grpc's, the
		// lightest at every turn.
		d7Shard(d7GoTests, 1, 4, d7Memql),
		d7Shard(d7GoTests, 2, 4, d7Tool),
		d7Shard(d7GoTests, 3, 4, d7Database, d7GRPC, d7Sense, d7Env),
		d7Shard(d7GoTests, 4, 4, d7Root, d7DatabaseX),
		// Three packages lie under the db-gated trees: three shards, not four.
		d7Shard(d7DBTests, 1, 3, d7Memql),
		d7Shard(d7DBTests, 2, 3, d7Database),
		d7Shard(d7DBTests, 3, 3, d7Sense),
		d7OSChecks,
		verify,
		notify,
	})

	for i := range 20 {
		if again := mustCompilePlan(t, d7ExampleSpec(), in); !reflect.DeepEqual(again, plan) {
			t.Fatalf("compile %d differs from the first: Compile is not deterministic", i)
		}
	}
}

// (c) only: db-gated and only: not-db-gated split the candidates between them,
// with no package in both and none in neither. A tree is matched on a "/"
// boundary, and "." is the root package alone, as the CI bridge reads it.
func TestCompileOnlySplitsTheCandidatesByTheDBGatedTrees(t *testing.T) {
	compileSplit := func(trees ...string) (db, rest []string) {
		spec := &Spec{
			Select: &Select{Go: SelectImportGraph, DBGated: trees},
			Stages: []StageSpec{{Name: "tests", Steps: []StepSpec{
				{Name: "db", Run: "go test $MEMQL_PACKAGES", Packages: PackagesAll, Only: OnlyDBGated},
				{Name: "unit", Run: "go test $MEMQL_PACKAGES", Packages: PackagesAll, Only: OnlyNotDBGated},
			}}},
		}
		plan := mustCompilePlan(t, spec, CompileInput{Mode: ModeAffected, Event: EventPullRequest, Selector: d7Selector(Selection{})})
		return planStepByKey(t, plan, "tests.db").Packages, planStepByKey(t, plan, "tests.unit").Packages
	}

	db, rest := compileSplit("component/memql", "./component/database/")
	if want := []string{d7Database, d7Memql, d7Sense}; !reflect.DeepEqual(db, want) {
		t.Errorf("db-gated = %v, want %v", db, want)
	}
	if want := []string{d7Root, d7Tool, d7DatabaseX, d7GRPC, d7Env}; !reflect.DeepEqual(rest, want) {
		t.Errorf("not-db-gated = %v, want %v", rest, want)
	}
	union := slices.Sorted(slices.Values(append(slices.Clone(db), rest...)))
	if !reflect.DeepEqual(union, slices.Sorted(maps.Keys(d7Dirs))) {
		t.Errorf("db-gated and not-db-gated together = %v, want every package exactly once", union)
	}

	db, rest = compileSplit(".")
	if !reflect.DeepEqual(db, []string{d7Root}) || len(rest) != len(d7Dirs)-1 {
		t.Errorf(`the tree "." selected %v; it is the root package alone`, db)
	}
}

// (d) A need routes a step to the fleet, which a cluster-only pipeline has not
// consented to. Absent compute means cluster.
func TestCompileRefusesANeedOnAClusterOnlyPipeline(t *testing.T) {
	for _, compute := range []Compute{ComputeCluster, ""} {
		selector := d7Selector(Selection{Packages: []string{d7Memql}})
		_, r := Compile(d7ExampleSpec(), CompileInput{
			Mode: ModeAffected, Event: EventPullRequest, Compute: compute,
			Selector:        selector,
			PackagePolicies: testPackagePolicies(d7ExampleSpec(), ModeAffected, selector),
			BucketSelection: &BucketSelection{Included: []string{"os"}},
			StageSelection:  &StageSelection{Included: []string{"checks", "tests"}},
		})
		if r == nil || r.Code != CodeFleetNotConsented || r.Scope != "tests/os-checks" {
			t.Errorf("compute %q: Compile = %v, want pipeline_fleet_not_consented (tests/os-checks)", compute, r)
			continue
		}
		if !strings.Contains(r.Detail, "fleet placement") {
			t.Errorf("detail %q does not explain placement", r.Detail)
		}
	}

	// Refused even on a pull request whose change would skip the step: the
	// answer must not depend on which files a change happened to touch.
	_, r := Compile(d7ExampleSpec(), CompileInput{
		Mode: ModeAffected, Event: EventPullRequest, Compute: ComputeCluster,
		Selector:        d7Selector(Selection{}),
		PackagePolicies: testPackagePolicies(d7ExampleSpec(), ModeAffected, d7Selector(Selection{})),
		BucketSelection: &BucketSelection{},
		StageSelection:  &StageSelection{Included: []string{"checks", "tests"}},
	})
	if r == nil || r.Code != CodeFleetNotConsented {
		t.Errorf("with os-checks skipped by its bucket: Compile = %v, want pipeline_fleet_not_consented", r)
	}
}

// (e) A secret is a name the pipeline's owner allows, or the run is refused
// before any executor could be handed it. Only the stages that run are asked.
func TestCompileRefusesASecretTheOwnerDidNotAllow(t *testing.T) {
	spec := d7ExampleSpec()
	spec.Stages[2].Steps[0].Secrets = []string{"DEPLOY_TOKEN"}
	push := CompileInput{
		Mode: ModeFull, Event: EventPush, Compute: ComputeClusterAndFleet,
		Selector: d7Selector(Selection{}), PackagePolicies: testPackagePolicies(spec, ModeFull, d7Selector(Selection{})),
		AllowedSecrets:  []string{"NPM_TOKEN"},
		BucketSelection: &BucketSelection{Included: []string{"os"}},
		StageSelection:  &StageSelection{Included: []string{"checks", "tests", "deploy", "notify"}},
	}

	_, r := Compile(spec, push)
	if r == nil || r.Code != CodeSecretNotAllowed || r.Scope != "deploy/verify-rollout" {
		t.Fatalf("Compile = %v, want pipeline_secret_not_allowed (deploy/verify-rollout)", r)
	}
	if !strings.Contains(r.Detail, "DEPLOY_TOKEN") {
		t.Errorf("detail %q does not name the secret", r.Detail)
	}

	push.AllowedSecrets = []string{"NPM_TOKEN", "DEPLOY_TOKEN"}
	plan := mustCompilePlan(t, spec, push)
	if got := planStepByKey(t, plan, "deploy.verify-rollout").Secrets; !reflect.DeepEqual(got, []string{"DEPLOY_TOKEN"}) {
		t.Errorf("verify-rollout secrets = %v, want the names as written", got)
	}

	// A pull request does not plan the deploy stage, so its secret is not asked.
	mustCompilePlan(t, spec, CompileInput{
		Mode: ModeAffected, Event: EventPullRequest, Compute: ComputeClusterAndFleet,
		Selector:       d7Selector(Selection{}),
		StageSelection: &StageSelection{Included: []string{"checks", "tests"}},
	})
}

// (f) Nothing affected: a step that selects packages is one skipped step, with
// its reason in words, never zero steps and never a step with no packages.
func TestCompileSkipsAPackageStepWhenNothingIsAffected(t *testing.T) {
	plan := mustCompilePlan(t, d7ExampleSpec(), CompileInput{
		Mode: ModeAffected, Event: EventPullRequest, Compute: ComputeClusterAndFleet,
		Selector: d7Selector(Selection{Reason: "no Go package changed"}), Timings: d7Timings,
		BucketSelection: &BucketSelection{},
		StageSelection:  &StageSelection{Included: []string{"checks", "tests"}},
	})
	goSkipped := d7GoTests
	goSkipped.Skip = &Skip{Code: CodeNotAffected, Reason: "No affected Go packages."}
	dbSkipped := d7DBTests
	dbSkipped.Skip = &Skip{Code: CodeNotAffected, Reason: "No db-gated packages are affected."}
	osSkipped := d7OSChecks
	osSkipped.Skip = &Skip{Code: CodeNotAffected, Reason: "No change under bucket os."}
	diffPlanSteps(t, plan.Steps(), []Step{d7BuildVet, goSkipped, dbSkipped, osSkipped})
}

// (g) An affected selection that came back Full (go.mod changed, the graph is
// incomplete) selects every package, exactly as full mode does.
func TestCompileAFullSelectionSelectsEveryPackage(t *testing.T) {
	affected := mustCompilePlan(t, d7ExampleSpec(), CompileInput{
		Mode: ModeAffected, Event: EventPullRequest, Compute: ComputeClusterAndFleet,
		Selector: d7Selector(Selection{Full: true, Reason: "go.mod changed"}), Timings: d7Timings,
		BucketSelection: &BucketSelection{Included: []string{"os"}},
	})
	full := mustCompilePlan(t, d7ExampleSpec(), CompileInput{
		Mode: ModeFull, Event: EventMergeGroup, Compute: ComputeClusterAndFleet,
		Selector: d7Selector(Selection{}), Timings: d7Timings,
		BucketSelection: &BucketSelection{Included: []string{"os"}},
	})
	for _, key := range []string{"tests.go-tests#1", "tests.go-tests#2", "tests.go-tests#3", "tests.go-tests#4", "tests.db-tests#3"} {
		if got, want := planStepByKey(t, affected, key).Packages, planStepByKey(t, full, key).Packages; !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v under a Full selection, want %v as in full mode", key, got, want)
		}
	}
}

// A notify stage may carry its own links (epic memql#5480: the docs link a
// release's message offers beside the run page). They ride the compiled notify
// step, in the order written, so the driver that executes the stage renders
// them and the runner is never asked about them.
func TestCompileCarriesANotifyStagesLinks(t *testing.T) {
	in := CompileInput{
		Mode: ModeFull, Event: EventPush, Compute: ComputeClusterAndFleet,
		Selector: d7Selector(Selection{}), Timings: d7Timings,
	}
	spec := d7ExampleSpec()
	spec.Stages[3].Links = []Link{
		{Label: "Docs", URL: "https://memql.io/docs/"},
		{Label: "Changelog", URL: "https://memql.io/changelog"},
	}
	plan := mustCompilePlan(t, spec, in)

	notify := planStepByKey(t, plan, "notify.notify")
	want := []Link{{Label: "Docs", URL: "https://memql.io/docs/"}, {Label: "Changelog", URL: "https://memql.io/changelog"}}
	if !reflect.DeepEqual(notify.Links, want) {
		t.Fatalf("the notify step's links = %+v, want %+v", notify.Links, want)
	}
	for _, step := range plan.Steps() {
		if step.Key != "notify.notify" && step.Links != nil {
			t.Errorf("step %s carries links %+v; only a notify step does", step.Key, step.Links)
		}
	}

	// A stage with none compiles to a step with none -- nil, so the step's wire
	// form is the one it had before links existed.
	plain := mustCompilePlan(t, d7ExampleSpec(), in)
	if got := planStepByKey(t, plain, "notify.notify").Links; got != nil {
		t.Errorf("a notify stage with no links compiled to links %+v, want nil", got)
	}

	// The step owns its links: changing one changes nothing in the spec.
	notify.Links[0].URL = "https://changed.example.test/"
	if got := spec.Stages[3].Links[0].URL; got != "https://memql.io/docs/" {
		t.Errorf("changing a compiled step's link changed the spec to %q", got)
	}
}

// Links belong to a notify stage and to nothing else, so a command step never
// compiles with any, and Compile refuses what Validate refuses.
func TestCompileRefusesLinksValidateRefuses(t *testing.T) {
	spec := d7ExampleSpec()
	spec.Stages[3].Links = linksOf(6)
	plan, r := Compile(spec, CompileInput{Mode: ModeFull, Event: EventPush, Compute: ComputeClusterAndFleet, Selector: d7Selector(Selection{})})
	if r == nil || r.Code != CodeStageInvalid || r.Scope != "notify" {
		t.Errorf("Compile = %v, want pipeline_stage_invalid (notify)", r)
	}
	if !reflect.DeepEqual(plan, Plan{}) {
		t.Errorf("a refused compile returned a plan: %+v", plan)
	}

	// And a stage with steps refuses links even when its event never plans it:
	// a typo in the deploy stage fails the pull request's run too.
	spec = d7ExampleSpec()
	spec.Stages[2].Links = linksOf(1)
	if _, r := Compile(spec, CompileInput{Mode: ModeAffected, Event: EventPullRequest, Compute: ComputeClusterAndFleet, Selector: d7Selector(Selection{})}); r == nil || r.Scope != "deploy" {
		t.Errorf("a pull request's Compile = %v, want a refusal of the deploy stage's links", r)
	}
}

// cloneCompiledStep is what a shard is made with: it must not hand two steps
// one slice, and a step's links are a slice like the rest.
func TestCloneCompiledStepSharesNoLinks(t *testing.T) {
	original := Step{Links: []Link{{Label: "Docs", URL: "https://memql.io/docs/"}}}
	clone := cloneCompiledStep(original)
	clone.Links[0].Label = "changed"
	if original.Links[0].Label != "Docs" {
		t.Errorf("the clone shares its links with the step it came from: %+v", original.Links)
	}
	if got := cloneCompiledStep(Step{}).Links; got != nil {
		t.Errorf("a step with no links cloned to %+v, want nil", got)
	}
}

func TestCompileRefusesAPackageStepWithNoSelector(t *testing.T) {
	_, r := Compile(d7ExampleSpec(), CompileInput{
		Mode: ModeFull, Event: EventPush, Compute: ComputeClusterAndFleet,
		BucketSelection: &BucketSelection{Included: []string{"os"}},
		StageSelection:  &StageSelection{Included: []string{"checks", "tests", "deploy", "notify"}},
	})
	if r == nil || r.Code != CodeSelectMissing || r.Scope != "tests/go-tests" {
		t.Errorf("Compile = %v, want pipeline_select_missing (tests/go-tests)", r)
	}
}

// A spec Validate refuses is refused by Compile, unchanged, with no plan.
func TestCompileReturnsValidatesRefusal(t *testing.T) {
	spec := d7ExampleSpec()
	spec.Stages[1].Steps[2].Needs = map[string]bool{"network": true}
	plan, r := Compile(spec, CompileInput{Mode: ModeFull, Event: EventPush, Compute: ComputeClusterAndFleet})
	if want := Validate(spec); !reflect.DeepEqual(r, want) || r == nil {
		t.Errorf("Compile refused %v, want Validate's %v", r, want)
	}
	if !reflect.DeepEqual(plan, Plan{}) {
		t.Errorf("a refused compile returned a plan: %+v", plan)
	}
}

// Compile consumes a stage set selected by the workflow policy and preserves
// dependency order among the included stages.
func TestCompileConsumesExplicitStageSelection(t *testing.T) {
	spec := &Spec{Stages: []StageSpec{
		{Name: "lint", Steps: []StepSpec{{Name: "vet", Run: "go vet ./..."}}},
		{Name: "slow", On: []string{"full"}, Steps: []StepSpec{{Name: "e2e", Run: "make e2e"}}},
		{Name: "queue", On: []string{"merge_group"}, Steps: []StepSpec{{Name: "smoke", Run: "make smoke"}}},
		{Name: "report", Steps: []StepSpec{{Name: "sum", Run: "true"}}},
	}}
	for _, tc := range []struct {
		mode   Mode
		event  Event
		stages []string
		after  []string // what report depends on
	}{
		{ModeAffected, EventPullRequest, []string{"lint", "report"}, []string{"lint.vet"}},
		{ModeFull, EventPush, []string{"lint", "slow", "report"}, []string{"slow.e2e"}},
		{ModeFull, EventMergeGroup, []string{"lint", "slow", "queue", "report"}, []string{"queue.smoke"}},
	} {
		plan := mustCompilePlan(t, spec, CompileInput{
			Mode: tc.mode, Event: tc.event,
			StageSelection: &StageSelection{Included: tc.stages},
		})
		if got := planStageNames(plan); !reflect.DeepEqual(got, tc.stages) {
			t.Errorf("%s/%s: stages = %v, want %v", tc.mode, tc.event, got, tc.stages)
		}
		if got := planStepByKey(t, plan, "report.sum").DependsOn; !reflect.DeepEqual(got, tc.after) {
			t.Errorf("%s/%s: report depends on %v, want %v", tc.mode, tc.event, got, tc.after)
		}
	}
}

func TestCompileRefusesConditionalStagesWithoutDSLSelection(t *testing.T) {
	spec := &Spec{Stages: []StageSpec{{
		Name: "tests", On: []string{"pull_request"}, Steps: []StepSpec{{Name: "unit", Run: "go test"}},
	}}}
	plan, refusal := Compile(spec, CompileInput{Mode: ModeAffected, Event: EventPullRequest})
	if refusal == nil || refusal.Code != CodeStageInvalid || refusal.Scope != "selection" {
		t.Fatalf("missing stage selection was accepted: plan=%+v refusal=%+v", plan, refusal)
	}
	if !reflect.DeepEqual(plan, Plan{}) {
		t.Fatalf("refused compile returned a plan: %+v", plan)
	}
}

func TestCompileRejectsUnknownOrDuplicateDSLStageSelections(t *testing.T) {
	spec := &Spec{Stages: []StageSpec{{Name: "tests", Steps: []StepSpec{{Name: "unit", Run: "go test"}}}}}
	for _, tc := range []struct {
		name     string
		selected []string
	}{
		{name: "unknown", selected: []string{"deploy"}},
		{name: "duplicate", selected: []string{"tests", "tests"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, refusal := Compile(spec, CompileInput{StageSelection: &StageSelection{Included: tc.selected}})
			if refusal == nil || refusal.Code != CodeStageInvalid || refusal.Scope != "selection" {
				t.Fatalf("invalid selection was accepted: plan=%+v refusal=%+v", plan, refusal)
			}
		})
	}
}

func TestCompileTimeoutsAndASingleShard(t *testing.T) {
	spec := &Spec{
		Select: &Select{Go: SelectImportGraph},
		Stages: []StageSpec{{Name: "tests", Steps: []StepSpec{
			{Name: "unit", Run: "go test $MEMQL_PACKAGES", Packages: PackagesAffected, Shards: 4, Timeout: "45m"},
			{Name: "lint", Run: "go vet ./...", Timeout: "1h30m"},
			{Name: "history", Run: "verify-history", Timeout: "6h"},
			{Name: "default", Run: "verify-default"},
		}}},
	}
	plan := mustCompilePlan(t, spec, CompileInput{
		Mode: ModeAffected, Event: EventPullRequest,
		Selector: d7Selector(Selection{Packages: []string{d7GRPC}}),
	})
	// One package cannot be split: the step runs unsharded, under its own key,
	// and exports no MEMQL_SHARD.
	unit := planStepByKey(t, plan, "tests.unit")
	if unit.Shard != (ShardRef{}) || !reflect.DeepEqual(unit.Packages, []string{d7GRPC}) {
		t.Errorf("a one-package shard set compiled to %+v, want one unsharded step", unit)
	}
	if unit.TimeoutSeconds != 45*60 {
		t.Errorf("unit timeout = %ds, want 2700", unit.TimeoutSeconds)
	}
	if got := planStepByKey(t, plan, "tests.lint").TimeoutSeconds; got != 90*60 {
		t.Errorf("lint timeout = %ds, want 5400", got)
	}
	if got := planStepByKey(t, plan, "tests.history").TimeoutSeconds; got != 6*60*60 {
		t.Errorf("explicit long timeout = %ds, want 21600", got)
	}
	if got := planStepByKey(t, plan, "tests.default").TimeoutSeconds; got != 20*60 {
		t.Errorf("default timeout = %ds, want unchanged 1200", got)
	}
}

// Compile consumes the DSL's explicit bucket set and does not re-evaluate
// paths itself.
func TestCompileRunsAnIncludedBucketStep(t *testing.T) {
	plan := mustCompilePlan(t, d7ExampleSpec(), CompileInput{
		Mode: ModeAffected, Event: EventPullRequest, Compute: ComputeClusterAndFleet,
		Selector: d7Selector(Selection{Full: true}), BucketSelection: &BucketSelection{Included: []string{"os"}},
	})
	if step := planStepByKey(t, plan, "tests.os-checks"); step.Skip != nil {
		t.Errorf("included os-checks skipped (%s)", step.Skip.Reason)
	}
}

func TestNeedsSelector(t *testing.T) {
	if !NeedsSelector(d7ExampleSpec()) {
		t.Error("NeedsSelector(D7) = false; go-tests selects packages")
	}
	spec := d7ExampleSpec()
	spec.Stages[1].Steps = spec.Stages[1].Steps[2:]
	if NeedsSelector(spec) {
		t.Error("NeedsSelector = true for a spec whose steps select no packages")
	}
	if NeedsSelector(nil) {
		t.Error("NeedsSelector(nil) = true")
	}
}

func TestCompileRequiresExplicitPackagePolicy(t *testing.T) {
	input := CompileInput{
		Selector:       d7Selector(Selection{Packages: []string{d7Memql}}),
		StageSelection: &StageSelection{Included: []string{"checks", "tests", "deploy", "notify"}},
	}
	if _, refusal := Compile(d7ExampleSpec(), input); refusal == nil || refusal.Code != CodeSelectMissing || refusal.Scope != "selection" {
		t.Fatalf("Compile without package policy = %v, want a selection refusal", refusal)
	}
}

func TestCompileRefusesMissingUnknownOrUnsafePackagePolicy(t *testing.T) {
	spec := &Spec{Select: &Select{DBGated: []string{"component/memql"}}, Stages: []StageSpec{{Name: "tests", Steps: []StepSpec{{
		Name: "db", Run: "go test", Packages: PackagesAffected, Only: OnlyDBGated,
	}}}}}
	selector := d7Selector(Selection{Packages: []string{d7Memql}})
	cases := []struct {
		name     string
		policies map[string]PackagePolicy
		full     bool
	}{
		{name: "missing"},
		{name: "unknown coverage", policies: map[string]PackagePolicy{"tests.db": {Coverage: "mystery", Filter: PackageFilterDBGated}}},
		{name: "unknown filter", policies: map[string]PackagePolicy{"tests.db": {Coverage: PackageCoverageAffected, Filter: "mystery"}}},
		{name: "affected when graph requires full", full: true, policies: map[string]PackagePolicy{"tests.db": {Coverage: PackageCoverageAffected, Filter: PackageFilterDBGated}}},
		{name: "undeclared step", policies: map[string]PackagePolicy{"elsewhere.unit": {Coverage: PackageCoverageAll, Filter: PackageFilterAll}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selected := selector
			if tc.full {
				selected = d7Selector(Selection{Full: true, Packages: []string{d7Memql}})
			}
			_, refusal := Compile(spec, CompileInput{Selector: selected, PackagePolicies: tc.policies})
			if refusal == nil || (tc.name == "missing" && refusal.Code != CodeSelectMissing) {
				t.Fatalf("invalid package policy accepted: %v", refusal)
			}
		})
	}
}

func TestCompileRequiresAnExplicitBucketSelection(t *testing.T) {
	input := CompileInput{
		Mode: ModeAffected, Event: EventPullRequest, Compute: ComputeClusterAndFleet,
		Selector: d7Selector(Selection{}),
		PackagePolicies: map[string]PackagePolicy{
			"tests.go-tests": {Coverage: PackageCoverageAffected, Filter: PackageFilterAll},
			"tests.db-tests": {Coverage: PackageCoverageAffected, Filter: PackageFilterDBGated},
		},
		StageSelection: &StageSelection{Included: []string{"checks", "tests"}},
	}
	if _, refusal := Compile(d7ExampleSpec(), input); refusal == nil || refusal.Code != CodeSelectMissing || refusal.Scope != "selection" {
		t.Fatalf("Compile without bucket selection = %v, want a selection refusal", refusal)
	}

	input.BucketSelection = &BucketSelection{Included: []string{"unknown"}}
	if _, refusal := Compile(d7ExampleSpec(), input); refusal == nil || refusal.Code != CodeSelectMissing {
		t.Fatalf("Compile with unknown bucket = %v, want a selection refusal", refusal)
	}

	input.BucketSelection = &BucketSelection{Included: []string{"os", "os"}}
	if _, refusal := Compile(d7ExampleSpec(), input); refusal == nil || refusal.Code != CodeSelectMissing {
		t.Fatalf("Compile with duplicate bucket = %v, want a selection refusal", refusal)
	}
}

func TestPlanStepsFlattensInExecutionOrder(t *testing.T) {
	p := Plan{Stages: []PlanStage{
		{Name: "a", Steps: []Step{{Key: "a/1"}, {Key: "a/2"}}},
		{Name: "b"},
		{Name: "c", Steps: []Step{{Key: "c/1"}}},
	}}
	if got, want := planStepKeys(p.Steps()), []string{"a/1", "a/2", "c/1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Steps() = %v, want %v", got, want)
	}
}

// The plan owns its values: changing a compiled step changes neither the spec
// nor a sibling step, so a driver may annotate steps freely.
func TestCompileSharesNoStorageWithTheSpecOrBetweenSteps(t *testing.T) {
	spec := d7ExampleSpec()
	plan := mustCompilePlan(t, spec, CompileInput{
		Mode: ModeFull, Event: EventPush, Compute: ComputeClusterAndFleet,
		Selector: d7Selector(Selection{}), Timings: d7Timings,
	})
	first := planStepByKey(t, plan, "tests.db-tests#1")
	first.Caches[0] = "changed"
	first.DependsOn[0] = "changed"
	first.Services["postgres"] = Service{Image: "changed"}

	if spec.Caches[0] != "go" || spec.Services["postgres"].Image != d7Postgres {
		t.Error("changing a compiled step changed the spec")
	}
	second := planStepByKey(t, plan, "tests.db-tests#2")
	if second.Caches[0] != "go" || second.DependsOn[0] != "checks.build-vet" || second.Services["postgres"].Image != d7Postgres {
		t.Errorf("changing one shard changed its sibling: %+v", second)
	}
}

// Consent asks Compile's per-run question of EVERY step, whatever event would
// plan it: connect uses it, so a step a push would refuse is refused when the
// pipeline is connected, not on the first push that reaches its stage.
func TestConsentChecksEveryStageNotOnlyThePlannedOnes(t *testing.T) {
	spec := d7ExampleSpec()
	spec.Stages[2].Steps[0].Secrets = []string{"DEPLOY_TOKEN"}
	// os-checks needs docker and deploy/verify-rollout uses a secret; neither
	// step runs on every event, and Consent must not care.
	if r := Consent(spec, ComputeCluster, []string{"DEPLOY_TOKEN", "VERIFY_TOKEN"}); r == nil || r.Code != CodeFleetNotConsented || r.Scope != "tests/os-checks" {
		t.Errorf("Consent on a cluster-only pipeline = %v, want pipeline_fleet_not_consented at tests/os-checks", r)
	}
	if r := Consent(spec, ComputeClusterAndFleet, nil); r == nil || r.Code != CodeSecretNotAllowed {
		t.Errorf("Consent with no allowed secrets = %v, want pipeline_secret_not_allowed", r)
	}
	if r := Consent(spec, ComputeClusterAndFleet, []string{"DEPLOY_TOKEN", "VERIFY_TOKEN"}); r != nil {
		t.Errorf("Consent with fleet consent and every secret allowed = %v, want nil", r)
	}
	if r := Consent(nil, ComputeCluster, nil); r != nil {
		t.Errorf("Consent(nil) = %v, want nil", r)
	}
}
