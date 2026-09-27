package selection

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Class is one budget class of a sharded lane: the packages under Trees,
// tested with one per-package timeout and one parallelism, spread over at
// most Shards matrix entries.
//
// Classes exist because the two lanes carry per-package budgets that must
// survive sharding unchanged: a `go test -timeout` applies to every package
// of the invocation, so a shard mixing component/memql (600s, serialised) with
// the fast db packages (180s) would silently hand the fast ones the slow
// one's budget. A shard therefore never mixes classes.
type Class struct {
	Lane     string   // "go" or "db"
	Name     string   // e.g. "memql"; also the shard name's stem
	Trees    []string // repo-relative dirs; empty for the lane's default class
	Timeout  string   // the `go test -timeout` value, e.g. "600s"
	Serial   bool     // `-p=1`: the class's packages never run concurrently
	Uncached bool     // `-count=1`: results must never come from the test cache
	Shards   int      // the most matrix entries the class may use
}

// maxShardsPerClass bounds one class's fan-out. Every shard is a job with its
// own runner, checkout, cache restore and (for db) Postgres service; past a
// handful the setup cost outweighs the split, and the org's concurrent-job
// ceiling is shared with every other run.
const maxShardsPerClass = 8

var (
	classNameRe = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	timeoutRe   = regexp.MustCompile(`^[1-9][0-9]*s$`)
	// safeDirRe is a package directory that is safe to place on a shell
	// command line unquoted: the workflow interpolates a shard's packages
	// straight into `go test ... ${{ matrix.packages }}`. Go's own import
	// path rules already exclude every shell metacharacter, so this refuses
	// nothing real; it exists so that a future loader bug cannot turn a
	// directory name into a command.
	safeDirRe = regexp.MustCompile(`^(\.|[A-Za-z0-9][A-Za-z0-9._+~-]*(/[A-Za-z0-9][A-Za-z0-9._+~-]*)*)$`)
)

// ParseClasses reads the class table ci.yml's plan step carries: one class
// per line, six whitespace-separated columns,
//
//	lane  class  trees  timeout  mode  shards
//
// trees is a comma-separated list of repo-relative directories, or "-" for
// the lane's default class (every package no other class claims); mode is
// "serial", "uncached", both joined by a comma, or "-". Blank lines and lines
// starting with # are skipped. Each lane needs exactly one default class, so
// every package lands in some class.
func ParseClasses(text string) ([]Class, error) {
	var out []Class
	seen := map[string]bool{}
	defaults := map[string]int{}
	for n, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 6 {
			return nil, fmt.Errorf("class table line %d: want 6 columns (lane class trees timeout mode shards), got %d: %q", n+1, len(f), line)
		}
		c := Class{Lane: f[0], Name: f[1], Timeout: f[3]}
		if c.Lane != "go" && c.Lane != "db" {
			return nil, fmt.Errorf("class table line %d: lane %q is not go or db", n+1, c.Lane)
		}
		if !classNameRe.MatchString(c.Name) {
			return nil, fmt.Errorf("class table line %d: class name %q is not lower-case alphanumeric", n+1, c.Name)
		}
		if key := c.Lane + "/" + c.Name; seen[key] {
			return nil, fmt.Errorf("class table line %d: class %s is declared twice", n+1, key)
		} else {
			seen[key] = true
		}
		if f[2] == "-" {
			defaults[c.Lane]++
		} else {
			for _, t := range strings.Split(f[2], ",") {
				t = strings.TrimSuffix(strings.TrimPrefix(t, "./"), "/")
				if t == "" || strings.Contains(t, "...") || path.IsAbs(t) || t == ".." || strings.HasPrefix(t, "../") {
					return nil, fmt.Errorf("class table line %d: tree %q must be a repo-relative directory (no ..., no wildcard)", n+1, t)
				}
				c.Trees = append(c.Trees, path.Clean(t))
			}
		}
		if !timeoutRe.MatchString(c.Timeout) {
			return nil, fmt.Errorf("class table line %d: timeout %q is not a whole number of seconds such as 600s", n+1, c.Timeout)
		}
		if f[4] != "-" {
			for _, m := range strings.Split(f[4], ",") {
				switch m {
				case "serial":
					c.Serial = true
				case "uncached":
					c.Uncached = true
				default:
					return nil, fmt.Errorf("class table line %d: mode %q is not serial or uncached", n+1, m)
				}
			}
		}
		shards, err := strconv.Atoi(f[5])
		if err != nil || shards < 1 || shards > maxShardsPerClass {
			return nil, fmt.Errorf("class table line %d: shards %q is not a number from 1 to %d", n+1, f[5], maxShardsPerClass)
		}
		c.Shards = shards
		out = append(out, c)
	}
	for _, lane := range []string{"go", "db"} {
		if defaults[lane] != 1 {
			return nil, fmt.Errorf("class table: lane %q needs exactly one default class (trees \"-\"), found %d", lane, defaults[lane])
		}
	}
	return out, nil
}

// Shard is one matrix entry of a sharded lane. The exported JSON fields are
// exactly what the workflow step interpolates.
type Shard struct {
	Name     string `json:"name"`
	Packages string `json:"packages"` // space-separated `go test` arguments
	Timeout  string `json:"timeout"`
	Parallel int    `json:"parallel"`
	Uncached bool   `json:"uncached"`

	Class   string   `json:"-"`
	Dirs    []string `json:"-"`
	Seconds float64  `json:"-"` // the measured seconds of the shard's packages
	Unknown []string `json:"-"` // packages the timing table has not measured
}

// unknownSeconds is the weight an unmeasured package is placed with. It lands
// in the lightest shard, as the design record asks; the nominal weight keeps
// several new packages from all landing in that same one.
const unknownSeconds = 5.0

// classOf returns the index of the first class whose tree contains dir, or
// the lane default's. The tree "." holds the ROOT PACKAGE ONLY -- read as a
// prefix it would hold everything, which is the narrowest selector read as
// the widest (the same trap scripts/cidb records for `./`).
func classOf(dir string, classes []int, all []Class) int {
	def := -1
	for _, i := range classes {
		c := all[i]
		if len(c.Trees) == 0 {
			def = i
			continue
		}
		for _, t := range c.Trees {
			if dir == t || (t != "." && strings.HasPrefix(dir, t+"/")) {
				return i
			}
		}
	}
	return def
}

// Partition spreads one lane's packages (repo-relative directories) over
// shards: grouped by class, then, within a class, longest-processing-time
// first over the measured seconds (ties broken by name, so the answer is
// deterministic), then each unmeasured package, by name, into the then
// lightest shard. cpus is the parallelism of a class that is not serial.
//
// It refuses to return shards whose union is not exactly dirs: a package
// dropped here would be a package no lane ever tests, and nothing downstream
// could notice.
func Partition(lane string, dirs []string, classes []Class, seconds map[string]float64, cpus int) ([]Shard, error) {
	if cpus < 1 {
		return nil, fmt.Errorf("cpus must be at least 1, got %d", cpus)
	}
	var laneClasses []int
	for i, c := range classes {
		if c.Lane == lane {
			laneClasses = append(laneClasses, i)
		}
	}
	if len(laneClasses) == 0 {
		return nil, fmt.Errorf("no class is declared for lane %q", lane)
	}
	byClass := map[int][]string{}
	seen := map[string]bool{}
	for _, d := range dirs {
		if !safeDirRe.MatchString(d) {
			return nil, fmt.Errorf("package directory %q is not a plain path and will not be placed on a command line", d)
		}
		if seen[d] {
			return nil, fmt.Errorf("package %s is listed twice for lane %q", d, lane)
		}
		seen[d] = true
		ci := classOf(d, laneClasses, classes)
		if ci < 0 {
			return nil, fmt.Errorf("lane %q has no default class to hold %s", lane, d)
		}
		byClass[ci] = append(byClass[ci], d)
	}

	var out []Shard
	for _, ci := range laneClasses {
		members := byClass[ci]
		if len(members) == 0 {
			continue
		}
		c := classes[ci]
		n := c.Shards
		if n > len(members) {
			n = len(members)
		}
		var known, unknown []string
		for _, d := range members {
			if _, ok := seconds[d]; ok {
				known = append(known, d)
			} else {
				unknown = append(unknown, d)
			}
		}
		sort.Slice(known, func(i, j int) bool {
			a, b := seconds[known[i]], seconds[known[j]]
			if a != b {
				return a > b
			}
			return known[i] < known[j]
		})
		sort.Strings(unknown)

		shards := make([]Shard, n)
		load := make([]float64, n)
		lightest := func() int {
			best := 0
			for i := 1; i < n; i++ {
				if load[i] < load[best] {
					best = i
				}
			}
			return best
		}
		for _, d := range known {
			i := lightest()
			shards[i].Dirs = append(shards[i].Dirs, d)
			shards[i].Seconds += seconds[d]
			load[i] += seconds[d]
		}
		for _, d := range unknown {
			i := lightest()
			shards[i].Dirs = append(shards[i].Dirs, d)
			shards[i].Unknown = append(shards[i].Unknown, d)
			load[i] += unknownSeconds
		}
		for i := range shards {
			s := &shards[i]
			sort.Strings(s.Dirs)
			s.Class = c.Name
			s.Name = c.Name
			if n > 1 {
				s.Name = fmt.Sprintf("%s-%d", c.Name, i+1)
			}
			s.Packages = packageArgs(s.Dirs)
			s.Timeout = c.Timeout
			s.Uncached = c.Uncached
			s.Parallel = cpus
			if c.Serial {
				s.Parallel = 1
			}
		}
		out = append(out, shards...)
	}

	got := map[string]bool{}
	for _, s := range out {
		if len(s.Dirs) == 0 {
			return nil, fmt.Errorf("lane %q: shard %s holds no package", lane, s.Name)
		}
		for _, d := range s.Dirs {
			if got[d] {
				return nil, fmt.Errorf("lane %q: package %s landed in two shards", lane, d)
			}
			got[d] = true
		}
	}
	if len(got) != len(seen) {
		return nil, fmt.Errorf("lane %q: %d packages in, %d placed", lane, len(seen), len(got))
	}
	return out, nil
}

// packageArgs renders directories as `go test` arguments: "." for the root,
// "./<dir>" for everything else. Never a "/..." wildcard -- a shard names
// exactly the packages it owns.
func packageArgs(dirs []string) string {
	args := make([]string, len(dirs))
	for i, d := range dirs {
		if d == "." {
			args[i] = "."
		} else {
			args[i] = "./" + d
		}
	}
	return strings.Join(args, " ")
}
