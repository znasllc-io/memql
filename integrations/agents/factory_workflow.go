package agents

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
)

// The scope binds catalogs, owner and the validated model decision. These are
// never accepted back from workflow arguments as evidence of authorization.
type factoryScope struct {
	i           *Integration
	owner, goal string
	run         factoryRun
	existing    []agentSnapshot
	roles       []roleSnapshot
	skills      []skillSnapshot
	decision    factoryDecision
	result      agentSnapshot
	attempts    int
	lastErr     error
	validated   bool
}

func (s *factoryScope) operations() map[string]workflowhost.Operation {
	return map[string]workflowhost.Operation{
		"agentFactoryReadAgents": func(ctx context.Context, _ map[string]any) (any, error) {
			s.existing = s.i.loadExistingAgents(ctx, s.owner)
			return nil, nil
		},
		"agentFactoryReadRoles": func(ctx context.Context, _ map[string]any) (any, error) {
			s.roles = s.i.loadRoleCatalog(ctx)
			return nil, nil
		},
		"agentFactoryReadSkills": func(ctx context.Context, _ map[string]any) (any, error) {
			s.skills = s.i.loadSkillCatalog(ctx)
			return nil, nil
		},
		"agentFactoryAnalyzeOnce": func(ctx context.Context, a map[string]any) (any, error) {
			if s.attempts >= maxFactoryAnalyzeAttempts {
				return nil, fmt.Errorf("factory model-call ceiling reached")
			}
			s.attempts++
			prior := ""
			if s.lastErr != nil {
				prior = s.lastErr.Error()
			}
			decision, err := s.i.analyzeGoal(ctx, s.goal, s.existing, s.roles, s.skills, prior, asString(a["prompt"]), asString(a["schema"]))
			if err == nil {
				err = validateFactoryDecision(decision, s.existing, s.roles)
			}
			s.lastErr = err
			s.validated = err == nil
			if err != nil {
				if !correctable(err) {
					return nil, fmt.Errorf("ensureForGoal: analyze: %w", err)
				}
				return map[string]any{"valid": false, "action": ""}, nil
			}
			s.decision = decision
			return map[string]any{"valid": true, "action": decision.Action}, nil
		},
		"agentFactoryRejectDecision": func(context.Context, map[string]any) (any, error) {
			return nil, fmt.Errorf("agentFactoryAnalyze did not produce an applicable decision in %d attempts; last rejection: %w", s.attempts, s.lastErr)
		},
		"agentFactoryMatch": func(context.Context, map[string]any) (any, error) {
			if !s.validated {
				return nil, fmt.Errorf("factory requires a validated decision")
			}
			match, ok := findById(s.existing, s.decision.TargetAgentId)
			if !ok {
				return nil, fmt.Errorf("factory target is outside the owner's agents")
			}
			s.result = match
			return nil, nil
		},
		"agentFactoryExtend": func(ctx context.Context, _ map[string]any) (any, error) {
			if !s.validated {
				return nil, fmt.Errorf("factory requires a validated decision")
			}
			result, err := s.i.extendAgent(ctx, s.owner, s.existing, s.decision, s.run)
			s.result = result
			return nil, err
		},
		"agentFactoryCreate": func(ctx context.Context, _ map[string]any) (any, error) {
			if !s.validated {
				return nil, fmt.Errorf("factory requires a validated decision")
			}
			result, err := s.i.createAgent(ctx, s.owner, s.decision, s.roles, s.run)
			s.result = result
			return nil, err
		},
	}
}
