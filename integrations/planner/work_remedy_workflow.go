package planner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

type remedyScope struct {
	remedy                                    *WorkRemedy
	runID, owner, step, reason, kind          string
	context                                   workintegration.ReplanContext
	draft                                     replanDraft
	auto                                      *automations.Automation
	resumeAt                                  string
	stepKeys                                  []string
	persisted                                 CompileOutcome
	generated, validated, saved, closed, took bool
}

func (s *remedyScope) run(ctx context.Context, entry string) bool {
	run, _ := common.RunFromContext(ctx)
	_, err := workflowhost.RunPhase(ctx, run.Spine, work.SpineContract, entry, nil, workflowhost.Options{Operations: s.operations()})
	if err != nil {
		s.remedy.warn("work remedy: DSL recipe refused", s.runID, err)
	}
	if s.generated && !s.closed {
		// Once a model was spent, leaving the remedy wait live would spend it on
		// every lease. The native envelope closes that loophole even if the recipe
		// catches a failure or simply returns early.
		return s.remedy.ask(ctx, s.owner, s.runID, s.step, s.kind, "The remedy recipe stopped before it could install a validated result.")
	}
	return s.took
}

func (s *remedyScope) operations() map[string]workflowhost.Operation {
	return map[string]workflowhost.Operation{
		"spineRemedyContext": func(context.Context, map[string]any) (any, error) {
			_, sandbox := s.sandbox()
			return map[string]any{"hasFailedStep": s.context.FailedStep != nil && s.step != "", "hasGoal": strings.TrimSpace(s.context.Statement) != "", "sandbox": sandbox}, nil
		},
		"spineRemedyGenerate": s.generate,
		"spineRemedyValidate": s.validate,
		"spineRemedyPersist":  s.persist,
		"spineRemedyInstall":  s.install,
		"spineRemedyRepair":   s.repair,
		"spineRemedyAsk": func(ctx context.Context, args map[string]any) (any, error) {
			if s.closed {
				return nil, fmt.Errorf("remedy already closed")
			}
			reason := strings.TrimSpace(getString(args, "reason"))
			if reason == "" {
				return nil, fmt.Errorf("remedy approval requires a reason")
			}
			s.closed = true
			s.took = s.remedy.ask(ctx, s.owner, s.runID, s.step, s.kind, reason)
			return s.took, nil
		},
	}
}

func (s *remedyScope) sandbox() (authoringSandbox, bool) {
	if s.remedy.loop == nil {
		return nil, false
	}
	sandbox, ok := s.remedy.loop.engine.(authoringSandbox)
	return sandbox, ok && sandbox != nil
}

func (s *remedyScope) generate(ctx context.Context, args map[string]any) (any, error) {
	_, sandbox := s.sandbox()
	if s.kind != workintegration.RemedyReplan || s.generated || s.closed || !sandbox || s.context.FailedStep == nil || s.step == "" || strings.TrimSpace(s.context.Statement) == "" {
		return nil, fmt.Errorf("replan generation requires a readable goal, prefix, failed step and Gate 1, and may run once")
	}
	prompt := strings.TrimSpace(getString(args, "prompt"))
	if prompt == "" {
		return nil, fmt.Errorf("replan requires a named prompt")
	}
	rc := s.context
	data := map[string]any{"statement": rc.Statement, "completedSteps": rc.CompletedSteps, "failedStep": rc.FailedStep, "inputKeys": inputKeys(rc.Variables), "now": s.remedy.clock().UTC().Format(time.RFC3339)}
	source := make([]map[string]any, 0, len(rc.Template))
	for _, construct := range rc.Template {
		source = append(source, map[string]any{"kind": construct.Kind, "name": construct.Name, "source": construct.Source})
	}
	data["originalTemplate"] = source
	data["templateName"] = rc.TemplateName
	if strings.TrimSpace(s.reason) != "" {
		data["remainingGoal"] = strings.TrimSpace(s.reason)
	}
	s.generated = true
	out, err := s.remedy.loop.engine.InvokeAI(systemActorContext(ctx), prompt, data)
	if err != nil {
		if memql.IsProviderUnavailable(err) {
			s.closed = true
			return map[string]any{"available": false, "ok": false, "message": err.Error()}, nil
		}
		return map[string]any{"available": true, "ok": false, "message": err.Error()}, nil
	}
	draft, err := parseReplanDraft(out)
	if err != nil {
		return map[string]any{"available": true, "ok": false, "message": err.Error()}, nil
	}
	s.draft = draft
	return map[string]any{"available": true, "ok": true, "goalAlreadyServed": draft.GoalAlreadyServed}, nil
}

func (s *remedyScope) validate(_ context.Context, _ map[string]any) (any, error) {
	if !s.generated || s.validated || s.closed || s.draft.Source == "" || s.draft.GoalAlreadyServed {
		return nil, fmt.Errorf("replan validation requires one generated unfinished plan")
	}
	s.validated = true
	auto, err := automations.NewLoader(automations.LoaderOptions{Logger: s.remedy.loop.logger}).CompileSource(s.draft.Source, "work-replan/"+s.runID+".memql")
	fail := func(stage string, err error) (any, error) {
		return map[string]any{"ok": false, "stage": stage, "message": err.Error()}, nil
	}
	if err != nil {
		return fail("source", err)
	}
	resumeAt, keys, err := replanKeepsPrefix(auto, s.context)
	if err != nil {
		return fail("prefix", err)
	}
	if err := automations.CheckArgs(auto, s.context.Variables); err != nil {
		return fail("args", err)
	}
	s.auto, s.resumeAt, s.stepKeys = auto, resumeAt, keys
	return map[string]any{"ok": true}, nil
}

func (s *remedyScope) persist(ctx context.Context, _ map[string]any) (any, error) {
	sandbox, ok := s.sandbox()
	if s.auto == nil || !s.validated || s.saved || s.closed || !ok {
		return nil, fmt.Errorf("replan persistence requires a validated prefix and may run once")
	}
	s.saved = true
	constructs := []memql.SandboxConstruct{{Kind: "automation", Name: s.auto.Name, Source: s.draft.Source}}
	// The replacement may still call private helpers, especially in the fixed
	// prefix. Carry the sealed dependencies to the new bundle so the executing
	// replica has them too. Only the headline may be replaced by the model.
	for _, construct := range s.context.Template {
		if construct.Kind == "automation" && construct.Name == s.context.TemplateName {
			continue
		}
		constructs = append(constructs, construct)
	}
	out, err := s.remedy.loop.persistWorkDraft(ctx, CompileRequest{RunId: s.runID, OwnerUserId: s.owner, Statement: s.context.Statement}, CompileOutcome{}, authoringBundle{AutomationName: s.auto.Name, Constructs: constructs}, sandbox)
	if err != nil {
		return map[string]any{"ok": false, "message": err.Error()}, nil
	}
	s.persisted = out
	return map[string]any{"ok": true}, nil
}

func (s *remedyScope) install(ctx context.Context, _ map[string]any) (any, error) {
	if s.closed || s.persisted.ConstructId == "" {
		return nil, fmt.Errorf("replan installation requires a persisted validated template")
	}
	s.closed = true
	out := s.persisted
	err := s.remedy.writer.InstallReplan(ctx, s.owner, s.runID, workintegration.ReplanTemplate{AutomationName: out.AutomationName, TemplateConstructId: out.ConstructId, TemplateFingerprint: out.TemplateFingerprint, TemplateVersion: out.TemplateVersion, StepKeys: s.stepKeys, ResumeAt: s.resumeAt,
		Outcome: map[string]any{"replannedFrom": s.step, "replannedAt": s.remedy.clock().UTC().Format(time.RFC3339), "prefixKept": len(s.context.CompletedSteps), "abandonedAssumption": s.draft.AbandonedAssumption}})
	if err != nil {
		s.remedy.warn("work remedy: could not install the re-planned template on the run", s.runID, err)
		return false, nil
	}
	s.took = true
	return true, nil
}

func (s *remedyScope) repair(ctx context.Context, _ map[string]any) (any, error) {
	if s.kind != workintegration.RemedyRepair || s.closed || s.saved {
		return nil, fmt.Errorf("repair may be requested once")
	}
	s.saved = true
	err := s.remedy.writer.RequestRepair(ctx, s.owner, s.runID, s.step, s.reason)
	if err != nil {
		moved := errors.Is(err, workintegration.ErrRemedyNotWaiting)
		if moved {
			s.closed = true
		}
		return map[string]any{"ok": false, "moved": moved, "message": err.Error()}, nil
	}
	s.closed, s.took = true, true
	return map[string]any{"ok": true}, nil
}
