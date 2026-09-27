package selection

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Timings is the checked-in timing table (scripts/ci/shard-timings.tsv):
// lane -> package directory -> measured `go test` seconds.
//
// It is DATA the planner balances shards with, never a gate: a package absent
// from it still runs, in the lightest shard, and a stale number only makes the
// balance worse. That is why a refresh rewrites it wholesale from recent logs
// (scripts/ci/refresh-shard-timings.sh) rather than anyone editing it by hand.
type Timings map[string]map[string]float64

// ReadTimings parses the table: `#` comments, then one `lane dir seconds` row
// per line, tab- or space-separated. A malformed row, an unknown lane or a
// duplicate refuses the whole table rather than half-reading it.
func ReadTimings(r io.Reader) (Timings, error) {
	t := Timings{}
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("timing table line %d: want `lane dir seconds`, got %q", n, line)
		}
		lane, dir := f[0], f[1]
		if lane != "go" && lane != "db" {
			return nil, fmt.Errorf("timing table line %d: lane %q is not go or db", n, lane)
		}
		if dir != path.Clean(dir) || path.IsAbs(dir) || strings.HasPrefix(dir, "..") || strings.Contains(dir, "...") {
			return nil, fmt.Errorf("timing table line %d: %q is not a clean repo-relative package directory", n, dir)
		}
		secs, err := strconv.ParseFloat(f[2], 64)
		if err != nil || secs < 0 {
			return nil, fmt.Errorf("timing table line %d: seconds %q is not a non-negative number", n, f[2])
		}
		if t[lane] == nil {
			t[lane] = map[string]float64{}
		}
		if _, dup := t[lane][dir]; dup {
			return nil, fmt.Errorf("timing table line %d: %s %s appears twice", n, lane, dir)
		}
		t[lane][dir] = secs
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return t, nil
}

// Write renders the table: the header lines as comments, then the rows,
// lanes in order and, within a lane, slowest first (ties by directory), so
// the shape of the suite reads off the top of the file.
func (t Timings) Write(w io.Writer, header []string) error {
	bw := bufio.NewWriter(w)
	for _, h := range header {
		fmt.Fprintf(bw, "# %s\n", h)
	}
	fmt.Fprintf(bw, "# lane\tpackage\tseconds\n")
	lanes := make([]string, 0, len(t))
	for lane := range t {
		lanes = append(lanes, lane)
	}
	sort.Strings(lanes)
	for _, lane := range lanes {
		dirs := make([]string, 0, len(t[lane]))
		for d := range t[lane] {
			dirs = append(dirs, d)
		}
		sort.Slice(dirs, func(i, j int) bool {
			a, b := t[lane][dirs[i]], t[lane][dirs[j]]
			if a != b {
				return a > b
			}
			return dirs[i] < dirs[j]
		})
		for _, d := range dirs {
			fmt.Fprintf(bw, "%s\t%s\t%.2f\n", lane, d, t[lane][d])
		}
	}
	return bw.Flush()
}

// logTimestamp is the prefix GitHub puts on every line of a job log.
var logTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z `)

// okLine matches a passing package line of `go test` output. It is anchored:
// a test's own log output can contain "ok <pkg> 1.0s" mid-line, and reading
// that as a measurement would teach the table a number nobody measured.
// `(cached)` results carry no duration and are not a measurement either.
var okLine = regexp.MustCompile(`^ok\s+(\S+)\s+([0-9]+(?:\.[0-9]+)?)s(?:\s|$)`)

// ParseGoTestOutput collects one duration sample per passing package line,
// keyed by package directory (the import path with prefix stripped, "." for
// the root). Lines for packages outside prefix are ignored. Only PASSING
// lines count: a failed package's duration is cut short by the failure or
// the timeout, and would teach the table the wrong weight.
func ParseGoTestOutput(r io.Reader, prefix string) (map[string][]float64, error) {
	out := map[string][]float64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		m := okLine.FindStringSubmatch(logTimestamp.ReplaceAllString(sc.Text(), ""))
		if m == nil || !firstParty(m[1], prefix) {
			continue
		}
		secs, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			return nil, fmt.Errorf("parsing %q: %w", sc.Text(), err)
		}
		dir := strings.TrimPrefix(strings.TrimPrefix(m[1], prefix), "/")
		if dir == "" {
			dir = "."
		}
		out[dir] = append(out[dir], secs)
	}
	return out, sc.Err()
}

// Merge replaces lane's rows for every package with samples by the median of
// those samples, and keeps every other row. A package that stops appearing in
// the logs keeps its last measurement until it is deleted from the tree; the
// planner ignores rows for packages the graph does not hold.
func (t Timings) Merge(lane string, samples map[string][]float64) {
	if t[lane] == nil {
		t[lane] = map[string]float64{}
	}
	for dir, xs := range samples {
		if len(xs) == 0 {
			continue
		}
		t[lane][dir] = median(xs)
	}
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}
