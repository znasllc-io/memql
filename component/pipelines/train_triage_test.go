package pipelines

import (
	"strings"
	"testing"
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
	unit := TrainFailedStep{Key: "tests.go-unit#2", Packages: []string{mod + "/component/memql"}}
	timeout := TrainFailedStep{Key: "tests.go-unit#3", ErrorCode: CodeStepTimeout, Packages: []string{mod + "/component/memql"}}
	files := map[int][]string{7: {"component/memql/x.go"}, 9: {"docs/a.md"}}
	complete := map[int]bool{7: true, 9: true}
	base := func() TrainTriageInput {
		return TrainTriageInput{Failed: []TrainFailedStep{unit}, MaxRebuilds: 2, Files: files, FilesComplete: complete, ModulePath: mod}
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
		{"the one pull request that touched the failing package", base, "drop", 7, ""},
		{"two pull requests touched it", func() TrainTriageInput {
			in := base()
			in.Files = map[int][]string{7: {"component/memql/x.go"}, 9: {"component/memql/y.go"}}
			return in
		}, "hold", 0, "could not tell which pull request caused tests.go-unit#2"},
		{"main fails the same step", func() TrainTriageInput {
			in := base()
			in.MainFailedKeys = []string{"tests.go-unit#2"}
			return in
		}, "hold", 0, "main fails tests.go-unit#2 too"},
		{"the rebuild cap", func() TrainTriageInput {
			in := base()
			in.Rebuilds = 2
			return in
		}, "hold", 0, "stopped after 2 rebuilds"},
		{"an incomplete file list suspects every package-scoped step", func() TrainTriageInput {
			in := base()
			in.FilesComplete = map[int]bool{7: true, 9: false}
			return in
		}, "hold", 0, "could not tell"},
		{"a step with no packages names no suspect", func() TrainTriageInput {
			in := base()
			in.Failed = []TrainFailedStep{{Key: "checks.lint"}}
			return in
		}, "hold", 0, "could not tell which pull request caused checks.lint"},
		{"a file under a directory suspects its ancestors' packages", func() TrainTriageInput {
			in := base()
			in.Failed = []TrainFailedStep{{Key: "tests.dsl", Packages: []string{mod + "/dsl"}}}
			in.Files = map[int][]string{7: {"dsl/pipelines/x.memql"}, 9: {"docs/a.md"}}
			return in
		}, "drop", 7, ""},
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
