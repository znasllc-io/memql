package automations

// journal.go -- the work journal (design record
// docs/superpowers/specs/2026-09-05-work-spine-design.md, section D).
//
// Every automation execution is a v1:work:run row and every step a
// v1:work:step row, written at the boundaries the executor already has:
// the run opens before the first step; a step is written at `running`
// BEFORE its body executes (the intent) and again at `done`, `failed` or
// `skipped` AFTER (the receipt); the run closes with its terminal status.
// resume.go reads these rows back. The checkpoint side-record they
// replace is gone.
//
// THE WRITER IS A SYNTHETIC CLUSTER ACTOR. The engine blanks the owner it
// would otherwise stamp for such an actor (rowauthz_nonprincipal_owner.go),
// so the rows are the deployment's, readable through the composite tier's
// cluster-owner escape. A goal-owned run (epic A2) runs its journal under
// the owner's borrowed authority instead; nothing here assumes the rows
// are unowned beyond the actor journalContext installs.
//
// A JOURNAL WRITE NEVER FAILS THE RUN. The run is the work and the
// journal is its record; a failed write is logged at Warn and the
// automation continues. The alternative -- failing a sweep because its
// record could not be written -- would let a journal outage stop the
// cluster.
//
// TWO THINGS ARE DELIBERATELY NOT RECORDED. Resolved step arguments, which
// may carry resolved secrets (epic A2 decides redaction), and a step's
// children (forEach and parallel branches), which ride inside the parent's
// trimmed result exactly as they ride inside AutomationExecution.Steps.
//
// A SANDBOXED DRY-RUN WRITES NOTHING, for the reason it minted no
// checkpoint (memql#2932): nothing resumes a preview, and the write would
// escape the sandbox.
//
// AN AUTOMATION THAT REACTS TO WORK ROWS IS NOT JOURNALED. Its own step
// rows would re-fire its trigger, and a feedback loop through the graph
// is the one failure the design's event-sourced substrate makes easy.
// journalSkipsAutomation is the guard; keep it beside the trigger check.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// workJournalActor names the synthetic principal the journal writes as.
// It is a label for log lines and the token subject, never an owner: the
// engine blanks it on write because the actor is Synthetic.
const workJournalActor = "cluster:work-journal"

// journalExecutor is the one seam the journal needs from the engine: run a
// rendered MemQL call. *memql.MemQLEngine satisfies it in production; a
// recording fake does in tests, which is what lets the exact call strings
// be asserted without a database.
type journalExecutor interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

type workJournal struct {
	exec           journalExecutor
	logger         *slog.Logger
	nodeId         string
	heartbeatEvery time.Duration

	// classifier is the failure path's ONE model call, installed from app/ on
	// a node that can reach a model. Nil is a working state: a table miss then
	// falls to ActAsk, which is what a failed run did before any of this was
	// wired. See failure_path.go.
	classifier SymptomClassifier

	// hold, when set, keeps every write until release: a directly called
	// logic's journal, whose run opens only if the logic writes.
	hold *heldWrites
}

// newWorkJournal returns nil when there is no executor, and every method
// on a nil journal is a no-op, so the executor can hold one field and
// never branch on it.
func newWorkJournal(exec journalExecutor, logger *slog.Logger) *workJournal {
	if exec == nil {
		return nil
	}
	return &workJournal{
		exec:   exec,
		logger: logger,
		nodeId: strings.TrimSpace(os.Getenv("MEMQL_NODE_ID")),
	}
}

// journalContext installs the synthetic cluster actor and internal origin
// the @serverOnly work mutations require. Modelled on the seed
// materializer's systemActorContext (component/memql/seed_materializer.go).
//
// RoleOwner is not decoration. AccessContext.IsClusterOwner() is exactly
// `Role == RoleOwner`, and the six run-scoped reads in dsl/work/queries.memql
// each carry `&& actor.isClusterOwner==true` so TestRowAuthzEnforcementLandGate
// can see the tier satisfied. Weaken the role here and every one of those
// reads returns zero rows -- which resume would read as "no journal" and
// answer by re-running completed steps.
func journalContext(ctx context.Context) context.Context {
	// A journal write is not a write the run made: a directly called logic's
	// run opens at the logic's first write (logic_statements.go), and the
	// journal writes that open it must not count as another (release).
	ctx = common.ContextWithoutWriteObserver(ctx)
	claims := map[string]any{"sub": workJournalActor, "role": "system"}
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: workJournalActor, Claims: claims})
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: workJournalActor,
		Role:   auth.RoleOwner,
		// The rank rules do not govern the cluster acting as itself (D4).
		Unranked: true,
		// And the rows are the deployment's, not this label's: Synthetic is
		// what makes undoNonPrincipalOwnerStamp blank the owner.
		Synthetic: true,
	})
	return auth.ContextWithInternalOrigin(ctx)
}

// workTriggerPattern matches a trigger event that names a work-namespace
// concept: graph.node.<verb>.<partition>.v1:work:<concept>, in either the
// partition-segment or the bare form.
var workTriggerPattern = regexp.MustCompile(`^graph\.node\.[a-z]+\.(\*\.|[^.]+\.)?v1:work:`)

// journalSkipsAutomation reports whether an automation must not be
// journaled because its trigger reacts to work rows.
func journalSkipsAutomation(a *Automation) bool {
	if a == nil || a.Trigger == nil {
		return false
	}
	return workTriggerPattern.MatchString(strings.TrimSpace(a.Trigger.Event))
}

// stepIdUnsafe matches every character a MemQL short id may not carry.
var stepIdUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// workStepId is the step row's short id: the run id and the step key,
// joined so a parallel branch id like `layer0.sales` stays a legal short
// id. Deterministic, so a retry writes a NEW VERSION of the SAME row --
// which is what makes "the latest version of each step decides its status"
// true, and therefore what makes resume work.
//
// Composed rather than hashed, and that is a departure from
// docs/public/concepts/identifiers.md's "use MustFromMap" guidance, taken
// deliberately. A hash satisfies determinism just as well and destroys the
// one property that matters to whoever is reading a broken run:
// `v1:work:step:run-1-layer0-sales` names the run and the step, while
// `v1:work:step:<64 hex>` names nothing. The rule the composition must not
// break is the one identifiers.md is actually about -- no concept name in
// the shortId, and no other row's canonical id glued in. Rehydrated runs
// can carry canonical IDs, so extract the run's shortId before composing.
func workStepId(runId, stepKey string) string {
	return memql.BareShortId(runId) + "-" + stepIdUnsafe.ReplaceAllString(stepKey, "-")
}

// stepKindFor derives the spec's step kind from the automation step type.
// Every type is deterministic except function, whose logic may reach a
// prompt; that is the loader rule epic A2 adds, so A1 leaves it empty.
func stepKindFor(step *Step) string {
	if step == nil {
		return ""
	}
	if step.Type == StepTypeFunction {
		return ""
	}
	return "deterministic"
}

// stepCallSummary names what a step invoked, by name only -- never the
// resolved arguments.
func stepCallSummary(step *Step) map[string]any {
	call := map[string]any{"construct": string(step.Type)}
	switch {
	case step.Function != nil:
		call["name"] = step.Function.Name
	case step.Automation != nil:
		call["name"] = step.Automation.Name
	case step.Action != nil:
		call["name"] = step.Action.Ref
	case step.Event != nil:
		call["name"] = step.Event.Topic
	}
	return call
}

// journalArgs renders one call in the NAMED-ARGS invocation form:
// `name(k: v, ...)`, with every value JSON-encoded so a value carrying a
// quote can never break out of its literal.
//
// NOT `name({...})`. That object-literal wrapper was removed in #2335 and
// the parser refuses it outright ("object-literal call args are removed"),
// for reads and writes alike. It is worth stating because the wrapper is
// still rendered by a dozen Go call sites in this tree and looks like the
// house style -- and because a journal write that fails to parse is
// SILENT: call() logs a Warn and the run continues, exactly as designed,
// so the whole journal can be empty with every DB-free test green. The
// db-gated tests in journal_db_test.go are what make that audible.
//
// Keys are sorted so a rendered call is stable, which is what lets a test
// assert one.
func journalArgs(name string, args map[string]any) (string, error) {
	if len(args) == 0 {
		return name + "()", nil
	}
	// A NIL VALUE IS AN ABSENT ARGUMENT, and it has to be dropped rather than
	// rendered. `input: null` is the case that found this: exec.Input is nil
	// whenever an automation declares no `input:` block -- most of them -- and
	// the concept declares `input object`, so the engine refused the whole row
	// with "expected object, but got null". The refusal was invisible, because
	// call() logs a Warn and lets the run continue: every step row landed and
	// no run row ever did.
	// Marshal first, then decide, because the test for "absent" is on the
	// RENDERED value rather than on the Go one. `v == nil` is false for a
	// TYPED nil inside an any -- a `map[string]any(nil)` in an `any` field
	// carries a type, so the interface is non-nil while the JSON is `null` --
	// and exec.Input is exactly that shape. Checking the marshalled bytes
	// catches both spellings with one rule.
	//
	// An EMPTY object or array is NOT absent and must survive: `{}` marshals
	// to "{}", passes this test, and means "this field, empty", which is a
	// different row from one where the field was never written.
	rendered := make(map[string]string, len(args))
	keys := make([]string, 0, len(args))
	for k, v := range args {
		encoded, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("work journal: marshal %s arg %q: %w", name, k, err)
		}
		if string(encoded) == "null" {
			continue
		}
		rendered[k] = string(encoded)
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return name + "()", nil
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('(')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(rendered[k])
	}
	b.WriteByte(')')
	return b.String(), nil
}

func (j *workJournal) call(ctx context.Context, name string, args map[string]any) {
	if j == nil {
		return
	}
	if j.hold.keep(ctx, name, args) {
		return
	}
	j.write(ctx, name, args)
}

// write renders and executes one journal call.
func (j *workJournal) write(ctx context.Context, name string, args map[string]any) {
	query, err := journalArgs(name, args)
	if err != nil {
		j.warn(name, err)
		return
	}
	writeCtx := journalContext(ctx)
	if run, ok := common.RunFromContext(ctx); ok && strings.TrimSpace(run.OwnerUserId) != "" {
		// The owning principal must write an adopted run and its steps.
		// The cluster actor remains the read identity for recovery, but its
		// synthetic writes would blank the step owner and fail owned updates.
		// Masked like journalContext: a journal write is not the run's write.
		writeCtx = auth.ContextWithInternalOrigin(auth.ContextWithUserActor(common.ContextWithoutWriteObserver(ctx), run.OwnerUserId))
	}
	if _, err := j.exec.Execute(writeCtx, query); err != nil {
		j.warn(name, err)
	}
}

// heldWrites keeps a journal's writes until the run they belong to opens: a
// directly called logic's run, which opens at the logic's first write
// (logic_statements.go). A journal whose run never opens writes nothing.
// Held writes are kept unrendered; they cost a render only if they are
// written.
type heldWrites struct {
	mu       sync.Mutex
	released bool
	// open writes the run row when the run opens, ahead of every held write.
	open   func()
	writes []heldWrite
}

type heldWrite struct {
	ctx  context.Context
	name string
	args map[string]any
}

// keep holds one write while the run is unopened, reporting whether it did.
func (h *heldWrites) keep(ctx context.Context, name string, args map[string]any) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return false
	}
	// Written later, if at all: past the deadline of the context it was made
	// under (a heartbeat's pulse is cancelled the moment its call returns).
	h.writes = append(h.writes, heldWrite{ctx: context.WithoutCancel(ctx), name: name, args: args})
	return true
}

// release opens a held journal's run: the run row, then every held write in
// the order it was made. A write made meanwhile waits and follows them, and
// every later write goes straight through. Idempotent.
//
// The journal's own writes are masked from the write observer that calls
// this (journalContext): were they not, the first write of the flush would
// call release again from inside it.
func (j *workJournal) release() {
	if j == nil || j.hold == nil {
		return
	}
	h := j.hold
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return
	}
	h.released = true
	if h.open != nil {
		h.open()
	}
	for _, w := range h.writes {
		j.write(w.ctx, w.name, w.args)
	}
	h.writes = nil
}

// runJournalKey carries, beside the run on a context, the journal that run
// writes through. A logic a statement calls journals its statements through
// it, so they land where the run's own rows land -- held with them while a
// directly called logic's run is unopened, and nowhere for a run that is not
// journaled (a typed nil: journalSkipsAutomation).
type runJournalKey struct{}

// withRunJournal pairs j with the run exec.ID on ctx. A context already
// associated with another run keeps that run's journal, as withRunContext
// keeps its association.
func withRunJournal(ctx context.Context, runId string, j *workJournal) context.Context {
	if run, ok := common.RunFromContext(ctx); ok && memql.BareShortId(run.RunId) != memql.BareShortId(runId) {
		return ctx
	}
	return context.WithValue(ctx, runJournalKey{}, j)
}

// runJournalFrom returns the journal paired with ctx's run, and whether one
// was paired (a nil journal paired is a run that is not journaled).
func runJournalFrom(ctx context.Context) (*workJournal, bool) {
	j, ok := ctx.Value(runJournalKey{}).(*workJournal)
	return j, ok
}

func (j *workJournal) warn(name string, err error) {
	if j == nil || j.logger == nil {
		return
	}
	j.logger.Warn("work journal write failed; the run continues", "component", ComponentName, "mutation", name, "error", err)
}

func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// openRun writes the run row. Called after the executor's own refusal
// gates (args validation, dedup, the cluster guard) so a refused fire
// leaves no row.
func (j *workJournal) openRun(ctx context.Context, automation *Automation, exec *AutomationExecution, triggeringEvent *events.Event) {
	if j == nil || automation == nil || exec == nil {
		return
	}
	j.call(ctx, "createWorkRun", j.openRunArgs(automation, exec, triggeringEvent))
}

// openRunArgs is the run row openRun writes.
func (j *workJournal) openRunArgs(automation *Automation, exec *AutomationExecution, triggeringEvent *events.Event) map[string]any {
	args := map[string]any{
		"runId":                 exec.ID,
		"automationName":        automation.Name,
		"templateFingerprint":   automation.DefinitionFingerprint(fingerprintEngine),
		"input":                 exec.Input,
		"inputFingerprint":      exec.InputFingerprint,
		"triggeredBy":           exec.TriggeredBy,
		"callerSuppliedPayload": exec.CallerSuppliedPayload,
		"mode":                  "live",
		"status":                "running",
		"nodeId":                j.nodeId,
		"initialChainHead":      exec.InitialChainHead,
		"startedAt":             rfc3339(exec.StartedAt),
	}
	if triggeringEvent != nil {
		args["triggerEvent"] = map[string]any{
			"topic":   triggeringEvent.Topic,
			"kind":    triggeringEvent.Kind.String(),
			"payload": triggeringEvent.Payload,
		}
	}
	return args
}

// stepRunning writes the intent version of a step row.
func (j *workJournal) stepRunning(ctx context.Context, exec *AutomationExecution, step *Step, seq int, attempt int) {
	if j == nil || exec == nil || step == nil {
		return
	}
	j.call(ctx, "createWorkStep", map[string]any{
		"stepId":         workStepId(exec.ID, step.ID),
		"runId":          exec.ID,
		"key":            step.ID,
		"seq":            seq,
		"stepType":       string(step.Type),
		"kind":           stepKindFor(step),
		"call":           stepCallSummary(step),
		"status":         "running",
		"attempt":        attempt,
		"idempotencyKey": exec.ID + ":" + step.ID + ":" + strconv.Itoa(attempt),
		"startedAt":      rfc3339(time.Now()),
	})
}

// stepFinished writes the receipt version of a step row and a heartbeat on
// the run. chainHead is passed rather than read off exec because
// AutomationExecution.ChainHead is assigned only on the success path -- the
// checkpoint this replaces was handed the executor loop's local variable for
// the same reason, and reading the struct would write an empty chain head on
// exactly the failed runs resume exists for. The result is the trimmed MinimalStepResult shape, the same
// shape the checkpoint carried and resume rehydrates from.
func (j *workJournal) stepFinished(ctx context.Context, exec *AutomationExecution, step *Step, result *StepResult, chainHead string) {
	if j == nil || exec == nil || step == nil || result == nil {
		return
	}
	j.stepFinishedRowOnly(ctx, exec, step, result)
	j.call(ctx, "updateWorkRun", map[string]any{
		"runId":       exec.ID,
		"heartbeatAt": rfc3339(time.Now()),
		"chainHead":   chainHead,
		"stepOrder":   exec.StepOrder,
	})
}

// stepFinishedRowOnly writes a step's receipt and nothing on the run: for the
// statements of a logic called INSIDE another run (logic_statements.go), whose
// run row -- its heartbeat, chain head and step order -- belongs to the
// caller's executor, not to the logic.
func (j *workJournal) stepFinishedRowOnly(ctx context.Context, exec *AutomationExecution, step *Step, result *StepResult) {
	if j == nil || exec == nil || step == nil || result == nil {
		return
	}
	status := "done"
	switch result.Status {
	case "failed":
		status = "failed"
	case "skipped":
		status = "skipped"
	}
	args := map[string]any{
		"stepId":            workStepId(exec.ID, step.ID),
		"status":            status,
		"resultFingerprint": StepDeterministicFingerprint(step, result),
		"finishedAt":        rfc3339(result.CompletedAt),
		"durationMs":        result.Duration.Milliseconds(),
	}
	if result.Error != "" {
		args["errorMessage"] = result.Error
	}
	if status == "done" {
		trimmed := ToMinimalStepResults(map[string]*StepResult{step.ID: result})
		if m, ok := trimmed[step.ID]; ok && m != nil {
			args["result"] = m
		}
	}
	j.call(ctx, "updateWorkStep", args)
}

// stepSkipped writes a step whose condition decided it would not run: one
// row at `skipped`, with no intent version, because nothing was intended.
func (j *workJournal) stepSkipped(ctx context.Context, exec *AutomationExecution, step *Step, seq int) {
	if j == nil || exec == nil || step == nil {
		return
	}
	j.call(ctx, "createWorkStep", map[string]any{
		"stepId":    workStepId(exec.ID, step.ID),
		"runId":     exec.ID,
		"key":       step.ID,
		"seq":       seq,
		"stepType":  string(step.Type),
		"kind":      stepKindFor(step),
		"call":      stepCallSummary(step),
		"status":    "skipped",
		"attempt":   1,
		"startedAt": rfc3339(time.Now()),
	})
}

// reopenRun flips a failed run back to running for a resume; the retried
// steps write new versions with attempt incremented.
func (j *workJournal) reopenRun(ctx context.Context, exec *AutomationExecution) {
	if j == nil || exec == nil {
		return
	}
	j.call(ctx, "updateWorkRun", map[string]any{
		"runId":  exec.ID,
		"status": "running",
	})
}

// closeRun writes the terminal status. The executor's status vocabulary
// (completed / failed / cancelled) maps onto the spec's.
func (j *workJournal) closeRun(ctx context.Context, exec *AutomationExecution, chainHead string) {
	if j == nil || exec == nil {
		return
	}
	status := "succeeded"
	switch exec.Status {
	case "failed":
		status = "failed"
	case "cancelled":
		status = "cancelled"
	}

	// A RUN THAT FAILED BECAUSE NO DOOR TO A MODEL IS OPEN PARKS INSTEAD
	// (epic memql#5096, design D9). It is not a failure: nothing about the
	// work was wrong, and the condition -- a lid shut, nobody signed in, a
	// ceiling reached -- changes on a human timescale for reasons that have
	// nothing to do with the run. Failing it would throw away a compiled
	// template and a journal because somebody closed a laptop.
	if status == "failed" {
		// DoorsFrom prefers the error VALUE, which carries the structured
		// door report, and falls back to matching the message when the
		// failure travelled as a string. An empty door list is the honest
		// answer in the second case: it says the structure did not reach
		// here, where a synthetic entry would claim a door nobody named.
		if code, doors, ok := work.DoorsFrom(runFailure(exec)); ok {
			j.parkOnInference(ctx, exec, chainHead, code, doors)
			return
		}

		// EVERY OTHER FAILURE IS CLASSIFIED BEFORE IT IS RECORDED AS ONE
		// (epic memql#5127, design D12). The deterministic rules run first and
		// most failures never reach a model at all; the act they select lands
		// on the run as a `waiting` state the sweep can serve, on the node that
		// can serve it. classifyAndAct returns true when it wrote that state,
		// in which case writing `failed` over the top of it here would undo the
		// classification the moment it was made.
		if j.classifyAndAct(ctx, exec, chainHead) {
			return
		}
	}
	j.closeRunRecord(ctx, exec, chainHead)
}

// closeRunRecord writes the run's terminal row as the run ended, with no
// failure path: the tail of closeRun, and the whole close of a directly called
// logic's run (logic_statements.go), whose caller already has its error and
// which nothing parks, classifies or resumes.
func (j *workJournal) closeRunRecord(ctx context.Context, exec *AutomationExecution, chainHead string) {
	if j == nil || exec == nil {
		return
	}
	status := "succeeded"
	switch exec.Status {
	case "failed":
		status = "failed"
	case "cancelled":
		status = "cancelled"
	}
	finished := exec.CompletedAt
	if finished.IsZero() {
		finished = time.Now()
	}
	outcome := map[string]any{"executorStatus": exec.Status}
	if exec.Returned {
		// The value a statement body's `return` ended the run with (epic
		// memql#5370).
		outcome["returned"] = exec.Output
	}
	args := map[string]any{
		"runId":      exec.ID,
		"status":     status,
		"finishedAt": rfc3339(finished),
		"chainHead":  chainHead,
		"stepOrder":  exec.StepOrder,
		"outcome":    outcome,
	}
	if exec.Error != "" {
		args["errorMessage"] = exec.Error
	}
	j.call(ctx, "updateWorkRun", args)
}

// parkOnInference writes the approval and puts the run at `waiting`.
//
// THE ORDER IS LOAD-BEARING: the approval first, the wait second. A run
// parked on an approval id that does not exist is a run waiting on nothing,
// which no person can decide and no sweep can resolve -- it would sit at
// `waiting` until the abandoned sweep eventually closed it saying the node
// stopped answering, a sentence with nothing true in it.
//
// THE SUBJECT IS WHAT THE ROUTER DECIDED, carried as a value rather than
// re-derived. The router built the door report; parsing it back out of the
// rendered message would be a second opinion about a decision already made,
// and the two would drift the first time the wording changed.
func (j *workJournal) parkOnInference(ctx context.Context, exec *AutomationExecution, chainHead, code string, doors []work.DoorReport) {
	now := time.Now().UTC()
	approvalId := "v1:work:approval:" + id.NewShortId()
	stepKey := ""
	if n := len(exec.StepOrder); n > 0 {
		stepKey = exec.StepOrder[n-1]
	}
	req := work.InferenceUnavailableApproval(exec.ID, stepKey, code, doors, now, workApprovalTTL)

	j.call(ctx, "createWorkApproval", map[string]any{
		"approvalId":   approvalId,
		"runId":        req.RunId,
		"stepKey":      req.StepKey,
		"kind":         req.Kind,
		"subject":      req.Subject,
		"artifactHash": req.ArtifactHash,
		"question":     req.Question,
		"options":      req.Options,
		"evidence": map[string]any{
			"tier":   req.Evidence.Tier,
			"reason": req.Evidence.Reason,
			"ruleId": req.Evidence.RuleId,
			"source": req.Evidence.Source,
		},
		"requestedAt": rfc3339(req.RequestedAt),
		"expiresAt":   rfc3339(req.ExpiresAt),
	})

	// `resumeAt` rides the wait so the sweep re-checks it. It is a POLL
	// because the event that would replace it -- a module-readiness feed --
	// is another epic's and this tree does not declare the concept; a
	// subscription to something absent is a resume path that never fires.
	//
	// A CEILING REFUSAL GETS NO resumeAt. Only a person changes a ceiling, so
	// re-dispatching against it would burn a dispatch every five minutes to
	// rediscover a number nobody touched.
	waiting := map[string]any{
		"kind":         "approval",
		"subject":      approvalId,
		"approvalKind": work.ApprovalKindInferenceUnavailable,
		"since":        rfc3339(now),
	}
	if code != work.RefusalCeilingReached {
		waiting["resumeAt"] = rfc3339(now.Add(work.InferenceRetryInterval))
	}
	j.call(ctx, "updateWorkRun", map[string]any{
		"runId":     exec.ID,
		"status":    "waiting",
		"chainHead": chainHead,
		"stepOrder": exec.StepOrder,
		"waitingOn": waiting,
		// NOT finishedAt, and not an errorCode: the run has not finished and
		// has not failed. Writing either would make every terminal-run reader
		// -- the sweep, Nexus, the goal rollup -- treat a parked run as done.
		"errorMessage": exec.Error,
	})
	if j.logger != nil {
		j.logger.Info("work journal: no door to a model is open, so the run parked instead of failing",
			"component", ComponentName, "run", exec.ID, "approval", approvalId, "code", code)
	}
}

// workApprovalTTL is how long a pending inference park stands before it
// lapses. It matches integrations/work's DefaultApprovalTTL: long enough that
// somebody who opens their laptop the next morning finds the run still
// waiting, short enough that a forgotten one does not outlast the awareness of
// why it was raised.
const workApprovalTTL = 24 * time.Hour

// runFailure returns the execution's failure as a VALUE when one survived, and
// as a plain error over the recorded text otherwise.
//
// The second case is not a fallback nobody hits: a run RESUMED from a
// checkpoint has only the string, because ErrorValue is deliberately not
// serialized. Answering with an error over that text lets the message matcher
// still recognise the condition, with an empty door list rather than an
// invented one.
func runFailure(exec *AutomationExecution) error {
	if exec == nil {
		return nil
	}
	if exec.ErrorValue != nil {
		return exec.ErrorValue
	}
	if strings.TrimSpace(exec.Error) == "" {
		return nil
	}
	return errors.New(exec.Error)
}
