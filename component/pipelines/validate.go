package pipelines

import (
	"fmt"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultStepTimeout applies when a step names none; MaxStepTimeout caps one.
const (
	DefaultStepTimeout = 20 * time.Minute
	MaxStepTimeout     = 3 * time.Hour
)

// minStepTimeout is the shortest timeout a step may name: under a minute, the
// clone and the image pull alone could time out a step that did nothing wrong.
const minStepTimeout = time.Minute

// What a notify stage's links may be (epic memql#5480). A message carries them
// beside the run page, so they are few, short and plainly addressed: the bounds
// keep a message inside Discord's limits and a label from becoming a paragraph.
const (
	maxStageLinks     = 5
	maxLinkLabelRunes = 40
	maxLinkURLBytes   = 512
)

// reservedSecretPrefix is the platform's own environment (StepRequest's
// Environment). A secret under it never resolves, so a manifest cannot shadow
// MEMQL_SHA or MEMQL_PACKAGES with a value of its own choosing.
const reservedSecretPrefix = "MEMQL_"

const manifestNameRule = "use lower-case letters, digits and hyphens, starting with a letter or digit, at most 40 characters"

var (
	// manifestNameRe is every name a pipeline declares: stages, steps,
	// channels, buckets and services. A name is part of a step key
	// ("stage.step#2") and of a refusal's scope ("stage/step"), so it may
	// hold no dot, slash or hash.
	manifestNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	workflowNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,127}$`)
	// serviceEnvKeyRe is an environment variable name a sidecar can be given.
	serviceEnvKeyRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	// secretNameRe is a globalSecret name a step may reference; it becomes an
	// environment variable of the same name.
	secretNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
	// dbGatedTreeRe is a repository directory that can hold a Go package:
	// the CI bridge's safeDirRe (scripts/ci/selection/shard.go). "." is the
	// root package alone. An element starting with "." or "_" is a directory
	// the go tool never reads, so a tree naming one could hold nothing.
	dbGatedTreeRe = regexp.MustCompile(`^(\.|[A-Za-z0-9][A-Za-z0-9._+~-]*(/[A-Za-z0-9][A-Za-z0-9._+~-]*)*)$`)
)

// stageOnValues is what a stage's `on` may name: the events a run is opened
// for (D5, plus release) and the two modes. A re-requested check run is not
// an event of its own, so check_run and check_suite are not here.
var stageOnValues = map[string]bool{
	string(EventPullRequest): true,
	string(EventMergeGroup):  true,
	string(EventPush):        true,
	string(EventRelease):     true,
	string(ModeAffected):     true,
	string(ModeFull):         true,
}

// Validate reports the first thing in spec that cannot mean anything, as a
// typed refusal scoped to the stage or "stage/step"; nil when it compiles.
//
// It reads the whole block, including stages this run will not plan: a typo
// in the deploy stage fails the pull request's run too, rather than waiting to
// fail the push after the change has landed. What depends on the pipeline ROW
// rather than the manifest -- whether the owner allowed a secret, consented to
// the fleet -- is Compile's question, asked of the stages that run.
//
// The order is fixed, and is the block's usual reading order: the services,
// the select block, then each stage and its steps as written, so one spec
// always reports the same refusal first.
func Validate(spec *Spec) *Refusal {
	if spec == nil {
		return Refuse(CodeNotDeclared, "", "The manifest has no pipeline: block.")
	}
	if spec.Workflow != "" && !workflowNameRe.MatchString(spec.Workflow) {
		return Refuse(CodeStageInvalid, "workflow", "workflow must name an installed MemQL automation (letters, digits or underscores, starting with a letter, at most 128 characters).")
	}
	if len(spec.Stages) == 0 {
		return Refuse(CodeStageInvalid, "", "The pipeline declares no stages.")
	}
	if r := validateServices(spec.Services); r != nil {
		return r
	}
	if r := validateSelect(spec.Select); r != nil {
		return r
	}
	declared := make(map[string]bool, len(spec.Stages))
	for _, stage := range spec.Stages {
		declared[stage.Name] = true
	}
	earlier := make(map[string]bool, len(spec.Stages))
	for i, stage := range spec.Stages {
		if r := validateStage(spec, i, stage, earlier, declared); r != nil {
			return r
		}
		earlier[stage.Name] = true
	}
	return nil
}

func validateServices(services map[string]Service) *Refusal {
	for _, name := range slices.Sorted(maps.Keys(services)) {
		if !manifestNameRe.MatchString(name) {
			return Refuse(CodeStepInvalid, "services", "Service name %q is not a name: %s.", name, manifestNameRule)
		}
		scope := "services/" + name
		service := services[name]
		if strings.TrimSpace(service.Image) == "" {
			return Refuse(CodeStepInvalid, scope, "Service %q names no image.", name)
		}
		for _, key := range slices.Sorted(maps.Keys(service.Env)) {
			if !serviceEnvKeyRe.MatchString(key) {
				return Refuse(CodeStepInvalid, scope,
					"Service %q sets %q, which is not an environment variable name: use upper-case letters, digits and underscores, not starting with a digit.",
					name, key)
			}
		}
	}
	return nil
}

func validateSelect(sel *Select) *Refusal {
	if sel == nil {
		return nil
	}
	if sel.Go != "" && sel.Go != SelectImportGraph {
		return Refuse(CodeSelectInvalid, "select", "select.go is %q; the only Go selection is %s.", sel.Go, SelectImportGraph)
	}
	for _, entry := range sel.DBGated {
		if _, ok := dbGatedTree(entry); !ok {
			return Refuse(CodeSelectInvalid, "select/dbGated",
				"select.dbGated names %q, which is not a directory that can hold a Go package: write a repository-relative path such as component/memql, with no leading slash, no \"..\" and no glob characters.",
				entry)
		}
	}
	if len(sel.Full) > 0 {
		if _, err := PathSet(sel.Full); err != nil {
			return Refuse(CodeSelectInvalid, "select/full", "select.full cannot be read: %v.", err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(sel.Buckets)) {
		if !manifestNameRe.MatchString(name) {
			return Refuse(CodeSelectInvalid, "select/buckets", "Bucket name %q is not a name: %s.", name, manifestNameRule)
		}
		scope := "select/buckets/" + name
		globs := sel.Buckets[name]
		if len(globs) == 0 {
			// A bucket that can never match skips every step gated on it on
			// every pull request: a silent green, not a choice anyone writes.
			return Refuse(CodeSelectInvalid, scope, "Bucket %q lists no paths, so a step gated on it would never run on a pull request.", name)
		}
		if _, err := PathSet(globs); err != nil {
			return Refuse(CodeSelectInvalid, scope, "Bucket %q cannot be read: %v.", name, err)
		}
	}
	return nil
}

func validateStage(spec *Spec, i int, stage StageSpec, earlier, declared map[string]bool) *Refusal {
	switch {
	case stage.Name == "":
		return Refuse(CodeStageInvalid, fmt.Sprintf("stages[%d]", i), "A stage has no name; every stage needs one.")
	case !manifestNameRe.MatchString(stage.Name):
		return Refuse(CodeStageInvalid, fmt.Sprintf("stages[%d]", i), "Stage name %q is not a name: %s.", stage.Name, manifestNameRule)
	case earlier[stage.Name]:
		return Refuse(CodeStageInvalid, stage.Name, "Two stages are named %q; a stage name must be unique.", stage.Name)
	}

	for _, need := range stage.Needs {
		switch {
		case earlier[need]:
		case need == stage.Name:
			return Refuse(CodeStageInvalid, stage.Name, "Stage %q needs itself.", stage.Name)
		case declared[need]:
			return Refuse(CodeStageInvalid, stage.Name,
				"Stage %q needs %q, which comes after it; stages run in the order written, so a stage can only need one before it.",
				stage.Name, need)
		default:
			return Refuse(CodeStageInvalid, stage.Name, "Stage %q needs %q, which is not a stage of this pipeline.", stage.Name, need)
		}
	}

	for _, on := range stage.On {
		if !stageOnValues[on] {
			return Refuse(CodeEventUnknown, stage.Name, "Stage %q runs on %q, which is neither an event nor a mode; use %s.",
				stage.Name, on, strings.Join(slices.Sorted(maps.Keys(stageOnValues)), ", "))
		}
	}

	switch {
	case stage.Channel != "" && len(stage.Steps) > 0:
		return Refuse(CodeStageInvalid, stage.Name,
			"Stage %q has both a channel and steps; a notify stage names a channel and carries no steps.", stage.Name)
	case stage.Channel == "" && len(stage.Steps) == 0:
		return Refuse(CodeStageInvalid, stage.Name, "Stage %q has neither steps nor a channel.", stage.Name)
	case stage.Channel == "" && len(stage.Links) > 0:
		// Steps are the only other thing a stage can have by now.
		return Refuse(CodeStageInvalid, stage.Name,
			"Stage %q has links, and only a notify stage carries them: a stage that names a channel and no steps.", stage.Name)
	case stage.Channel != "":
		if !manifestNameRe.MatchString(stage.Channel) {
			return Refuse(CodeStageInvalid, stage.Name, "Stage %q names channel %q, which is not a name: %s.",
				stage.Name, stage.Channel, manifestNameRule)
		}
		return validateLinks(stage.Name, stage.Links)
	}

	seen := make(map[string]bool, len(stage.Steps))
	for j, step := range stage.Steps {
		if r := validateStep(spec, stage.Name, j, step, seen); r != nil {
			return r
		}
		seen[step.Name] = true
	}
	return nil
}

// validateLinks holds a notify stage's links to what a message can carry: at
// most maxStageLinks of them, each with a label and an address.
func validateLinks(stage string, links []Link) *Refusal {
	if len(links) > maxStageLinks {
		return Refuse(CodeStageInvalid, stage, "Stage %q lists %d links; a notify stage carries at most %d.", stage, len(links), maxStageLinks)
	}
	for i, link := range links {
		label := strings.TrimSpace(link.Label)
		switch n := utf8.RuneCountInString(label); {
		case n == 0:
			return Refuse(CodeStageInvalid, stage, "Link %d of stage %q has no label; every link needs one.", i+1, stage)
		case n > maxLinkLabelRunes:
			return Refuse(CodeStageInvalid, stage, "Link %d of stage %q has a label of %d characters; a label is at most %d.",
				i+1, stage, n, maxLinkLabelRunes)
		}
		if r := validateLinkURL(stage, label, link.URL); r != nil {
			return r
		}
	}
	return nil
}

// validateLinkURL asks of one link's address what a message needs of it: an
// absolute https URL that names a host, carries no credential and is at most
// maxLinkURLBytes. A refusal names the link by its label and never repeats the
// URL: it is printed in the check run, and the reason for refusing one may be
// that it carries a secret.
//
// White space of any kind is part of "not a URL" here, so a link a message
// would leave out for it is refused where its author reads the reason, and a
// message never drops a link without a word.
func validateLinkURL(stage, label, raw string) *Refusal {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Scheme != "https" || u.Hostname() == "" || strings.ContainsFunc(raw, isSpaceOrControl):
		return Refuse(CodeStageInvalid, stage,
			"The URL of link %q in stage %q is not an absolute https URL, such as https://example.com/docs.", label, stage)
	case u.User != nil:
		return Refuse(CodeStageInvalid, stage,
			"The URL of link %q in stage %q carries a user name or password; a link is published in every message, so it must not.", label, stage)
	case len(raw) > maxLinkURLBytes:
		return Refuse(CodeStageInvalid, stage,
			"The URL of link %q in stage %q is %d bytes; a URL is at most %d.", label, stage, len(raw), maxLinkURLBytes)
	}
	return nil
}

func validateStep(spec *Spec, stage string, j int, step StepSpec, seen map[string]bool) *Refusal {
	switch {
	case step.Name == "":
		return Refuse(CodeStepInvalid, fmt.Sprintf("%s/steps[%d]", stage, j), "A step of stage %q has no name; every step needs one.", stage)
	case !manifestNameRe.MatchString(step.Name):
		return Refuse(CodeStepInvalid, fmt.Sprintf("%s/steps[%d]", stage, j), "Step name %q is not a name: %s.", step.Name, manifestNameRule)
	case seen[step.Name]:
		return Refuse(CodeStepInvalid, stage+"/"+step.Name,
			"Stage %q has two steps named %q; a step name must be unique within its stage.", stage, step.Name)
	}
	scope := stage + "/" + step.Name

	if strings.TrimSpace(step.Run) == "" && step.ImageBuild == nil {
		return Refuse(CodeStepInvalid, scope, "Step %s has no command to run.", scope)
	}

	switch step.Packages {
	case "", PackagesAffected, PackagesAll:
	default:
		return Refuse(CodeStepInvalid, scope, "packages is %q; write %s or %s.", step.Packages, PackagesAffected, PackagesAll)
	}
	switch step.Only {
	case "", OnlyDBGated, OnlyNotDBGated:
	default:
		return Refuse(CodeStepInvalid, scope, "only is %q; write %s or %s.", step.Only, OnlyDBGated, OnlyNotDBGated)
	}
	if step.Only != "" && step.Packages == "" {
		return Refuse(CodeStepInvalid, scope,
			"only narrows a step's packages, and this step selects none; add packages: %s or packages: %s.", PackagesAffected, PackagesAll)
	}
	if step.Packages != "" {
		if spec.Select == nil {
			return Refuse(CodeSelectMissing, scope,
				"The step selects Go packages, and the pipeline has no select: block; add select: { go: %s }.", SelectImportGraph)
		}
		if spec.Select.Go != SelectImportGraph {
			return Refuse(CodeSelectMissing, scope,
				"The step selects Go packages, and select.go does not name the import graph; add go: %s to select.", SelectImportGraph)
		}
	}
	if step.Only != "" && (spec.Select == nil || len(spec.Select.DBGated) == 0) {
		return Refuse(CodeSelectInvalid, scope,
			"only: %s needs select.dbGated to name the trees whose packages need a database, and it names none.", step.Only)
	}

	if step.Shards < 0 || step.Shards > MaxShards {
		return Refuse(CodeStepInvalid, scope, "shards is %d; write a number from 1 to %d.", step.Shards, MaxShards)
	}
	if step.Shards > 1 && step.Packages == "" {
		return Refuse(CodeStepInvalid, scope,
			"shards is %d, and the step selects no packages to split; add packages: %s or packages: %s.",
			step.Shards, PackagesAffected, PackagesAll)
	}

	if step.When != nil {
		declared := false
		if spec.Select != nil {
			_, declared = spec.Select.Buckets[step.When.Bucket]
		}
		if !declared {
			return Refuse(CodeBucketUnknown, scope, "when.bucket is %q, which select.buckets does not declare.", step.When.Bucket)
		}
	}

	var unknown []string
	for _, need := range slices.Sorted(maps.Keys(step.Needs)) {
		if !IsNeed(need) {
			unknown = append(unknown, strconv.Quote(need))
		}
	}
	if len(unknown) > 0 {
		return Refuse(CodeNeedUnknown, scope, "needs names %s; a need is one of %s.",
			strings.Join(unknown, ", "), strings.Join(Needs(), ", "))
	}

	for _, name := range step.Services {
		if _, ok := spec.Services[name]; !ok {
			return Refuse(CodeServiceUnknown, scope, "The step names service %q, which services does not declare.", name)
		}
	}

	if step.Timeout != "" {
		d, err := time.ParseDuration(step.Timeout)
		switch {
		case err != nil:
			return Refuse(CodeStepInvalid, scope, "timeout %q is not a duration such as 20m or 1h30m.", step.Timeout)
		case d < minStepTimeout:
			return Refuse(CodeStepInvalid, scope, "timeout %s is under the %s minimum.", step.Timeout, timeoutWords(minStepTimeout))
		case d > MaxStepTimeout:
			return Refuse(CodeStepInvalid, scope, "timeout %s is over the %s maximum.", step.Timeout, timeoutWords(MaxStepTimeout))
		}
	}

	for _, name := range step.SecretNames() {
		if !secretNameRe.MatchString(name) {
			return Refuse(CodeSecretInvalid, scope,
				"Secret name %q is not a secret name: use upper-case letters, digits and underscores, starting with a letter, at most 128 characters.",
				name)
		}
		if strings.HasPrefix(name, reservedSecretPrefix) {
			return Refuse(CodeSecretInvalid, scope,
				"Secret name %q begins %s, which is reserved for the platform's own environment.", name, reservedSecretPrefix)
		}
	}
	return validateStepRuntime(spec, step, scope)
}

// dbGatedTree reads one select.dbGated entry as the CI bridge reads a class
// tree (scripts/ci/selection ParseClasses): a leading "./" and a trailing "/"
// are dropped and the rest is cleaned. It reports false for an entry that is
// not a repository directory a Go package could live in.
func dbGatedTree(entry string) (string, bool) {
	tree := strings.TrimSuffix(strings.TrimPrefix(entry, "./"), "/")
	if tree == "" || strings.Contains(tree, "..") || !dbGatedTreeRe.MatchString(tree) {
		return "", false
	}
	return path.Clean(tree), true
}

// timeoutWords renders a bound as a person writes it: 2h, not 2h0m0s.
func timeoutWords(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
