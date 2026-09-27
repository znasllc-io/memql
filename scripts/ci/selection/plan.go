package selection

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The floors below refuse an implausibly small answer the way
// scripts/ci/db-gated-packages.sh's MIN_COMPLEMENT does: a count far under
// what the tree holds means the load saw less than the tree, and a plan built
// on it would test less than it reports. They are deliberately loose -- 254
// packages, 206 in the go lane and 48 in the db lane when this was written --
// so ordinary package churn never trips them; they catch a broken load, not
// a smaller repository.
const (
	minGraphPackages = 200
	minFullGo        = 150
	minFullDB        = 40
)

// PlanInput is everything a plan is computed from.
type PlanInput struct {
	// Event is github.event_name. Only "pull_request" is ever narrowed.
	Event string
	// Changed is the pull request's changed files, repo-relative. Nil when
	// the event is not a pull request.
	Changed []string
	// DiffError is why Changed could not be computed on a pull request. A
	// non-empty value makes the run full: an unreadable diff is never read as
	// an empty one.
	DiffError string
	Graph     *Graph
	// DBTrees is `db-gated-packages.sh --trees`: the db lane owns every
	// package under these directories.
	DBTrees []string
	// Complement is `db-gated-packages.sh --complement` (import paths), the
	// script's own answer for the go lane. The planner computes the same set
	// from the graph and refuses to run when the two disagree.
	Complement []string
	Classes    []Class
	Timings    Timings
	// GatePatterns are the gate-input packages (see Select).
	GatePatterns []string
	// TagTrees are the directories whose packages the node-tag passes test
	// (app, component/node, component/server): a selected package under one
	// of them means the tag passes run.
	TagTrees []string
	// CPUs is the parallelism of a class that is not serial.
	CPUs int
}

// Plan is what the plan job hands the lanes.
type Plan struct {
	Mode     Mode
	Reason   string
	Seeds    int
	Selected int
	GoShards []Shard
	DBShards []Shard
	// Tags says whether the seven node-tag passes run.
	Tags bool
	// Modules are the module directories module-boundaries checks.
	Modules []string
}

// MakePlan computes the plan. It returns an error -- which fails the plan job
// and therefore ci-required -- only for a state in which no plan can be
// trusted: a graph or a full-mode lane under its floor, a class table that
// cannot place a package, or a planner that disagrees with
// db-gated-packages.sh about what the go lane is. A change it merely cannot
// narrow is a full plan, not an error.
func MakePlan(in PlanInput) (Plan, error) {
	g := in.Graph
	if g == nil || g.Len() < minGraphPackages {
		n := 0
		if g != nil {
			n = g.Len()
		}
		return Plan{}, fmt.Errorf("the package graph holds %d packages, under the floor of %d: the load saw less than the tree, and a plan over it would test less than it reports", n, minGraphPackages)
	}
	if len(in.DBTrees) == 0 {
		return Plan{}, fmt.Errorf("no db-gated trees were supplied: the db lane would be empty by construction")
	}
	if in.CPUs < 1 {
		return Plan{}, fmt.Errorf("cpus must be at least 1, got %d", in.CPUs)
	}

	inDB := func(dir string) bool {
		for _, t := range in.DBTrees {
			if dir == t || strings.HasPrefix(dir, t+"/") {
				return true
			}
		}
		return false
	}
	goSet := map[string]bool{}
	for _, ipth := range g.ImportPaths() {
		p, _ := g.Package(ipth)
		if !inDB(p.Dir) {
			goSet[ipth] = true
		}
	}
	complement := map[string]bool{}
	for _, c := range in.Complement {
		complement[strings.TrimSpace(c)] = true
	}
	if missing, extra := setDiff(complement, goSet), setDiff(goSet, complement); len(missing) > 0 || len(extra) > 0 {
		return Plan{}, fmt.Errorf("the planner and db-gated-packages.sh disagree about the go lane: only the script has %v, only the graph has %v", missing, extra)
	}

	plan := Plan{Mode: ModeFull}
	selected := g.ImportPaths()
	switch {
	case in.Event != "pull_request":
		plan.Reason = fmt.Sprintf("a %s run tests everything", orUnknown(in.Event))
	case in.DiffError != "":
		plan.Reason = "the pull request's changed files could not be read (" + in.DiffError + "), so nothing may be skipped"
	default:
		sel, err := Select(g, in.Changed, in.GatePatterns)
		if err != nil {
			return Plan{}, err
		}
		plan.Reason = sel.Reason
		if sel.Mode == ModeAffected {
			plan.Mode = ModeAffected
			plan.Seeds = len(sel.Seeds)
			selected = sel.Packages
			plan.Tags = sel.TagChange
		}
	}
	plan.Selected = len(selected)

	var goDirs, dbDirs []string
	modules := map[string]bool{}
	for _, ipth := range selected {
		p, ok := g.Package(ipth)
		if !ok {
			return Plan{}, fmt.Errorf("selected package %s is not in the graph", ipth)
		}
		if goSet[ipth] {
			goDirs = append(goDirs, p.Dir)
		} else {
			dbDirs = append(dbDirs, p.Dir)
		}
		modules[p.ModuleDir] = true
		for _, t := range in.TagTrees {
			if p.Dir == t || strings.HasPrefix(p.Dir, t+"/") {
				plan.Tags = true
			}
		}
	}
	if plan.Mode == ModeFull {
		if len(goDirs) < minFullGo || len(dbDirs) < minFullDB {
			return Plan{}, fmt.Errorf("a full plan holds %d go and %d db packages, under the floors of %d and %d", len(goDirs), len(dbDirs), minFullGo, minFullDB)
		}
		plan.Tags = true
		plan.Modules = g.Modules()
	} else {
		plan.Modules = sortedKeys(modules)
	}

	var err error
	if plan.GoShards, err = Partition("go", goDirs, in.Classes, in.Timings["go"], in.CPUs); err != nil {
		return Plan{}, err
	}
	if plan.DBShards, err = Partition("db", dbDirs, in.Classes, in.Timings["db"], in.CPUs); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// GitHubOutput renders the plan as $GITHUB_OUTPUT lines. Every value is on
// one line; the matrices are JSON objects with an `include` list, exactly
// what `strategy.matrix: ${{ fromJSON(...) }}` takes.
func (p Plan) GitHubOutput() (string, error) {
	goM, err := matrixJSON(p.GoShards)
	if err != nil {
		return "", err
	}
	dbM, err := matrixJSON(p.DBShards)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "mode=%s\n", p.Mode)
	fmt.Fprintf(&b, "reason=%s\n", oneLine(p.Reason))
	fmt.Fprintf(&b, "go_matrix=%s\n", goM)
	fmt.Fprintf(&b, "go_count=%d\n", len(p.GoShards))
	fmt.Fprintf(&b, "db_matrix=%s\n", dbM)
	fmt.Fprintf(&b, "db_count=%d\n", len(p.DBShards))
	fmt.Fprintf(&b, "tags=%t\n", p.Tags)
	fmt.Fprintf(&b, "modules=%s\n", strings.Join(p.Modules, " "))
	return b.String(), nil
}

// Summary renders the plan for the job summary: what runs, where, and what
// the table knows about it.
func (p Plan) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### CI plan: %s\n\n%s\n\n", p.Mode, oneLine(p.Reason))
	if p.Mode == ModeAffected {
		fmt.Fprintf(&b, "%d seed package(s), %d selected.\n\n", p.Seeds, p.Selected)
	}
	b.WriteString("| lane | shard | packages | measured s | unmeasured | timeout | -p |\n")
	b.WriteString("|---|---|---:|---:|---:|---|---:|\n")
	for _, lane := range []struct {
		name   string
		shards []Shard
	}{{"go-tests", p.GoShards}, {"db-tests", p.DBShards}} {
		if len(lane.shards) == 0 {
			fmt.Fprintf(&b, "| %s | (none) | 0 | | | | |\n", lane.name)
			continue
		}
		for _, s := range lane.shards {
			fmt.Fprintf(&b, "| %s | %s | %d | %.0f | %d | %s | %d |\n",
				lane.name, s.Name, len(s.Dirs), s.Seconds, len(s.Unknown), s.Timeout, s.Parallel)
		}
	}
	tags := "skipped"
	if p.Tags {
		tags = "run"
	}
	fmt.Fprintf(&b, "\nNode-tag passes: %s. Modules for module-boundaries: %d.\n", tags, len(p.Modules))
	return b.String()
}

func matrixJSON(shards []Shard) (string, error) {
	if shards == nil {
		shards = []Shard{}
	}
	out, err := json.Marshal(struct {
		Include []Shard `json:"include"`
	}{shards})
	return string(out), err
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func orUnknown(s string) string {
	if s == "" {
		return "(unnamed event)"
	}
	return s
}

func setDiff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
