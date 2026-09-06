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
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
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
	exec   journalExecutor
	logger *slog.Logger
	nodeId string
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
// the shortId, and no other row's canonical id glued in -- and neither the
// run id nor a step key is either.
func workStepId(runId, stepKey string) string {
	return runId + "-" + stepIdUnsafe.ReplaceAllString(stepKey, "-")
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
	case step.Query != nil:
		call["name"] = step.Query.Query
	case step.Mutation != nil:
		call["name"] = step.Mutation.Concept
	case step.Function != nil:
		call["name"] = step.Function.Name
	case step.Automation != nil:
		call["name"] = step.Automation.Name
	case step.Action != nil:
		call["name"] = step.Action.Ref
	case step.Webhook != nil:
		call["name"] = step.Webhook.URL
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
	query, err := journalArgs(name, args)
	if err != nil {
		j.warn(name, err)
		return
	}
	if _, err := j.exec.Execute(journalContext(ctx), query); err != nil {
		j.warn(name, err)
	}
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
	j.call(ctx, "createWorkRun", args)
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
	j.call(ctx, "updateWorkRun", map[string]any{
		"runId":       exec.ID,
		"heartbeatAt": rfc3339(time.Now()),
		"chainHead":   chainHead,
		"stepOrder":   exec.StepOrder,
	})
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
	finished := exec.CompletedAt
	if finished.IsZero() {
		finished = time.Now()
	}
	args := map[string]any{
		"runId":      exec.ID,
		"status":     status,
		"finishedAt": rfc3339(finished),
		"chainHead":  chainHead,
		"stepOrder":  exec.StepOrder,
		"outcome":    map[string]any{"executorStatus": exec.Status},
	}
	if exec.Error != "" {
		args["errorMessage"] = exec.Error
	}
	j.call(ctx, "updateWorkRun", args)
}
