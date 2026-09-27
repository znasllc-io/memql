package work

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// judgeEngine is the recording engine plus the one model call the validator
// makes. The prompt is the REAL validateStepAnswer, loaded from the DSL tree,
// so a data map the prompt's input schema refuses fails here exactly as it
// would at the engine.
type judgeEngine struct {
	*recordingEngine
	prompts *memqlengine.PromptRegistry

	mu       sync.Mutex
	answer   string
	served   airoute.Level
	model    string
	fail     error
	requests []airoute.ResolveRequest
	runs     []common.RunContext
	rendered []string
}

var (
	loadedPrompts     *memqlengine.PromptRegistry
	loadedPromptsErr  error
	loadedPromptsOnce sync.Once
)

func realPrompts(t *testing.T) *memqlengine.PromptRegistry {
	t.Helper()
	loadedPromptsOnce.Do(func() {
		loadedPrompts = memqlengine.NewPromptRegistry()
		_, loadedPromptsErr = memqlengine.LoadUnifiedPrompts(nil, loadedPrompts, template.New("partials"))
	})
	if loadedPromptsErr != nil {
		t.Fatalf("load the prompt corpus: %v", loadedPromptsErr)
	}
	if _, ok := loadedPrompts.Get(validatorPrompt); !ok {
		t.Fatalf("prompt %s is not in the DSL tree", validatorPrompt)
	}
	return loadedPrompts
}

func (e *judgeEngine) Prompts() *memqlengine.PromptRegistry { return e.prompts }

func (e *judgeEngine) RenderPrompt(name string, data map[string]any) (string, error) {
	p, ok := e.prompts.Get(name)
	if !ok {
		return "", fmt.Errorf("unknown prompt %q", name)
	}
	// The engine normalizes the data through JSON before validating it.
	raw, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	var normalized map[string]any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return "", err
	}
	if err := p.ValidateData(normalized); err != nil {
		return "", fmt.Errorf("data for prompt %q invalid: %w", name, err)
	}
	text, err := p.Render(normalized)
	if err == nil {
		e.mu.Lock()
		e.rendered = append(e.rendered, text)
		e.mu.Unlock()
	}
	return text, err
}

func (e *judgeEngine) CallAIStructured(ctx context.Context, req airoute.ResolveRequest, messages []common.ChatMessage, schema common.StructuredSchema) (memqlengine.StructuredAIResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.requests = append(e.requests, req)
	if rc, ok := common.RunFromContext(ctx); ok {
		e.runs = append(e.runs, rc)
	}
	if e.fail != nil {
		return memqlengine.StructuredAIResult{}, e.fail
	}
	served := e.served
	if served == "" {
		served = req.Level
	}
	out := memqlengine.StructuredAIResult{Text: e.answer, Served: "live"}
	out.Resolution.Model = e.model
	out.Resolution.Decision.Level = req.Level
	out.Resolution.Decision.ServedLevel = served
	return out, nil
}

func (e *judgeEngine) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.requests)
}

// newValidatorIntegration is a finished goal run asked for through Nexus whose
// draft step reasoned and answered.
func newValidatorIntegration(t *testing.T) (*Integration, *judgeEngine) {
	t.Helper()
	rec := newRecordingEngine()
	judge := &judgeEngine{
		recordingEngine: rec,
		prompts:         realPrompts(t),
		answer:          `{"product": false, "process": true, "performance": false, "reason": "It skipped the ledger reconciliation the goal asked for."}`,
		model:           "chat54",
	}
	i := New(judge, testLogger())
	i.SetNow(func() time.Time { return testNow })
	i.admitRow = func(context.Context, memorynodes.MemoryNode) bool { return true }

	run := actRunRow(runStatusSucceeded)
	run["spent"] = map[string]any{"modelCalls": float64(1)}
	rec.reply("workRunForOwner", run)
	rec.reply("workGoalForOwner", map[string]any{
		"id": actGoalId, "ownerUserId": "v1:identity:user:" + actOwner,
		"statement": "Draft this week's sales report", "requestedVia": "nexus", "status": "active",
	})
	rec.reply("workStepsForOwnerRun",
		map[string]any{"id": "v1:work:step:r-acts-fetch", "key": "fetch", "seq": float64(0), "status": "done", "kind": "deterministic", "stepType": "function", "attempt": float64(1), "version": float64(1)},
		map[string]any{
			"id": "v1:work:step:r-acts-draft", "key": "draft", "seq": float64(1), "status": "done", "kind": "reasoning",
			"stepType": "function", "attempt": float64(2), "version": float64(2),
			"call":          map[string]any{"construct": "function", "name": "draftReport"},
			"result":        map[string]any{"status": "done", "result": "Sales rose 4% week on week."},
			"binding":       map[string]any{"level": "reasoning", "provider": "federationStrongest"},
			"postcondition": map[string]any{"kind": "check", "ref": "hasTotals", "passed": true},
		},
		map[string]any{"id": "v1:work:step:r-acts-publish", "key": "publish", "seq": float64(2), "status": "done", "kind": "deterministic", "stepType": "action", "attempt": float64(1), "version": float64(1)},
	)
	rec.reply("workModelCallsForOwnerRun", map[string]any{"id": "v1:work:modelCall:m1", "stepKey": "draft", "promptRef": "draftReport"})
	return i, judge
}

// validatorCaller is the validateGoalAnswer automation: its synthetic actor,
// under internal origin (a trusted automation with a payload no caller
// supplied).
func validatorCaller() context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: validatorAutomationActor, Role: auth.RoleReader, Unranked: true, Synthetic: true,
	})
	return auth.ContextWithInternalOrigin(ctx)
}

func validatorArgs() map[string]any {
	return map[string]any{"runId": actRunId, "ownerUserId": "v1:identity:user:" + actOwner}
}

// Issue #5416's acceptance: the validator's verdict never changes the ladder.
// Structurally that is three facts, and each is asserted: it writes exactly
// one `decision` observation and one run update naming only the validation,
// it never writes a `feedback` observation and never reads or writes a
// construct, and nothing in integrations/procedure is reachable from it.
func TestTheValidatorNeverWritesFeedbackOrTouchesAConstruct(t *testing.T) {
	i, judge := newValidatorIntegration(t)

	nodes, err := i.handleValidateAnswer(validatorCaller(), validatorArgs(), 0)
	if err != nil {
		t.Fatalf("workValidateAnswer: %v", err)
	}
	reply := decodeReply(t, nodes)
	if reply["verdict"] != "flag" || rowString(reply, "observationId") == "" {
		t.Fatalf("reply = %v", reply)
	}

	writes := mutationsIn(judge.recordingEngine)
	if len(writes) != 2 || writes[0].Name() != "createWorkObservation" || writes[1].Name() != "updateWorkRun" {
		t.Fatalf("the validator wrote %s; it writes ONE decision observation and ONE run update, nothing else", judge.summary())
	}
	decision := writes[0].Args(t)
	if decision["kind"] != "decision" {
		t.Fatalf("the validator wrote an observation of kind %v; it never writes feedback -- a feedback row is what the candidate gate counts as a person's like or dislike", decision["kind"])
	}
	data := rowMap(decision, "data")
	validator := rowMap(data, "validator")
	if validator["verdict"] != "flag" || !strings.Contains(rowString(validator, "reason"), "ledger") {
		t.Errorf("validator = %v", validator)
	}
	if axes := work.ParseAxes(validator["axes"]); axes.Product || !axes.Process || axes.Performance {
		t.Errorf("axes = %v", validator["axes"])
	}
	if target := rowMap(data, "target"); target["stepKey"] != "draft" || target["version"] != float64(2) {
		t.Errorf("target = %v, want the answer step's current version", target)
	}
	if data["level"] != "reasoning" || data["model"] != "chat54" {
		t.Errorf("level/model = %v/%v", data["level"], data["model"])
	}
	update := writes[1].Args(t)
	if len(update) != 2 {
		t.Errorf("the run update names %v; the validator writes run.validation and nothing else about the run", update)
	}
	validation := rowMap(update, "validation")
	if validation["verdict"] != "flag" || validation["stepKey"] != "draft" || validation["version"] != float64(2) ||
		validation["observationId"] != decision["observationId"] || validation["level"] != "reasoning" {
		t.Errorf("validation = %v", validation)
	}
	for _, w := range writes {
		if !w.Origin.IsInternal() || w.Actor != "v1:identity:user:"+actOwner {
			t.Errorf("%s was written as %q with origin %v; the check writes on the owner's run as its owner", w.Name(), w.Actor, w.Origin)
		}
	}
	for _, c := range judge.recorded() {
		name := strings.ToLower(c.Name())
		if strings.Contains(name, "construct") || strings.Contains(name, "authoring") || strings.Contains(name, "procedure") {
			t.Errorf("the validator reached %s; it never reads or writes a construct", c.Construct())
		}
		// The automation's internal origin gets it through the gate and no
		// further: every read runs as the owner, unstamped.
		if strings.HasPrefix(c.Query, "query ") && c.Origin.IsInternal() {
			t.Errorf("%s ran with the automation's internal origin; a read must be the owner's own", c.Construct())
		}
	}

	// And the ladder is not reachable from here at all: validator.go imports
	// nothing of integrations/procedure. Read from the import block rather
	// than the text, because the file's own comments name the package.
	parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(packageDir(t), "validator.go"), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse validator.go: %v", err)
	}
	if len(parsed.Imports) == 0 {
		t.Fatal("validator.go declares no imports, so this assertion reads nothing")
	}
	for _, imp := range parsed.Imports {
		if strings.Contains(imp.Path.Value, "integrations/procedure") {
			t.Errorf("validator.go imports %s; the ladder must not be reachable from the pre-filter", imp.Path.Value)
		}
	}
	assertEveryCallParses(t, judge.recordingEngine)
}

// The check runs at the RUN's level -- the answer step's recorded binding --
// names the run and the step so its spend is the run's, and is always a live
// call. The prompt it renders carries the goal, the answer and what the step
// was asked for.
func TestTheValidatorChecksAtTheRunsLevelAndNamesTheRun(t *testing.T) {
	i, judge := newValidatorIntegration(t)
	if _, err := i.handleValidateAnswer(validatorCaller(), validatorArgs(), 0); err != nil {
		t.Fatalf("workValidateAnswer: %v", err)
	}
	if judge.calls() != 1 {
		t.Fatalf("the check made %d model calls, want exactly one", judge.calls())
	}
	req := judge.requests[0]
	if req.Level != airoute.LevelReasoning || req.PromptName != validatorPrompt || req.Modality != airoute.ModalityStructured {
		t.Errorf("request = %+v; the check runs at the answer step's recorded level", req)
	}
	if len(judge.runs) != 1 || judge.runs[0].RunId != actRunId || judge.runs[0].StepKey != "draft" ||
		judge.runs[0].Mode != common.RunModeLive || judge.runs[0].GoalId != actGoalId {
		t.Errorf("run context = %+v; the call names the run and the step it is about, live", judge.runs)
	}
	rendered := judge.rendered[0]
	for _, want := range []string{"Draft this week's sales report", "Sales rose 4% week on week.", "draftReport", "hasTotals"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the rendered check does not carry %q:\n%s", want, rendered)
		}
	}
}

// With no level recorded on the answer, the check runs at the prompt's own.
func TestTheValidatorFallsBackToThePromptsLevel(t *testing.T) {
	i, judge := newValidatorIntegration(t)
	judge.reply("workStepsForOwnerRun", map[string]any{
		"id": "v1:work:step:r-acts-draft", "key": "draft", "seq": float64(1), "status": "done", "kind": "reasoning",
		"attempt": float64(1), "result": map[string]any{"result": "An answer."},
	})
	if _, err := i.handleValidateAnswer(validatorCaller(), validatorArgs(), 0); err != nil {
		t.Fatalf("workValidateAnswer: %v", err)
	}
	p, _ := realPrompts(t).Get(validatorPrompt)
	if got := judge.requests[0].Level; string(got) != p.Level {
		t.Errorf("level = %v, want the prompt's declared %v", got, p.Level)
	}
}

func TestTheValidatorSkipsWhenThePolicyIsOff(t *testing.T) {
	i, judge := newValidatorIntegration(t)
	judge.reply("feedbackPolicyCurrent", map[string]any{"id": "v1:work:feedbackPolicy:primary", "validateAnswers": false, "reusableAfterSignatures": float64(2)})

	nodes, err := i.handleValidateAnswer(validatorCaller(), validatorArgs(), 0)
	if err != nil {
		t.Fatalf("workValidateAnswer: %v", err)
	}
	if reason := rowString(decodeReply(t, nodes), "skipped"); !strings.Contains(reason, "feedbackPolicy") {
		t.Errorf("reply = %v; a skip says why", decodeReply(t, nodes))
	}
	if judge.calls() != 0 || len(mutationsIn(judge.recordingEngine)) != 0 {
		t.Errorf("a switched-off validator made %d model calls and wrote %s", judge.calls(), judge.summary())
	}
}

func TestTheValidatorSkipsARunThatReachedNoModel(t *testing.T) {
	i, judge := newValidatorIntegration(t)
	run := actRunRow(runStatusSucceeded)
	run["spent"] = map[string]any{"modelCalls": float64(0)}
	judge.reply("workRunForOwner", run)
	judge.reply("workModelCallsForOwnerRun")
	judge.reply("workStepsForOwnerRun",
		map[string]any{"id": "v1:work:step:r-acts-fetch", "key": "fetch", "seq": float64(0), "status": "done", "kind": "deterministic", "attempt": float64(1)},
		map[string]any{"id": "v1:work:step:r-acts-publish", "key": "publish", "seq": float64(2), "status": "done", "kind": "deterministic", "attempt": float64(1)},
	)

	nodes, err := i.handleValidateAnswer(validatorCaller(), validatorArgs(), 0)
	if err != nil {
		t.Fatalf("workValidateAnswer: %v", err)
	}
	if reason := rowString(decodeReply(t, nodes), "skipped"); !strings.Contains(reason, "reached no model") {
		t.Errorf("reply = %v", decodeReply(t, nodes))
	}
	if judge.calls() != 0 || len(mutationsIn(judge.recordingEngine)) != 0 {
		t.Errorf("a run that reached no model was checked anyway: %d calls, writes %s", judge.calls(), judge.summary())
	}
}

// Every other skip says why, and none of them spends a model call.
func TestTheValidatorSkipsWhatItMustNotCheck(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*judgeEngine)
		want  string
	}{
		{"a goal asked for some other way", func(e *judgeEngine) {
			e.reply("workGoalForOwner", map[string]any{"id": actGoalId, "statement": "x", "requestedVia": "responsibility"})
		}, "responsibility"},
		{"a replay", func(e *judgeEngine) {
			run := actRunRow(runStatusSucceeded)
			run["mode"] = modeReplay
			e.reply("workRunForOwner", run)
		}, "replay"},
		{"a run with no goal", func(e *judgeEngine) {
			run := actRunRow(runStatusSucceeded)
			run["goalId"] = ""
			e.reply("workRunForOwner", run)
		}, "no goal"},
		{"an answer version already checked", func(e *judgeEngine) {
			e.reply("workObservationsForOwnerRun", validatorDecisionRow("v1:work:observation:val-1", "draft", 2, work.Axes{}, "fine"))
		}, "already checked"},
		{"an answer version the run's summary already names", func(e *judgeEngine) {
			run := actRunRow(runStatusSucceeded)
			run["spent"] = map[string]any{"modelCalls": float64(1)}
			run["validation"] = map[string]any{"verdict": "pass", "stepKey": "draft", "version": float64(2)}
			e.reply("workRunForOwner", run)
		}, "already checked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, judge := newValidatorIntegration(t)
			tc.setup(judge)
			nodes, err := i.handleValidateAnswer(validatorCaller(), validatorArgs(), 0)
			if err != nil {
				t.Fatalf("workValidateAnswer: %v", err)
			}
			if reason := rowString(decodeReply(t, nodes), "skipped"); !strings.Contains(reason, tc.want) {
				t.Errorf("reply = %v, want a skip naming %q", decodeReply(t, nodes), tc.want)
			}
			if judge.calls() != 0 || len(mutationsIn(judge.recordingEngine)) != 0 {
				t.Errorf("a skipped check spent %d calls and wrote %s", judge.calls(), judge.summary())
			}
		})
	}
}

// The builtin's argument names WHOSE run to read, so only the automation --
// under internal origin -- may name it.
func TestTheValidatorAdmitsOnlyItsAutomation(t *testing.T) {
	owner := "v1:identity:user:" + actOwner
	cases := []struct {
		name  string
		ctx   context.Context
		owner string
		ok    bool
	}{
		{"the automation", validatorCaller(), owner, true},
		{"a client naming an owner", callerContext("u-mallory"), owner, false},
		{"the automation's actor without internal origin", auth.ContextWithAccess(context.Background(), &auth.AccessContext{
			UserId: validatorAutomationActor, Role: auth.RoleReader, Synthetic: true, Unranked: true,
		}), owner, false},
		{"another automation", auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
			UserId: "system:automation:somethingElse", Role: auth.RoleReader, Synthetic: true, Unranked: true,
		})), owner, false},
		{"a person naming somebody else", auth.ContextWithInternalOrigin(callerContext("u-mallory")), owner, false},
		{"a person naming themselves", auth.ContextWithInternalOrigin(callerContext(actOwner)), owner, true},
		{"the automation with no owner", validatorCaller(), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, judge := newValidatorIntegration(t)
			_, err := i.handleValidateAnswer(tc.ctx, map[string]any{"runId": actRunId, "ownerUserId": tc.owner}, 0)
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("admitted")
				}
				if got := judge.summary(); got != "(none)" {
					t.Errorf("the refused call reached the engine: %s", got)
				}
			}
		})
	}
}

// The run is re-read under the owner the event carried; a hint that does not
// own the run reads nothing, and nothing is written.
func TestTheValidatorRereadsTheRunUnderTheOwnerItWasGiven(t *testing.T) {
	i, judge := newValidatorIntegration(t)
	judge.reply("workRunForOwner")
	_, err := i.handleValidateAnswer(validatorCaller(), map[string]any{"runId": actRunId, "ownerUserId": "u-mallory"}, 0)
	if err == nil {
		t.Fatal("a run the named owner cannot read was checked")
	}
	read := judge.callTo(t, "workRunForOwner")
	if read.Actor != "u-mallory" || read.Origin.IsInternal() {
		t.Errorf("the run was read as %q with origin %v; it is read as the named owner, unstamped", read.Actor, read.Origin)
	}
	if judge.calls() != 0 || len(mutationsIn(judge.recordingEngine)) != 0 {
		t.Errorf("wrote %s", judge.summary())
	}
}

// A long answer is cut, and the check is told so.
func TestTheValidatorBoundsTheAnswerAndSaysSo(t *testing.T) {
	long := strings.Repeat("a", maxValidatorAnswerBytes+500)
	text, truncated := answerText(map[string]any{"result": map[string]any{"result": long}})
	if !truncated || len(text) > maxValidatorAnswerBytes+200 || !strings.Contains(text, "cut at") {
		t.Errorf("answerText of %d bytes = %d bytes, truncated %v", len(long), len(text), truncated)
	}
	if text, truncated := answerText(map[string]any{"result": map[string]any{"answer": map[string]any{"total": 3}}}); truncated || text != `{"total":3}` {
		t.Errorf("an app's structured answer = %q", text)
	}
	if text, _ := answerText(map[string]any{}); text != "" {
		t.Errorf("no result = %q, want nothing to check", text)
	}
}

// A reply that is not the verdict asked for is refused rather than read as
// "no problem found".
func TestTheValidatorRefusesAReplyMissingAnAxis(t *testing.T) {
	i, judge := newValidatorIntegration(t)
	judge.answer = `{"product": false, "reason": "fine"}`
	if _, err := i.handleValidateAnswer(validatorCaller(), validatorArgs(), 0); err == nil {
		t.Fatal("a verdict missing two axes was accepted")
	}
	if len(mutationsIn(judge.recordingEngine)) != 0 {
		t.Errorf("wrote %s", judge.summary())
	}
}
