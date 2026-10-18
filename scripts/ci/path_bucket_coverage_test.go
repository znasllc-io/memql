package ci

// Every tracked file must reach a path-dependent native PR step. In particular,
// gates covers repository-reading tests whose dependencies the Go import graph
// cannot discover. The native pipeline has no exempt paths: the old hosted
// workflow's exemptions and separate workflow claims no longer describe it.

import (
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

func consumedNativePRBuckets(spec *pipelines.Spec) map[string]bool {
	used := map[string]bool{}
	for _, stage := range spec.Stages {
		if len(stage.On) > 0 && !slices.Contains(stage.On, "pull_request") && !slices.Contains(stage.On, "affected") {
			continue
		}
		for _, step := range stage.Steps {
			if step.Run != "" && step.When != nil {
				used[step.When.Bucket] = true
			}
		}
	}
	return used
}

func TestEveryTrackedFileReachesAConsumedBucket(t *testing.T) {
	spec := nativePipeline(t)
	used := consumedNativePRBuckets(spec)
	if len(used) == 0 {
		t.Fatal("native PR pipeline consumes no path buckets")
	}
	var matchers []func(string) bool
	for name := range used {
		match, err := pipelines.PathSet(spec.Select.Buckets[name])
		if err != nil {
			t.Fatal(err)
		}
		matchers = append(matchers, match)
	}
	var orphans []string
	for _, file := range trackedFiles(t) {
		matched := false
		for _, match := range matchers {
			if match(file) {
				matched = true
				break
			}
		}
		if !matched {
			orphans = append(orphans, file)
		}
	}
	if len(orphans) > 0 {
		corpus := loadGateCorpus(t)
		var evidence strings.Builder
		for _, file := range orphans[:min(25, len(orphans))] {
			evidence.WriteString(file + ": read or mentioned by " + strings.Join(corpus.mentionedBy(file), ", ") + "\n")
		}
		t.Fatalf("%d tracked paths reach no consumed native PR bucket; route them to their verifying step:\n%s", len(orphans), evidence.String())
	}
}

func TestUnusedOrScheduledBucketCannotClaimPRCoverage(t *testing.T) {
	spec := &pipelines.Spec{Stages: []pipelines.StageSpec{
		{Name: "pr", On: []string{"pull_request"}, Steps: []pipelines.StepSpec{{Name: "real", Run: "check", When: &pipelines.When{Bucket: "real"}}, {Name: "empty", When: &pipelines.When{Bucket: "empty"}}}},
		{Name: "audit", On: []string{"schedule"}, Steps: []pipelines.StepSpec{{Name: "hidden", Run: "check", When: &pipelines.When{Bucket: "hidden"}}}},
	}}
	got := consumedNativePRBuckets(spec)
	if len(got) != 1 || !got["real"] {
		t.Fatalf("non-PR commands counted as coverage: %v", got)
	}
}
