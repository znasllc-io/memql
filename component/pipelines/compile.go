package pipelines

import (
	"cmp"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"
)

// CompileInput is everything a compile reads besides the spec.
type CompileInput struct {
	Mode    Mode
	Event   Event
	Compute Compute
	// AllowedSecrets is the pipeline row's secretNames.
	AllowedSecrets []string
	// Selector answers which Go packages a step selects. Nil when no step
	// selects packages; Compile refuses pipeline_select_missing if one does.
	Selector Selector
	// Changed and ChangedKnown gate `when: bucket` steps in affected mode.
	Changed      []string
	ChangedKnown bool
	// Timings is the pipeline's timing table: import path -> seconds.
	Timings map[string]float64
}

// Plan is a compiled pipeline: the stages one run executes, in order. It
// becomes one work goal with one v1:work:step per Step (D7).
type Plan struct {
	Workflow string      `json:"workflow,omitempty"`
	Mode     Mode        `json:"mode"`
	Event    Event       `json:"event"`
	Stages   []PlanStage `json:"stages"`
}

// PlanStage is one stage the run executes.
type PlanStage struct {
	Name string `json:"name"`
	// Needs are the stage's needs as written. They are recorded rather than
	// enforced: stages run in the order written, and every step already
	// depends on the steps of the stage planned before it.
	Needs []string `json:"needs,omitempty"`
	Steps []Step   `json:"steps"`
}

// Steps flattens the plan in execution order.
func (p Plan) Steps() []Step {
	var out []Step
	for _, stage := range p.Stages {
		out = append(out, stage.Steps...)
	}
	return out
}

// NeedsSelector reports whether any step of spec selects packages: whether a
// run must read the repository's Go import graph before it compiles. It asks
// of every stage, planned or not, so the caller can decide before it knows
// the plan.
func NeedsSelector(spec *Spec) bool {
	if spec == nil {
		return false
	}
	for _, stage := range spec.Stages {
		for _, step := range stage.Steps {
			if step.Packages != "" {
				return true
			}
		}
	}
	return false
}

// Compile turns a pipeline: block into the plan one run executes.
//
// A spec Validate refuses is refused unchanged. Otherwise a stage is planned
// when its `on` is empty or names this run's event or mode; a stage whose
// `on` excludes the run is absent from the plan, not skipped. Each planned
// stage compiles, in order:
//
//   - a notify stage to one notify step, "<stage>.notify", which carries the
//     stage's links;
//   - every other step to one step "<stage>.<step>", or, split by the timing
//     table into k > 1 shards, to k steps "<stage>.<step>#<i>";
//   - every step depends on every step of the stage planned before it, so
//     stages run strictly in the order written.
//
// The pipeline row is consulted for the stages that run: a step that names a
// need when the owner has not consented to the fleet, or a secret the owner
// did not allow, refuses the whole run before any executor sees it. Those
// refusals do not depend on what changed, so a pipeline that refuses one
// pull request refuses every one.
//
// What a step selects is decided last. A `when: { bucket }` step is skipped
// only in affected mode, and only when the change list is known, holds at
// least one path, and touches none of the bucket's globs; a bucket is a
// PathSet, so a path is in it when a plain glob matches and no `!` glob does.
// A packages step takes every package in any mode but
// affected, under `packages: all`, or when the affected selection is Full,
// and the affected packages otherwise; `only` narrows that to the db-gated
// trees or to the rest, and nothing left is one skipped step. A skip is
// work that had nothing to do, never a refusal in disguise.
func Compile(spec *Spec, in CompileInput) (Plan, *Refusal) {
	if r := Validate(spec); r != nil {
		return Plan{}, r
	}
	c := &planCompiler{spec: spec, in: in}
	if spec.Select != nil {
		for _, entry := range spec.Select.DBGated {
			if tree, ok := dbGatedTree(entry); ok {
				c.trees = append(c.trees, tree)
			}
		}
	}

	plan := Plan{Workflow: spec.Workflow, Mode: in.Mode, Event: in.Event}
	var previous []string // the keys of the stage planned before this one
	for _, stage := range spec.Stages {
		if !stagePlanned(stage, in) {
			continue
		}
		planned := PlanStage{Name: stage.Name, Needs: compiledCopy(stage.Needs)}
		if stage.Channel != "" {
			planned.Steps = []Step{{
				Key: StepKey(stage.Name, "notify"), Stage: stage.Name, Name: "notify", Kind: StepNotify,
				Channel: stage.Channel, Links: compiledLinks(stage.Links), TimeoutSeconds: int(DefaultStepTimeout / time.Second),
				DependsOn: compiledCopy(previous),
			}}
		} else {
			for _, step := range stage.Steps {
				steps, r := c.compileStep(stage.Name, step, previous)
				if r != nil {
					return Plan{}, r
				}
				planned.Steps = append(planned.Steps, steps...)
			}
		}
		previous = make([]string, 0, len(planned.Steps))
		for i, step := range planned.Steps {
			planned.Steps[i].RunAfterFailure = stage.RunAfterFailure
			previous = append(previous, step.Key)
		}
		plan.Stages = append(plan.Stages, planned)
	}
	return plan, nil
}

func stagePlanned(stage StageSpec, in CompileInput) bool {
	if len(stage.On) == 0 {
		return true
	}
	for _, on := range stage.On {
		if on == string(in.Event) || on == string(in.Mode) {
			return true
		}
	}
	return false
}

// planCompiler carries one compile's inputs and the answers it asks the
// Selector for once.
type planCompiler struct {
	spec  *Spec
	in    CompileInput
	trees []string // select.dbGated, normalized

	all          []string
	allRead      bool
	affected     Selection
	affectedRead bool
}

func (c *planCompiler) compileStep(stage string, declared StepSpec, dependsOn []string) ([]Step, *Refusal) {
	scope := stage + "/" + declared.Name

	needs := stepNeeds(declared)
	if r := stepConsent(scope, declared, c.in.Compute, c.in.AllowedSecrets); r != nil {
		return nil, r
	}

	if declared.Packages != "" && c.in.Selector == nil {
		return nil, Refuse(CodeSelectMissing, scope, "The step selects Go packages, and no import graph was read for this run.")
	}

	step := Step{
		ImageBuild: CloneImageBuild(declared.ImageBuild),
		Key:        StepKey(stage, declared.Name), Stage: stage, Name: declared.Name, Kind: StepCommand, Run: declared.Run,
		MemoryMiB:       declared.MemoryMiB,
		Execution:       ExecutionOf(declared.Execution),
		Placement:       PlacementOf(declared),
		Platform:        PlatformOf(c.spec, declared),
		Image:           cmp.Or(declared.Image, c.spec.Image),
		Services:        c.servicesFor(declared.Services),
		Caches:          compiledCopy(cachesFor(c.spec, declared)),
		Needs:           needs,
		TimeoutSeconds:  stepTimeoutSeconds(declared.Timeout),
		Artifacts:       compiledCopy(declared.Artifacts),
		Secrets:         compiledCopy(declared.Secrets),
		ImagePullSecret: declared.ImagePullSecret,
		DependsOn:       compiledCopy(dependsOn),
	}

	if step.Execution == ExecutionNative {
		step.Image = ""
	}
	if step.ImageBuild != nil {
		step.Image = "" // the operator-approved builder image is never source configuration
		step.Artifacts = ImageBuildArtifacts()
	}

	if declared.When != nil && c.bucketUntouched(declared.When.Bucket) {
		step.Skip = &Skip{Code: CodeNotAffected, Reason: "No change under bucket " + declared.When.Bucket + "."}
		return []Step{step}, nil
	}
	if declared.Packages == "" {
		return []Step{step}, nil
	}

	candidates := c.packagesFor(declared)
	if len(candidates) == 0 {
		reason := "No affected Go packages."
		if declared.Only == OnlyDBGated {
			reason = "No db-gated packages are affected."
		}
		step.Skip = &Skip{Code: CodeNotAffected, Reason: reason}
		return []Step{step}, nil
	}
	if declared.Shards > 1 {
		// One package, or several measured at zero, cannot be split: the step
		// then runs unsharded, under its own key, exporting no MEMQL_SHARD.
		if shards := Partition(candidates, c.in.Timings, declared.Shards); len(shards) > 1 {
			out := make([]Step, len(shards))
			for i, packages := range shards {
				shard := cloneCompiledStep(step)
				shard.Key = fmt.Sprintf("%s#%d", step.Key, i+1)
				shard.Packages = packages
				shard.Shard = ShardRef{Index: i + 1, Count: len(shards)}
				out[i] = shard
			}
			return out, nil
		}
	}
	step.Packages = candidates
	return []Step{step}, nil
}

// packagesFor is a packages step's candidates, sorted, after `only`. Any mode
// but affected selects everything: selecting too much is the safe direction.
func (c *planCompiler) packagesFor(declared StepSpec) []string {
	var candidates []string
	switch {
	case c.in.Mode != ModeAffected || declared.Packages == PackagesAll:
		candidates = c.allPackages()
	case c.affectedSelection().Full:
		candidates = c.allPackages()
	default:
		candidates = c.affectedSelection().Packages
	}
	// A copy: the plan must not share storage with the Selector's answers.
	sorted := slices.Clone(candidates)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)

	var out []string
	for _, pkg := range sorted {
		switch declared.Only {
		case OnlyDBGated:
			if !c.dbGated(pkg) {
				continue
			}
		case OnlyNotDBGated:
			if c.dbGated(pkg) {
				continue
			}
		}
		out = append(out, pkg)
	}
	return out
}

func (c *planCompiler) allPackages() []string {
	if !c.allRead {
		c.all, c.allRead = c.in.Selector.All(), true
	}
	return c.all
}

func (c *planCompiler) affectedSelection() Selection {
	if !c.affectedRead {
		c.affected, c.affectedRead = c.in.Selector.Affected(), true
	}
	return c.affected
}

// dbGated reports whether a package's directory is a db-gated tree or lies
// under one, on a "/" boundary: component/database holds
// component/database/x and not component/databasex. The tree "." holds the
// root package alone -- read as a prefix it would hold everything, the
// narrowest declaration read as the widest -- as the CI bridge's classOf
// reads it. A package whose directory the Selector does not know is under no
// tree.
func (c *planCompiler) dbGated(importPath string) bool {
	dir := c.in.Selector.DirOf(importPath)
	if dir == "" {
		return false
	}
	dir = path.Clean(dir)
	for _, tree := range c.trees {
		if dir == tree || (tree != "." && strings.HasPrefix(dir, tree+"/")) {
			return true
		}
	}
	return false
}

// bucketUntouched reports whether a step gated on bucket may be skipped: an
// affected run whose change list is known and not empty, with no changed path
// in the bucket. An empty list is read as unknown, as the affected set reads
// it (Full), because a change that touched nothing is likelier a change
// nobody could read.
func (c *planCompiler) bucketUntouched(bucket string) bool {
	if c.in.Mode != ModeAffected || !c.in.ChangedKnown || len(c.in.Changed) == 0 {
		return false
	}
	inBucket, err := PathSet(c.spec.Select.Buckets[bucket])
	if err != nil {
		return false // Validate compiled it; if it cannot, run rather than skip
	}
	for _, changed := range c.in.Changed {
		if inBucket(changed) {
			return false
		}
	}
	return true
}

// servicesFor is the declared services a step names, copied so the plan and
// the spec share no map.
func (c *planCompiler) servicesFor(names []string) map[string]Service {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]Service, len(names))
	for _, name := range names {
		service := c.spec.Services[name]
		service.Env = maps.Clone(service.Env)
		out[name] = service
	}
	return out
}

func stepTimeoutSeconds(timeout string) int {
	d := DefaultStepTimeout
	if timeout != "" {
		if parsed, err := time.ParseDuration(timeout); err == nil {
			d = parsed
		}
	}
	return int(d / time.Second)
}

// compiledCopy copies a slice for a plan, nil when it is empty.
func compiledCopy(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return slices.Clone(s)
}

// compiledLinks copies a stage's links for a plan, nil when it has none.
func compiledLinks(links []Link) []Link {
	if len(links) == 0 {
		return nil
	}
	return slices.Clone(links)
}

// cloneCompiledStep copies a step so that no slice or map is shared with the
// step it came from: each shard of a step is a value of its own.
func cloneCompiledStep(s Step) Step {
	if s.Services != nil {
		services := make(map[string]Service, len(s.Services))
		for name, service := range s.Services {
			service.Env = maps.Clone(service.Env)
			services[name] = service
		}
		s.Services = services
	}
	s.Caches = slices.Clone(s.Caches)
	s.Needs = slices.Clone(s.Needs)
	s.Artifacts = slices.Clone(s.Artifacts)
	s.Secrets = slices.Clone(s.Secrets)
	s.Packages = slices.Clone(s.Packages)
	s.DependsOn = slices.Clone(s.DependsOn)
	s.Links = slices.Clone(s.Links)
	if s.Skip != nil {
		skip := *s.Skip
		s.Skip = &skip
	}
	return s
}

// Consent checks what a pipeline row allows against EVERY step of spec,
// whatever event would plan it: fleet consent for a step that names a need,
// and the secret allowlist for a step that names a secret. Compile asks the
// same question of the planned stages only, at run time; connect asks it of
// all of them, so a pipeline that one event could never run is refused when
// it is connected rather than on the first push that reaches that stage.
func Consent(spec *Spec, compute Compute, allowedSecrets []string) *Refusal {
	if spec == nil {
		return nil
	}
	for _, stage := range spec.Stages {
		for _, step := range stage.Steps {
			if r := stepConsent(stage.Name+"/"+step.Name, step, compute, allowedSecrets); r != nil {
				return r
			}
		}
	}
	return nil
}

// stepNeeds is a step's needs set true, sorted.
func stepNeeds(declared StepSpec) []string {
	var needs []string
	for _, need := range slices.Sorted(maps.Keys(declared.Needs)) {
		if declared.Needs[need] {
			needs = append(needs, need)
		}
	}
	return needs
}

// stepConsent refuses a step whose needs the owner has not consented to send
// to the fleet, or whose secrets the owner has not allowed.
func stepConsent(scope string, declared StepSpec, compute Compute, allowedSecrets []string) *Refusal {
	if PlacementOf(declared) == PlacementFleet && compute != ComputeClusterAndFleet {
		if compute == "" {
			compute = ComputeCluster
		}
		return Refuse(CodeFleetNotConsented, scope,
			"The step requests fleet placement, and this pipeline's compute is %s: the owner has not consented to the fleet (compute: %s).",
			compute, ComputeClusterAndFleet)
	}
	var notAllowed []string
	for _, name := range declared.SecretNames() {
		if !slices.Contains(allowedSecrets, name) && !slices.Contains(notAllowed, name) {
			notAllowed = append(notAllowed, name)
		}
	}
	if len(notAllowed) > 0 {
		return Refuse(CodeSecretNotAllowed, scope,
			"The step uses %s, which the pipeline's owner has not allowed; a secret resolves only when the pipeline's allowed secret names include it.",
			strings.Join(notAllowed, ", "))
	}
	return nil
}
