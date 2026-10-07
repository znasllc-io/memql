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
	// StageSelection is the pipeline DSL's explicit applicability decision.
	// It is required when any stage declares `on`; nil cannot silently broaden
	// a conditional pipeline to every event.
	StageSelection *StageSelection
	// AllowedSecrets is the pipeline row's secretNames.
	AllowedSecrets []string
	// Selector supplies the import graph's package candidates and directories.
	// Nil when no step selects packages.
	Selector Selector
	// PackagePolicies are the pinned DSL's per-step coverage/filter decisions.
	// Compile refuses a missing or extra decision for a package-selecting step.
	PackagePolicies map[string]PackagePolicy
	// BucketSelection is the pipeline DSL's explicit decision about which
	// changed-path buckets run. Nil cannot silently skip a conditional step.
	BucketSelection *BucketSelection
	// Timings is the pipeline's timing table: import path -> seconds.
	Timings map[string]float64
}

// StageSelection is the stage set selected by the installed pipeline policy.
// Names are checked against the manifest before any stage is compiled.
type StageSelection struct {
	Included []string
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

// NeedsBucketSelection reports whether any declared step asks the changed-
// path policy to decide whether it runs.
func NeedsBucketSelection(spec *Spec) bool {
	if spec == nil {
		return false
	}
	for _, stage := range spec.Stages {
		for _, step := range stage.Steps {
			if step.When != nil {
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
// What a step selects is decided last. The pipeline DSL explicitly selects
// which `when: { bucket }` steps run and returns each packages step's coverage
// and filter. Go applies those choices to the import graph; nothing left is
// one skipped step. A skip is work that had nothing to do, never a refusal in
// disguise.
func Compile(spec *Spec, in CompileInput) (Plan, *Refusal) {
	if r := Validate(spec); r != nil {
		return Plan{}, r
	}
	if in.StageSelection == nil {
		for _, stage := range spec.Stages {
			if len(stage.On) > 0 {
				return Plan{}, Refuse(CodeStageInvalid, "selection",
					"The pipeline DSL did not select stages for this run; conditional stages cannot be compiled without an explicit selection.")
			}
		}
	}
	if !NeedsSelector(spec) && len(in.PackagePolicies) > 0 {
		return Plan{}, Refuse(CodeSelectInvalid, "selection",
			"The pipeline DSL selected package coverage when no step declares packages.")
	}
	knownPackageSteps := map[string]bool{}
	for _, stage := range spec.Stages {
		for _, step := range stage.Steps {
			if step.Packages != "" {
				knownPackageSteps[StepKey(stage.Name, step.Name)] = true
			}
		}
	}
	for key := range in.PackagePolicies {
		if !knownPackageSteps[key] {
			return Plan{}, Refuse(CodeSelectInvalid, "selection/"+key,
				"The pipeline DSL selected packages for an undeclared packages step.")
		}
	}
	selectedBuckets := map[string]bool(nil)
	if NeedsBucketSelection(spec) && in.BucketSelection == nil {
		return Plan{}, Refuse(CodeSelectMissing, "selection",
			"The pipeline DSL did not select changed-path buckets; conditional steps cannot be compiled without an explicit selection.")
	}
	if in.BucketSelection != nil {
		selectedBuckets = make(map[string]bool, len(in.BucketSelection.Included))
		knownBuckets := map[string]bool{}
		if spec.Select != nil {
			for name := range spec.Select.Buckets {
				knownBuckets[name] = true
			}
		}
		for _, name := range in.BucketSelection.Included {
			if !knownBuckets[name] || selectedBuckets[name] {
				return Plan{}, Refuse(CodeSelectMissing, "selection",
					"The pipeline DSL selected unknown or duplicate path bucket %q.", name)
			}
			selectedBuckets[name] = true
		}
	}
	selected := map[string]bool(nil)
	if in.StageSelection != nil {
		selected = make(map[string]bool, len(in.StageSelection.Included))
		known := make(map[string]bool, len(spec.Stages))
		for _, stage := range spec.Stages {
			known[stage.Name] = true
		}
		for _, name := range in.StageSelection.Included {
			if !known[name] || selected[name] {
				return Plan{}, Refuse(CodeStageInvalid, "selection",
					"The pipeline DSL selected unknown or duplicate stage %q.", name)
			}
			selected[name] = true
		}
	}
	c := &planCompiler{spec: spec, in: in, selectedBuckets: selectedBuckets}
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
		if selected != nil && !selected[stage.Name] {
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

// planCompiler carries one compile's inputs and the answers it asks the
// Selector for once.
type planCompiler struct {
	spec            *Spec
	in              CompileInput
	trees           []string // select.dbGated, normalized
	selectedBuckets map[string]bool

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
	if declared.Packages != "" {
		policy, ok := c.in.PackagePolicies[StepKey(stage, declared.Name)]
		if !ok {
			return nil, Refuse(CodeSelectMissing, scope, "The pipeline DSL did not select package coverage for this step.")
		}
		switch policy.Coverage {
		case PackageCoverageAll:
		case PackageCoverageAffected:
			if c.affectedSelection().Full {
				return nil, Refuse(CodeSelectInvalid, scope,
					"The pipeline DSL selected affected coverage when the import-graph decision requires full coverage.")
			}
		default:
			return nil, Refuse(CodeSelectInvalid, scope, "The pipeline DSL returned unsupported package coverage %q.", policy.Coverage)
		}
		switch policy.Filter {
		case PackageFilterAll, PackageFilterDBGated, PackageFilterNotDBGated:
		default:
			return nil, Refuse(CodeSelectInvalid, scope, "The pipeline DSL returned unsupported package filter %q.", policy.Filter)
		}
	}

	step := Step{
		ImageBuild: CloneImageBuild(declared.ImageBuild),
		Key:        StepKey(stage, declared.Name), Stage: stage, Name: declared.Name, Kind: StepCommand, Run: declared.Run,
		MemoryMiB:       declared.MemoryMiB,
		CPUMilli:        declared.CPUMilli,
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

	if declared.When != nil && !c.selectedBuckets[declared.When.Bucket] {
		step.Skip = &Skip{Code: CodeNotAffected, Reason: "No change under bucket " + declared.When.Bucket + "."}
		return []Step{step}, nil
	}
	if declared.Packages == "" {
		return []Step{step}, nil
	}

	candidates, refusal := c.packagesFor(stage, declared)
	if refusal != nil {
		return nil, refusal
	}
	if len(candidates) == 0 {
		reason := "No affected Go packages."
		if c.in.PackagePolicies[StepKey(stage, declared.Name)].Filter == PackageFilterDBGated {
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

// packagesFor mechanically applies the DSL's source and filter decision to
// the import graph. It does not decide which coverage or filter a step needs.
func (c *planCompiler) packagesFor(stage string, declared StepSpec) ([]string, *Refusal) {
	policy := c.in.PackagePolicies[StepKey(stage, declared.Name)]
	var candidates []string
	switch policy.Coverage {
	case PackageCoverageAll:
		candidates = c.allPackages()
	case PackageCoverageAffected:
		candidates = c.affectedSelection().Packages
	default:
		return nil, Refuse(CodeSelectInvalid, "selection/"+StepKey(stage, declared.Name),
			"The pipeline DSL returned unsupported package coverage %q.", policy.Coverage)
	}
	// A copy: the plan must not share storage with the Selector's answers.
	sorted := slices.Clone(candidates)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)

	var out []string
	for _, pkg := range sorted {
		switch policy.Filter {
		case PackageFilterAll:
		case PackageFilterDBGated:
			if !c.dbGated(pkg) {
				continue
			}
		case PackageFilterNotDBGated:
			if c.dbGated(pkg) {
				continue
			}
		default:
			return nil, Refuse(CodeSelectInvalid, "selection/"+StepKey(stage, declared.Name),
				"The pipeline DSL returned unsupported package filter %q.", policy.Filter)
		}
		out = append(out, pkg)
	}
	return out, nil
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
