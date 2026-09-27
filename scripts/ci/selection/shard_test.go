package selection

import (
	"reflect"
	"strings"
	"testing"
)

// ciClasses mirrors the table ci.yml's plan step carries. It is a fixture, not
// a pin: scripts/cidb's live test is what holds ci.yml's real table to the
// provisioned packages.
const ciClasses = `
# lane  class     trees               timeout  mode      shards
go      root      .                   300s     uncached  1
go      default   -                   600s     -         3
db      memql     component/memql     600s     serial    2
db      packages  component/packages  300s     -         1
db      default   -                   180s     -         2
`

func mustClasses(t *testing.T, text string) []Class {
	t.Helper()
	c, err := ParseClasses(text)
	if err != nil {
		t.Fatalf("ParseClasses: %v", err)
	}
	return c
}

func TestParseClassesReadsTheTable(t *testing.T) {
	got := mustClasses(t, ciClasses)
	want := []Class{
		{Lane: "go", Name: "root", Trees: []string{"."}, Timeout: "300s", Uncached: true, Shards: 1},
		{Lane: "go", Name: "default", Timeout: "600s", Shards: 3},
		{Lane: "db", Name: "memql", Trees: []string{"component/memql"}, Timeout: "600s", Serial: true, Shards: 2},
		{Lane: "db", Name: "packages", Trees: []string{"component/packages"}, Timeout: "300s", Shards: 1},
		{Lane: "db", Name: "default", Timeout: "180s", Shards: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseClasses =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseClassesRefusesABadTable(t *testing.T) {
	valid := "go default - 600s - 1\ndb default - 180s - 1\n"
	if _, err := ParseClasses(valid); err != nil {
		t.Fatalf("the minimal valid table must parse, or the cases below prove nothing: %v", err)
	}
	cases := map[string]string{
		"five columns":            "go default - 600s -\ndb default - 180s - 1",
		"unknown lane":            valid + "ui x a 60s - 1",
		"bad class name":          valid + "go Root . 60s - 1",
		"duplicate class":         valid + "go default - 60s - 1",
		"missing go default":      "db default - 180s - 1",
		"two db defaults":         valid + "db extra - 60s - 1",
		"wildcard tree":           valid + "go x ./cmd/... 60s - 1",
		"tree climbing out":       valid + "go x ../x 60s - 1",
		"timeout without unit":    valid + "go x cmd 600 - 1",
		"timeout in minutes":      valid + "go x cmd 10m - 1",
		"unknown mode":            valid + "go x cmd 60s parallel 1",
		"zero shards":             valid + "go x cmd 60s - 0",
		"too many shards":         valid + "go x cmd 60s - 99",
		"non-numeric shard count": valid + "go x cmd 60s - many",
	}
	for name, table := range cases {
		if _, err := ParseClasses(table); err == nil {
			t.Errorf("%s: ParseClasses accepted it", name)
		}
	}
}

// measured is a slice of the real 2026-09-27 medians, enough to show the
// balance the table buys.
var measured = map[string]float64{
	"component/memql":             496.00,
	"component/memql/offline":     78.82,
	"component/packages":          161.86,
	"component/automations":       155.77,
	"component/automations/steps": 158.15,
	"integrations/work":           130.14,
	"component/grpc":              119.09,
	"app":                         98.64,
	"cmd/memqllint":               445.63,
	"cmd/memql-lsp":               345.97,
	"test/conformance":            283.19,
	".":                           111.40,
	"scripts/k3d":                 56.71,
	"component/architecture":      54.88,
}

func shardByName(shards []Shard, name string) (Shard, bool) {
	for _, s := range shards {
		if s.Name == name {
			return s, true
		}
	}
	return Shard{}, false
}

func TestPartitionKeepsBudgetsApart(t *testing.T) {
	classes := mustClasses(t, ciClasses)
	dbDirs := []string{"component/memql", "component/memql/offline", "component/memql/sense", "component/packages",
		"component/automations", "component/automations/steps", "integrations/work", "component/grpc", "app"}
	shards, err := Partition("db", dbDirs, classes, measured, 4)
	if err != nil {
		t.Fatal(err)
	}

	memql, ok := shardByName(shards, "memql-1")
	if !ok || !reflect.DeepEqual(memql.Dirs, []string{"component/memql"}) {
		t.Errorf("component/memql must run alone in its shard, got %+v", memql)
	}
	if memql.Parallel != 1 || memql.Timeout != "600s" {
		t.Errorf("the memql class is serial at 600s, got -p=%d -timeout=%s", memql.Parallel, memql.Timeout)
	}
	pkgs, ok := shardByName(shards, "packages")
	if !ok || !reflect.DeepEqual(pkgs.Dirs, []string{"component/packages"}) || pkgs.Timeout != "300s" {
		t.Errorf("component/packages must run in its own 300s class, never beside component/memql; got %+v", pkgs)
	}
	for _, s := range shards {
		if s.Class == "default" && (s.Timeout != "180s" || s.Parallel != 4) {
			t.Errorf("default db shard %s: -p=%d -timeout=%s, want -p=4 -timeout=180s", s.Name, s.Parallel, s.Timeout)
		}
		for _, d := range s.Dirs {
			if strings.HasPrefix(d, "component/memql") && s.Class != "memql" {
				t.Errorf("%s left the memql class for %s", d, s.Name)
			}
		}
	}
	// component/memql/sense is not in the table: it must land in the memql
	// class's LIGHTER shard, which is the one without component/memql.
	two, _ := shardByName(shards, "memql-2")
	if !contains(two.Dirs, "component/memql/sense") || !contains(two.Unknown, "component/memql/sense") {
		t.Errorf("an unmeasured package must land in the lightest shard of its class; memql-2 = %+v", two)
	}
}

func TestPartitionBalancesLongestFirst(t *testing.T) {
	classes := mustClasses(t, ciClasses)
	goDirs := []string{".", "cmd/memqllint", "cmd/memql-lsp", "test/conformance", "scripts/k3d", "component/architecture"}
	shards, err := Partition("go", goDirs, classes, measured, 4)
	if err != nil {
		t.Fatal(err)
	}
	root, ok := shardByName(shards, "root")
	if !ok || root.Packages != "." || !root.Uncached || root.Timeout != "300s" {
		t.Errorf("the root package runs alone, uncached, at 300s: %+v", root)
	}
	var heavy []string
	for _, s := range shards {
		if s.Class != "default" {
			continue
		}
		n := 0
		for _, d := range s.Dirs {
			if d == "cmd/memqllint" || d == "cmd/memql-lsp" || d == "test/conformance" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("shard %s holds %d of the three heaviest packages; LPT puts one in each: %v", s.Name, n, s.Dirs)
		}
		heavy = append(heavy, s.Packages)
		if s.Uncached {
			t.Errorf("shard %s is uncached; only the root package's class is", s.Name)
		}
	}
	if len(heavy) != 3 {
		t.Fatalf("want 3 default go shards, got %d", len(heavy))
	}
}

func TestPartitionIsDeterministicAndLossless(t *testing.T) {
	classes := mustClasses(t, ciClasses)
	dirs := []string{"b", "a", "c", "component/memql", "d"}
	first, err := Partition("db", dirs, classes, map[string]float64{"a": 10, "b": 10}, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := Partition("db", dirs, classes, map[string]float64{"a": 10, "b": 10}, 2)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("Partition is not deterministic:\n%+v\n%+v", first, again)
		}
	}
	placed := map[string]bool{}
	for _, s := range first {
		for _, d := range s.Dirs {
			placed[d] = true
		}
	}
	for _, d := range dirs {
		if !placed[d] {
			t.Errorf("%s was not placed in any shard", d)
		}
	}
}

func TestPartitionRefusesWhatItCannotPlace(t *testing.T) {
	classes := mustClasses(t, ciClasses)
	if _, err := Partition("db", []string{"a", "a"}, classes, nil, 4); err == nil {
		t.Error("a package listed twice must be refused")
	}
	if _, err := Partition("ui", []string{"a"}, classes, nil, 4); err == nil {
		t.Error("a lane with no class must be refused")
	}
	if _, err := Partition("db", []string{"a"}, classes, nil, 0); err == nil {
		t.Error("zero cpus must be refused")
	}
	noDefault := []Class{{Lane: "go", Name: "only", Trees: []string{"x"}, Timeout: "60s", Shards: 1}}
	if _, err := Partition("go", []string{"y"}, noDefault, nil, 4); err == nil {
		t.Error("a package no class holds must be refused")
	}
}

func TestPartitionTreeDotIsTheRootOnly(t *testing.T) {
	classes := mustClasses(t, ciClasses)
	shards, err := Partition("go", []string{".", "cmd/memqlfmt"}, classes, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := shardByName(shards, "root")
	if !reflect.DeepEqual(root.Dirs, []string{"."}) {
		t.Errorf("the tree \".\" must hold the root package only, got %v", root.Dirs)
	}
}
