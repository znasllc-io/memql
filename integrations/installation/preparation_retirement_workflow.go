package installation

import (
	"context"
	"errors"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

const installationRetirementWorkflow = "installationRetirementWorkflow"

type retirementWorkflow struct {
	definition *automations.Automation
	digest     string
}

func loadRetirementWorkflow(engineRevision string) (*retirementWorkflow, error) {
	definition, err := workflowhost.Load(installationRetirementWorkflow)
	if err != nil {
		return nil, err
	}
	return newRetirementWorkflow(definition, engineRevision)
}

func newRetirementWorkflow(definition *automations.Automation, engineRevision string) (*retirementWorkflow, error) {
	if definition == nil || !definition.Trusted || !commitDigest.MatchString(engineRevision) {
		return nil, errors.New("cleanup requires an installed recipe and immutable engine revision")
	}
	owned, err := automations.NewLoader(automations.LoaderOptions{}).Snapshot(definition)
	if err != nil {
		return nil, err
	}
	digest := "memql-id:" + string(id.NewUntracked().FromString("installation-retirement-native-v1:"+engineRevision+":"+owned.DefinitionFingerprint(id.NewUntracked())))
	return &retirementWorkflow{owned, digest}, nil
}

// run executes one bounded reconciliation pass. A nonterminal returned native
// record means more pages remain; it never claims completed cleanup. The caller
// owns scheduling another pass. Recovery reloads the journal on a fresh host,
// without relying on a workflow return value or the first process's memory.
func (w *retirementWorkflow) run(ctx context.Context, journal *preparationJournal, installation, key string, executor sourceCaptureExecutor, files preparationRetirementFiles) (preparationRecord, error) {
	if w == nil || w.definition == nil || journal == nil || executor == nil || files == nil {
		return preparationRecord{}, errors.New("cleanup has no complete native scope")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	operator, err := preparationActor(ctx)
	if err != nil {
		return preparationRecord{}, err
	}
	r, err := journal.get(ctx, installation, key)
	if err != nil {
		return preparationRecord{}, err
	}
	if operator != r.Scope.RequestedBy {
		return preparationRecord{}, errors.New("cleanup requires the original admitted operator")
	}
	if r.State == "cancelled" {
		return r, nil
	} // Read-only historical recovery.
	if r.State != "preparing" && r.State != "retiring" {
		return preparationRecord{}, errors.New("preparation is unavailable for cleanup")
	}
	if r.Retirement != nil && r.Retirement.WorkflowDigest != w.digest {
		return preparationRecord{}, errors.New("cleanup recipe changed since retirement began")
	}
	s := &retirementWorkflowScope{journal: journal, installation: installation, key: key, workflow: r.Scope.WorkflowDigest, cleanup: w.digest, operator: operator, executor: executor, files: files}
	_, err = workflowhost.Run(ctx, w.definition.Name, nil, workflowhost.Options{Operations: s.operations(),
		Load: func(name string) (*automations.Automation, error) {
			if name != w.definition.Name {
				return nil, errors.New("cleanup cannot load an unbound child")
			}
			return w.definition, nil
		},
		LoadLogic: func(string) (*memql.Function, error) { return nil, errors.New("cleanup cannot load unbound logic") },
	})
	if err != nil {
		return preparationRecord{}, err
	}
	if err := ctx.Err(); err != nil {
		return preparationRecord{}, err
	}
	if !s.observed && !s.completed {
		return preparationRecord{}, errors.New("cleanup recipe returned no native progress observation")
	}
	// Even an ignored operation failure or forged DSL return cannot manufacture
	// completion: read the current durable state and its receipt invariants.
	return journal.get(ctx, installation, key)
}

type retirementWorkflowScope struct {
	journal                                        *preparationJournal
	installation, key, workflow, cleanup, operator string
	executor                                       sourceCaptureExecutor
	files                                          preparationRetirementFiles
	observed, completed                            bool
}

func retirementRole(args map[string]any) (string, error) {
	role, ok := args["role"].(string)
	if !ok || len(args) != 1 || (role != "candidate" && role != "rollback") {
		return "", errors.New("cleanup requires one scoped source role")
	}
	return role, nil
}

func (s *retirementWorkflowScope) operations() map[string]workflowhost.Operation {
	noArgs := func(f func(context.Context) (preparationRecord, error)) workflowhost.Operation {
		return func(ctx context.Context, args map[string]any) (any, error) {
			if len(args) != 0 {
				return nil, errors.New("cleanup operation accepts no supplied evidence")
			}
			_, err := f(ctx)
			return nil, err
		}
	}
	roleEffect := func(f func(context.Context, string, string, string, string, string, preparationRetirementFiles) (preparationRecord, error)) workflowhost.Operation {
		return func(ctx context.Context, args map[string]any) (any, error) {
			role, err := retirementRole(args)
			if err != nil {
				return nil, err
			}
			_, err = f(ctx, s.installation, s.key, s.workflow, s.cleanup, role, s.files)
			return nil, err
		}
	}
	ops := map[string]workflowhost.Operation{
		"installationRetirementBegin": noArgs(func(ctx context.Context) (preparationRecord, error) {
			return s.journal.beginRetirement(ctx, s.installation, s.key, s.workflow, s.cleanup)
		}),
		"installationRetirementStop": noArgs(func(ctx context.Context) (preparationRecord, error) {
			return s.journal.stopRetiringProducer(ctx, s.installation, s.key, s.workflow, s.cleanup, s.executor)
		}),
		"installationRetirementFence":    roleEffect(s.journal.fenceRetiringCapture),
		"installationRetirementRelease":  roleEffect(s.journal.releaseRetiringCapturePins),
		"installationRetirementPage":     s.page,
		"installationRetirementArtifact": s.artifact,
		"installationRetirementObserve":  s.observe,
		"installationRetirementComplete": noArgs(func(ctx context.Context) (preparationRecord, error) {
			r, err := s.journal.finishRetirement(ctx, s.installation, s.key, s.workflow, s.cleanup)
			if err == nil {
				s.completed = true
			}
			return r, err
		}),
	}
	for name, operation := range ops {
		ops[name] = func(ctx context.Context, args map[string]any) (any, error) {
			operator, err := preparationActor(ctx)
			if err != nil || operator != s.operator || s.completed {
				return nil, errors.New("cleanup operation is outside its active native scope")
			}
			return operation(ctx, args)
		}
	}
	return ops
}

func (s *retirementWorkflowScope) page(ctx context.Context, args map[string]any) (any, error) {
	role, err := retirementRole(args)
	if err != nil {
		return nil, err
	}
	r, err := s.journal.nextRetiringArtifacts(ctx, s.installation, s.key, s.workflow, s.cleanup, role, s.files)
	if err != nil {
		return nil, err
	}
	c := r.Retirement.Captures[role]
	items := make([]any, 0, len(c.Page))
	for _, artifact := range c.Page {
		if artifact.ReceiptDigest == "" {
			items = append(items, map[string]any{"intentId": artifact.IntentID})
		}
	}
	return map[string]any{"items": items, "complete": c.Complete}, nil
}

func (s *retirementWorkflowScope) artifact(ctx context.Context, args map[string]any) (any, error) {
	role, ok := args["role"].(string)
	intent, okID := args["intentId"].(string)
	if !ok || !okID || len(args) != 2 {
		return nil, errors.New("artifact cleanup requires its role and scoped intent identity")
	}
	_, err := s.journal.retireCaptureArtifact(ctx, s.installation, s.key, s.workflow, s.cleanup, role, intent, s.files)
	return nil, err
}

func (s *retirementWorkflowScope) observe(ctx context.Context, args map[string]any) (any, error) {
	if len(args) != 0 {
		return nil, errors.New("cleanup observation accepts no supplied evidence")
	}
	r, err := s.journal.retirementRecord(ctx, s.installation, s.key, s.workflow, s.cleanup)
	if err != nil {
		return nil, err
	}
	ready := r.Retirement.ProducerStopped
	for _, c := range r.Retirement.Captures {
		ready = ready && c.Fenced && c.PinsReleased && c.Complete
	}
	s.observed = true
	return map[string]any{"ready": ready}, nil
}
