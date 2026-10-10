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
	PauseReplanForBudget(ctx context.Context, ownerUserId, runId, stepKey string, ceiling *memql.RunCeilingError) error
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
		r.warn("work remedy: could not read the run's replan context; the run stays parked", runId, err)
		return false
	}
	failedKey := strings.TrimSpace(stepKey)
	if failedKey == "" && rc.FailedStep != nil {
		failedKey, _ = rc.FailedStep["key"].(string)
	}
	s := &remedyScope{remedy: r, runID: runId, owner: ownerUserId, step: failedKey, reason: reason, kind: workintegration.RemedyReplan, context: rc}
	return s.run(ctx, "workSpineReplan")
}

// Repair keeps the completed prefix and binds guidance to one failed step.
func (r *WorkRemedy) Repair(ctx context.Context, runId, ownerUserId, stepKey, violation string) bool {
	if r == nil || r.writer == nil {
		return false
	}
	s := &remedyScope{remedy: r, runID: runId, owner: ownerUserId, step: stepKey, reason: violation, kind: workintegration.RemedyRepair}
	return s.run(ctx, "workSpineRepair")
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
	Source              string             `json:"source"`
	Edits               []replanSourceEdit `json:"edits"`
	GoalAlreadyServed   bool               `json:"goalAlreadyServed"`
	AbandonedAssumption string             `json:"abandonedAssumption"`
}

// The model emits only the executable draft and its verdict. The runtime
// derives step identities and dependencies from the validated source rather
// than asking for a second, potentially inconsistent plan description.
var replanDraftSchema = json.RawMessage(`{
 "type":"object","additionalProperties":false,
 "required":["source","goalAlreadyServed","abandonedAssumption"],
 "properties":{
  "source":{"type":"string"},
  "goalAlreadyServed":{"type":"boolean"},
  "abandonedAssumption":{"type":"string"}
 }
}`)

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
	if draft.Source == "" && len(draft.Edits) == 0 && !draft.GoalAlreadyServed {
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
