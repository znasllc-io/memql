package pipelinerun

import (
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/pipelines"
)

// attempts.go -- a step's attempts: the record each one leaves on the step's
// row, and where the next one goes (spec
// 2026-10-28-pipeline-fleet-first-retries-failover-design.md, sections 3 and 5,
// with the rulings recorded at each decision below). The loop that runs them
// is runStep's (driver.go).
//
// THE HISTORY LIVES IN THE STEP'S BINDING, an open object, under two keys:
// `attempts`, every ended attempt's record in order, and `nextAttemptAt`, the
// RFC3339 time the next attempt may start, present only on the version written
// between two attempts (Step.Retrying). Not a concept field, so an engine
// rollback cannot leave rows carrying a field the older concept does not
// declare; and not the step's `resumeAt` field (ruling 2), which the work
// spine's own sweeps read for parked work.
//
// THE BINDING IS REPLACED WHOLE on every write that names it (the journal's
// read-merge is shallow), so every write restates the whole history: the next
// attempt's intent without nextAttemptAt, Retrying with it, and every receipt
// of a step that reached the executor (settle) with the history and the place
// the last attempt ran. A write that named no binding would keep a stale
// nextAttemptAt on a row whose step has moved on.

const (
	bindingAttempts      = "attempts"
	bindingNextAttemptAt = "nextAttemptAt"
)

// attemptTextMaxBytes bounds every string an attempt record keeps, after it is
// masked. It is the bound the runner puts on a failure's sentence
// (failureMaxBytes, integrations/pipelinesteps), so a record never holds more of
// any one text than the receipt it summarizes, and eight of them cannot grow a
// row without end.
const attemptTextMaxBytes = 2 << 10

// maxAttemptWaitSeconds is the longest wait a placed attempt may carry: the
// executor refuses a longer one as pipeline_step_invalid (maxPlacementWaitSeconds,
// integrations/pipelinesteps). The installed policy's own bounds (a delay of at
// most 900 s, fleetWaitSeconds at most 3600) keep every wait inside it; the clamp
// is for a policy built by hand.
const maxAttemptWaitSeconds = 3600

// attemptRecord is one ended attempt of a step, as the step's binding keeps it
// (section 3): what the executor said the attempt proved (Class), how it ended,
// where it ran and its log. Every string is the runner's or the executor's,
// masked with every secret the drive resolved and cut to attemptTextMaxBytes
// before it is kept (attemptOf), as a receipt's strings are.
//
// Class is empty only for an attempt that succeeded: a failed attempt the
// executor did not classify is recorded AttemptFinal (spec D3), so a reader
// tells a pass from a failure without a status field.
//
// The PLACEMENT each attempt was sent with is not recorded. It is derived:
// chooseNext is a pure function of the step, the run's captured policy and the
// records before the attempt, and the recovery definition binds both the engine
// revision and the policy (definitionOf), so a replica that takes the run over
// recomputes the same placements from the same records.
type attemptRecord struct {
	Attempt                                                                             int
	Class                                                                               pipelines.AttemptClass
	Code, Message, Surface, WorkerID, NodeID, JobName, StartedAt, FinishedAt, LogFileID string
	NotRetriedHere                                                                      bool
}

// asMap is the record as the binding stores it. A field with nothing to say is
// left out rather than written blank, as bindingOf leaves out an empty place.
func (a attemptRecord) asMap() map[string]any {
	out := map[string]any{"attempt": a.Attempt}
	for key, value := range map[string]string{
		"class": string(a.Class), "code": a.Code, "message": a.Message, "surface": a.Surface,
		"workerId": a.WorkerID, "nodeId": a.NodeID, "jobName": a.JobName,
		"startedAt": a.StartedAt, "finishedAt": a.FinishedAt, "logFileId": a.LogFileID,
	} {
		if value != "" {
			out[key] = value
		}
	}
	if a.NotRetriedHere {
		out["notRetriedHere"] = true
	}
	return out
}

// attemptsFromBinding reads a step's history back off its binding: its records
// in order, and when the next attempt may start (the zero time when the binding
// names none). It is the reader of what attemptsBinding writes, through the
// engine's own parse of it (numbers as JSON numbers, the list as []any).
func attemptsFromBinding(b map[string]any) ([]attemptRecord, time.Time) {
	var records []attemptRecord
	for _, o := range rowObjects(b, bindingAttempts) {
		records = append(records, attemptRecord{
			Attempt: rowInt(o, "attempt"), Class: pipelines.AttemptClass(rowString(o, "class")),
			Code: rowString(o, "code"), Message: rowString(o, "message"), Surface: rowString(o, "surface"),
			WorkerID: rowString(o, "workerId"), NodeID: rowString(o, "nodeId"), JobName: rowString(o, "jobName"),
			StartedAt: rowString(o, "startedAt"), FinishedAt: rowString(o, "finishedAt"), LogFileID: rowString(o, "logFileId"),
			NotRetriedHere: rowBool(o, "notRetriedHere"),
		})
	}
	return records, rowTime(b, bindingNextAttemptAt)
}

// attemptsBinding is base with the history written into it: every record in
// history, and next as nextAttemptAt when it is set. Any nextAttemptAt base
// carried is dropped, so an intent or a receipt built on a Retrying's binding
// names no next attempt. With no history and no next it is base itself, which
// for a step's first attempt is nil: the intent Step has always written.
func attemptsBinding(base map[string]any, history []attemptRecord, next time.Time) map[string]any {
	if len(history) == 0 && next.IsZero() {
		return base
	}
	out := maps.Clone(base)
	if out == nil {
		out = map[string]any{}
	}
	delete(out, bindingNextAttemptAt)
	records := make([]any, 0, len(history))
	for _, r := range history {
		records = append(records, r.asMap())
	}
	out[bindingAttempts] = records
	if !next.IsZero() {
		out[bindingNextAttemptAt] = next.UTC().Format(time.RFC3339)
	}
	return out
}

// attemptOf is the record one ended attempt leaves in its step's history: what
// the executor said it proved, its receipt's code and words, where it ran, when,
// and its log. Every string is masked with the secrets the drive resolved and
// cut to attemptTextMaxBytes, as a receipt's are -- the code and message come
// masked from the receipt already (masking them again changes nothing), the
// place and times straight from the runner.
//
// Where the runner named no place, the record is placed by attemptSurface. A
// PINNED attempt that named no machine belongs to the machine it was pinned to
// (ruling 12): its wait for that machine ran out before any answered, or it was
// refused before dispatch, and either way that machine is the one to count, or
// to mark not retried here. Times the runner did not report are the driver's
// own: when it handed the attempt over, and when the answer came.
func (dr *runDriver) attemptOf(req pipelines.StepRequest, end stepEnd, rec receipt, began time.Time) attemptRecord {
	masks := dr.maskValues()
	text := func(s string) string {
		return cutText(pipelines.MaskSecrets(strings.TrimSpace(s), masks), attemptTextMaxBytes)
	}
	a := attemptRecord{
		Attempt: req.Attempt, Code: text(rec.code), Message: text(rec.message),
		StartedAt: began.UTC().Format(time.RFC3339), FinishedAt: dr.d.now().UTC().Format(time.RFC3339),
	}
	var where pipelines.Where
	if end.kind == endResult {
		res := end.result
		where = res.Where
		if rec.status != WorkStepDone {
			a.Class = attemptClassOf(res.AttemptClass)
		}
		a.NotRetriedHere = res.NotRetriedHere
		a.LogFileID = text(rec.logFileID)
		if s := text(res.StartedAt); isRFC3339(s) {
			a.StartedAt = s
		}
		if s := text(res.FinishedAt); isRFC3339(s) {
			a.FinishedAt = s
		}
	} else {
		// A timeout, an error or a cancel: no answer to read a class from, and
		// none of them is retried.
		a.Class = pipelines.AttemptFinal
	}
	a.Surface = attemptSurface(req, text(where.Surface))
	if a.Surface == pipelines.PlacementFleet {
		a.WorkerID = text(where.WorkerID)
		if a.WorkerID == "" && req.Placement != nil {
			a.WorkerID = text(req.Placement.PinWorker)
		}
	}
	a.NodeID, a.JobName = text(where.NodeID), text(where.JobName)
	return a
}

func isRFC3339(s string) bool {
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

// attemptClassOf is the class the driver retries from: the executor's, when it
// is one this engine knows, and AttemptFinal otherwise -- an empty class
// included, so an older executor's answer, or one nothing classified, is never
// retried (spec D3).
func attemptClassOf(c pipelines.AttemptClass) pipelines.AttemptClass {
	switch c {
	case pipelines.AttemptNothingRan, pipelines.AttemptRetryRequested, pipelines.AttemptOutcomeUnknown:
		return c
	}
	return pipelines.AttemptFinal
}

// attemptSurface is where an attempt ran: what the executor reported, when it is
// a surface; else where the attempt was sent; else where the step's own
// placement sends a first attempt -- a pool step's starts on the owner's
// machines, and the executor stamps "cluster" on any answer that came from a
// Job. Many endings report no place at all (a refusal made before routing, a
// clone token that could not be minted), and the next place depends on this one.
func attemptSurface(req pipelines.StepRequest, reported string) string {
	switch reported {
	case pipelines.PlacementCluster, pipelines.PlacementFleet:
		return reported
	}
	if p := req.Placement; p != nil && (p.Surface == pipelines.PlacementCluster || p.Surface == pipelines.PlacementFleet) {
		return p.Surface
	}
	if req.Step.RequiresFleet() || req.Step.Placement == pipelines.PlacementPool {
		return pipelines.PlacementFleet
	}
	return pipelines.PlacementCluster
}

// cutText keeps at most n bytes of s, never splitting a rune.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ---------------------------------------------------------------------------
// Choosing the next attempt
// ---------------------------------------------------------------------------

// chooseNext is where a step's next attempt goes and how long after the last
// one it may start; ok is false when the step has had its attempts and ends
// with its last attempt's failure. history is every attempt so far, in order,
// the last the one that just ended. It decides from the records and the run's
// captured policy alone, so every replica decides alike.
//
// THE ORDER (spec section 5). A PLACE is one machine, or the cluster as a whole.
// After NOTHING RAN or RETRY REQUESTED (exit 75 needs no `repeatable`: the
// command itself said another run is safe) at place P:
//
//  1. P again, while P has had fewer than sameMachine retries and is not marked
//     not-retried-here, after delaysSeconds[i] (the last value repeats). Each
//     place has its own count. A cluster step's retries stay on the cluster
//     under this count; under this rule a pool step's retries reach the
//     cluster at most once (an unknown outcome there is ruling 18's, below).
//  2. Otherwise, for a fleet or pool step with otherMachines, an eligible machine
//     not yet tried, at once.
//  3. Otherwise, for a pool step with cloudFallback, the cluster, if it has not
//     been a place yet.
//  4. Otherwise the step fails.
//
// After an OUTCOME UNKNOWN at P, only for a repeatable step (spec D5):
//
//  1. an eligible machine not yet tried (rule 2's step);
//  2. P again: ONCE when P is a machine; up to sameMachine times, after the same
//     delays as rule 1, when P is the cluster (ruling 18);
//  3. rule 3;
//  4. otherwise the step fails.
//
// The two "P again" differ because what guards against two copies of the
// command differs. On a machine only the machine's own reservation does: a
// machine still holding the old command refuses the next one (which counts as
// nothing ran), and that guard is worth one more try. On the cluster the
// driver itself confirms the previous attempt's Job, Secret and pods are gone
// before it sends the next attempt (ruling 13, runStep), so a repeatable step
// cannot overlap itself there however often it is retried; and since nearly
// every cluster failure is an unknown outcome (rulings 14 and 15), a cluster
// count of one would leave repeatable steps without their cluster retries.
//
// Either "P again" stays owed while the untried machines are tried, and is paid
// after them, a machine's before the cluster's, then rule 3: a pool step whose
// unknown outcome was on the cluster tries the machines first, then the
// cluster again within sameMachine.
//
// Bounds no policy can widen: MaxStepAttempts in all; a fleet step never reaches
// the cluster and a cluster step never a machine (the executor refuses both,
// refusePlacement); the cluster only where cloudFallback allows it. The policy
// can narrow every rule, and the zero policy retries nothing: rule 1 and both
// owed retries need sameMachine > 0, rule 2 needs otherMachines, rule 3
// cloudFallback.
//
// THE WAITS a placed attempt carries (AttemptPlacement.MaxWaitSeconds): a pinned
// machine is waited for at most the largest delay, and at least a second; a
// pool step's failover to another machine at most fleetWaitSeconds and at least
// a second (ruling 8: zero would mean no bound); a fleet step's failover, like
// its first attempt, until the run's ceiling.
//
// The run's ceiling is not this function's: the caller refuses to wait into it
// or to start an attempt with less than a second of it left (runStep).
//
// Regression tests: TestChooseNextFollowsTheRetryOrder (every rule and ruling,
// row by row) and the driver's own in driver_retry_test.go.
func chooseNext(step pipelines.Step, policy pipelines.ComputePlacementPolicy, history []attemptRecord) (*pipelines.AttemptPlacement, time.Duration, bool) {
	// Every attempt's placement, recomputed from the records before it: attempt
	// 1 has none (its manifest placement decides), and each later one is this
	// function's answer for its prefix (see attemptRecord on why that is
	// stable). Whether an attempt was a pinned retry or an unpinned failover
	// decides what its answer proves (ruling 9).
	sent := make([]*pipelines.AttemptPlacement, len(history))
	for k := 1; k < len(history); k++ {
		placement, _, ok := decideNext(step, policy, history[:k], sent[:k])
		if !ok {
			// A history these rules could not have written: nothing in it can be
			// trusted to choose a place, so the step ends as it stands.
			return nil, 0, false
		}
		sent[k] = placement
	}
	return decideNext(step, policy, history, sent)
}

// decideNext is chooseNext for one history whose attempts' placements are known.
func decideNext(step pipelines.Step, policy pipelines.ComputePlacementPolicy, history []attemptRecord, sent []*pipelines.AttemptPlacement) (*pipelines.AttemptPlacement, time.Duration, bool) {
	n := len(history)
	if n == 0 || n >= pipelines.MaxStepAttempts || history[n-1].Attempt >= pipelines.MaxStepAttempts {
		// The engine's cap, on the count of records and on the attempt number
		// alike, so a history that lost records still cannot pass it.
		return nil, 0, false
	}
	last := history[n-1]
	switch last.Class {
	case pipelines.AttemptNothingRan, pipelines.AttemptRetryRequested:
	case pipelines.AttemptOutcomeUnknown:
		if !step.Repeatable {
			// The command may have run; a second copy of it could publish or spend
			// twice, so only the manifest's word that repeating it is harmless
			// retries it (spec D5).
			return nil, 0, false
		}
	default:
		return nil, 0, false
	}
	h := readHistory(step, policy, history, sent)

	if last.Class != pipelines.AttemptOutcomeUnknown && !exhausting(last, sent[n-1]) {
		// Rule 1. Not after an unknown outcome, whose same-place retry is an
		// owed one below, and not after an attempt that found the fleet
		// exhausted: ruling 9 sends that one straight to rule 3, and the machine
		// it may name (the last one busy) is never pinned.
		if placement, delay, ok := h.samePlace(last); ok {
			return placement, delay, true
		}
	}
	if placement, ok := h.anotherMachine(); ok { // rule 2, and the unknown outcome's first resort
		return placement, 0, true
	}
	if placement, ok := h.owedRetry(); ok { // an unknown outcome's "P again", P a machine: once
		return placement, 0, true
	}
	if placement, delay, ok := h.owedCluster(); ok { // an unknown outcome's "P again", P the cluster (ruling 18)
		return placement, delay, true
	}
	if placement, ok := h.cluster(); ok { // rule 3
		return placement, 0, true
	}
	return nil, 0, false
}

// exhausting reports an attempt that found no machine to take the step: an
// UNPINNED failover the driver placed on the fleet that came back nothing ran,
// either not retried here (its wait for a busy machine ran out, or the only
// candidates refused for good) or naming no machine at all (no candidate was
// left once the tried ones were excluded). Ruling 9: the fleet is exhausted for
// now, and stays so for the rest of the step's attempts -- trying it again
// would spend the same wait to the same answer.
//
// A first attempt is not one of these: it was not placed, and a refusal there
// names the one machine to move away from (rule 2). Nor is a pinned attempt,
// whose answer is about its one machine.
func exhausting(r attemptRecord, sent *pipelines.AttemptPlacement) bool {
	return sent != nil && sent.Surface == pipelines.PlacementFleet && sent.PinWorker == "" &&
		r.Class == pipelines.AttemptNothingRan && r.Surface == pipelines.PlacementFleet &&
		(r.NotRetriedHere || r.WorkerID == "")
}

// attemptHistory is what a step's records say, read once for one decision.
type attemptHistory struct {
	policy pipelines.ComputePlacementPolicy
	// mayFleet and mayCluster say which surfaces the step may be placed on at
	// all: a fleet or pool step the owner's machines, a cluster step or a pool
	// step whose policy allows cloud the cluster.
	mayFleet, mayCluster bool
	pool                 bool
	// repeatable: the step says running it again after an unknown outcome is
	// harmless (spec D5), which every owed retry needs.
	repeatable bool

	// tried is every machine an attempt ran on or was pinned to, in the order
	// first tried: what a failover excludes.
	tried []string
	// notRetried is the machines an attempt marked not to be tried again.
	notRetried map[string]bool
	// at counts the attempts at each place (placeKey).
	at map[string]int
	// clusterUsed: the cluster has been a place; clusterRetries counts the
	// attempts after the first that ran there. clusterOwed: the latest attempt
	// on the cluster ended with an unknown outcome, so the cluster is owed its
	// retries (ruling 18) until one runs there.
	clusterUsed    bool
	clusterRetries int
	clusterOwed    bool
	// exhausted: an attempt found the fleet exhausted (exhausting).
	exhausted bool
	// owed is the machines an unknown outcome left owed their one retry, in the
	// order of their first unknown outcome: no attempt has been at the machine
	// since.
	owed []string
}

// placeKey names the place a record ran at: the cluster, one machine, or the
// owner's fleet when the attempt named no machine (the fleet's routing is then
// the place, and retrying it is routing again).
func placeKey(r attemptRecord) string {
	switch {
	case r.Surface == pipelines.PlacementCluster:
		return "cluster"
	case r.WorkerID != "":
		return "machine:" + r.WorkerID
	}
	return "fleet"
}

func readHistory(step pipelines.Step, policy pipelines.ComputePlacementPolicy, records []attemptRecord, sent []*pipelines.AttemptPlacement) attemptHistory {
	pool := step.Placement == pipelines.PlacementPool && !step.RequiresFleet()
	h := attemptHistory{
		policy: policy, pool: pool, repeatable: step.Repeatable,
		mayFleet:   step.RequiresFleet() || pool,
		mayCluster: !step.RequiresFleet() && (!pool || policy.CloudFallback),
		notRetried: map[string]bool{},
		at:         map[string]int{},
	}
	firstUnknown := map[string]int{}
	lastAt := map[string]int{}
	for i, r := range records {
		h.at[placeKey(r)]++
		if exhausting(r, sent[i]) {
			h.exhausted = true
		}
		if r.Surface == pipelines.PlacementCluster {
			h.clusterUsed = true
			if i > 0 {
				h.clusterRetries++
			}
			h.clusterOwed = r.Class == pipelines.AttemptOutcomeUnknown
			continue
		}
		if r.WorkerID == "" {
			continue
		}
		if !slices.Contains(h.tried, r.WorkerID) {
			h.tried = append(h.tried, r.WorkerID)
		}
		if r.NotRetriedHere {
			h.notRetried[r.WorkerID] = true
		}
		lastAt[r.WorkerID] = i
		if _, seen := firstUnknown[r.WorkerID]; !seen && r.Class == pipelines.AttemptOutcomeUnknown {
			firstUnknown[r.WorkerID] = i
		}
	}
	for _, machine := range h.tried {
		if first, unknown := firstUnknown[machine]; unknown && lastAt[machine] == first && !h.notRetried[machine] {
			h.owed = append(h.owed, machine)
		}
	}
	return h
}

// samePlace is rule 1: the place last ran at, again, after its delay.
func (h attemptHistory) samePlace(last attemptRecord) (*pipelines.AttemptPlacement, time.Duration, bool) {
	key := placeKey(last)
	retries := h.at[key] - 1
	if retries >= h.policy.Retries.SameMachine {
		return nil, 0, false
	}
	delay := retryDelay(h.policy.Retries, retries)
	switch {
	case key == "cluster":
		if !h.mayCluster || (h.pool && h.clusterRetries > 0) {
			// Under this rule a pool step's retries reach the cluster at most
			// once: the cluster is the one place an attempt spends money. An
			// unknown outcome there is retried under owedCluster instead.
			return nil, 0, false
		}
		return &pipelines.AttemptPlacement{Surface: pipelines.PlacementCluster}, delay, true
	case last.WorkerID != "":
		if !h.mayFleet || h.notRetried[last.WorkerID] {
			return nil, 0, false
		}
		return h.pinned(last.WorkerID), delay, true
	default:
		// The fleet named no machine (a clone token that could not be minted
		// before any dispatch, a node with no dispatcher): routing again is the
		// same place.
		if !h.mayFleet || last.NotRetriedHere {
			return nil, 0, false
		}
		return h.failover(), delay, true
	}
}

// anotherMachine is rule 2: any eligible machine the step has not tried.
func (h attemptHistory) anotherMachine() (*pipelines.AttemptPlacement, bool) {
	if !h.mayFleet || !h.policy.Retries.OtherMachines || h.exhausted {
		return nil, false
	}
	return h.failover(), true
}

// owedRetry is an unknown outcome's "P again" when P is a machine: once, at the
// first machine still owed it. The policy's sameMachine: 0 says no place is
// tried again, which an unknown outcome cannot override (spec D7).
func (h attemptHistory) owedRetry() (*pipelines.AttemptPlacement, bool) {
	if !h.repeatable || !h.mayFleet || h.policy.Retries.SameMachine < 1 || len(h.owed) == 0 {
		return nil, false
	}
	return h.pinned(h.owed[0]), true
}

// owedCluster is an unknown outcome's "P again" when P is the cluster (ruling
// 18): the cluster again while it has had fewer than sameMachine retries, after
// the same delays as rule 1, its count shared with rule 1's. Each such attempt
// is sent only once the previous one's Job, Secret and pods are confirmed gone
// (runStep), which is what makes more than one safe here. A pool step's retries
// may come back to the cluster for this after trying its untried machines; the
// policy's cloudFallback still decides whether a pool step may be on the
// cluster at all (mayCluster).
func (h attemptHistory) owedCluster() (*pipelines.AttemptPlacement, time.Duration, bool) {
	if !h.repeatable || !h.mayCluster || !h.clusterOwed {
		return nil, 0, false
	}
	retries := h.at["cluster"] - 1
	if retries >= h.policy.Retries.SameMachine {
		return nil, 0, false
	}
	return &pipelines.AttemptPlacement{Surface: pipelines.PlacementCluster}, retryDelay(h.policy.Retries, retries), true
}

// cluster is rule 3: a pool step's one move to the cluster.
func (h attemptHistory) cluster() (*pipelines.AttemptPlacement, bool) {
	if !h.pool || !h.policy.CloudFallback || h.clusterUsed {
		return nil, false
	}
	return &pipelines.AttemptPlacement{Surface: pipelines.PlacementCluster}, true
}

// pinned is an attempt on one machine, waited for at most the largest retry
// delay and at least a second: a machine that is offline, or still holds the
// step's previous command, then counts as nothing ran there.
func (h attemptHistory) pinned(machine string) *pipelines.AttemptPlacement {
	longest := 0
	for _, d := range h.policy.Retries.DelaysSeconds {
		longest = max(longest, d)
	}
	return &pipelines.AttemptPlacement{Surface: pipelines.PlacementFleet, PinWorker: machine, MaxWaitSeconds: attemptWait(longest)}
}

// failover is an attempt on any eligible machine but those tried. A pool step's
// waits for a busy machine at most fleetWaitSeconds, and at least a second
// (ruling 8), then counts as nothing ran, which brings rule 3 in. A fleet step's
// waits as its first attempt does, until the run's ceiling: nothing else can
// run it.
func (h attemptHistory) failover() *pipelines.AttemptPlacement {
	p := &pipelines.AttemptPlacement{Surface: pipelines.PlacementFleet}
	if len(h.tried) > 0 {
		p.ExcludeWorkers = slices.Clone(h.tried)
	}
	if h.pool {
		p.MaxWaitSeconds = attemptWait(h.policy.FleetWaitSeconds)
	}
	return p
}

// attemptWait is a placed attempt's wait in seconds: at least one, because zero
// means no bound, and at most what the executor accepts.
func attemptWait(seconds int) int {
	return min(max(seconds, 1), maxAttemptWaitSeconds)
}

// retryDelay is the wait before a place's retry number i (from 0): the policy's
// delays in order, the last repeating. An empty list (a policy built by hand) is
// no delay.
func retryDelay(r pipelines.RetryPolicy, i int) time.Duration {
	if len(r.DelaysSeconds) == 0 {
		return 0
	}
	seconds := r.DelaysSeconds[min(max(i, 0), len(r.DelaysSeconds)-1)]
	return time.Duration(max(seconds, 0)) * time.Second
}

// describePlacement is a placement as a log line reads it.
func describePlacement(p *pipelines.AttemptPlacement) string {
	if p == nil {
		return "the step's own placement"
	}
	var b strings.Builder
	b.WriteString(p.Surface)
	if p.PinWorker != "" {
		b.WriteString(", pinned to " + p.PinWorker)
	}
	if len(p.ExcludeWorkers) > 0 {
		b.WriteString(", excluding " + strings.Join(p.ExcludeWorkers, ", "))
	}
	return b.String()
}
