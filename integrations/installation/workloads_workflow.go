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

const installationWorkloadWorkflow = "installationWorkloadWorkflow"

// The native completion host must bind this installed recipe and engine
// revision into its journal plan. This scoped read alone cannot finish an update.
type workloadWorkflow struct {
	definition *automations.Automation
	digest     string
}

func loadWorkloadWorkflow(engineRevision string) (*workloadWorkflow, error) {
	definition, err := workflowhost.Load(installationWorkloadWorkflow)
	if err != nil {
		return nil, err
	}
	return newWorkloadWorkflow(definition, engineRevision)
}

func newWorkloadWorkflow(definition *automations.Automation, engineRevision string) (*workloadWorkflow, error) {
	if definition == nil || !definition.Trusted || !commitDigest.MatchString(engineRevision) {
		return nil, errors.New("installation workloads require an installed recipe and immutable engine revision")
	}
	owned, err := automations.NewLoader(automations.LoaderOptions{}).Snapshot(definition)
	if err != nil {
		return nil, err
	}
	digest := artifactHash("workflow", []string{"scoped-workload-readiness-v1", engineRevision, owned.DefinitionFingerprint(id.NewUntracked())})
	return &workloadWorkflow{owned, digest}, nil
}

// run never elevates origin, accepts proof arguments or manufactures scope.
// Recovery repeats these read-only checks in a fresh receiver-owned scope.
// No installation effect or durable completion is available through these ports.
func (w *workloadWorkflow) run(ctx context.Context, admission *workloadAdmission, expectedOperator string) (workloadEvidence, error) {
	operator, err := workloadWorkflowActor(ctx)
	if err != nil || operator != expectedOperator || admission == nil || operator != admission.operator {
		return workloadEvidence{}, errors.New("workload recipe requires its original admitted operator")
	}
	if w == nil || w.definition == nil || admission == nil {
		return workloadEvidence{}, errors.New("workload recipe has no native scope")
	}
	s := &workloadWorkflowScope{admission: admission, operator: operator, observations: map[string]workloadObservation{}}
	ctx, cancel := context.WithDeadline(ctx, admission.expires)
	defer cancel()
	_, err = workflowhost.Run(ctx, w.definition.Name, nil, workflowhost.Options{
		Operations: s.operations(),
		Load: func(name string) (*automations.Automation, error) {
			if name != w.definition.Name {
				return nil, errors.New("workload recipe cannot load an unbound child")
			}
			return w.definition, nil
		},
		LoadLogic: func(string) (*memql.Function, error) {
			return nil, errors.New("workload recipe cannot load unbound logic")
		},
	})
	if err != nil {
		return workloadEvidence{}, err
	}
	if err := ctx.Err(); err != nil {
		return workloadEvidence{}, err
	}
	if s.result == nil || !s.result.expires.After(time.Now()) {
		return workloadEvidence{}, errors.New("workload recipe did not complete fresh native evidence")
	}
	out := *s.result
	out.workflow, out.operator = w.digest, operator
	out.digest = artifactHash("workflow-evidence", []string{out.digest, w.digest, operator})
	return out, nil
}

func workloadWorkflowActor(ctx context.Context) (string, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return "", errors.New("workload recipe requires native installation admission")
	}
	return installationActor(ctx)
}

type workloadWorkflowScope struct {
	admission    *workloadAdmission
	operator     string
	observations map[string]workloadObservation
	result       *workloadEvidence
}

func (s *workloadWorkflowScope) operations() map[string]workflowhost.Operation {
	ops := map[string]workflowhost.Operation{
		"installationWorkloadRequirements": s.requirements,
		"installationWorkloadCheck":        s.check,
		"installationWorkloadsComplete":    s.complete,
	}
	for name, operation := range ops {
		ops[name] = func(ctx context.Context, args map[string]any) (any, error) {
			operator, err := workloadWorkflowActor(ctx)
			if err != nil || operator != s.operator || s.result != nil || !s.admission.expires.After(time.Now()) {
				return nil, errors.New("workload operation is outside its active admitted scope")
			}
			return operation(ctx, args)
		}
	}
	return ops
}

func (s *workloadWorkflowScope) requirements(_ context.Context, args map[string]any) (any, error) {
	if len(args) != 0 {
		return nil, errors.New("workload requirements accept no caller inputs")
	}
	items := make([]any, 0, len(s.admission.requirements))
	for _, want := range s.admission.requirements {
		// Resource paths, credentials and opaque proofs remain native.
		items = append(items, map[string]any{"key": want.Key, "kind": want.Kind})
	}
	return items, nil
}

func (s *workloadWorkflowScope) check(ctx context.Context, args map[string]any) (any, error) {
	key, ok := args["key"].(string)
	if !ok || len(args) != 1 || !internalDigest.MatchString(key) {
		return nil, errors.New("workload read requires one scoped obligation key")
	}
	if _, exists := s.observations[key]; exists {
		return nil, errors.New("workload obligation already has an observation in this recipe")
	}
	observation, err := s.admission.check(ctx, key)
	if err != nil {
		return nil, err
	}
	s.observations[key] = observation
	return nil, nil
}

func (s *workloadWorkflowScope) complete(_ context.Context, args map[string]any) (any, error) {
	if len(args) != 0 {
		return nil, errors.New("workload completion accepts no caller evidence")
	}
	observations := make([]workloadObservation, 0, len(s.observations))
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
