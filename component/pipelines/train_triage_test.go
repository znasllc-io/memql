package pipelines

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestApprovalLands(t *testing.T) {
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	review := func(login, state, commit string, minutes int) TrainReview {
		return TrainReview{Login: login, State: state, CommitID: commit, SubmittedAt: t0.Add(time.Duration(minutes) * time.Minute)}
	}
	cases := []struct {
		name    string
		reviews []TrainReview
		want    bool
	}{
		{"the owner approved this head", []TrainReview{review("znas-io", "APPROVED", "abc", 0)}, true},
		{"the login is compared without case", []TrainReview{review("Znas-IO", "APPROVED", "abc", 0)}, true},
		{"someone else approved", []TrainReview{review("jmendivilznas", "APPROVED", "abc", 0)}, false},
		{"a comment after the approval does not undo it", []TrainReview{review("znas-io", "APPROVED", "abc", 0), review("znas-io", "COMMENTED", "abc", 5)}, true},
		{"changes requested after the approval undo it", []TrainReview{review("znas-io", "APPROVED", "abc", 0), review("znas-io", "CHANGES_REQUESTED", "abc", 5)}, false},
		{"an approval after requested changes lands", []TrainReview{review("znas-io", "CHANGES_REQUESTED", "abc", 0), review("znas-io", "APPROVED", "abc", 5)}, true},
		{"a dismissed approval decides nothing", []TrainReview{review("znas-io", "DISMISSED", "abc", 0)}, false},
		{"no reviews", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ApprovalLands(c.reviews, "znas-io", "abc"); got != c.want {
				t.Errorf("ApprovalLands = %v, want %v", got, c.want)
			}
		})
	}
}

func TestApprovalForAnOlderHeadDoesNotLand(t *testing.T) {
	// The train was rebuilt after the owner approved: the approval names the
	// commit the owner looked at, not the one that would land.
	reviews := []TrainReview{{Login: "znas-io", State: "APPROVED", CommitID: "old", SubmittedAt: time.Now()}}
	if ApprovalLands(reviews, "znas-io", "new") {
		t.Fatal("an approval of an older head landed the new one")
	}
	if ApprovalLands(reviews, "", "old") {
		t.Fatal("an empty owner login matched someone")
	}
}

func TestTriageTrainFailure(t *testing.T) {
	const mod = "github.com/znasllc-io/memql"
	memqlPkg, numPkg := mod+"/component/memql", mod+"/core/num"
	unit := TrainFailedStep{Key: "tests.go-unit#2", Packages: []string{memqlPkg}}
	timeout := TrainFailedStep{Key: "tests.go-unit#3", ErrorCode: CodeStepTimeout, Packages: []string{memqlPkg}}
	base := func() TrainTriageInput {
		return TrainTriageInput{Failed: []TrainFailedStep{unit}, MaxRebuilds: 2,
			Reach: map[int]TrainReach{7: {Packages: []string{memqlPkg}}, 9: {}}}
	}

	cases := []struct {
		name   string
		in     func() TrainTriageInput
		action string
		drop   int
		reason string
	}{
		{"an environmental failure retries once", func() TrainTriageInput {
			in := base()
			in.Failed = []TrainFailedStep{timeout}
			return in
		}, "retry", 0, ""},
		{"a second environmental failure is judged like any other", func() TrainTriageInput {
			in := base()
			in.Failed, in.Retried = []TrainFailedStep{timeout}, true
			return in
		}, "drop", 7, ""},
		{"a test failure with mixed codes never retries", func() TrainTriageInput {
			in := base()
			in.Failed = []TrainFailedStep{timeout, unit}
			return in
		}, "drop", 7, ""},
		{"the one pull request whose change reaches the failing package", base, "drop", 7, "#7"},
		{"a dependency makes two suspects", func() TrainTriageInput {
			in := base()
			in.Reach = map[int]TrainReach{7: {Packages: []string{memqlPkg}}, 9: {Packages: []string{numPkg, memqlPkg}}}
			return in
		}, "hold", 0, "could not tell which pull request caused tests.go-unit#2"},
		{"main fails the same unsharded step", func() TrainTriageInput {
			in := base()
			in.Failed = []TrainFailedStep{{Key: "vulnerabilities.go-vulnerabilities"}}
			in.MainFailed = []TrainFailedStep{{Key: "vulnerabilities.go-vulnerabilities"}}
			return in
		}, "hold", 0, "main fails vulnerabilities.go-vulnerabilities too"},
		{"main fails the same packages in another shard", func() TrainTriageInput {
			in := base()
			in.MainFailed = []TrainFailedStep{{Key: "tests.go-unit#5", Packages: []string{numPkg, memqlPkg}}}
			return in
		}, "hold", 0, "main fails tests.go-unit#2 too"},
		{"main's same shard number tested other packages", func() TrainTriageInput {
			in := base()
			in.MainFailed = []TrainFailedStep{{Key: "tests.go-unit#2", Packages: []string{numPkg}}}
			return in
		}, "drop", 7, ""},
		{"the rebuild cap", func() TrainTriageInput {
			in := base()
			in.Rebuilds = 2
			return in
		}, "hold", 0, "stopped after 2 rebuilds"},
		{"an unbounded change is a suspect everywhere", func() TrainTriageInput {
			in := base()
			in.Reach[9] = TrainReach{Unbounded: true, Why: "a Go module graph file changed"}
			return in
		}, "hold", 0, "could not tell"},
		{"an unbounded change is the one suspect when the rest is out of reach", func() TrainTriageInput {
			in := base()
			in.Reach = map[int]TrainReach{7: {Packages: []string{numPkg}}, 9: {Unbounded: true, Why: "a Go module graph file changed"}}
			return in
		}, "drop", 9, "a Go module graph file changed"},
		{"a step with no packages names no suspect", func() TrainTriageInput {
			in := base()
			in.Failed = []TrainFailedStep{{Key: "checks.lint"}}
			return in
		}, "hold", 0, "could not tell which pull request caused checks.lint"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TriageTrainFailure(c.in())
			if got.Action != c.action || got.Drop != c.drop {
				t.Fatalf("TriageTrainFailure = %+v, want action %q drop %d", got, c.action, c.drop)
			}
			if c.reason != "" && !strings.Contains(got.Reason, c.reason) {
				t.Errorf("reason %q does not say %q", got.Reason, c.reason)
			}
			if got.Reason == "" {
				t.Errorf("verdict %q carries no reason for the owner", got.Action)
			}
		})
	}
}

// TestTrainReachOf: a pull request's reach is what the pipeline's own
// affected analysis would select for its change -- the packages its files
// are in and every package importing them -- and unbounded wherever the
// shipped selection policy would run everything.
func TestTrainReachOf(t *testing.T) {
	g, err := ScanGoTree(fstest.MapFS{
		"go.mod":               {Data: []byte("module example.com/shop\n\ngo 1.26\n")},
		"core/num/num.go":      {Data: []byte("package num\n")},
		"component/memql/x.go": {Data: []byte("package memql\n\nimport _ \"example.com/shop/core/num\"\n")},
		"component/other/o.go": {Data: []byte("package other\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	full := []string{"deploy/**"}
	cases := []struct {
		name     string
		files    []string
		complete bool
		graph    *Graph
		want     []string
		why      string
	}{
		{"a package change reaches its importers", []string{"core/num/num.go"}, true, g, []string{"example.com/shop/component/memql", "example.com/shop/core/num"}, ""},
		{"a change outside every package reaches nothing", []string{"docs/a.md"}, true, g, nil, ""},
		{"a module graph file is unbounded", []string{"go.sum"}, true, g, nil, "a Go module graph file changed"},
		{"the manifest is unbounded", []string{"memql-package.yaml"}, true, g, nil, "the pipeline manifest changed"},
		{"a select.full path is unbounded", []string{"deploy/k8s/x.yaml"}, true, g, nil, "a configured full-selection path changed"},
		{"vendored code is unbounded", []string{"vendor/x/y.go"}, true, g, nil, "vendored code changed"},
		{"an incomplete file list is unbounded", []string{"core/num/num.go"}, false, g, nil, "the changed-file comparison is unavailable"},
		{"an empty file list is unbounded", nil, true, g, nil, "the change list is empty"},
		{"no import graph is unbounded", []string{"core/num/num.go"}, true, nil, nil, "the import graph is unavailable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TrainReachOf(c.graph, c.files, c.complete, full)
			if c.why != "" {
				if !got.Unbounded || got.Why != c.why {
					t.Fatalf("reach = %+v, want unbounded because %q", got, c.why)
				}
				return
			}
			if got.Unbounded || strings.Join(got.Packages, ",") != strings.Join(c.want, ",") {
				t.Fatalf("reach = %+v, want packages %v", got, c.want)
			}
		})
	}
}
