package pipelinesteps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/deploycontrol"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// These began as the decision and pass tests of the installation-owned Python
// promoter this file's production half replaced, case for case, so the behaviour
// that ran in production moved with the code. (Its manifest tests became the
// component's render gates.) The rest is what moving it into the engine added:
// windows that follow each run's ceiling, the compare-and-swap, and the contract
// with the Job the runner really builds.

var promotionNow = time.Date(2026, 10, 29, 14, 30, 0, 0, time.UTC)

const (
	promotionQueue = "memql-background"
	boostedRun     = "79bb3043f59bf98ed9c0898cb74d823f"
	unboostedRun   = "f16407c4342a0f40314b9f96a7c65a6d"
	// Under the fixed windows below: 60 minutes left, then 248 minutes left.
	dueExpiring = "2026-10-29T15:30:15Z"
	dueAged     = "2026-10-29T18:38:08Z"
	notYetDue   = "2026-10-30T02:00:24Z"
)

// fixedWindows are the numbers production ran by hand before they followed the
// ceiling: three hours and six, of a 720-minute ceiling.
func fixedWindows() DeadlinePriority {
	return DeadlinePriority{Queue: promotionQueue, ExpiringWindow: 180 * time.Minute, AgedWindow: 360 * time.Minute}
}

type waitingJobOption func(*Job)

func withDeadline(value string) waitingJobOption {
	return func(j *Job) { j.Metadata.Annotations[AnnotRunDeadline] = value }
}
func withoutDeadline() waitingJobOption {
	return func(j *Job) { delete(j.Metadata.Annotations, AnnotRunDeadline) }
}
func withClass(class string) waitingJobOption {
	return func(j *Job) {
		if class == "" {
			delete(j.Metadata.Labels, queuePriorityLabel)
			return
		}
		j.Metadata.Labels[queuePriorityLabel] = class
	}
}
func withRun(id string) waitingJobOption {
	return func(j *Job) { j.Metadata.Labels[LabelRun] = id }
}
func withQueue(name string) waitingJobOption {
	return func(j *Job) { j.Metadata.Labels[queueLabel] = name }
}
func withSuspend(value *bool) waitingJobOption {
	return func(j *Job) { j.Spec.Suspend = value }
}
func withCeiling(value string) waitingJobOption {
	return func(j *Job) { j.Metadata.Annotations[AnnotRunCeiling] = value }
}
func jobNamed(name string) waitingJobOption {
	return func(j *Job) { j.Metadata.Name = name }
}

func waitingJob(options ...waitingJobOption) Job {
	job := Job{
		Metadata: ObjectMeta{
			Name:            "mp-1",
			ResourceVersion: "41",
			Labels: map[string]string{
				queueLabel:         promotionQueue,
				LabelManagedBy:     ManagedBy,
				LabelRun:           unboostedRun,
				queuePriorityLabel: "memql-pipeline-audit",
			},
			Annotations: map[string]string{
				AnnotStepKey:     "secrets-history.gitleaks",
				AnnotRunDeadline: dueExpiring,
			},
		},
		Spec: JobSpec{Suspend: ptr(true)},
	}
	for _, option := range options {
		option(&job)
	}
	return job
}

func TestAWaitingJobNearItsDeadlineMovesToTheExpiringClass(t *testing.T) {
	promotion, reason := fixedWindows().decide(waitingJob(), promotionNow)
	require.Equal(t, promotionDue, reason)
	require.Equal(t, jobPromotion{To: ClassExpiring, From: "memql-pipeline-audit", Reason: "run deadline within 180 minutes"}, promotion)
}

func TestAnOldRunMovesToTheAgedClass(t *testing.T) {
	promotion, reason := fixedWindows().decide(waitingJob(withDeadline(dueAged), withClass("memql-pipeline-check")), promotionNow)
	require.Equal(t, promotionDue, reason)
	require.Equal(t, ClassAged, promotion.To)
	require.Equal(t, "run deadline within 360 minutes", promotion.Reason)
}

func TestAnAgedJobMovesUpWhenItsRunNearsExpiry(t *testing.T) {
	promotion, _ := fixedWindows().decide(waitingJob(withClass(ClassAged)), promotionNow)
	require.Equal(t, ClassExpiring, promotion.To)
	require.Equal(t, ClassAged, promotion.From)
}

func TestAJobNeverMovesDownATier(t *testing.T) {
	for name, job := range map[string]Job{
		"expiring, with only the aged window due": waitingJob(withClass(ClassExpiring), withDeadline(dueAged)),
		"aged, with the aged window due":          waitingJob(withClass(ClassAged), withDeadline(dueAged)),
		"expiring, with the expiring window due":  waitingJob(withClass(ClassExpiring)),
	} {
		_, reason := fixedWindows().decide(job, promotionNow)
		require.Equal(t, promotionAlreadyDone, reason, name)
	}
}

func TestAdmittedWorkIsNeverTouched(t *testing.T) {
	for _, suspend := range []*bool{ptr(false), nil} {
		_, reason := fixedWindows().decide(waitingJob(withSuspend(suspend)), promotionNow)
		require.Equal(t, promotionNotWaiting, reason)
	}
}

func TestAnExpiredJobIsLeftForTheRunnerToFail(t *testing.T) {
	for _, deadline := range []string{"2026-10-29T14:29:59Z", "2026-10-29T14:30:00Z"} {
		_, reason := fixedWindows().decide(waitingJob(withDeadline(deadline)), promotionNow)
		require.Equal(t, promotionExpired, reason, deadline)
	}
}

func TestAYoungRunKeepsItsEventClass(t *testing.T) {
	_, reason := fixedWindows().decide(waitingJob(withDeadline("2026-10-29T20:30:01Z")), promotionNow)
	require.Equal(t, promotionNotDue, reason)
	aged, _ := fixedWindows().decide(waitingJob(withDeadline("2026-10-29T20:30:00Z")), promotionNow)
	require.Equal(t, ClassAged, aged.To, "a window's edge is inside it")
	expiring, _ := fixedWindows().decide(waitingJob(withDeadline("2026-10-29T17:30:00Z")), promotionNow)
	require.Equal(t, ClassExpiring, expiring.To, "a window's edge is inside it")
}

func TestTheExpiringTierWorksAlone(t *testing.T) {
	alone := DeadlinePriority{Queue: promotionQueue, ExpiringWindow: 180 * time.Minute, AgedOff: true}
	_, reason := alone.decide(waitingJob(withDeadline(dueAged)), promotionNow)
	require.Equal(t, promotionNotDue, reason)
	promotion, _ := alone.decide(waitingJob(), promotionNow)
	require.Equal(t, ClassExpiring, promotion.To)
}

func TestAJobWithoutAReadableDeadlineIsLeftAlone(t *testing.T) {
	_, reason := fixedWindows().decide(waitingJob(withoutDeadline()), promotionNow)
	require.Equal(t, promotionNoDeadline, reason)
	for _, value := range []string{"", "tomorrow", "2026-10-29T15:30:15"} {
		_, reason := fixedWindows().decide(waitingJob(withDeadline(value)), promotionNow)
		require.Equal(t, promotionNoDeadline, reason, value)
	}
}

func TestOnlyTheNamedQueueIsReordered(t *testing.T) {
	_, reason := fixedWindows().decide(waitingJob(withQueue("other")), promotionNow)
	require.Equal(t, promotionOtherQueue, reason)
}

func TestAJobWithNoClassRecordsAnEmptyOrigin(t *testing.T) {
	promotion, reason := fixedWindows().decide(waitingJob(withClass("")), promotionNow)
	require.Equal(t, promotionDue, reason)
	require.Empty(t, promotion.From)
}

func TestRunnerInstantsParseWithNanosecondsAndOffsets(t *testing.T) {
	for _, deadline := range []string{"2026-10-29T15:30:15.123456789Z", "2026-10-29T08:30:15.123456-07:00"} {
		promotion, reason := fixedWindows().decide(waitingJob(withDeadline(deadline)), promotionNow)
		require.Equal(t, promotionDue, reason, deadline)
		require.Equal(t, ClassExpiring, promotion.To, deadline)
	}
}

func boostingWindows() DeadlinePriority {
	p := fixedWindows()
	p.BoostRuns = []string{boostedRun}
	return p
}

func TestABoostedRunGoesAheadOfOldRuns(t *testing.T) {
	promotion, reason := boostingWindows().decide(waitingJob(withRun(boostedRun), withDeadline(notYetDue), withClass("memql-pipeline-check")), promotionNow)
	require.Equal(t, promotionDue, reason)
	require.Equal(t, jobPromotion{To: ClassBoosted, From: "memql-pipeline-check", Reason: "boosted run"}, promotion)
}

func TestExpiryStillOutranksABoost(t *testing.T) {
	promotion, _ := boostingWindows().decide(waitingJob(withRun(boostedRun)), promotionNow)
	require.Equal(t, ClassExpiring, promotion.To)
}

func TestAnAgedJobOfABoostedRunMovesUp(t *testing.T) {
	promotion, _ := boostingWindows().decide(waitingJob(withRun(boostedRun), withClass(ClassAged), withDeadline(dueAged)), promotionNow)
	require.Equal(t, ClassBoosted, promotion.To)
}

func TestABoostedJobNeverMovesDown(t *testing.T) {
	_, reason := boostingWindows().decide(waitingJob(withRun(boostedRun), withClass(ClassExpiring), withDeadline(notYetDue)), promotionNow)
	require.Equal(t, promotionAlreadyDone, reason)
}

// The installation-owned copy forgot a class once no run named it: a Job boosted
// earlier whose run left the list read as unranked, so entering the aged window
// moved it DOWN from 375 to 350, against the promise that a Job only moves up.
func TestARunDroppedFromTheBoostListKeepsItsClass(t *testing.T) {
	_, reason := fixedWindows().decide(waitingJob(withRun(boostedRun), withClass(ClassBoosted), withDeadline(dueAged)), promotionNow)
	require.Equal(t, promotionAlreadyDone, reason)
	promotion, _ := fixedWindows().decide(waitingJob(withRun(boostedRun), withClass(ClassBoosted)), promotionNow)
	require.Equal(t, ClassExpiring, promotion.To, "expiry still reaches it")
}

func TestAnUnlistedYoungRunKeepsItsClass(t *testing.T) {
	_, reason := boostingWindows().decide(waitingJob(withDeadline(notYetDue)), promotionNow)
	require.Equal(t, promotionNotDue, reason)
}

func TestABoostedButExpiredJobIsLeftAlone(t *testing.T) {
	_, reason := boostingWindows().decide(waitingJob(withRun(boostedRun), withDeadline("2026-10-29T14:00:00Z")), promotionNow)
	require.Equal(t, promotionExpired, reason)
}

// Windows are minutes before a deadline, and the ceiling they are measured
// against is an installation's value: 120 by default, 480 under the long-run
// profile, 720 on the installation these first ran on. Fixed defaults of 180 and
// 360 minutes would put every waiting Job on a default installation in the top
// class at the first pass, and with that erase the event order the queue exists
// for. So by default a Job's windows are shares of its own run's ceiling.
func TestWindowsFollowEachRunsOwnCeiling(t *testing.T) {
	derived := DeadlinePriority{Queue: promotionQueue}
	at := func(left time.Duration) string { return promotionNow.Add(left).Format(time.RFC3339) }
	for _, c := range []struct {
		ceiling        string
		expiring, aged time.Duration
	}{
		{"120", 30 * time.Minute, 60 * time.Minute},
		{"480", 120 * time.Minute, 240 * time.Minute},
		{"720", 180 * time.Minute, 360 * time.Minute},
	} {
		t.Run(c.ceiling, func(t *testing.T) {
			_, reason := derived.decide(waitingJob(withCeiling(c.ceiling), withDeadline(at(c.aged+time.Second))), promotionNow)
			require.Equal(t, promotionNotDue, reason)
			aged, _ := derived.decide(waitingJob(withCeiling(c.ceiling), withDeadline(at(c.aged))), promotionNow)
			require.Equal(t, ClassAged, aged.To)
			stillAged, _ := derived.decide(waitingJob(withCeiling(c.ceiling), withDeadline(at(c.expiring+time.Second))), promotionNow)
			require.Equal(t, ClassAged, stillAged.To)
			expiring, _ := derived.decide(waitingJob(withCeiling(c.ceiling), withDeadline(at(c.expiring))), promotionNow)
			require.Equal(t, ClassExpiring, expiring.To)
			require.Equal(t, "run deadline within "+strconv.Itoa(int(c.expiring/time.Minute))+" minutes", expiring.Reason)
		})
	}
}

func TestABoostNeedsNoWindowDue(t *testing.T) {
	derived := DeadlinePriority{Queue: promotionQueue, BoostRuns: []string{boostedRun}}
	promotion, _ := derived.decide(waitingJob(withRun(boostedRun), withCeiling("720"), withDeadline(notYetDue)), promotionNow)
	require.Equal(t, ClassBoosted, promotion.To)
	// Nor a ceiling: an owner naming a run across an upgrade must not be ignored
	// because its Jobs were built before the runner wrote one.
	promotion, _ = derived.decide(waitingJob(withRun(boostedRun), withDeadline(notYetDue)), promotionNow)
	require.Equal(t, ClassBoosted, promotion.To)
}

// A Job from before the runner wrote its ceiling, or with a value nothing wrote,
// has no window to measure. It is left where it is and counted, never promoted
// by a guess: the guess that matters is the one that promotes everything.
func TestAJobWithNoReadableCeilingIsLeftAloneUnderDerivedWindows(t *testing.T) {
	derived := DeadlinePriority{Queue: promotionQueue}
	_, reason := derived.decide(waitingJob(), promotionNow)
	require.Equal(t, promotionNoCeiling, reason)
	for _, value := range []string{"", "soon", "0", "-5"} {
		_, reason := derived.decide(waitingJob(withCeiling(value)), promotionNow)
		require.Equal(t, promotionNoCeiling, reason, value)
	}
}

func TestStatedWindowsNeedNoCeiling(t *testing.T) {
	promotion, reason := fixedWindows().decide(waitingJob(), promotionNow)
	require.Equal(t, promotionDue, reason)
	require.Equal(t, ClassExpiring, promotion.To)
}

func TestOneStatedWindowLeavesTheOtherDerived(t *testing.T) {
	statedExpiring := DeadlinePriority{Queue: promotionQueue, ExpiringWindow: 90 * time.Minute}
	fiveHoursLeft := withDeadline(promotionNow.Add(5 * time.Hour).Format(time.RFC3339))
	promotion, _ := statedExpiring.decide(waitingJob(withCeiling("720"), fiveHoursLeft), promotionNow)
	require.Equal(t, ClassAged, promotion.To, "half of 720 minutes still applies")
	_, reason := statedExpiring.decide(waitingJob(fiveHoursLeft), promotionNow)
	require.Equal(t, promotionNoCeiling, reason, "the aged window still needs the ceiling")
	promotion, _ = statedExpiring.decide(waitingJob(), promotionNow)
	require.Equal(t, ClassExpiring, promotion.To, "the stated window does not: this Job is inside it")
}

// The contract across the seam: the Job the runner actually builds, as the API
// server hands it back, is one a pass can read. The labels, the annotations and
// the deadline's format are all named by the runner, so a change to any of them
// fails here rather than in a queue that quietly stops being reordered.
func TestAQueuedJobTheRunnerBuildsIsOneAPassCanPromote(t *testing.T) {
	cfg, run := testConfig(), testRun()
	cfg.QueueName, cfg.NodePool = promotionQueue, "builds"
	cfg.QueuePriorityClasses = `{"pull_request":"memql-pipeline-check"}`
	cfg.RunCeiling = 720 * time.Minute
	run.Event = pl.EventPullRequest
	run.Env = map[string]string{eventVar: string(pl.EventPullRequest)}
	run.RunID = unboostedRun
	run.RunDeadline = promotionNow.Add(2*time.Hour + 123456789*time.Nanosecond).Format(time.RFC3339Nano)
	built, err := BuildJob(cfg, run, testJobName)
	require.NoError(t, err)
	require.Equal(t, "720", built.Metadata.Annotations[AnnotRunCeiling])

	wire, err := json.Marshal(built)
	require.NoError(t, err)
	var read Job
	require.NoError(t, json.Unmarshal(wire, &read))

	promotion, reason := DeadlinePriority{Queue: promotionQueue}.decide(read, promotionNow)
	require.Equal(t, promotionDue, reason)
	require.Equal(t, jobPromotion{To: ClassExpiring, From: "memql-pipeline-check", Reason: "run deadline within 180 minutes"}, promotion)
}

func TestAJobOutsideAQueueCarriesNoCeiling(t *testing.T) {
	cfg, run := testConfig(), testRun()
	cfg.RunCeiling = 720 * time.Minute
	built, err := BuildJob(cfg, run, testJobName)
	require.NoError(t, err)
	require.NotContains(t, built.Metadata.Annotations, AnnotRunCeiling)
}

// promotionAPI is the part of the API server a pass talks to: the Jobs list,
// served a page at a time, and a merge patch on one Job.
type promotionAPI struct {
	mu       sync.Mutex
	pages    [][]Job
	status   map[string]int
	patches  map[string]map[string]any
	paths    []string
	listCode int
}

func newPromotionAPI(t *testing.T, pages ...[]Job) (*promotionAPI, *Kube) {
	t.Helper()
	api := &promotionAPI{pages: pages, status: map[string]int{}, patches: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(srv.Close)
	return api, NewKube(deploycontrol.NewClusterAPIWith(srv.URL, "test-token", srv.Client()), "memql-pipelines")
}

func (a *promotionAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	const jobs = "/apis/batch/v1/namespaces/memql-pipelines/jobs"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == jobs:
		if a.listCode != 0 {
			http.Error(w, `{"message":"forbidden"}`, a.listCode)
			return
		}
		a.paths = append(a.paths, r.URL.RawQuery)
		page := 0
		if cont := r.URL.Query().Get("continue"); cont != "" {
			page, _ = strconv.Atoi(cont)
		}
		list := map[string]any{"items": []Job{}, "metadata": map[string]any{}}
		if page < len(a.pages) {
			list["items"] = a.pages[page]
		}
		if page+1 < len(a.pages) {
			list["metadata"] = map[string]any{"continue": strconv.Itoa(page + 1)}
		}
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, jobs+"/"):
		name := strings.TrimPrefix(r.URL.Path, jobs+"/")
		if r.Header.Get("Content-Type") != "application/merge-patch+json" {
			http.Error(w, "not a merge patch", http.StatusUnsupportedMediaType)
			return
		}
		if code := a.status[name]; code != 0 {
			http.Error(w, `{"message":"refused by the fixture"}`, code)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var patch map[string]any
		if err := json.Unmarshal(body, &patch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a.patches[name] = patch
		_, _ = w.Write([]byte(`{"metadata":{"name":"` + name + `"}}`))
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
	}
}

func patchedClass(t *testing.T, a *promotionAPI, name string) string {
	t.Helper()
	patch, found := a.patches[name]
	require.True(t, found, "no patch for %s", name)
	labels := patch["metadata"].(map[string]any)["labels"].(map[string]any)
	require.Len(t, labels, 1, "a pass writes the class label and no other")
	return labels[queuePriorityLabel].(string)
}

func TestAPassPatchesOnlyDueJobsAndSurvivesARefusal(t *testing.T) {
	api, kube := newPromotionAPI(t, []Job{
		waitingJob(jobNamed("due")),
		waitingJob(jobNamed("old"), withDeadline(dueAged)),
		waitingJob(jobNamed("refused")),
		waitingJob(jobNamed("young"), withDeadline(notYetDue)),
		waitingJob(jobNamed("running"), withSuspend(ptr(false))),
	})
	api.status["refused"] = http.StatusForbidden

	pass, err := fixedWindows().Pass(context.Background(), kube, promotionNow)
	require.NoError(t, err)

	require.Equal(t, ClassExpiring, patchedClass(t, api, "due"))
	require.Equal(t, ClassAged, patchedClass(t, api, "old"))
	require.Len(t, api.patches, 2)
	require.Equal(t, []PromotedJob{
		{Job: "due", From: "memql-pipeline-audit", To: ClassExpiring, Deadline: dueExpiring, Step: "secrets-history.gitleaks"},
		{Job: "old", From: "memql-pipeline-audit", To: ClassAged, Deadline: dueAged, Step: "secrets-history.gitleaks"},
	}, pass.Promoted)
	require.Len(t, pass.Failed, 1)
	require.Equal(t, "refused", pass.Failed[0].Job)
	require.Contains(t, pass.Failed[0].Error, "refused by the fixture")
	require.Equal(t, map[string]int{promotionNotDue: 1, promotionNotWaiting: 1}, pass.Skipped)
	require.Equal(t, "2026-10-29T14:30:00Z", pass.At)
	require.Equal(t, []PromotionTier{{Class: ClassExpiring, Window: "180m"}, {Class: ClassAged, Window: "360m"}}, pass.Tiers)
}

func TestTheProvenanceOfAPromotionIsWrittenOnTheJob(t *testing.T) {
	api, kube := newPromotionAPI(t, []Job{waitingJob()})
	_, err := fixedWindows().Pass(context.Background(), kube, promotionNow)
	require.NoError(t, err)
	metadata := api.patches["mp-1"]["metadata"].(map[string]any)
	require.Equal(t, map[string]any{
		annotPromotedFrom:   "memql-pipeline-audit",
		annotPromotedAt:     "2026-10-29T14:30:00Z",
		annotPromotedReason: "run deadline within 180 minutes",
	}, metadata["annotations"])
}

func TestADryRunReportsWithoutPatching(t *testing.T) {
	api, kube := newPromotionAPI(t, []Job{waitingJob(jobNamed("due"))})
	p := fixedWindows()
	p.DryRun = true
	pass, err := p.Pass(context.Background(), kube, promotionNow)
	require.NoError(t, err)
	require.Empty(t, api.patches)
	require.True(t, pass.DryRun)
	require.Equal(t, "due", pass.Promoted[0].Job)
}

// Relabelling only what is still suspended is the promise, and the list is only
// a moment. Every patch names the resourceVersion it read, so a Job admitted (or
// heartbeaten) since then answers 409 and waits for the next pass: not a failure,
// and never a relabelled admitted Job.
func TestAJobThatChangedSinceTheListIsLeftForTheNextPass(t *testing.T) {
	api, kube := newPromotionAPI(t, []Job{waitingJob(jobNamed("raced")), waitingJob(jobNamed("due"))})
	api.status["raced"] = http.StatusConflict

	pass, err := fixedWindows().Pass(context.Background(), kube, promotionNow)
	require.NoError(t, err)
	require.Empty(t, pass.Failed)
	require.Equal(t, 1, pass.Skipped[promotionRaced])
	require.Len(t, pass.Promoted, 1)
	require.Equal(t, "41", api.patches["due"]["metadata"].(map[string]any)["resourceVersion"], "the patch is a compare-and-swap")
}

func TestAPassReadsEveryPageOfTheRunnersJobs(t *testing.T) {
	api, kube := newPromotionAPI(t, []Job{waitingJob(jobNamed("first"))}, []Job{waitingJob(jobNamed("second"))})
	pass, err := fixedWindows().Pass(context.Background(), kube, promotionNow)
	require.NoError(t, err)
	require.Len(t, pass.Promoted, 2)
	require.Len(t, api.paths, 2)
	require.Contains(t, api.paths[0], "labelSelector=app.kubernetes.io%2Fmanaged-by%3Dmemql-workbench")
}

func TestAPassThatCannotListSaysSo(t *testing.T) {
	api, kube := newPromotionAPI(t)
	api.listCode = http.StatusForbidden
	_, err := fixedWindows().Pass(context.Background(), kube, promotionNow)
	require.Error(t, err)
}

func TestAPassReportsItsSettingsAsOneJSONLine(t *testing.T) {
	_, kube := newPromotionAPI(t, []Job{})
	pass, err := DeadlinePriority{Queue: promotionQueue, BoostRuns: []string{boostedRun}, AgedOff: true}.Pass(context.Background(), kube, promotionNow)
	require.NoError(t, err)
	wire, err := json.Marshal(pass)
	require.NoError(t, err)
	require.JSONEq(t, `{"at":"2026-10-29T14:30:00Z","boostRuns":["`+boostedRun+`"],"dryRun":false,"failed":[],"promoted":[],"skipped":{},`+
		`"tiers":[{"class":"memql-pipeline-expiring","window":"a quarter of the run's ceiling"}]}`, string(wire))
}

func TestABoostAppliesThroughTheRunLabelTheRunnerWrites(t *testing.T) {
	require.Equal(t, boostedRun, RunLabelValue(boostedRun), "a run id is its own label, so a boost names what kubectl shows")
}

func TestValidateRefusesWindowsThatCannotWork(t *testing.T) {
	for name, p := range map[string]DeadlinePriority{
		"no queue":                 {},
		"expiring below 15":        {Queue: promotionQueue, ExpiringWindow: 14 * time.Minute},
		"expiring above a day":     {Queue: promotionQueue, ExpiringWindow: 1441 * time.Minute},
		"aged above a day":         {Queue: promotionQueue, AgedWindow: 1441 * time.Minute},
		"aged not above expiring":  {Queue: promotionQueue, ExpiringWindow: 180 * time.Minute, AgedWindow: 180 * time.Minute},
		"aged both off and stated": {Queue: promotionQueue, AgedWindow: 360 * time.Minute, AgedOff: true},
		"boost is not a run id":    {Queue: promotionQueue, BoostRuns: []string{"6381"}},
	} {
		require.Error(t, p.Validate(), name)
	}
	for name, p := range map[string]DeadlinePriority{
		"derived":        {Queue: promotionQueue},
		"stated":         fixedWindows(),
		"aged off":       {Queue: promotionQueue, AgedOff: true},
		"expiring alone": {Queue: promotionQueue, ExpiringWindow: 15 * time.Minute},
		"boosting":       boostingWindows(),
	} {
		require.NoError(t, p.Validate(), name)
	}
}
