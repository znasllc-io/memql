// Command prtiming measures GitHub Actions pull-request runs: the M0 baseline
// of the pipelines programme (memql#5697; design record
// docs/superpowers/specs/2026-10-11-pipelines-local-green-and-fast-design.md,
// section 7). Its clock runs from a run's creation, which is when GitHub saw
// the push, to the completion of ci-required, the check the ruleset
// requires; MemQL's own check is later timed on the same clock.
package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Job is one job of a workflow run, as the Actions API reports it.
type Job struct {
	ID          int64
	Name        string
	Conclusion  string
	StartedAt   time.Time
	CompletedAt time.Time
}

// Run is one ci.yml run, the jobs it ran and what its plan job printed.
type Run struct {
	ID        int64
	CreatedAt time.Time
	Jobs      []Job
	Mode      string // "affected", "full", or "" when the plan log could not be read
	Selected  int    // packages an affected plan selected
}

const requiredJob = "ci-required"

func ran(j Job) bool { return j.Conclusion == "success" || j.Conclusion == "failure" }

// GoTouching reports whether a run tested Go: a go-tests or db-tests shard ran.
func GoTouching(jobs []Job) bool {
	for _, j := range jobs {
		if (strings.HasPrefix(j.Name, "go-tests (") || strings.HasPrefix(j.Name, "db-tests (")) && ran(j) {
			return true
		}
	}
	return false
}

// Clocks returns a run's two wall clocks, both ending when ci-required
// completed: from the run's creation (T's clock) and from its first job's
// start (#5697's clock). ok is false when ci-required never completed.
func Clocks(r Run) (fromCreation, fromFirstJob time.Duration, ok bool) {
	var end, first time.Time
	for _, j := range r.Jobs {
		if j.Name == requiredJob && !j.CompletedAt.IsZero() {
			end = j.CompletedAt
		}
		if !j.StartedAt.IsZero() && (first.IsZero() || j.StartedAt.Before(first)) {
			first = j.StartedAt
		}
	}
	if end.IsZero() || first.IsZero() || r.CreatedAt.IsZero() {
		return 0, 0, false
	}
	return end.Sub(r.CreatedAt), end.Sub(first), true
}

// Quantile is the q-th quantile by linear interpolation between closest ranks
// (R type 7, NumPy's default). The input need not be sorted; empty answers 0.
func Quantile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	pos := q * float64(len(s)-1)
	lo, hi := int(math.Floor(pos)), int(math.Ceil(pos))
	return s[lo] + time.Duration((pos-float64(lo))*float64(s[hi]-s[lo]))
}

var (
	planModeLine     = regexp.MustCompile(`### CI plan: (affected|full)`)
	planSelectedLine = regexp.MustCompile(`(\d+) seed package\(s\), (\d+) selected\.`)
)

// ParsePlanLog reads the plan job's mode and selected-package count from its
// log (scripts/ci/selection Plan.Summary). An unreadable log answers "".
func ParsePlanLog(log string) (mode string, selected int) {
	if m := planModeLine.FindStringSubmatch(log); m != nil {
		mode = m[1]
	}
	if m := planSelectedLine.FindStringSubmatch(log); m != nil {
		selected, _ = strconv.Atoi(m[2])
	}
	return mode, selected
}

// SlowestDBShard is the longest db-tests shard of a run: what the lane's
// timeout has to cover.
func SlowestDBShard(r Run) time.Duration {
	var slowest time.Duration
	for _, j := range r.Jobs {
		if strings.HasPrefix(j.Name, "db-tests (") && ran(j) && !j.StartedAt.IsZero() && !j.CompletedAt.IsZero() {
			if d := j.CompletedAt.Sub(j.StartedAt); d > slowest {
				slowest = d
			}
		}
	}
	return slowest
}

func minutes(d time.Duration) string { return fmt.Sprintf("%.1f min", d.Minutes()) }

// Report renders the baseline for ci-design.md D6. Medians, never a best case.
func Report(runs []Run, since, until string) string {
	var b strings.Builder
	var creation, firstJob, dbSlowest []time.Duration
	byMode := map[string][]time.Duration{}
	var selected []time.Duration
	perJob := map[string][]time.Duration{}
	touching := 0
	for _, r := range runs {
		if !GoTouching(r.Jobs) {
			continue
		}
		c, f, ok := Clocks(r)
		if !ok {
			continue
		}
		touching++
		creation = append(creation, c)
		firstJob = append(firstJob, f)
		byMode[r.Mode] = append(byMode[r.Mode], c)
		if r.Mode == "affected" {
			selected = append(selected, time.Duration(r.Selected))
		}
		if d := SlowestDBShard(r); d > 0 {
			dbSlowest = append(dbSlowest, d)
		}
		for _, j := range r.Jobs {
			if ran(j) && !j.StartedAt.IsZero() && !j.CompletedAt.IsZero() {
				perJob[j.Name] = append(perJob[j.Name], j.CompletedAt.Sub(j.StartedAt))
			}
		}
	}
	fmt.Fprintf(&b, "Window: %s to %s, green pull-request ci.yml runs.\n\n", since, until)
	fmt.Fprintf(&b, "Runs measured: %d\nGo-touching: %d\n\n", len(runs), touching)
	fmt.Fprintf(&b, "T (median, creation to ci-required): %s; middle half %s to %s\n",
		minutes(Quantile(creation, 0.5)), minutes(Quantile(creation, 0.25)), minutes(Quantile(creation, 0.75)))
	fmt.Fprintf(&b, "First job to ci-required (#5697's clock): median %s; middle half %s to %s\n\n",
		minutes(Quantile(firstJob, 0.5)), minutes(Quantile(firstJob, 0.25)), minutes(Quantile(firstJob, 0.75)))
	fmt.Fprintf(&b, "| Plan | Runs | Median (creation clock) |\n|---|---:|---:|\n")
	for _, mode := range []string{"affected", "full", ""} {
		name := mode
		if name == "" {
			name = "unread"
		}
		if ds := byMode[mode]; len(ds) > 0 {
			fmt.Fprintf(&b, "| %s | %d | %s |\n", name, len(ds), minutes(Quantile(ds, 0.5)))
		}
	}
	if len(selected) > 0 {
		fmt.Fprintf(&b, "\nAffected plans selected a median of %d packages.\n", int(Quantile(selected, 0.5)))
	}
	fmt.Fprintf(&b, "\nSlowest db-tests shard per run: median %s, maximum %s.\n\n", minutes(Quantile(dbSlowest, 0.5)), minutes(Quantile(dbSlowest, 1)))
	names := make([]string, 0, len(perJob))
	for name := range perJob {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return Quantile(perJob[names[i]], 0.5) > Quantile(perJob[names[j]], 0.5) })
	fmt.Fprintf(&b, "| Job | Runs | Median | Middle half |\n|---|---:|---:|---|\n")
	for _, name := range names {
		ds := perJob[name]
		fmt.Fprintf(&b, "| %s | %d | %s | %s to %s |\n", name, len(ds), minutes(Quantile(ds, 0.5)), minutes(Quantile(ds, 0.25)), minutes(Quantile(ds, 0.75)))
	}
	return b.String()
}
