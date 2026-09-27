package automations

// rerun.go -- running a step of a finished run again, and a branch that
// serves its prefix by reference (epic memql#5414, task memql#5415; design
// record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// D18 to D20).
//
// A RE-RUN IS A RESUME OF A FINISHED RUN. The person's act (integrations/work,
// on whichever node served it) writes the request onto the run row --
// `run.rerun`, beside `status: running` -- and the agent that claims the run
// resumes it from the step the request names. Nothing the act knew rides in
// memory: the executing replica is another process, usually another node, and
// the row is the only thing the two share. So everything below is decided from
// the row and the step rows, which is also what makes an interrupted re-run
// resumable by whichever replica the sweep hands it to next.
//
// EVERY STEP FROM THE TARGET ON RUNS AS A NEW VERSION, and every step before it
// is served from what it recorded. The version is one past the highest the
// step has recorded, so a re-run never reuses a version number -- and so never
// reuses the idempotency key a side effect already ran under.
//
// A BRANCH IS A NEW RUN WHOSE PREFIX LIVES IN ANOTHER RUN (D19). The act opens
// it with a head whose entries before the fork point name the source run;
// PrepareRerun rehydrates those steps from the source's rows and ResumeFrom
// serves them. The prefix never executes in the branch: no model call, no
// write, no row -- only the fork step and what follows run, live.
//
// THE OVERRIDE BELONGS TO ONE VERSION OF ONE STEP (D20). withRunContext puts it
// on the targeted step's context only, and the journal records it on that
// step's intent; the next step's context never carries it.

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// Re-run reasons, as run.rerun.reason carries them.
const (
	// RerunReasonRerun is a person running one step again, as a new version.
	RerunReasonRerun = "rerun"
	// RerunReasonHeadMove is the re-run a head move starts when the version
	// it made current has no matching downstream: it re-runs the first stale
	// step and everything after it.
	RerunReasonHeadMove = "headMove"
	// RerunReasonBranch is a fork run's first execution: the fork step and
	// what follows run live, the prefix is served from the source by
	// reference.
	RerunReasonBranch = "branch"
)

var (
	// ErrRerunStepInvalid refuses a re-run whose target is not one of the
	// automation's own top-level steps. It wraps work.ErrNestedStep or
	// work.ErrStepNotInRun, which say which.
	ErrRerunStepInvalid = errors.New("the re-run names a step this run cannot re-run")
	// ErrForkPrefixUnavailable refuses a run whose prefix cannot be served by
	// reference: the run holding a step's version was not loaded, never
	// finished the step, or has moved on to another version of it since.
	// Executing the step instead would break the one promise a branch makes.
	ErrForkPrefixUnavailable = errors.New("a step before the re-run cannot be served from the run that holds its version")
)

// RerunSpec is a re-run request as the executor serves it: decoded from
// run.rerun, or built by a caller that drives ResumeFrom itself.
type RerunSpec struct {
	// RequestId identifies the request. The dispatcher claims the run under
	// it, so a re-run is never refused by the lease of the execution before
	// it.
	RequestId string
	// Reason is RerunReasonRerun, RerunReasonHeadMove or RerunReasonBranch.
	Reason string
	// StepKey is the top-level step the request targets: the step re-run, the
	// first stale step of a head move, or the fork step of a branch.
	StepKey string
	// Override is what the person changed for the targeted step (D20); nil
	// when they changed nothing about the call.
	Override *common.StepOverride
	// Snapshot is the workspace the targeted session step starts against.
	Snapshot *common.WorkspaceSnapshot
	// Workspace is a fresh workspace for every step of this execution; empty
	// keeps the default.
	Workspace string
	// RequestedBy is the person who asked. The targeted version names them
	// as its author when the override carries a prompt or inputs.
	RequestedBy string
	// OverrideRecord is the override exactly as the request stored it. The
	// targeted version's `override` field records it verbatim, so a reader
	// sees what the person asked for rather than this package's reading of
	// it. Nil records one rendered from Override.
	OverrideRecord map[string]any
	// Versions is the version each re-executed step runs as, when the act
	// that planned the request wrote it (rerun.versions, from
	// work.PlanRerun over EVERY recorded version). It is a FLOOR: the step
	// rows this package reads are the collapsed newest row-versions, and
	// after a head move re-asserted an earlier version the newest row no
	// longer carries the highest version number -- so without the plan's
	// numbers a re-run could reuse one. Nil falls back to the newest row.
	Versions map[string]int
}

// targets reports whether the step key is the targeted step or a step inside
// it: a nested key belongs to the version of the step that holds it, so a
// logic or loop body the targeted step runs is part of its execution.
func (s *RerunSpec) targets(key string) bool {
	return s != nil && s.StepKey != "" && topLevelKey(key) == s.StepKey
}

// authored reports whether the person wrote this version's input (D20): a
// prompt or inputs, as against a level, a model or an effort.
func (s *RerunSpec) authored() bool {
	return s != nil && s.Override != nil && (s.Override.Prompt != "" || len(s.Override.Inputs) > 0)
}

// overrideObject is the stored form of the override for the targeted
// version's intent: the request's own record when it has one, else one
// rendered from the typed override. Never nil, because `{}` is how a version
// nobody overrode says so.
func (s *RerunSpec) overrideObject() map[string]any {
	if s == nil {
		return map[string]any{}
	}
	if len(s.OverrideRecord) > 0 {
		return s.OverrideRecord
	}
	o := s.Override
	if o == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	for k, v := range map[string]string{"level": o.Level, "model": o.Model, "effort": o.Effort, "prompt": o.Prompt, "requestedBy": o.RequestedBy} {
		if v != "" {
			out[k] = v
		}
	}
	if len(o.Inputs) > 0 {
		out["inputs"] = o.Inputs
	}
	if len(o.GuidanceAxes) > 0 || o.GuidanceReason != "" || o.FeedbackId != "" {
		axes := map[string]any{"product": false, "process": false, "performance": false}
		for _, a := range o.GuidanceAxes {
			axes[a] = true
		}
		guidance := map[string]any{"axes": axes}
		if o.GuidanceReason != "" {
			guidance["reason"] = o.GuidanceReason
		}
		if o.FeedbackId != "" {
			guidance["feedbackId"] = o.FeedbackId
		}
		out["guidance"] = guidance
	}
	return out
}

// rerunSpecFrom decodes run.rerun. An absent request and `{}` -- how the
// journal clears one when the run closes -- are both nil.
func rerunSpecFrom(v any) *RerunSpec {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	spec := &RerunSpec{
		RequestId:   strings.TrimSpace(stringField(m, "requestId")),
		Reason:      strings.TrimSpace(stringField(m, "reason")),
		StepKey:     strings.TrimSpace(stringField(m, "stepKey")),
		Workspace:   strings.TrimSpace(stringField(m, "workspace")),
		RequestedBy: strings.TrimSpace(stringField(m, "requestedBy")),
		Snapshot:    workspaceSnapshotFrom(m["snapshot"]),
	}
	if record, ok := m["override"].(map[string]any); ok && len(record) > 0 {
		spec.OverrideRecord = record
		spec.Override = stepOverrideFrom(record)
		if spec.RequestedBy == "" {
			spec.RequestedBy = strings.TrimSpace(stringField(record, "requestedBy"))
		}
	}
	if versions, ok := m["versions"].(map[string]any); ok {
		for key := range versions {
			if n := intField(versions, key); n > 0 {
				if spec.Versions == nil {
					spec.Versions = map[string]int{}
				}
				spec.Versions[key] = n
			}
		}
	}
	return spec
}

// guidanceAxes is the order a disliked version's axes are named in, which is
// the framework's own: the result, the approach, the behaviour.
var guidanceAxes = []string{"product", "process", "performance"}

// stepOverrideFrom decodes a stored override object: {level, model, effort,
// prompt, inputs, guidance: {axes: {product, process, performance}, reason,
// feedbackId}, requestedBy}. It answers nil for an override that changes
// nothing about the call, so a context carries an override only when there is
// one to apply.
func stepOverrideFrom(m map[string]any) *common.StepOverride {
	if len(m) == 0 {
		return nil
	}
	o := &common.StepOverride{
		Level:       strings.TrimSpace(stringField(m, "level")),
		Model:       strings.TrimSpace(stringField(m, "model")),
		Effort:      strings.TrimSpace(stringField(m, "effort")),
		Prompt:      stringField(m, "prompt"),
		RequestedBy: strings.TrimSpace(stringField(m, "requestedBy")),
	}
	if inputs, ok := m["inputs"].(map[string]any); ok && len(inputs) > 0 {
		o.Inputs = inputs
	}
	if g, ok := m["guidance"].(map[string]any); ok {
		if axes, ok := g["axes"].(map[string]any); ok {
			for _, name := range guidanceAxes {
				if on, _ := axes[name].(bool); on {
					o.GuidanceAxes = append(o.GuidanceAxes, name)
				}
			}
		}
		o.GuidanceReason = stringField(g, "reason")
		o.FeedbackId = strings.TrimSpace(stringField(g, "feedbackId"))
	}
	if o.Empty() {
		return nil
	}
	return o
}

// workspaceSnapshotFrom decodes rerun.snapshot, {files: [{path, fileId}],
// unrecordedCommands}. A snapshot that names its files -- even none of them --
// is a snapshot: the new session starts against exactly that workspace. An
// absent one, or `{}`, is nil.
func workspaceSnapshotFrom(v any) *common.WorkspaceSnapshot {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	raw, present := m["files"]
	if !present {
		return nil
	}
	snap := &common.WorkspaceSnapshot{}
	files, _ := raw.([]any)
	for _, f := range files {
		fm, ok := f.(map[string]any)
		if !ok {
			continue
		}
		path, fileId := strings.TrimSpace(stringField(fm, "path")), strings.TrimSpace(stringField(fm, "fileId"))
		if path == "" || fileId == "" {
			continue
		}
		snap.Files = append(snap.Files, common.SnapshotFile{Path: path, FileId: fileId})
	}
	return snap
}

// topLevelKey is the run's own step a key belongs to: the key itself, or the
// first segment of a nested one (`for_x/0/touch` belongs to `for_x`).
func topLevelKey(key string) string {
	if i := strings.IndexByte(key, '/'); i >= 0 {
		return key[:i]
	}
	return key
}

// rerunTargetIndex finds the targeted step among the automation's own steps,
// refusing a nested key and one the automation does not have. A branch names
// its fork point twice, as forkAtStepKey and as rerun.stepKey; the two must
// agree, because serving a prefix up to one and executing from the other would
// run a step twice or not at all.
func rerunTargetIndex(automation *Automation, spec *RerunSpec, forkAtStepKey string) (int, error) {
	key := spec.StepKey
	if spec.Reason == RerunReasonBranch {
		fork := strings.TrimSpace(forkAtStepKey)
		switch {
		case key == "":
			key = fork
		case fork != "" && fork != key:
			return 0, fmt.Errorf("%w: the branch forks at %q but the request names %q", ErrRerunStepInvalid, fork, key)
		}
	}
	if key == "" {
		return 0, fmt.Errorf("%w: the request names no step", ErrRerunStepInvalid)
	}
	if isNestedStepKey(key) {
		return 0, fmt.Errorf("%w: %w: %s", ErrRerunStepInvalid, work.ErrNestedStep, key)
	}
	for i, step := range automation.Steps {
		if step != nil && step.ID == key {
			return i, nil
		}
	}
	return 0, fmt.Errorf("%w: %w: %s", ErrRerunStepInvalid, work.ErrStepNotInRun, key)
}

// finishedFor reports whether a recorded step state is one the body moves past
// without running it again: done, skipped by its condition, or failed under
// `on error continue`.
func finishedFor(state StepState, step *Step) bool {
	switch state.Status {
	case "done", "skipped":
		return true
	case "failed":
		return step != nil && step.OnError == ErrorStrategyContinue
	}
	return false
}

// rerunResumePoint is where a run serving a re-run resumes: the targeted step,
// or -- when an execution of the request was interrupted, or parked on a
// failure -- the first step at or after it the request has not yet given a
// finished new version.
//
// Which steps those are is read three ways, strongest first:
//
//   - a BRANCH: every row the branch holds for a step at or after the fork
//     point is its own execution's, so the first such step it has not
//     finished is the answer;
//   - the plan's VERSIONS: a step whose newest row is a finished version at
//     or past the one the plan gave it was re-run by this request;
//   - the run's STALE STEPS: the journal takes each step off the list as its
//     new version finishes (stepFinished), so the first one still on it is
//     where the request stopped.
//
// With none of them the answer is the targeted step, which at worst runs a
// finished step once more as a new version -- never a step skipped.
func rerunResumePoint(j *RunJournal, automation *Automation, spec *RerunSpec, target int) string {
	steps := automation.Steps
	if spec.Reason == RerunReasonBranch {
		for _, step := range steps[target:] {
			if step != nil && !finishedFor(j.StepStates[step.ID], step) {
				return step.ID
			}
		}
		return steps[target].ID
	}
	if len(spec.Versions) > 0 {
		for _, step := range steps[target:] {
			if step == nil {
				continue
			}
			state, want := j.StepStates[step.ID], spec.Versions[step.ID]
			if want <= 0 || j.MaxAttempt[step.ID] < want || !finishedFor(state, step) {
				return step.ID
			}
		}
		return steps[target].ID
	}
	if len(j.StaleSteps) > 0 {
		stale := map[string]bool{}
		for _, k := range j.StaleSteps {
			stale[k] = true
		}
		for _, step := range steps[target:] {
			if step != nil && stale[step.ID] {
				return step.ID
			}
		}
	}
	return steps[target].ID
}

// rerunBases is the attempt base of every step a re-run may execute: one less
// than the version it runs as. That version is one past the highest the step
// recorded -- a step with a row has recorded at least one, whatever its attempt
// field says -- and, from the resume point on, never below the version the
// plan gave it. The steps before the resume point are served rather than run;
// they carry a base only so that one which does run (a step the run never
// reached) cannot write over a version it already has.
//
// A branch executes its steps in its OWN run, whose rows start empty, so the
// plan's numbers (the source's) do not apply there.
func rerunBases(j *RunJournal, automation *Automation, spec *RerunSpec, at int) map[string]int {
	bases := map[string]int{}
	for i, step := range automation.Steps {
		if step == nil {
			continue
		}
		base := 0
		if _, recorded := j.StepStates[step.ID]; recorded {
			base = max(j.MaxAttempt[step.ID], 1)
		}
		if i >= at && spec.Reason != RerunReasonBranch {
			if want := spec.Versions[step.ID]; want-1 > base {
				base = want - 1
			}
		}
		bases[step.ID] = base
	}
	return bases
}

// RerunSources lists the runs a re-run's prefix is served from: every run the
// head names for a step before the target other than the run itself, and, for
// a branch step the head does not name, the run it was forked from. The
// dispatcher loads each (LoadRunJournal) and passes them to PrepareRerun.
func RerunSources(run *RunJournal, automation *Automation) []string {
	if run == nil || run.Rerun == nil || automation == nil {
		return nil
	}
	target, err := rerunTargetIndex(automation, run.Rerun, run.ForkAtStepKey)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, step := range automation.Steps[:target] {
		if step == nil {
			continue
		}
		if ref, ok := prefixReference(run, step.ID); ok && !seen[memql.BareShortId(ref.RunId)] {
			seen[memql.BareShortId(ref.RunId)] = true
			out = append(out, ref.RunId)
		}
	}
	return out
}

// prefixReference answers whether a step before the target is served from
// ANOTHER run, and which version of it. The head says so for every step it
// names by reference. A branch step the head does not name at all, and the
// branch holds no row for, falls back to the run it was forked from at that
// run's newest version (Version 0: not checked) -- the shape of a branch whose
// act found no head on its source.
func prefixReference(run *RunJournal, key string) (work.HeadEntry, bool) {
	if e, ok := run.Head[key]; ok {
		if e.RunId != "" && memql.BareShortId(e.RunId) != memql.BareShortId(run.RunId) {
			return e, true
		}
		return work.HeadEntry{}, false
	}
	if run.Rerun != nil && run.Rerun.Reason == RerunReasonBranch && strings.TrimSpace(run.ForkedFromRunId) != "" {
		if _, own := run.StepStates[key]; !own {
			return work.HeadEntry{RunId: run.ForkedFromRunId}, true
		}
	}
	return work.HeadEntry{}, false
}

// PrepareRerun builds what a run carrying a re-run request resumes from, and
// the options to resume it with.
//
// The journal is the run's own -- its id, its variables, its head, its request
// -- with every step before the target that the head names by reference
// rehydrated from the run holding that version (sources, by id). A reference is
// served only if that run's newest row of the step is the version the head
// names and finished: a run that has since moved on no longer holds the result
// the head promises, and serving whatever it holds now would put an answer
// under the branch that its upstream never computed. A branch's every prefix
// step must resolve this way or the request is refused
// (ErrForkPrefixUnavailable) -- executing it instead is exactly what a branch
// promises not to do.
//
// The resume point is rerunResumePoint's. The options allow side effects: the
// resume point of a re-run is a step the person asked to run again, which is a
// decision to run its effect again under a new idempotency key.
//
// A goal run's variables came from the goal's caller, so its journal is marked
// caller-supplied whatever the row says (ExecuteAdopted's rule, memql#2888): a
// branch's row is written by the act, and an unmarked copy would promote the
// template's steps to internal origin on caller-chosen arguments.
func PrepareRerun(run *RunJournal, sources []*RunJournal, automation *Automation) (*RunJournal, *ResumeOptions, error) {
	if run == nil || automation == nil {
		return nil, nil, ErrRunJournalInvalid
	}
	spec := run.Rerun
	if spec == nil {
		return nil, nil, fmt.Errorf("%w: run %s carries no re-run request", ErrRunJournalInvalid, run.RunId)
	}
	if err := ensurePrepared(automation); err != nil {
		return nil, nil, err
	}
	target, err := rerunTargetIndex(automation, spec, run.ForkAtStepKey)
	if err != nil {
		return nil, nil, err
	}
	bySource := map[string]*RunJournal{}
	for _, s := range sources {
		if s != nil {
			bySource[memql.BareShortId(s.RunId)] = s
		}
	}

	out := *run
	out.Steps = make(map[string]*MinimalStepResult, len(run.Steps))
	for k, v := range run.Steps {
		out.Steps[k] = v
	}
	out.StepStates = make(map[string]StepState, len(run.StepStates))
	for k, v := range run.StepStates {
		out.StepStates[k] = v
	}
	out.MaxAttempt = make(map[string]int, len(run.MaxAttempt))
	for k, v := range run.MaxAttempt {
		out.MaxAttempt[k] = v
	}
	out.Head = work.Head{}
	for k, v := range run.Head {
		out.Head[k] = v
	}
	if out.GoalId != "" {
		out.CallerSuppliedPayload = true
	}

	for _, step := range automation.Steps[:target] {
		if step == nil {
			continue
		}
		ref, isRef := prefixReference(run, step.ID)
		if !isRef {
			if spec.Reason == RerunReasonBranch && !finishedFor(run.StepStates[step.ID], step) {
				return nil, nil, fmt.Errorf("%w: %s has no version the branch can serve", ErrForkPrefixUnavailable, step.ID)
			}
			continue
		}
		src := bySource[memql.BareShortId(ref.RunId)]
		if src == nil {
			return nil, nil, fmt.Errorf("%w: %s lives in run %s, which was not loaded", ErrForkPrefixUnavailable, step.ID, ref.RunId)
		}
		state, recorded := src.StepStates[step.ID]
		if !recorded || !finishedFor(state, step) {
			return nil, nil, fmt.Errorf("%w: run %s holds no finished version of %s", ErrForkPrefixUnavailable, ref.RunId, step.ID)
		}
		newest := src.MaxAttempt[step.ID]
		if ref.Version > 0 && newest != ref.Version {
			return nil, nil, fmt.Errorf("%w: the head names version %d of %s in run %s, which now holds version %d", ErrForkPrefixUnavailable, ref.Version, step.ID, ref.RunId, newest)
		}
		out.StepStates[step.ID] = state
		if state.Status == "done" {
			out.Steps[step.ID] = src.Steps[step.ID]
		} else {
			delete(out.Steps, step.ID)
		}
		// A step this run does not hold keeps no attempt count of its own:
		// its versions are the other run's.
		delete(out.MaxAttempt, step.ID)
		out.Head[step.ID] = work.HeadEntry{Version: max(newest, 1), RunId: ref.RunId}
	}
	out.FailedStep = ""

	from := rerunResumePoint(&out, automation, spec, target)
	return &out, &ResumeOptions{FromStep: from, AllowSideEffects: true, Rerun: spec}, nil
}

// runHead is the head a run keeps while it executes (epic memql#5414): the
// current version of every top-level step, advanced at each step's intent and
// written whole on every receipt's run write. Whole, because the run row's
// read-merge is shallow -- a map naming only the step that finished would
// erase every other entry.
//
// stale is the request's staleSteps while the run serves a re-run: each step
// leaves it as its new version finishes, and every receipt writes what is left,
// which is how an interrupted re-run knows where it stopped. Nil when the run
// serves none, and then staleSteps is never written.
type runHead struct {
	mu    sync.Mutex
	head  work.Head
	stale []string
}

// newRunHead starts a run's head at initial, cloned so the run's writes never
// reach the journal it was read from.
func newRunHead(initial work.Head, stale []string) *runHead {
	h := &runHead{head: work.Head{}}
	for k, v := range initial {
		h.head[k] = v
	}
	if stale != nil {
		h.stale = append([]string{}, stale...)
	}
	return h
}

// advance records that key's version is starting, and answers the head of
// every earlier step at that moment -- the version's basis. Nested keys are
// not entries: a nested step belongs to the version of the step holding it.
func (h *runHead) advance(order []string, key string, version int) work.Head {
	if h == nil || isNestedStepKey(key) {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	basis := work.BasisFor(order, h.head, key)
	h.head[key] = work.HeadEntry{Version: version}
	return basis
}

// finished takes a step off the stale list: its new version finished, or the
// run moved past it.
func (h *runHead) finished(key string) {
	if h == nil || h.stale == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := h.stale[:0]
	for _, k := range h.stale {
		if k != key {
			kept = append(kept, k)
		}
	}
	h.stale = kept
}

// write adds the head -- and, while a re-run is served, what is left of its
// stale steps -- to a run write.
func (h *runHead) write(args map[string]any) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	args["head"] = h.head.Object()
	if h.stale != nil {
		args["staleSteps"] = append([]string{}, h.stale...)
	}
}

// resumeHead is the head a resumed run starts from: the head its row stored,
// with each step the run holds rows for at its newest row's version. The
// newest rows ARE the head -- a head move re-asserts the version it makes
// current as a new row-version -- so a stored head that missed the last intent
// (the node died before the receipt that would have written it) is corrected
// rather than trusted, and a run written before the head existed gets one. An
// entry naming another run is a version this run does not hold, and no row of
// this run replaces it.
func resumeHead(j *RunJournal) work.Head {
	head := work.Head{}
	for k, v := range j.Head {
		head[k] = v
	}
	for key := range j.StepStates {
		if isNestedStepKey(key) {
			continue
		}
		if e, ok := head[key]; ok && e.RunId != "" && memql.BareShortId(e.RunId) != memql.BareShortId(j.RunId) {
			continue
		}
		if v := j.MaxAttempt[key]; v > 0 {
			head[key] = work.HeadEntry{Version: v}
		} else {
			head[key] = work.HeadEntry{Version: 1}
		}
	}
	return head
}
