package main

import (
	"strings"
	"testing"
	"time"
)

func at(min int) time.Time {
	return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute)
}

func TestGoTouchingCountsAShardThatRan(t *testing.T) {
	cases := []struct {
		name string
		jobs []Job
		want bool
	}{
		{"a go shard passed", []Job{{Name: "go-tests (default-1)", Conclusion: "success"}}, true},
		{"a db shard failed", []Job{{Name: "db-tests (memql-1)", Conclusion: "failure"}}, true},
		{"shards skipped", []Job{{Name: "go-tests (default-1)", Conclusion: "skipped"}, {Name: "os-checks", Conclusion: "success"}}, false},
		{"a tag pass is not a shard", []Job{{Name: "go-tests -tags agent", Conclusion: "success"}}, false},
	}
	for _, c := range cases {
		if got := GoTouching(c.jobs); got != c.want {
			t.Errorf("%s: GoTouching = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestClocksEndAtCiRequired(t *testing.T) {
	r := Run{CreatedAt: at(0), Jobs: []Job{
		{Name: "plan", StartedAt: at(1), CompletedAt: at(2)},
		{Name: "go-checks", StartedAt: at(2), CompletedAt: at(9)},
		{Name: "ci-required", StartedAt: at(10), CompletedAt: at(11)},
	}}
	fromCreation, fromFirstJob, ok := Clocks(r)
	if !ok || fromCreation != 11*time.Minute || fromFirstJob != 10*time.Minute {
		t.Fatalf("Clocks = %v, %v, %v; want 11m, 10m, true", fromCreation, fromFirstJob, ok)
	}
	if _, _, ok := Clocks(Run{CreatedAt: at(0), Jobs: []Job{{Name: "plan", StartedAt: at(1), CompletedAt: at(2)}}}); ok {
		t.Fatal("a run whose ci-required never completed has a clock")
	}
}

func TestQuantileInterpolatesBetweenRanks(t *testing.T) {
	var ds []time.Duration
	for _, m := range []int{7, 1, 9, 3, 5, 2, 10, 4, 8, 6} {
		ds = append(ds, time.Duration(m)*time.Minute)
	}
	for _, c := range []struct {
		q    float64
		want time.Duration
	}{{0.5, 330 * time.Second}, {0.25, 195 * time.Second}, {0.75, 465 * time.Second}} {
		if got := Quantile(ds, c.q); got != c.want {
			t.Errorf("Quantile(%v) = %v, want %v", c.q, got, c.want)
		}
	}
	if Quantile(nil, 0.5) != 0 || Quantile([]time.Duration{time.Minute}, 0.75) != time.Minute {
		t.Error("empty or single-sample quantile is wrong")
	}
}

func TestParsePlanLogReadsModeAndSelection(t *testing.T) {
	affected := "2026-10-10T05:15:40.1234567Z ### CI plan: affected\n2026-10-10T05:15:40.2Z \n2026-10-10T05:15:40.3Z 3 seed package(s), 42 selected.\n"
	if mode, selected := ParsePlanLog(affected); mode != "affected" || selected != 42 {
		t.Errorf("affected log = %q, %d", mode, selected)
	}
	if mode, selected := ParsePlanLog("2026-10-10T05:15:40Z ### CI plan: full\n"); mode != "full" || selected != 0 {
		t.Errorf("full log = %q, %d", mode, selected)
	}
	if mode, _ := ParsePlanLog("no plan here"); mode != "" {
		t.Errorf("garbage log read as %q", mode)
	}
}

func TestSlowestDBShardIsTheLongestDbShard(t *testing.T) {
	r := Run{Jobs: []Job{
		{Name: "db-tests (memql-1)", Conclusion: "success", StartedAt: at(0), CompletedAt: at(7)},
		{Name: "db-tests (work)", Conclusion: "success", StartedAt: at(0), CompletedAt: at(3)},
		{Name: "go-tests (default-1)", Conclusion: "success", StartedAt: at(0), CompletedAt: at(9)},
	}}
	if got := SlowestDBShard(r); got != 7*time.Minute {
		t.Fatalf("SlowestDBShard = %v, want 7m", got)
	}
}

func TestLogArgsPassTheEscapeFlagOnlyWhereGhOffersIt(t *testing.T) {
	if got := strings.Join(logArgs("acme/x", 7, "      --allow-escape-sequences   Allow printing terminal escape sequences"), " "); got != "api --allow-escape-sequences repos/acme/x/actions/jobs/7/logs" {
		t.Errorf("gh 2.102: %q", got)
	}
	if got := strings.Join(logArgs("acme/x", 7, "      --jq string   Query to select values"), " "); got != "api repos/acme/x/actions/jobs/7/logs" {
		t.Errorf("an older gh: %q", got)
	}
}

func TestReportCountsGoTouchingRunsAndPrintsT(t *testing.T) {
	run := func(id int64, end int, mode string, goShard bool) Run {
		jobs := []Job{{Name: "ci-required", Conclusion: "success", StartedAt: at(end - 1), CompletedAt: at(end)}}
		if goShard {
			jobs = append(jobs, Job{Name: "go-tests (default-1)", Conclusion: "success", StartedAt: at(1), CompletedAt: at(end - 2)})
		}
		return Run{ID: id, CreatedAt: at(0), Jobs: jobs, Mode: mode}
	}
	out := Report([]Run{run(1, 10, "affected", true), run(2, 12, "full", true), run(3, 5, "affected", false)}, "2026-10-04", "2026-10-10")
	for _, want := range []string{"Runs measured: 3", "Go-touching: 2", "T (median, creation to ci-required): 11.0 min"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}
