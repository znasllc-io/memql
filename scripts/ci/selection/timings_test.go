package selection

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseGoTestOutputCountsOnlyPassingResultLines(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "go-test-output.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := ParseGoTestOutput(f, fixturePrefix)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]float64{
		".":             {114.859},
		"cmd/memqlfmt":  {0.005},
		"cmd/memqllint": {458.829, 432.1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseGoTestOutput =\n%v\nwant\n%v\n(cached, FAIL, [no test files], foreign packages and a test's own mid-line output must not count)", got, want)
	}
}

func TestTimingTableRoundTrips(t *testing.T) {
	in := Timings{
		"go": {"cmd/memqllint": 445.63, ".": 111.4, "cmd/memqlfmt": 0.01},
		"db": {"component/memql": 496},
	}
	var buf bytes.Buffer
	if err := in.Write(&buf, []string{"scripts/ci/shard-timings.tsv", "measured somewhere"}); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	if !strings.HasPrefix(text, "# scripts/ci/shard-timings.tsv\n# measured somewhere\n") {
		t.Errorf("the header must come first, as comments:\n%s", text)
	}
	if i, j := strings.Index(text, "cmd/memqllint"), strings.Index(text, "cmd/memqlfmt"); i > j {
		t.Errorf("rows are slowest first within a lane:\n%s", text)
	}
	out, err := ReadTimings(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip changed the table:\n%v\n%v", in, out)
	}
}

func TestReadTimingsRefusesAMalformedTable(t *testing.T) {
	cases := map[string]string{
		"two columns":      "go cmd/x\n",
		"unknown lane":     "ui cmd/x 1.0\n",
		"negative seconds": "go cmd/x -1\n",
		"not a number":     "go cmd/x fast\n",
		"duplicate row":    "go cmd/x 1\ngo cmd/x 2\n",
		"unclean dir":      "go cmd/../x 1\n",
		"wildcard dir":     "go cmd/... 1\n",
		"absolute dir":     "go /cmd/x 1\n",
	}
	for name, table := range cases {
		if _, err := ReadTimings(strings.NewReader(table)); err == nil {
			t.Errorf("%s: ReadTimings accepted it", name)
		}
	}
}

func TestMergeReplacesMeasuredRowsWithTheMedian(t *testing.T) {
	tm := Timings{"go": {"a": 1, "b": 2}}
	tm.Merge("go", map[string][]float64{"a": {10, 30, 20}, "c": {4, 6}})
	want := Timings{"go": {"a": 20, "b": 2, "c": 5}}
	if !reflect.DeepEqual(tm, want) {
		t.Errorf("Merge = %v, want %v (a: median of three; b: kept; c: median of two)", tm, want)
	}
	tm.Merge("db", map[string][]float64{"x": {7}})
	if tm["db"]["x"] != 7 {
		t.Errorf("Merge into a lane the table did not have yet: %v", tm)
	}
}
