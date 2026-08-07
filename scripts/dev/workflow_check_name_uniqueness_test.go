// Static guard over check-run name uniqueness across .github/workflows
// (znasllc-io/memql#3210).
//
// # What is being guarded
//
// A GitHub Actions job publishes a check-run whose name is the job's `name:`
// when set, otherwise the job key. Two jobs -- in the same workflow or in two
// different ones -- may carry the same name, and GitHub does not object. The
// result is two independent check-runs indistinguishable in the checks UI, and
// a branch-protection / ruleset `required_status_checks` context that names one
// of them cannot say which it means. Whichever run reports last wins the
// rollup, so a required context can flip between two unrelated scanners.
//
// This repo shipped exactly that: `gitleaks.yml` and `govulncheck.yml` both
// declared `jobs.scan` with `name: scan`, and they share three triggers
// (`push: main`, `merge_group`, `schedule`). #3210 renamed them to `gitleaks`
// and `govulncheck`; this guard is what stops the shape coming back -- in those
// two files or in any workflow added later.
//
// The ruleset is deliberately NOT read here. It lives outside the repo (GitHub
// API state, not a checked-in file), and the property that makes a required set
// readable is a property of the workflows: if every job publishes a distinct
// name, then any context the ruleset names maps to at most one job. That is the
// invariant this file asserts, and it holds regardless of what the ruleset
// currently requires.
//
// # Why it is written this way
//
// Parsed with yaml.v3 rather than scanned line by line, following this
// package's vscode_lane_scope_test.go -- whose header records that an
// adversarial review defeated a line-scanning version twice, by putting the
// expected pattern in a step's `name:` and in a trailing comment. Reading
// structure makes both unrepresentable rather than merely tested for. Here it
// matters twice over: a step can carry `name: scan` (gitleaks.yml has one) and
// a line scan would confuse it with a job name.
//
// The collector fails CLOSED on every workflow shape it cannot expand
// faithfully -- reusable-workflow calls, `matrix.exclude`, and the mixed
// base-keys-plus-`include` matrix form. Under-expanding a matrix would hide a
// duplicate rather than report one, so an unrecognised shape is reported as a
// guard defect with instructions, not skipped.
package dev

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// checkNameWorkflow is the subset of a workflow file this guard reads.
//
// `on:` is deliberately absent. The uniqueness question is asked across ALL
// workflows regardless of trigger: two jobs whose triggers do not overlap today
// are one `on:` edit away from overlapping, and the ambiguity they would create
// is not visible at the site of that edit.
type checkNameWorkflow struct {
	Jobs map[string]checkNameJob `yaml:"jobs"`
}

type checkNameJob struct {
	Name string `yaml:"name"`
	// Uses is a job-level reusable-workflow call. Such a job publishes
	// `<caller job name> / <called job name>` per job in the called workflow,
	// which cannot be expanded without following the reference. None exist in
	// this repo; one appearing is a guard defect, not a pass.
	Uses     string `yaml:"uses"`
	Strategy struct {
		// yaml.Node, not map[string]any: the auto-appended matrix suffix is
		// ordered by the matrix keys' DECLARATION order, which a Go map
		// destroys.
		Matrix yaml.Node `yaml:"matrix"`
	} `yaml:"strategy"`
}

// matrixInterpolation matches a `${{ matrix.<key> }}` reference in a job name.
var matrixInterpolation = regexp.MustCompile(`\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}`)

// checkRun is one effective check-run name and where it came from.
type checkRun struct {
	name     string
	workflow string // file base name, e.g. "gitleaks.yml"
	jobKey   string
}

func (c checkRun) origin() string {
	return fmt.Sprintf(".github/workflows/%s jobs.%s", c.workflow, c.jobKey)
}

// workflowFiles lists every workflow file, sorted, so failures are stable.
func workflowFiles(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read .github/workflows: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch filepath.Ext(e.Name()) {
		case ".yml", ".yaml":
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// matrixCombinations expands a job's `strategy.matrix` into the ordered value
// lists GitHub renders in a check-run name.
//
// Returns nil when the job has no matrix. Reports a guard defect (via t.Fatalf)
// for any matrix shape it cannot expand faithfully -- see the file header.
func matrixCombinations(t *testing.T, origin string, matrix yaml.Node) []map[string]string {
	t.Helper()
	if matrix.Kind == 0 {
		return nil // no `strategy.matrix`
	}
	if matrix.Kind != yaml.MappingNode {
		t.Fatalf("%s: strategy.matrix is not a mapping (yaml kind %d).\n"+
			"This guard expands matrices to compute check-run names and cannot read "+
			"this shape, so it would UNDER-report duplicates. Extend "+
			"matrixCombinations rather than removing the job from the guard (memql#3210).",
			origin, matrix.Kind)
	}

	// Keys in declaration order; `include` / `exclude` handled separately.
	var keys []string
	values := map[string][]string{}
	var include []map[string]string
	for i := 0; i+1 < len(matrix.Content); i += 2 {
		key := matrix.Content[i].Value
		val := matrix.Content[i+1]
		switch key {
		case "exclude":
			t.Fatalf("%s: strategy.matrix uses `exclude:`, which this guard does not "+
				"model. Excluded combinations do not produce check-runs, so the guard "+
				"would report names that never exist -- and could then miss a real "+
				"duplicate among the ones that do. Teach matrixCombinations to apply "+
				"`exclude` (memql#3210).", origin)
		case "include":
			if val.Kind != yaml.SequenceNode {
				t.Fatalf("%s: strategy.matrix.include is not a sequence (yaml kind %d) "+
					"-- extend matrixCombinations (memql#3210).", origin, val.Kind)
			}
			for _, entry := range val.Content {
				if entry.Kind != yaml.MappingNode {
					t.Fatalf("%s: a strategy.matrix.include entry is not a mapping "+
						"(yaml kind %d) -- extend matrixCombinations (memql#3210).",
						origin, entry.Kind)
				}
				combo := map[string]string{}
				for j := 0; j+1 < len(entry.Content); j += 2 {
					combo[entry.Content[j].Value] = entry.Content[j+1].Value
				}
				include = append(include, combo)
			}
		default:
			if val.Kind != yaml.SequenceNode {
				t.Fatalf("%s: strategy.matrix.%s is not a sequence (yaml kind %d) "+
					"-- extend matrixCombinations (memql#3210).", origin, key, val.Kind)
			}
			keys = append(keys, key)
			for _, item := range val.Content {
				if item.Kind != yaml.ScalarNode {
					t.Fatalf("%s: strategy.matrix.%s holds a non-scalar entry (yaml kind %d). "+
						"GitHub renders object matrix values in the check-run name in a form "+
						"this guard does not model -- extend matrixCombinations (memql#3210).",
						origin, key, item.Kind)
				}
				values[key] = append(values[key], item.Value)
			}
		}
	}

	switch {
	case len(keys) > 0 && len(include) > 0:
		// `include` can extend an existing combination (adding a key that then
		// appears in the auto-appended suffix) or append a wholly new one. Both
		// change the set of check-run names. Approximating would under-report.
		t.Fatalf("%s: strategy.matrix mixes base keys %v with `include:`, a shape this "+
			"guard does not model. `include` can both extend existing combinations and "+
			"add new ones, either of which changes the check-run names GitHub publishes, "+
			"so approximating here would silently miss a duplicate. Teach "+
			"matrixCombinations the include-merge rules (memql#3210).", origin, keys)
	case len(include) > 0:
		return include
	case len(keys) == 0:
		t.Fatalf("%s: strategy.matrix parsed with no keys at all -- the parse is wrong, "+
			"not the workflow. Fix matrixCombinations rather than letting the job "+
			"contribute a single unexpanded name (memql#3210).", origin)
	}

	// Cartesian product over the base keys, in declaration order.
	combos := []map[string]string{{}}
	for _, key := range keys {
		var next []map[string]string
		for _, base := range combos {
			for _, v := range values[key] {
				merged := map[string]string{}
				for k, existing := range base {
					merged[k] = existing
				}
				merged[key] = v
				next = append(next, merged)
			}
		}
		combos = next
	}
	// Record key order so the suffix can be rendered deterministically.
	for _, c := range combos {
		c[matrixKeyOrder] = strings.Join(keys, "\x00")
	}
	return combos
}

// matrixKeyOrder is the reserved pseudo-key carrying declaration order through
// a combination map. A real matrix key cannot collide with it -- YAML keys
// cannot contain a NUL, and GitHub rejects `matrix` keys outside
// [A-Za-z0-9_-].
const matrixKeyOrder = "\x00order\x00"

// effectiveCheckRunNames computes the check-run name(s) a job publishes.
//
// The rules, as observed on this repo's own runs (docs/ci-audit.md records the
// live `statusCheckRollup` names):
//
//   - No matrix: the name is `name:` verbatim, or the job key when `name:` is
//     absent.
//   - Matrix, and the name does NOT reference `matrix.*`: GitHub appends the
//     combination's values in parentheses -- `Analyze` + `language: [go, ...]`
//     reports `Analyze (go)`.
//   - Matrix, and the name DOES reference `matrix.*`: the reference is
//     substituted and no parenthetical is appended -- `build memql-${{
//     matrix.node }}` reports `build memql-identity`.
//
// A name interpolating a non-matrix context (`inputs.*`, `github.*`) is not
// statically resolvable; the expression is left in place so the name is still
// compared as a template. Two jobs sharing a template would be reported, which
// is the conservative direction.
func effectiveCheckRunNames(t *testing.T, workflow, jobKey string, job checkNameJob) []checkRun {
	t.Helper()
	origin := fmt.Sprintf(".github/workflows/%s jobs.%s", workflow, jobKey)

	if strings.TrimSpace(job.Uses) != "" {
		t.Fatalf("%s calls a reusable workflow (`uses: %s`). Such a job publishes one "+
			"check-run per job in the CALLED workflow, named `<caller job> / <called job>`, "+
			"which this guard cannot expand without following the reference -- so it would "+
			"silently stop covering those names. Teach effectiveCheckRunNames to resolve "+
			"local `uses:` targets (memql#3210).", origin, job.Uses)
	}

	base := strings.TrimSpace(job.Name)
	if base == "" {
		base = jobKey
	}

	combos := matrixCombinations(t, origin, job.Strategy.Matrix)
	if len(combos) == 0 {
		return []checkRun{{name: base, workflow: workflow, jobKey: jobKey}}
	}

	interpolatesMatrix := matrixInterpolation.MatchString(base)
	out := make([]checkRun, 0, len(combos))
	for _, combo := range combos {
		name := base
		if interpolatesMatrix {
			name = matrixInterpolation.ReplaceAllStringFunc(base, func(m string) string {
				key := matrixInterpolation.FindStringSubmatch(m)[1]
				if v, ok := combo[key]; ok {
					return v
				}
				return m
			})
		} else {
			name = base + " (" + strings.Join(comboValues(combo), ", ") + ")"
		}
		out = append(out, checkRun{name: name, workflow: workflow, jobKey: jobKey})
	}
	return out
}

// comboValues renders a combination's values in matrix-key declaration order,
// which is the order GitHub uses for the auto-appended parenthetical.
func comboValues(combo map[string]string) []string {
	order, ok := combo[matrixKeyOrder]
	var keys []string
	if ok && order != "" {
		keys = strings.Split(order, "\x00")
	} else {
		for k := range combo {
			if k == matrixKeyOrder {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, combo[k])
	}
	return out
}

// collectCheckRunNames parses every workflow file and returns every check-run
// name the repo publishes, sorted by name then origin.
func collectCheckRunNames(t *testing.T) []checkRun {
	t.Helper()
	files := workflowFiles(t)
	if len(files) < 2 {
		t.Fatalf("found %d workflow file(s) under .github/workflows -- uniqueness across "+
			"workflows is not being tested at all, so this guard verifies nothing. Fix the "+
			"lookup rather than accepting the pass (memql#3210).", len(files))
	}

	var all []checkRun
	for _, file := range files {
		path := filepath.Join(repoRoot(t), ".github", "workflows", file)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var wf checkNameWorkflow
		if err := yaml.Unmarshal(raw, &wf); err != nil {
			t.Fatalf("parse .github/workflows/%s: %v", file, err)
		}
		if len(wf.Jobs) == 0 {
			t.Fatalf(".github/workflows/%s parsed with no jobs at all -- a workflow with no "+
				"jobs does nothing, so this is a parse failure rather than a real file. The "+
				"guard cannot verify what it was written to verify, so it fails closed "+
				"(memql#3210).", file)
		}
		jobKeys := make([]string, 0, len(wf.Jobs))
		for k := range wf.Jobs {
			jobKeys = append(jobKeys, k)
		}
		sort.Strings(jobKeys)
		for _, k := range jobKeys {
			all = append(all, effectiveCheckRunNames(t, file, k, wf.Jobs[k])...)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].name != all[j].name {
			return all[i].name < all[j].name
		}
		return all[i].origin() < all[j].origin()
	})
	return all
}

// TestWorkflowCheckRunNamesAreUnique is the guard.
//
// It is red on main as of #3210's filing: gitleaks.yml and govulncheck.yml both
// publish `scan`. It lands WITH the rename that makes it green.
func TestWorkflowCheckRunNamesAreUnique(t *testing.T) {
	all := collectCheckRunNames(t)

	byName := map[string][]checkRun{}
	for _, c := range all {
		byName[c.name] = append(byName[c.name], c)
	}

	dupNames := make([]string, 0, len(byName))
	for name, runs := range byName {
		if len(runs) > 1 {
			dupNames = append(dupNames, name)
		}
	}
	sort.Strings(dupNames)

	for _, name := range dupNames {
		var origins []string
		for _, c := range byName[name] {
			origins = append(origins, "  - "+c.origin())
		}
		t.Errorf("check-run name %q is published by %d jobs:\n%s\n"+
			"Two jobs under one name are indistinguishable in the checks UI, and a ruleset "+
			"`required_status_checks` context naming it cannot say which job it means -- "+
			"whichever run reports last wins the rollup. Give each job a distinct `name:` "+
			"(memql#3210).",
			name, len(byName[name]), strings.Join(origins, "\n"))
	}
}

// TestWorkflowCheckRunNameCollectorIsNotVacuous guards the guard.
//
// TestWorkflowCheckRunNamesAreUnique passes trivially if the collector returns
// nothing, or returns one name per job while quietly failing to expand a
// matrix -- and a matrix job is exactly where a duplicate is easiest to
// introduce by accident. These floors are structural rather than a pinned
// count, so deleting a workflow does not fail them, but a broken collector
// does.
func TestWorkflowCheckRunNameCollectorIsNotVacuous(t *testing.T) {
	all := collectCheckRunNames(t)

	if len(all) == 0 {
		t.Fatal("collected zero check-run names from .github/workflows -- the uniqueness " +
			"guard would pass vacuously (memql#3210)")
	}

	// Every workflow file must contribute at least one name.
	seen := map[string]bool{}
	for _, c := range all {
		seen[c.workflow] = true
		if strings.TrimSpace(c.name) == "" {
			t.Errorf("%s produced an empty check-run name -- the fallback to the job key "+
				"is not working (memql#3210)", c.origin())
		}
	}
	for _, file := range workflowFiles(t) {
		if !seen[file] {
			t.Errorf(".github/workflows/%s contributed no check-run name; every workflow "+
				"has at least one job, so the collector is dropping it (memql#3210)", file)
		}
	}

	// At least one job must expand to more than one name. ci.yml's
	// `build-node-tags` and codeql.yml's `analyze` are both matrix jobs; if
	// neither expands, matrixCombinations has stopped working and every matrix
	// job is being compared as a single unexpanded name.
	byJob := map[string]int{}
	for _, c := range all {
		byJob[c.origin()]++
	}
	expanded := 0
	for _, n := range byJob {
		if n > 1 {
			expanded++
		}
	}
	if expanded == 0 {
		t.Error("no job expanded to more than one check-run name, yet .github/workflows " +
			"declares matrix jobs -- matrix expansion is not running, so a duplicate " +
			"introduced by a matrix value would go unreported (memql#3210)")
	}

	// The auto-appended parenthetical must actually be produced. Without it a
	// matrix job's combinations all collapse to the bare `name:`, which would
	// make the uniqueness test fail loudly rather than silently -- but it would
	// fail for the wrong reason, so pin the shape here instead.
	suffixed := 0
	for _, c := range all {
		if strings.HasSuffix(c.name, ")") && strings.Contains(c.name, " (") {
			suffixed++
		}
	}
	if suffixed == 0 {
		t.Error("no check-run name carries a matrix parenthetical such as `Analyze (go)`, " +
			"yet .github/workflows declares matrix jobs whose `name:` does not interpolate " +
			"`matrix.*` -- effectiveCheckRunNames is not appending the suffix GitHub appends " +
			"(memql#3210)")
	}
}
