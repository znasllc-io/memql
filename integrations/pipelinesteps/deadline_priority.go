package pipelinesteps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// Moving a waiting step forward as its run ages, so old runs finish first and a
// step near its deadline goes to the front rather than expiring behind younger work.
//
// Two rules combine to expire work that never ran. A StrictFIFO ClusterQueue
// admits by WorkloadPriorityClass first and age second, so age only breaks ties
// within a class; and a run's ceiling (MEMQL_PIPELINES_RUN_MAX_MINUTES) counts
// from the run's start, queue time included. A step of a low class, or one
// created late in an old run, can wait out the whole ceiling. On 2026-10-29 two
// scheduled scans did exactly that, twelve hours behind check and release jobs.
//
// A pass relabels each SUSPENDED step Job by how close its run is to its
// deadline: past half its ceiling, above every event's class; in its last
// quarter, above that. Kueue re-prioritizes a workload that has not reserved
// quota when the label changes (TestAPromotedJobIsAdmittedFirst holds that
// against the pinned controller), and strict order then accumulates room for
// the oldest Job of the highest class.
//
// Deliberately not done:
//   - A Job past its deadline is left alone. The runner fails it, and promoting
//     it would only hold the queue's head for nothing.
//   - Admitted work is never relabelled. Preemption stays Never, and every patch
//     is a compare-and-swap on the resourceVersion it read while suspended.
//   - The ceiling is not extended. A step's deadline is pinned in its creation
//     definition (creation.go), so no change here reaches a step already waiting.
//   - The queue stays StrictFIFO. Backfilling starved 40 GiB database steps behind
//     a stream of 20 GiB checks.
//
// This is the body of `memql pipeline-deadline-priority`, run by a CronJob
// (deploy/k8s/components/pipelines-capacity), and not a loop in the runner, for
// three reasons: a bug here costs a failed pass rather than a workbench that
// supervises every running step; its grant stays list and patch on Jobs in one
// namespace; and an owner naming a run to go first changes a CronJob's arguments
// rather than rolling the workbench under running steps. Why the promoter is Go
// on an engine image at all, rather than the Python it began as:
// docs/internal/ops/harness-decisions.md.

// The classes a pass promotes to, most urgent first. No event may start at one
// and only a pass sets them, so finding one on a Job says how it got there. The
// component ships them (priorities.yaml); they are constants rather than
// settings because the promise that a Job only moves up rests on knowing their
// order without reading them, which the grant does not allow.
const (
	ClassExpiring = "memql-pipeline-expiring"
	ClassBoosted  = "memql-pipeline-boosted"
	ClassAged     = "memql-pipeline-aged"
)

// promotedRank orders the promoted classes, lowest rank most urgent. Every class
// is ranked whatever this pass's settings are: a Job boosted earlier whose run
// has since left the list stays above the aged class rather than reading as
// unranked and being moved down to it.
func promotedRank(class string) (int, bool) {
	switch class {
	case ClassExpiring:
		return 0, true
	case ClassBoosted:
		return 1, true
	case ClassAged:
		return 2, true
	}
	return 0, false
}

// What a promotion records on the Job, so a queue's order can be explained after
// the fact from the Job alone.
const (
	annotPromotedFrom   = "memql.io/priority-promoted-from"
	annotPromotedAt     = "memql.io/priority-promoted-at"
	annotPromotedReason = "memql.io/priority-promoted-reason"
)

// Why a pass left a Job where it was, as counted in the pass's JSON line.
const (
	promotionDue         = "promote"
	promotionOtherQueue  = "other-queue"
	promotionNotWaiting  = "not-waiting"
	promotionNoDeadline  = "no-deadline"
	promotionNoCeiling   = "no-ceiling"
	promotionExpired     = "expired"
	promotionNotDue      = "not-due"
	promotionAlreadyDone = "already-promoted"
	promotionRaced       = "changed-since-list"
)

// How far a window may be stated. The floor is three passes: under it a Job can
// cross the window between passes with little or nothing left. The ceiling is the
// longest a run may last (pl.ParseRunCeiling's bound).
const (
	MinDeadlineWindowMinutes = 15
	MaxDeadlineWindowMinutes = 1440

	minStatedWindow = MinDeadlineWindowMinutes * time.Minute
	maxStatedWindow = MaxDeadlineWindowMinutes * time.Minute
)

// boostRunShape is what a pipeline run's id looks like. Refusing anything else
// catches a pull request number pasted where a run id belongs, which would
// otherwise match nothing, silently, while the owner waits for it to go first.
var boostRunShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

// DeadlinePriority is one pass's settings. They are declared configuration, the
// CronJob's arguments in the installation's overlay, because each is an
// operator's decision: the boundary rule for what must not be compiled in
// (docs/public/build/integration-boundary.md).
type DeadlinePriority struct {
	// Queue is the LocalQueue whose waiting Jobs this pass reorders.
	Queue string
	// ExpiringWindow is how close to its deadline a Job takes ClassExpiring.
	// Zero is a quarter of that Job's own run ceiling.
	ExpiringWindow time.Duration
	// AgedWindow is how close to its deadline a Job takes ClassAged. Zero is half
	// of that Job's own run ceiling, unless AgedOff.
	AgedWindow time.Duration
	// AgedOff keeps event classes in force until a run nears expiry.
	AgedOff bool
	// BoostRuns are pipeline run ids an owner named to go first: ClassBoosted,
	// above old runs and below expiring ones, so a boost can never be what makes
	// another run expire.
	BoostRuns []string
	// DryRun reports what a pass would do and writes nothing.
	DryRun bool
}

// Validate refuses settings that could not work, before a pass reads anything.
func (p DeadlinePriority) Validate() error {
	if p.Queue == "" {
		return errors.New("a pass needs the queue whose Jobs it reorders")
	}
	if p.ExpiringWindow != 0 && (p.ExpiringWindow < minStatedWindow || p.ExpiringWindow > maxStatedWindow) {
		return fmt.Errorf("the expiring window must be between %d and %d minutes", MinDeadlineWindowMinutes, MaxDeadlineWindowMinutes)
	}
	if p.AgedOff && p.AgedWindow != 0 {
		return errors.New("the aged tier cannot be both off and given a window")
	}
	if p.AgedWindow > maxStatedWindow {
		return fmt.Errorf("the aged window must be at most %d minutes", MaxDeadlineWindowMinutes)
	}
	if p.AgedWindow != 0 && p.ExpiringWindow != 0 && p.AgedWindow <= p.ExpiringWindow {
		return errors.New("the aged window must exceed the expiring window, or it would never apply")
	}
	for _, id := range p.BoostRuns {
		if !boostRunShape.MatchString(id) {
			return fmt.Errorf("%q is not a pipeline run id (32 hex characters)", id)
		}
	}
	return nil
}

// jobPromotion is one decided relabel.
type jobPromotion struct {
	To, From, Reason string
}

// runCeilingOf is the ceiling the runner wrote on the Job (AnnotRunCeiling), read
// through the seam's one parse. Absent or unreadable is not a ceiling.
func runCeilingOf(job Job) (time.Duration, bool) {
	raw := strings.TrimSpace(job.Metadata.Annotations[AnnotRunCeiling])
	if minutes, err := strconv.Atoi(raw); err != nil || minutes <= 0 {
		return 0, false
	}
	return pl.ParseRunCeiling(raw), true
}

// windowsFor is this Job's two windows. A zero window applies to nothing: the aged
// tier when it is off, and either tier when it needs a ceiling this Job lacks.
//
// A window not stated follows the Job's own run ceiling. Fixed defaults cannot
// be right: the ceiling is 120 minutes by default, 480 under the long-run profile
// and 720 where this first ran, and the windows that suit 720 (180 and 360) would
// put every waiting Job of a default installation in the top class at the first
// pass, which erases the event order the queue exists for. The shares are the
// proportions those two windows were chosen at. Known limit: the ceiling read is
// the one the workbench that built the Job was configured with, while the
// deadline is the agent's; the two are configured alike (components/pipelines-long-runs).
//
// measured is false for a Job some window needed a ceiling for and that carries
// none: one built before the runner wrote it. That window is not guessed, because
// the only thing a guess could do is promote too much, and the Job is counted if
// nothing else applies to it.
func (p DeadlinePriority) windowsFor(job Job) (expiring, aged time.Duration, measured bool) {
	expiring, aged = p.ExpiringWindow, p.AgedWindow
	deriveExpiring, deriveAged := expiring == 0, aged == 0 && !p.AgedOff
	if !deriveExpiring && !deriveAged {
		return expiring, aged, true
	}
	ceiling, found := runCeilingOf(job)
	if !found {
		return expiring, aged, false
	}
	if deriveExpiring {
		expiring = ceiling / 4
	}
	if deriveAged {
		aged = ceiling / 2
	}
	return expiring, aged, true
}

func (p DeadlinePriority) boosted(runLabel string) bool {
	for _, id := range p.BoostRuns {
		if RunLabelValue(id) == runLabel {
			return true
		}
	}
	return false
}

func withinWindow(window time.Duration) string {
	return fmt.Sprintf("run deadline within %d minutes", int(window/time.Minute))
}

// decide answers one Job: a promotion and promotionDue, or why not. A Job takes
// the most urgent class that applies to it and is never moved down.
func (p DeadlinePriority) decide(job Job, now time.Time) (jobPromotion, string) {
	labels := job.Metadata.Labels
	if labels[queueLabel] != p.Queue {
		return jobPromotion{}, promotionOtherQueue
	}
	if job.Spec.Suspend == nil || !*job.Spec.Suspend {
		return jobPromotion{}, promotionNotWaiting
	}
	deadline, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(job.Metadata.Annotations[AnnotRunDeadline]))
	if err != nil {
		return jobPromotion{}, promotionNoDeadline
	}
	left := deadline.Sub(now)
	if left <= 0 {
		return jobPromotion{}, promotionExpired
	}
	expiring, aged, measured := p.windowsFor(job)
	var target, why string
	switch {
	case left <= expiring:
		target, why = ClassExpiring, withinWindow(expiring)
	case p.boosted(labels[LabelRun]):
		target, why = ClassBoosted, "boosted run"
	case left <= aged:
		target, why = ClassAged, withinWindow(aged)
	case !measured:
		return jobPromotion{}, promotionNoCeiling
	default:
		return jobPromotion{}, promotionNotDue
	}
	current := labels[queuePriorityLabel]
	if rank, promoted := promotedRank(current); promoted {
		if targetRank, _ := promotedRank(target); rank <= targetRank {
			return jobPromotion{}, promotionAlreadyDone
		}
	}
	return jobPromotion{To: target, From: current, Reason: why}, promotionDue
}

// DeadlinePriorityPass is what a pass prints: one JSON line, so `kubectl logs`
// on the CronJob's last Job answers what moved, what did not and why.
type DeadlinePriorityPass struct {
	At        string             `json:"at"`
	BoostRuns []string           `json:"boostRuns"`
	DryRun    bool               `json:"dryRun"`
	Failed    []PromotionFailure `json:"failed"`
	Promoted  []PromotedJob      `json:"promoted"`
	Skipped   map[string]int     `json:"skipped"`
	Tiers     []PromotionTier    `json:"tiers"`
}

// PromotionFailure is a patch the API server refused for a reason other than the
// Job having changed.
type PromotionFailure struct {
	Job   string `json:"job"`
	Error string `json:"error"`
}

// PromotedJob is one Job a pass moved, or in a dry run would have.
type PromotedJob struct {
	Job      string `json:"job"`
	From     string `json:"from"`
	To       string `json:"to"`
	Deadline string `json:"deadline"`
	Step     string `json:"step"`
}

// PromotionTier states a tier's window as this pass applies it.
type PromotionTier struct {
	Class  string `json:"class"`
	Window string `json:"window"`
}

func statedOr(window time.Duration, derived string) string {
	if window == 0 {
		return derived
	}
	return strconv.Itoa(int(window/time.Minute)) + "m"
}

// maxPromotionErrorBytes bounds an API server's refusal in the pass's line.
const maxPromotionErrorBytes = 200

// Pass promotes every due Job once. One refused Job does not stop the others: it
// is reported, and fails the pass's exit code rather than its work. Only a list
// that could not be read is an error, because then the pass knows nothing.
func (p DeadlinePriority) Pass(ctx context.Context, kube *Kube, now time.Time) (DeadlinePriorityPass, error) {
	pass := DeadlinePriorityPass{
		At:        now.UTC().Format(time.RFC3339),
		BoostRuns: append([]string{}, p.BoostRuns...),
		DryRun:    p.DryRun,
		Failed:    []PromotionFailure{},
		Promoted:  []PromotedJob{},
		Skipped:   map[string]int{},
		Tiers:     []PromotionTier{{Class: ClassExpiring, Window: statedOr(p.ExpiringWindow, "a quarter of the run's ceiling")}},
	}
	if !p.AgedOff {
		pass.Tiers = append(pass.Tiers, PromotionTier{Class: ClassAged, Window: statedOr(p.AgedWindow, "half of the run's ceiling")})
	}
	jobs, err := kube.ManagedJobs(ctx)
	if err != nil {
		return pass, err
	}
	for _, job := range jobs {
		promotion, reason := p.decide(job, now)
		if reason != promotionDue {
			pass.Skipped[reason]++
			continue
		}
		if !p.DryRun {
			annotations := map[string]string{
				annotPromotedFrom:   promotion.From,
				annotPromotedAt:     pass.At,
				annotPromotedReason: promotion.Reason,
			}
			err := kube.PromoteJob(ctx, job.Metadata.Name, promotion.To, annotations, job.Metadata.ResourceVersion)
			if deploycontrol.IsConflict(err) {
				pass.Skipped[promotionRaced]++
				continue
			}
			if err != nil {
				message := err.Error()
				if len(message) > maxPromotionErrorBytes {
					message = message[:maxPromotionErrorBytes]
				}
				pass.Failed = append(pass.Failed, PromotionFailure{Job: job.Metadata.Name, Error: message})
				continue
			}
		}
		pass.Promoted = append(pass.Promoted, PromotedJob{
			Job:      job.Metadata.Name,
			From:     promotion.From,
			To:       promotion.To,
			Deadline: job.Metadata.Annotations[AnnotRunDeadline],
			Step:     job.Metadata.Annotations[AnnotStepKey],
		})
	}
	return pass, nil
}

// managedJobsPageSize is one page of ManagedJobs. Whole Jobs are bigger than the
// metadata ManagedSecrets asks for, and the namespace's Job quota bounds how many
// there are, so this is rarely more than one request.
const managedJobsPageSize = 500

// ManagedJobs is every Job the runner created in the namespace, whole: whether a
// Job is still waiting is spec.suspend, which a metadata-only list does not carry.
func (k *Kube) ManagedJobs(ctx context.Context) ([]Job, error) {
	var jobs []Job
	cont := ""
	for {
		q := url.Values{"labelSelector": {LabelManagedBy + "=" + ManagedBy}, "limit": {strconv.Itoa(managedJobsPageSize)}}
		if cont != "" {
			q.Set("continue", cont)
		}
		out, err := k.api.Do(ctx, http.MethodGet, k.jobsPath("")+"?"+q.Encode(), "", nil)
		if err != nil {
			return nil, err
		}
		var list struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []Job `json:"items"`
		}
		if err := json.Unmarshal(out, &list); err != nil {
			return nil, fmt.Errorf("pipelinesteps: reading the list of step Jobs: %w", err)
		}
		jobs = append(jobs, list.Items...)
		if cont = list.Metadata.Continue; cont == "" {
			return jobs, nil
		}
	}
}

// PromoteJob sets a Job's admission class with a merge patch, recording why in
// annotations. resourceVersion makes it a compare-and-swap (AnnotateJob says
// how): the caller read the Job suspended at that version, so a Job admitted since
// answers 409 (deploycontrol.IsConflict) and is never relabelled.
func (k *Kube) PromoteJob(ctx context.Context, name, class string, annotations map[string]string, resourceVersion string) error {
	if err := named("job", name); err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"labels":          map[string]string{queuePriorityLabel: class},
		"annotations":     annotations,
		"resourceVersion": resourceVersion,
	}})
	if err != nil {
		return fmt.Errorf("pipelinesteps: encoding the promotion of job %s: %w", name, err)
	}
	_, err = k.api.Do(ctx, http.MethodPatch, k.jobsPath(name), contentMergePatch, patch)
	return err
}
