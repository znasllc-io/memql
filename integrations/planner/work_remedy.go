package planner

// work_remedy.go -- the planner half of the work spine's failure path
// (epic memql#5127, design D12).
//
// The executor classifies a failed run and lands the ACT on the run as a
// `waiting` state; integrations/work hands the two acts that need more than a
// row to this seam, under a claim per wait, from the run's own event and from
// the sweep. It lives here for the reason work_compile_adapter.go lives here:
// the machinery is the planner's, the WRITES are integrations/work's, and the
// split is not stylistic. The planner is not in the call-origin allowlist, so
// a @serverOnly write from here is REFUSED with one WARN nothing above it
// hears.
//
// REPAIR AND REPLAN ARE DIFFERENT SIZES OF WRONG, and conflating them is the
// mistake this file exists to avoid. A contract miss means one step did
// something other than what it promised: the plan is fine and the step is not.
// A plan miss means the plan is wrong from that step ON: every remaining step
// is suspect and the completed ones are not. So repair touches one step and
// replan re-emits a suffix -- and neither of them ever re-runs from the start,
// which is the unguided rerun the debugging literature measured as
// substantially worse than localized repair.
//
// A REMEDY SPENDS ITS ATTEMPT ONCE. Every outcome after the model was asked
// moves the run off its remedy wait -- onto its new template, or onto a
// person's question -- because a wait left in place is served again by the
// next pass, and a re-plan that failed would cost a reasoning-level call every
// lease, for ever. Only a failure that spent nothing (a read, a door that was
// shut) leaves the run parked to be served again.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

// replanContextLoader is the read half. It is an interface rather than the
// concrete integration so this file can be tested without a database, and so
// the direction of the dependency stays "planner asks work", never the reverse.
type replanContextLoader interface {
	LoadReplanContext(ctx context.Context, ownerUserId, runId, stepKey string) (workintegration.ReplanContext, error)
}

// remedyWriter is the write half. Every write goes through integrations/work
// for the call-origin reason in this file's header, and each re-reads the run
// first and refuses (workintegration.ErrRemedyNotWaiting) when it has moved
// on, so a remedy that ran long cannot overwrite a decision made meanwhile.
type remedyWriter interface {
	InstallReplan(ctx context.Context, ownerUserId, runId string, t workintegration.ReplanTemplate) error
	RequestRepair(ctx context.Context, ownerUserId, runId, stepKey, violation string) error
	AskAboutFailedRemedy(ctx context.Context, ownerUserId, runId, stepKey, kind, reason string) error
}

// WorkRemedy satisfies workintegration.Remedy.
type WorkRemedy struct {
	loop   *PlannerAgentLoop
	reader replanContextLoader
	writer remedyWriter
	now    func() time.Time
}

// NewWorkRemedy returns nil without a loop or without the work integration, so
// app wiring can call SetRemedy unconditionally and a node that runs no
// planner installs nothing. A nil remedy leaves replan and repair waits parked
// and logged, which is visible; a remedy that cannot record its outcome would
// move the run in the log and never in the graph.
func NewWorkRemedy(loop *PlannerAgentLoop, work *workintegration.Integration) *WorkRemedy {
	if loop == nil || work == nil {
		return nil
	}
	return &WorkRemedy{loop: loop, reader: work, writer: work, now: time.Now}
}

// Replan re-plans the gap from the failed step on, KEEPING the completed
// prefix, and installs the new template on the run.
//
// INSTALLING IS COMPILE'S PATH, NOT A SECOND ONE. The draft replanGap returns
// is persisted exactly as a compiled draft is -- sealed, through Gate 1, as a
// validated authoring bundle bound to this run (persistWorkDraft) -- and its
// automation name, construct, fingerprint and version are recorded on the run
// the way WorkCompiler.Compile records them, which is what the executing node
// loads a run's template from. It used to record only the draft's LENGTH and
// put the run back to `running` on the template that had just failed.
//
// The prefix is loaded rather than reconstructed, and a read failure ABORTS
// rather than replanning against an empty one: an empty prefix tells the model
// nothing has been done, so it re-emits every completed step, and the side
// effects those steps already had happen a second time. A draft that does not
// keep the completed steps where resume will serve them is refused for the
// same reason (replanKeepsPrefix).
func (r *WorkRemedy) Replan(ctx context.Context, runId, ownerUserId, stepKey, reason string) bool {
	if r == nil || r.loop == nil || r.loop.engine == nil || r.reader == nil || r.writer == nil {
		return false
	}
	rc, err := r.reader.LoadReplanContext(ctx, ownerUserId, runId, stepKey)
	if err != nil {
		r.warn("work remedy: could not read the run's replan context; the run stays parked rather than re-planning against an empty prefix", runId, err)
		return false
	}
	failedKey := strings.TrimSpace(stepKey)
	if failedKey == "" && rc.FailedStep != nil {
		failedKey, _ = rc.FailedStep["key"].(string)
	}
	if rc.FailedStep == nil || failedKey == "" {
		return r.ask(ctx, ownerUserId, runId, stepKey, workintegration.RemedyReplan,
			"This run was to be re-planned from the step it failed at, and it has no failed step to re-plan from.")
	}
	if strings.TrimSpace(rc.Statement) == "" {
		return r.ask(ctx, ownerUserId, runId, failedKey, workintegration.RemedyReplan,
			"This run's goal could not be read, so there is no plan to re-plan from its failed step.")
	}
	sandbox, ok := r.loop.engine.(authoringSandbox)
	if !ok || sandbox == nil {
		// Nothing was spent, and a planner that has Gate 1 can serve this:
		// parked, to be served again once the claim lapses. A draft that has
		// not passed Gate 1 must not be run.
		r.warn("work remedy: this node has no Gate 1 sandbox, so it cannot install a re-planned draft; the run stays parked", runId, fmt.Errorf("no sandbox"))
		return false
	}

	data := map[string]any{
		"statement":      rc.Statement,
		"completedSteps": rc.CompletedSteps,
		"failedStep":     rc.FailedStep,
		"now":            r.clock().UTC().Format(time.RFC3339),
	}
	if trimmed := strings.TrimSpace(reason); trimmed != "" {
		data["remainingGoal"] = trimmed
	}

	// replanGap declares @level("reasoning"), so the router resolves it at the
	// level the prompt asked for and the shipped reasoningParks rule decides
	// what happens when no door can serve it. Nothing here names a model. The
	// call belongs to the run -- integrations/work put it on the context -- so
	// it is journaled on the run and charged to its ceilings; it is made as
	// the planner's system actor, as every authoring prompt here is.
	out, err := r.loop.engine.InvokeAI(systemActorContext(ctx), "replanGap", data)
	if err != nil {
		if memql.IsProviderUnavailable(err) {
			// No door could serve the call, so nothing was spent, and a door
			// may open: parked, served again once the claim lapses.
			r.warn("work remedy: no model can serve the re-plan right now; the run stays parked", runId, err)
			return false
		}
		return r.ask(ctx, ownerUserId, runId, failedKey, workintegration.RemedyReplan,
			"The re-plan for this run's failed step could not be made: "+err.Error())
	}
	draft, err := parseReplanDraft(out)
	if err != nil {
		return r.ask(ctx, ownerUserId, runId, failedKey, workintegration.RemedyReplan,
			"The re-plan returned no plan this run can use: "+err.Error())
	}
	if draft.GoalAlreadyServed {
		// A model's judgment that the work is done does not close a run as
		// succeeded; a person who can see the completed steps does.
		return r.ask(ctx, ownerUserId, runId, failedKey, workintegration.RemedyReplan,
			"The re-plan found nothing left to do: it judged the completed steps already serve the goal. Retry the failed step, or abandon the run.")
	}
	auto, err := automations.NewLoader(automations.LoaderOptions{Logger: r.loop.logger}).CompileSource(draft.Source, "work-replan/"+runId+".memql")
	if err != nil {
		return r.ask(ctx, ownerUserId, runId, failedKey, workintegration.RemedyReplan,
			"The re-planned draft does not compile: "+err.Error())
	}
	resumeAt, stepKeys, err := replanKeepsPrefix(auto, rc)
	if err != nil {
		return r.ask(ctx, ownerUserId, runId, failedKey, workintegration.RemedyReplan,
			"The re-planned draft does not keep the completed steps where they would be served: "+err.Error())
	}
	persisted, err := r.loop.persistWorkDraft(ctx,
		CompileRequest{RunId: runId, OwnerUserId: ownerUserId, Statement: rc.Statement},
		CompileOutcome{},
		authoringBundle{AutomationName: auto.Name, Constructs: []memql.SandboxConstruct{{Kind: "automation", Name: auto.Name, Source: draft.Source}}},
		sandbox)
	if err != nil {
		return r.ask(ctx, ownerUserId, runId, failedKey, workintegration.RemedyReplan,
			"The re-planned draft could not be made this run's template: "+err.Error())
	}

	if err := r.writer.InstallReplan(ctx, ownerUserId, runId, workintegration.ReplanTemplate{
		AutomationName:      persisted.AutomationName,
		TemplateConstructId: persisted.ConstructId,
		TemplateFingerprint: persisted.TemplateFingerprint,
		TemplateVersion:     persisted.TemplateVersion,
		StepKeys:            stepKeys,
		ResumeAt:            resumeAt,
		Outcome: map[string]any{
			"replannedFrom":       failedKey,
			"replannedAt":         r.clock().UTC().Format(time.RFC3339),
			"prefixKept":          len(rc.CompletedSteps),
			"abandonedAssumption": draft.AbandonedAssumption,
		},
	}); err != nil {
		// The run moved on while the draft was made -- cancelled or decided --
		// or the write failed. Either way its current state stands; the
		// persisted draft is a validated bundle nothing runs.
		r.warn("work remedy: could not install the re-planned template on the run", runId, err)
		return false
	}
	if r.loop.logger != nil {
		r.loop.logger.Info("work remedy: re-planned the gap from a failed step and installed the new template, keeping the completed prefix",
			"run", runId, "step", failedKey, "prefixKept", len(rc.CompletedSteps),
			"automation", persisted.AutomationName, "construct", persisted.ConstructId)
	}
	return true
}

// Repair re-runs the failed step with the violation as guidance (spec section
// E): the completed prefix is served, the step runs as a new version, and the
// violation reaches its model calls as what was wrong with the previous
// version. integrations/work writes it as a re-run request -- the one a
// person's rerunStep writes -- so the agent serves it on that path.
//
// It is still mostly UNREACHABLE TODAY: the rules produce a contract symptom
// only from Signal.PostconditionFailed, which nothing in the automations
// executor evaluates, so a contract miss reaches here only when the
// classifier model names one. It is served properly for when it does.
func (r *WorkRemedy) Repair(ctx context.Context, runId, ownerUserId, stepKey, violation string) bool {
	if r == nil || r.writer == nil {
		return false
	}
	if err := r.writer.RequestRepair(ctx, ownerUserId, runId, stepKey, violation); err != nil {
		if errors.Is(err, workintegration.ErrRemedyNotWaiting) {
			r.warn("work remedy: the run moved on before its repair could be requested", runId, err)
			return false
		}
		// The step is not one this run can re-run, or the run has nothing to
		// run again. No model was asked and asking again changes nothing, so
		// a person decides.
		return r.ask(ctx, ownerUserId, runId, stepKey, workintegration.RemedyRepair,
			"The failed step could not be run again with the violation as guidance: "+err.Error())
	}
	if r.loop != nil && r.loop.logger != nil {
		r.loop.logger.Info("work remedy: re-running a failed step with its contract violation as guidance",
			"run", runId, "step", stepKey)
	}
	return true
}

// ask moves a run whose remedy could not be carried out onto a person's
// question, reporting whether the run left its remedy wait.
func (r *WorkRemedy) ask(ctx context.Context, ownerUserId, runId, stepKey, kind, reason string) bool {
	if err := r.writer.AskAboutFailedRemedy(ctx, ownerUserId, runId, stepKey, kind, reason); err != nil {
		r.warn("work remedy: could not ask a person about a remedy that failed; the run stays parked", runId, err)
		return false
	}
	r.warn("work remedy: the remedy could not be carried out, so the run asks a person", runId, errors.New(reason))
	return true
}

// replanDraft is what replanGap answers (dsl/work/prompts/replanGap.tmpl).
type replanDraft struct {
	Source              string `json:"source"`
	GoalAlreadyServed   bool   `json:"goalAlreadyServed"`
	AbandonedAssumption string `json:"abandonedAssumption"`
}

// parseReplanDraft reads replanGap's answer, tolerating a string (raw model
// text, fenced or wrapped in prose) and a map (schema-enforced), the way the
// authoring parsers do. A draft with no source and no "already served" verdict
// is no answer.
func parseReplanDraft(resp any) (replanDraft, error) {
	if resp == nil {
		return replanDraft{}, fmt.Errorf("replanGap returned nothing")
	}
	var raw []byte
	switch v := resp.(type) {
	case string:
		raw = []byte(v)
	default:
		b, err := json.Marshal(resp)
		if err != nil {
			return replanDraft{}, err
		}
		raw = b
	}
	var draft replanDraft
	if err := json.Unmarshal(extractJSONObject(raw), &draft); err != nil {
		return replanDraft{}, fmt.Errorf("parse replanGap JSON: %w (raw=%s)", err, truncate(string(raw), 200))
	}
	draft.Source = strings.TrimSpace(draft.Source)
	if draft.Source == "" && !draft.GoalAlreadyServed {
		return replanDraft{}, fmt.Errorf("replanGap returned no source")
	}
	return draft, nil
}

// replanKeepsPrefix checks a re-planned template against what the run
// recorded, and answers where the install's `replan` request starts it.
//
// The request is served as a re-run (component/automations' PrepareRerun): the
// steps before its target are served from their rows -- a finished one bound
// to what it recorded, a skipped one or a failure continued past left as it
// was -- and the target and every step after it run. So the target is the
// template's first step the run has not given a finished version, finished
// meaning what resume means by it: done, skipped, or a failure the new template
// continues past. Every completed step must be in the template and come before
// that target, or it runs a second time, its side effects with it; and there
// must be a step left to run.
func replanKeepsPrefix(auto *automations.Automation, rc workintegration.ReplanContext) (resumeAt string, stepKeys []string, err error) {
	if auto == nil {
		return "", nil, fmt.Errorf("no template")
	}
	at := map[string]int{}
	resume := -1
	for i, step := range auto.Steps {
		if step == nil {
			continue
		}
		at[step.ID] = i
		stepKeys = append(stepKeys, step.ID)
		if resume >= 0 {
			continue
		}
		switch status := rc.Recorded[step.ID]; {
		case status == "done", status == "skipped":
		case status == "failed" && step.OnError == automations.ErrorStrategyContinue:
		default:
			resume = i
		}
	}
	if resume < 0 {
		return "", nil, fmt.Errorf("every step of the template has already run, so there is nothing left to do")
	}
	for _, done := range rc.CompletedSteps {
		key, _ := done["key"].(string)
		i, ok := at[key]
		if !ok {
			return "", nil, fmt.Errorf("the completed step %q is not in the template", key)
		}
		if i > resume {
			return "", nil, fmt.Errorf("the completed step %q comes after the step the run would resume at (%q), so it would run again", key, auto.Steps[resume].ID)
		}
	}
	return auto.Steps[resume].ID, stepKeys, nil
}

func (r *WorkRemedy) clock() time.Time {
	if r == nil || r.now == nil {
		return time.Now()
	}
	return r.now()
}

func (r *WorkRemedy) warn(msg, runId string, err error) {
	if r == nil || r.loop == nil || r.loop.logger == nil {
		return
	}
	r.loop.logger.Warn(msg, "run", runId, "error", err)
}

// WorkRemedy builds the remedy off this integration's own loop, mirroring
// WorkCompiler. app/ asks the integration rather than reaching for the loop,
// which is unexported for the reason every seam here is: the loop's lifecycle
// is the integration's, and a caller holding it across a re-materialize would
// hold a loop wired to a dead engine.
func (p *PlannerIntegration) WorkRemedy(work *workintegration.Integration) *WorkRemedy {
	if p == nil || p.agentLoop == nil {
		return nil
	}
	return NewWorkRemedy(p.agentLoop, work)
}

// WorkHealer builds the healing subscriber off this integration's own loop.
// The proposer is supplied by the caller because it needs a structured-output
// provider, which the integration does not hold.
func (p *PlannerIntegration) WorkHealer(proposer patchProposer, raiser approvalRaiser, ttl time.Duration) *WorkHealer {
	if p == nil || p.agentLoop == nil {
		return nil
	}
	return NewWorkHealer(proposer, raiser, p.agentLoop, ttl)
}
