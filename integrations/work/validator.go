package work

// validator.go -- workValidateAnswer: check a finished goal run's answer
// before a person looks (epic memql#5414, #5416; design D22).
//
// A PRE-FILTER, NEVER A CERTIFIER, AND THAT IS STRUCTURAL RATHER THAN A
// PROMISE. One bounded model call at the run's own level applies the AI
// Fluency framework's three Discernment axes to the answer against what was
// asked, and the verdict is written as a `decision` observation plus the run's
// validation summary. That is ALL it writes: never a `feedback` observation
// (epic D's candidate gate reads feedback and nothing else, so the validator
// cannot count as a like), never a construct, and nothing in
// integrations/procedure is reachable from here -- which is what "it never
// moves a procedure's ladder" means in code, and a test pins all three. A
// person's own verdict outranks it; when the two disagree, recordFeedback
// keeps the disagreement on the person's row, where it is the signal that
// this prompt needs work.
//
// WHO MAY RUN IT. The builtin is not @sdk, and its ownerUserId argument names
// WHOSE run to read, so the argument is an authorization decision. The
// handler admits internal origin only, under the validateGoalAnswer
// automation's own actor (whose completion event carries the owner), the
// cluster acting through trusted server-side Go, or a person checking a run of
// their own -- and even then the owner is only a HINT: the run is re-read
// under that owner's borrowed authority, and a hint that does not own the run
// reads nothing. The shape learnFromSucceededRun's handler already has.
//
// WHEN IT SKIPS, IT SAYS WHY: the cluster's feedbackPolicy turns it off, the
// run is not a goal's run asked for through Nexus or the API, it is a replay
// (which exists to reach no model, and a check would spend one), it is a
// driver-owned JOURNAL, it reached no model, or this answer version was
// already checked. A skip is an answer, not an error.
//
// A JOURNAL IS NEVER CHECKED, and the reason is a recursion seen live
// (2026-09-28): an app session's recording run and the D7 delegate's child run
// each carry a goal of their own that says `api`, so the check judged them --
// and when routing sends the check itself to an app, its own call opened a
// session whose recording was judged next. One Ask "hi" opened three Claude
// Code sessions. A journal records a step of ANOTHER run; that run is the one
// a person asked for, and the one whose answer this check is for.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

const (
	// validatorAutomationActor is the synthetic actor the validateGoalAnswer
	// automation runs under (component/automations' system actor prefix and
	// the automation's name).
	validatorAutomationActor = "system:automation:validateGoalAnswer"
	// validatorPrompt is the prompt the check renders.
	validatorPrompt = "validateStepAnswer"
	// validatorClaimName names the cross-replica claim on one answer
	// version's check (SetValidatorClaimer).
	validatorClaimName = "work.validateAnswer"
	// validatorClaimTTL bounds how long a claim holds. Long enough to cover a
	// check -- one bounded model call -- and short enough that a replica that
	// died mid-check does not hold the version forever.
	validatorClaimTTL = 15 * time.Minute
	// maxValidatorAnswerBytes bounds the answer handed to the check. A longer
	// one is cut, and the prompt is told so: a judge that saw half an answer
	// must not be read as having judged the whole of it.
	maxValidatorAnswerBytes = 16 << 10
)

// validatorSchema is the check's structured answer: true on an axis is a
// PROBLEM found on it, for the validator exactly as for a person's dislike.
var validatorSchema = common.StructuredSchema{
	Name:   "stepAnswerVerdict",
	Strict: true,
	Schema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["product","process","performance","reason"],"properties":{"product":{"type":"boolean"},"process":{"type":"boolean"},"performance":{"type":"boolean"},"reason":{"type":"string"}}}`),
}

// answerJudge is the engine's routed structured path, which the check calls
// with the level it declares. *memql.MemQLEngine satisfies it; the package's
// narrow Engine does not need to.
type answerJudge interface {
	RenderPrompt(templateId string, data map[string]any) (string, error)
	Prompts() *memqlengine.PromptRegistry
	CallAIStructured(ctx context.Context, req airoute.ResolveRequest, messages []common.ChatMessage, schema common.StructuredSchema) (memqlengine.StructuredAIResult, error)
}

func (i *Integration) handleValidateAnswer(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := validatorOwner(ctx, args)
	if err != nil {
		return nil, err
	}
	runId := argString(args, "runId")
	if runId == "" {
		return nil, fmt.Errorf("work: validateAnswer needs a runId")
	}
	reply, err := i.validateAnswer(ctx, owner, runId)
	if err != nil {
		return nil, err
	}
	return i.resultNode(reply), nil
}

// validatorOwner admits the callers the check may run for and answers the
// owner whose authority it borrows. See the file header.
func validatorOwner(ctx context.Context, args map[string]any) (string, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return "", fmt.Errorf("work: validateAnswer runs only as the validateGoalAnswer automation; its ownerUserId names whose run to read, which a caller may not decide")
	}
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || trim(ac.UserId) == "" {
		return "", fmt.Errorf("work: validateAnswer needs an authenticated actor")
	}
	owner := argString(args, "ownerUserId")
	if owner == "" {
		return "", fmt.Errorf("work: validateAnswer needs the run's ownerUserId")
	}
	switch {
	case ac.Synthetic && ac.UserId == validatorAutomationActor:
	case ac.Synthetic && ac.IsClusterOwner():
	case !ac.Synthetic && sameUserId(ac.UserId, owner):
	default:
		return "", fmt.Errorf("work: only the validateGoalAnswer automation may name the owner whose run it checks")
	}
	return owner, nil
}

// validateAnswer is the handler body, under the owner's borrowed authority.
func (i *Integration) validateAnswer(ctx context.Context, owner, runId string) (map[string]any, error) {
	st := i.store()
	// The automation's internal origin is what got this call through the
	// gate, and it stops there. Every read below runs as the OWNER, unstamped
	// (store.go RULE 2): a trusted origin inherited by a read would open every
	// @serverOnly read to it, and the owner's actor is what must decide the
	// rows. The writes stamp their own, at the package's one stamp site.
	scoped := ownerActor(auth.ContextWithClientOrigin(ctx), owner)
	run, err := st.runForOwner(scoped, runId)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("work: run %s is not readable as %s; the owner the event carried does not own it", runId, owner)
	}
	if id := rowString(run, "id"); id != "" {
		runId = id
	}

	if !i.feedbackPolicy(scoped).ValidateAnswers {
		return skipped("the cluster's feedbackPolicy turns the answer validator off"), nil
	}
	goalId := rowString(run, "goalId")
	if goalId == "" {
		return skipped("the run serves no goal"), nil
	}
	if rowString(run, "status") != runStatusSucceeded {
		return skipped("the run has not succeeded"), nil
	}
	if rowString(run, "mode") == modeReplay {
		return skipped("a replay reproduces a run's answer from its journal and reaches no model; checking it would spend the call the replay exists to avoid"), nil
	}
	if by := rowString(run, "triggeredBy"); IsDriverOwnedRun(by) {
		return skipped(fmt.Sprintf("the run is a %s journal: it records a step of another run, and its answer is that run's to be checked, not a goal a person asked for", by)), nil
	}
	goal, err := st.goalForOwner(scoped, goalId)
	if err != nil {
		return nil, err
	}
	if goal == nil {
		return skipped("the run's goal is not readable as its owner"), nil
	}
	if via := rowString(goal, "requestedVia"); via != "nexus" && via != "api" {
		return skipped(fmt.Sprintf("the goal arrived through %q; only goals asked for through Nexus or the API are checked", via)), nil
	}

	steps, err := st.query(scoped, "query "+call("workStepsForOwnerRun", map[string]any{"runId": runId}))
	if err != nil {
		return nil, err
	}
	calls, err := st.query(scoped, "query "+call("workModelCallsForOwnerRun", map[string]any{"runId": runId}))
	if err != nil {
		return nil, err
	}
	// The validator's own journaled calls are not the run's work.
	calledModel := map[string]bool{}
	anyCall := false
	for _, c := range calls {
		if rowString(c, "promptRef") == validatorPrompt {
			continue
		}
		anyCall = true
		calledModel[rowString(c, "stepKey")] = true
	}
	answer, session, err := i.answerStep(scoped, owner, run, steps, calledModel)
	if err != nil {
		return nil, err
	}
	reached := anyCall || rowInt(rowMap(run, "spent"), "modelCalls") > 0 ||
		(answer != nil && (session || rowString(answer, "stepType") == work.StepTypeAppAnswer))
	if !reached {
		return skipped("the run reached no model, so there is no model's answer to check"), nil
	}
	if answer == nil {
		return skipped("no finished step of the run answered with a model"), nil
	}
	stepKey, version := rowString(answer, "key"), rowVersion(answer)

	observations, err := st.query(scoped, "query "+call("workObservationsForOwnerRun", map[string]any{"runId": runId}))
	if err != nil {
		return nil, err
	}
	checked := rowMap(run, "validation")
	if newestValidatorDecision(observations, stepKey, version) != nil ||
		(rowString(checked, "stepKey") == stepKey && rowInt(checked, "version") == version) {
		return skipped(fmt.Sprintf("version %d of %s was already checked", version, stepKey)), nil
	}

	text, truncated := answerText(answer)
	if text == "" {
		return skipped(fmt.Sprintf("version %d of %s recorded no answer to check", version, stepKey)), nil
	}
	// ONE CHECK PER ANSWER VERSION ACROSS THE CLUSTER, claimed after every
	// skip that spends nothing and right before the one that spends a model
	// call. The automation's own cluster guard keys on the triggering event's
	// fingerprint, and one run transition can reach a replica twice with two
	// fingerprints (live on 2026-09-28 the planner took each run update from
	// the bridge and again from the durable run-delivery path), so the agent
	// and the planner each ran this check for one answer. The "already
	// checked" test above cannot close that race: both read before either
	// writes.
	if claimer := i.validatorClaimerRef(); claimer != nil {
		key := fmt.Sprintf("%s#%s@%d", runId, stepKey, version)
		if !claimer.ClaimWithTTL(ctx, validatorClaimName, key, validatorClaimTTL) {
			return skipped(fmt.Sprintf("version %d of %s is being checked by another replica", version, stepKey)), nil
		}
	}
	judge, ok := i.engine.(answerJudge)
	if !ok {
		return nil, fmt.Errorf("work: validateAnswer needs the engine's routed model path, and this engine has none")
	}
	level, err := validatorLevel(answer, judge)
	if err != nil {
		return nil, err
	}
	data := map[string]any{
		"goal":    rowString(goal, "statement"),
		"stepKey": stepKey,
		"answer":  text,
		"now":     rfc(i.clock()),
	}
	if description := i.answerDescription(scoped, owner, answer, session); len(description) > 0 {
		data["description"] = description
	}
	rendered, err := judge.RenderPrompt(validatorPrompt, data)
	if err != nil {
		return nil, fmt.Errorf("work: render %s: %w", validatorPrompt, err)
	}

	// THE CALL NAMES THE RUN AND THE STEP IT IS ABOUT, so its spend is the
	// run's -- journaled on it, counted against the goal's ceilings, and
	// attributed on the decision record -- rather than a model call nobody
	// can trace to the work that caused it. It is always a LIVE call: the
	// check is new work, and nothing recorded answers it.
	callCtx := common.ContextWithRun(scoped, common.RunContext{
		RunId:       runId,
		GoalId:      goalId,
		StepKey:     stepKey,
		Mode:        common.RunModeLive,
		OwnerUserId: owner,
	})
	result, err := judge.CallAIStructured(callCtx, airoute.ResolveRequest{
		Level:      level,
		Modality:   airoute.ModalityStructured,
		PromptName: validatorPrompt,
		// Nobody is waiting on the first token of a pre-filter.
		Tags: []string{airoute.TagBackground},
		Needs: airoute.Needs{
			Structured:       true,
			MinContextTokens: airoute.EstimateMinContextTokens(rendered, 0),
		},
	}, []common.ChatMessage{
		{Role: "system", Content: rendered},
		{Role: "user", Content: "Return the structured result requested above."},
	}, validatorSchema)
	if err != nil {
		var ceiling *memqlengine.RunCeilingError
		if errors.As(err, &ceiling) {
			return skipped("the run's ceilings leave no room for the check: " + ceiling.Breach.Reason), nil
		}
		return nil, fmt.Errorf("work: the answer check's model call failed: %w", err)
	}
	verdict, err := parseValidatorAnswer(result.Text)
	if err != nil {
		return nil, err
	}
	servedLevel := string(result.Resolution.Decision.ServedLevel)
	if servedLevel == "" {
		servedLevel = string(level)
	}

	observationId := newRowId(observationConcept)
	decision := map[string]any{
		"validator": map[string]any{
			"axes":    verdict.Axes.Object(),
			"reason":  verdict.Reason,
			"verdict": verdict.Word(),
		},
		"target": map[string]any{"stepKey": stepKey, "version": version},
		"level":  servedLevel,
		"model":  result.Resolution.Model,
	}
	if truncated {
		decision["answerTruncated"] = true
	}
	if err := st.writeInternal(scoped, "mutation "+call("createWorkObservation", map[string]any{
		"observationId": observationId,
		"runId":         runId,
		"stepKey":       stepKey,
		"kind":          "decision",
		"content":       validatorContent(verdict, stepKey, version),
		"data":          decision,
	})); err != nil {
		return nil, err
	}
	if err := st.updateRun(scoped, runId, map[string]any{
		"validation": map[string]any{
			"verdict":       verdict.Word(),
			"stepKey":       stepKey,
			"version":       version,
			"observationId": observationId,
			"level":         servedLevel,
			"at":            rfc(i.clock()),
		},
	}); err != nil {
		return nil, err
	}
	return map[string]any{"observationId": observationId, "verdict": verdict.Word()}, nil
}

func skipped(reason string) map[string]any { return map[string]any{"skipped": reason} }

// SetValidatorClaimer installs the cross-replica claim on one answer version's
// check. First call wins, as SetRunClaimer does.
func (i *Integration) SetValidatorClaimer(c RunClaimer) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.validatorClaimer == nil {
		i.validatorClaimer = c
	}
}

func (i *Integration) validatorClaimerRef() RunClaimer {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.validatorClaimer
}

// feedbackPolicy is v1:work:feedbackPolicy:primary as values. An absent or
// unreadable row is the seed's own values (component/work.FeedbackPolicyFrom),
// so a policy read that fails behaves exactly as the shipped policy does.
func (i *Integration) feedbackPolicy(ctx context.Context) work.FeedbackPolicy {
	row, err := one(i.store().query(ctx, "query feedbackPolicyCurrent()"))
	if err != nil {
		i.log().Warn("work: the feedback policy could not be read; the seeded values apply",
			"component", "work.validator", "err", err)
		return work.FeedbackPolicyFrom(nil)
	}
	return work.FeedbackPolicyFrom(row)
}

// answerStep is the step whose answer the run gave: the LAST finished
// top-level step, in the run's own order, that spent intelligence -- a
// reasoning step, an app's answer, a step that made a model call, or a step
// an app session answered. session reports the last of those.
func (i *Integration) answerStep(ctx context.Context, owner string, run map[string]any, steps []map[string]any, calledModel map[string]bool) (map[string]any, bool, error) {
	order := topLevelOrder(rowStringSlice(run, "stepOrder"))
	position := make(map[string]int, len(order))
	for n, k := range order {
		position[k] = n
	}
	rank := func(row map[string]any) int {
		if n, ok := position[rowString(row, "key")]; ok {
			return n
		}
		return len(order) + rowInt(row, "seq")
	}
	var best map[string]any
	bestSession := false
	for _, row := range steps {
		key := rowString(row, "key")
		if key == "" || strings.Contains(key, "/") || rowString(row, "status") != "done" {
			continue
		}
		if best != nil && rank(row) <= rank(best) {
			continue
		}
		intelligent := rowString(row, "kind") == "reasoning" ||
			rowString(row, "stepType") == work.StepTypeAppAnswer || calledModel[key]
		recording, err := i.recordingOf(ctx, owner, row)
		if err != nil {
			return nil, false, err
		}
		if session := recording != ""; intelligent || session {
			best, bestSession = row, session
		}
	}
	return best, bestSession, nil
}

// answerDescription is what the step was asked for, as far as the run
// recorded it. An absent key was not recorded, and the prompt tells the model
// to judge only what is present.
func (i *Integration) answerDescription(ctx context.Context, owner string, answer map[string]any, session bool) map[string]any {
	d := map[string]any{}
	if session {
		if s, err := one(i.store().query(ownerActor(ctx, owner),
			"query "+call("appSessionsForStep", map[string]any{"stepId": rowString(answer, "id")}))); err == nil && s != nil {
			if p := rowString(s, "prompt"); p != "" {
				d["prompt"] = p
			}
			if schema := rowString(s, "responseSchema"); schema != "" {
				d["responseSchema"] = schema
			}
		}
	}
	if _, has := d["prompt"]; !has {
		if p := rowString(rowMap(answer, "override"), "prompt"); p != "" {
			d["prompt"] = p
		}
	}
	callInfo := rowMap(answer, "call")
	if purpose := trim(rowString(callInfo, "construct") + " " + rowString(callInfo, "name")); purpose != "" {
		d["purpose"] = purpose
	}
	if pc := rowMap(answer, "postcondition"); len(pc) > 0 {
		d["postcondition"] = pc
	}
	return d
}

// answerText is the step's answer as a person would read it, bounded.
func answerText(row map[string]any) (string, bool) {
	result := rowMap(row, "result")
	var v any = result
	if a, ok := result["answer"]; ok {
		v = a
	} else if r, ok := result["result"]; ok {
		v = r
	}
	var text string
	switch t := v.(type) {
	case nil:
	case string:
		text = t
	default:
		if b, err := json.Marshal(t); err == nil {
			text = string(b)
		}
	}
	text = trim(text)
	if text == "" || text == "null" || text == "{}" {
		return "", false
	}
	if len(text) <= maxValidatorAnswerBytes {
		return text, false
	}
	cut := maxValidatorAnswerBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("\n\n[The answer was cut at %d bytes for this check; judge only what is shown.]", cut), true
}

// validatorLevel is the level the check runs at: the run's own, read off the
// answer step -- the level its binding recorded, else the level a person asked
// that version to run at -- and the prompt's declared level for an answer that
// recorded neither. Embeddings is never a level an answer is judged at.
func validatorLevel(answer map[string]any, judge answerJudge) (airoute.Level, error) {
	for _, candidate := range []string{
		rowString(rowMap(answer, "binding"), "level"),
		rowString(rowMap(answer, "override"), "level"),
	} {
		if l, err := airoute.ParseLevel(candidate); err == nil && l != airoute.LevelEmbeddings {
			return l, nil
		}
	}
	if p, ok := judge.Prompts().Get(validatorPrompt); ok && p != nil {
		if l, err := airoute.ParseLevel(strings.TrimSpace(p.Level)); err == nil {
			return l, nil
		}
	}
	return "", fmt.Errorf("work: prompt %s is not loaded or declares no level", validatorPrompt)
}

// parseValidatorAnswer reads the check's structured answer, refusing one that
// does not name all three axes and a reason: a verdict with a missing axis
// would read as "no problem there", which the model never said.
func parseValidatorAnswer(text string) (work.ValidatorVerdict, error) {
	var out struct {
		Product     *bool   `json:"product"`
		Process     *bool   `json:"process"`
		Performance *bool   `json:"performance"`
		Reason      *string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &out); err != nil {
		return work.ValidatorVerdict{}, fmt.Errorf("work: the answer check's reply is not the verdict it was asked for: %w", err)
	}
	if out.Product == nil || out.Process == nil || out.Performance == nil || out.Reason == nil {
		return work.ValidatorVerdict{}, fmt.Errorf("work: the answer check's reply did not name all three axes and a reason")
	}
	return work.ValidatorVerdict{
		Axes:   work.Axes{Product: *out.Product, Process: *out.Process, Performance: *out.Performance},
		Reason: trim(*out.Reason),
	}, nil
}

// validatorContent is the decision observation's one sentence, its embedding
// source.
func validatorContent(v work.ValidatorVerdict, stepKey string, version int) string {
	s := fmt.Sprintf("The answer validator passed version %d of %s", version, stepKey)
	if v.Flagged() {
		s = fmt.Sprintf("The answer validator flagged version %d of %s (%s)", version, stepKey, strings.Join(v.Axes.Names(), ", "))
	}
	if r := trim(v.Reason); r != "" {
		s += ": " + r
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// sameUserId compares two user ids that may be spelled canonically
// (v1:identity:user:<id>) or bare: a run row stores the canonical form of a
// relationship field, and a caller's token carries whichever the identity
// service minted.
func sameUserId(a, b string) bool {
	bare := func(s string) string { return strings.TrimPrefix(trim(s), "v1:identity:user:") }
	return bare(a) != "" && bare(a) == bare(b)
}
