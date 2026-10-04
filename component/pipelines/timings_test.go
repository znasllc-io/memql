package pipelines

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The fixture is a copy of the CI bridge's (scripts/ci/selection/testdata),
// held byte-identical to it by the root module's parity test: a recorded job
// log with GitHub's timestamps, a cached result, a failure, a package with no
// tests, a package outside the repository, a second sample of one package,
// and a test's own output quoting a result line mid-line.
func TestParseGoTestOutputReadsPassingPackagesOnly(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "go-test-output.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := ParseGoTestOutput(f)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"example.test/memql":              114.859,
		"example.test/memql/cmd/memqlfmt": 0.005,
		// Two samples: the median, as the bridge's table refresh reads them.
		"example.test/memql/cmd/memqllint": (458.829 + 432.100) / 2,
		// Every package that passed, wherever it lives; which belong to the
		// repository is the caller's question.
		"github.com/elsewhere/tool": 3.0,
	}
	if !sameTimings(got, want) {
		t.Errorf("ParseGoTestOutput =\n%v\nwant\n%v", got, want)
	}
	for _, absent := range []string{
		"example.test/memql/cmd/memqlbreaking", // (cached): no measurement
		"example.test/memql/cmd/memqlmigrate",  // FAIL: cut short, the wrong weight
		"example.test/memql/cmd/nothing",       // [no test files]
		"example.test/memql/fake",              // quoted mid-line by a test
	} {
		if _, ok := got[absent]; ok {
			t.Errorf("%s has a time, but its line is not a passing result", absent)
		}
	}
}

// A runner's log is not GitHub's: no timestamps, Windows line ends, a
// coverage suffix, and lines far longer than any result line. None of them
// may hide a result or stop the read.
func TestParseGoTestOutputReadsAnyRunnersLog(t *testing.T) {
	// The long line carries a decoy result exactly where the reader's buffer
	// splits it: a reader that took the rest of a long line for a new line
	// would read the decoy as a measurement.
	huge := strings.Repeat("x", 2*maxResultLine) + "ok  \texample.test/shop/decoy\t9s"
	log := strings.Join([]string{
		"=== RUN   TestThing",
		huge,
		"ok  \texample.test/shop/a\t1.5s\tcoverage: 40.0% of statements\r",
		"ok  \texample.test/shop/b\t2s",
		"2026-10-03T09:21:44Z ok  \texample.test/shop/c\t0.25s",
		"ok  \texample.test/shop/d\t7.75s",
	}, "\n")
	got, err := ParseGoTestOutput(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"example.test/shop/a": 1.5,
		"example.test/shop/b": 2,
		"example.test/shop/c": 0.25,
		"example.test/shop/d": 7.75, // the last line has no newline
	}
	if !sameTimings(got, want) {
		t.Errorf("ParseGoTestOutput =\n%v\nwant\n%v", got, want)
	}
}

func TestParseGoTestOutputOfNothingIsAnEmptyTable(t *testing.T) {
	got, err := ParseGoTestOutput(strings.NewReader(""))
	if err != nil || len(got) != 0 {
		t.Errorf("ParseGoTestOutput(\"\") = %v, %v", got, err)
	}
}

// A full run's observations replace their rows; every row it did not observe
// is kept, because a package missing from one run's output is not a package
// that stopped existing. A time that cannot be a time is not written.
func TestMergeTimingsReplacesWhatWasObservedAndKeepsTheRest(t *testing.T) {
	table := map[string]float64{"a": 1, "b": 2, "c": 3}
	observed := map[string]float64{
		"b": 20,
		"d": 4,
		"c": math.NaN(),
		"e": -1,
		"f": math.Inf(1),
		"":  9,
	}
	got := MergeTimings(table, observed)
	want := map[string]float64{"a": 1, "b": 20, "c": 3, "d": 4}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeTimings = %v, want %v", got, want)
	}
	if table["b"] != 2 || len(table) != 3 {
		t.Errorf("MergeTimings changed the table it was given: %v", table)
	}
	if got := MergeTimings(nil, map[string]float64{"a": 1}); !reflect.DeepEqual(got, map[string]float64{"a": 1}) {
		t.Errorf("MergeTimings(nil, ...) = %v", got)
	}
	if got := MergeTimings(map[string]float64{"a": 1}, nil); !reflect.DeepEqual(got, map[string]float64{"a": 1}) {
		t.Errorf("MergeTimings(..., nil) = %v", got)
	}
}

func sameTimings(got, want map[string]float64) bool {
	if len(got) != len(want) {
		return false
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok || math.Abs(g-w) > 1e-9 {
			return false
		}
	}
	return true
}
