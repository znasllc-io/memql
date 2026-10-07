package installation

import (
	"context"
	"errors"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

const installationArtifactWorkflow = "installationArtifactWorkflow"

// This immutable recipe is private preparation input, never a caller-supplied
// definition. The full native preparer must bind digest into its durable
// workflow identity BEFORE dispatch and authenticate the engine revision.
type artifactWorkflow struct {
	definition *automations.Automation
	digest     string
}

func loadArtifactWorkflow(engineRevision string) (*artifactWorkflow, error) {
	definition, err := workflowhost.Load(installationArtifactWorkflow)
	if err != nil {
		return nil, err
	}
	return newArtifactWorkflow(definition, engineRevision)
}

func newArtifactWorkflow(definition *automations.Automation, engineRevision string) (*artifactWorkflow, error) {
	if definition == nil || !definition.Trusted || !commitDigest.MatchString(engineRevision) {
		return nil, errors.New("installation artifacts require an installed recipe and immutable engine revision")
	}
	owned, err := automations.NewLoader(automations.LoaderOptions{}).Snapshot(definition)
	if err != nil {
		return nil, err
	}
	digest := artifactHash("workflow", []string{"scoped-artifact-reads-v1", engineRevision, owned.DefinitionFingerprint(id.NewUntracked())})
	return &artifactWorkflow{owned, digest}, nil
}

// run never elevates origin, accepts proof arguments or manufactures scope.
// Recovery repeats these read-only checks in a fresh receiver-owned scope.
// No installation effect or registry retention is available through these ports.
func (w *artifactWorkflow) run(ctx context.Context, admission *artifactAdmission, expectedOperator string) (artifactEvidence, error) {
	operator, err := artifactWorkflowActor(ctx)
	if err != nil || operator != expectedOperator {
		return artifactEvidence{}, errors.New("artifact recipe requires its original admitted operator")
	}
	if w == nil || w.definition == nil || admission == nil {
		return artifactEvidence{}, errors.New("artifact recipe has no native scope")
	}
	s := &artifactWorkflowScope{admission: admission, operator: operator, observations: map[string]artifactObservation{}}
	ctx, cancel := context.WithDeadline(ctx, admission.created.Add(time.Hour))
	defer cancel()
	_, err = workflowhost.Run(ctx, w.definition.Name, nil, workflowhost.Options{
		Operations: s.operations(),
		Load: func(name string) (*automations.Automation, error) {
			if name != w.definition.Name {
				return nil, errors.New("artifact recipe cannot load an unbound child")
			}
			return w.definition, nil
		},
		LoadLogic: func(string) (*memql.Function, error) {
			return nil, errors.New("artifact recipe cannot load unbound logic")
		},
	})
	if err != nil {
		return artifactEvidence{}, err
	}
	if err := ctx.Err(); err != nil {
		return artifactEvidence{}, err
	}
	if s.result == nil || !s.result.expires.After(time.Now()) {
		return artifactEvidence{}, errors.New("artifact recipe did not complete fresh native evidence")
	}
	out := *s.result
	out.workflow, out.operator = w.digest, operator
	out.digest = artifactHash("workflow-evidence", []string{out.digest, w.digest, operator})
	return out, nil
}

func artifactWorkflowActor(ctx context.Context) (string, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return "", errors.New("artifact recipe requires native installation admission")
	}
	return installationActor(ctx)
}

type artifactWorkflowScope struct {
	admission    *artifactAdmission
	operator     string
	observations map[string]artifactObservation
	result       *artifactEvidence
}

func (s *artifactWorkflowScope) operations() map[string]workflowhost.Operation {
	ops := map[string]workflowhost.Operation{
		"installationArtifactRequirements": s.requirements,
		"installationArtifactCheck":        s.check,
		"installationArtifactsComplete":    s.complete,
	}
	for name, operation := range ops {
		ops[name] = func(ctx context.Context, args map[string]any) (any, error) {
			operator, err := artifactWorkflowActor(ctx)
			if err != nil || operator != s.operator || s.result != nil {
				return nil, errors.New("artifact operation is outside its active admitted scope")
			}
			return operation(ctx, args)
		}
	}
	return ops
}

func (s *artifactWorkflowScope) requirements(_ context.Context, args map[string]any) (any, error) {
	if len(args) != 0 {
		return nil, errors.New("artifact requirements accept no caller inputs")
	}
	items := make([]any, 0, len(s.admission.requirements))
	for _, want := range s.admission.requirementsForWorkflow() {
		// Registry routes, credentials and opaque proofs remain native.
		items = append(items, map[string]any{"key": want.Key, "platform": want.Platform})
	}
	return items, nil
}

func (s *artifactWorkflowScope) check(ctx context.Context, args map[string]any) (any, error) {
	key, ok := args["key"].(string)
	if !ok || len(args) != 1 || !internalDigest.MatchString(key) {
		return nil, errors.New("artifact read requires one scoped obligation key")
	}
	if _, exists := s.observations[key]; exists {
		return nil, errors.New("artifact obligation already has an observation in this recipe")
	}
	observation, err := s.admission.check(ctx, key)
	if err != nil {
		return nil, err
	}
	s.observations[key] = observation
	return nil, nil
}

func (s *artifactWorkflowScope) complete(_ context.Context, args map[string]any) (any, error) {
	if len(args) != 0 {
		return nil, errors.New("artifact completion accepts no caller evidence")
	}
	observations := make([]artifactObservation, 0, len(s.observations))
	for _, observation := range s.observations {
		observations = append(observations, observation)
	}
	evidence, err := s.admission.seal(observations)
	if err != nil {
		return nil, err
	}
	s.result = &evidence
	return nil, nil
}
