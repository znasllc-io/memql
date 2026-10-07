package installation

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/argocd"
)

const installationRevisionWorkflow = "installationRevisionWorkflow"

type revisionMode uint8

const (
	revisionStart revisionMode = iota + 1
	revisionObserve
)

type revisionWorkflow struct {
	definition *automations.Automation
	digest     string
}

func loadRevisionWorkflow(engineRevision string) (*revisionWorkflow, error) {
	definition, err := workflowhost.Load(installationRevisionWorkflow)
	if err != nil {
		return nil, err
	}
	return newRevisionWorkflow(definition, engineRevision)
}

func newRevisionWorkflow(definition *automations.Automation, engineRevision string) (*revisionWorkflow, error) {
	if definition == nil || !definition.Trusted || !commitDigest.MatchString(engineRevision) {
		return nil, errors.New("installation execution requires an installed recipe and immutable engine revision")
	}
	owned, err := automations.NewLoader(automations.LoaderOptions{}).Snapshot(definition)
	if err != nil {
		return nil, err
	}
	digest := "memql-id:" + string(id.NewUntracked().FromString("installation-execution-native-v1:"+engineRevision+":"+owned.DefinitionFingerprint(id.NewUntracked())))
	return &revisionWorkflow{definition: owned, digest: digest}, nil
}

// run performs one bounded pass. Start is a native entry choice requiring the
// original operator and execution recipe; an observation pass grants no write.
// In particular, a replacement engine with a different recipe may record fresh
// Argo facts but cannot inherit the old engine's permission to apply an intent.
// Facts never complete an installation or release its slot: continuity and
// rollback qualification remain separate native operations.
func (w *revisionWorkflow) run(ctx context.Context, journal *revisionJournal, api argocd.API, installation, key string, mode revisionMode, configuration *receiverSnapshot) (revisionRecord, error) {
	actor, err := preparationActor(ctx)
	if err != nil {
		return revisionRecord{}, err
	}
	if w == nil || w.definition == nil || journal == nil || api == nil || (mode != revisionStart && mode != revisionObserve) {
		return revisionRecord{}, errors.New("installation execution has no complete native scope")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	r, err := journal.get(ctx, installation, key)
	if err != nil {
		return revisionRecord{}, err
	}
	if r.Plan.Preparation == nil || (r.State != "prepared" && r.State != "applying") {
		return revisionRecord{}, errors.New("installation execution requires an admitted active preparation")
	}
	if mode == revisionStart && (r.Plan.RequestedBy != actor || r.Plan.ExecutionWorkflowDigest != w.digest) {
		return revisionRecord{}, errors.New("installation execution authority or installed recipe changed")
	}
	client, err := argocd.New(api)
	if err != nil {
		return revisionRecord{}, err
	}
	s := &revisionWorkflowScope{workflow: w, journal: journal, client: client, installation: installation, key: key, operator: actor, mode: mode, configuration: configuration}
	_, err = workflowhost.Run(ctx, w.definition.Name, nil, workflowhost.Options{Operations: s.operations(),
		Load: func(name string) (*automations.Automation, error) {
			if name != w.definition.Name {
				return nil, errors.New("installation execution cannot load an unbound child")
			}
			return w.definition, nil
		},
		LoadLogic: func(string) (*memql.Function, error) {
			return nil, errors.New("installation execution cannot load unbound logic")
		},
	})
	if err != nil {
		return revisionRecord{}, err
	}
	if err := ctx.Err(); err != nil {
		return revisionRecord{}, err
	}
	if !s.observed.Load() {
		return revisionRecord{}, errors.New("installation execution returned no native observation")
	}
	return journal.get(ctx, installation, key)
}

// Call-local state is accessed by workflowhost's serialized native callbacks.
type revisionWorkflowScope struct {
	workflow          *revisionWorkflow
	journal           *revisionJournal
	client            *argocd.Client
	installation, key string
	operator          string
	mode              revisionMode
	configuration     *receiverSnapshot
	observed          atomic.Bool
}

func (s *revisionWorkflowScope) operations() map[string]workflowhost.Operation {
	ops := map[string]workflowhost.Operation{
		"installationRevisionRead": func(ctx context.Context, _ map[string]any) (any, error) {
			r, err := s.journal.get(ctx, s.installation, s.key)
			if err != nil {
				return nil, err
			}
			return map[string]any{"mayApply": s.mode == revisionStart, "state": r.State}, nil
		},
		"installationRevisionBegin":   s.begin,
		"installationRevisionApply":   s.apply,
		"installationRevisionObserve": s.observe,
	}
	for name, operation := range ops {
		ops[name] = func(ctx context.Context, args map[string]any) (any, error) {
			actor, err := preparationActor(ctx)
			if err != nil || actor != s.operator || len(args) != 0 {
				return nil, errors.New("installation execution operation is outside its native scope")
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return operation(ctx, args)
		}
	}
	return ops
}

func (s *revisionWorkflowScope) writable(ctx context.Context) (revisionRecord, error) {
	r, err := s.journal.get(ctx, s.installation, s.key)
	if err != nil {
		return revisionRecord{}, err
	}
	if s.mode != revisionStart || r.Plan.Preparation == nil || r.Plan.RequestedBy != s.operator || r.Plan.ExecutionWorkflowDigest != s.workflow.digest {
		return revisionRecord{}, errors.New("installation observation does not grant revision write authority")
	}
	return r, nil
}

func (s *revisionWorkflowScope) begin(ctx context.Context, _ map[string]any) (any, error) {
	r, err := s.writable(ctx)
	if err != nil {
		return nil, err
	}
	if r.State == "prepared" {
		if err := s.freshWriteEvidence(r); err != nil {
			return nil, err
		}
	}
	_, err = s.journal.begin(ctx, s.installation, s.key, s.workflow.digest)
	return nil, err
}

func (s *revisionWorkflowScope) freshWriteEvidence(r revisionRecord) error {
	cfg, binding := s.configuration, r.Plan.Preparation
	if binding == nil || cfg == nil || cfg.digest != binding.ConfigurationDigest || cfg.observed.IsZero() || cfg.observed.After(time.Now()) || time.Since(cfg.observed) > time.Minute {
		return errors.New("installation write requires freshly authenticated receiving configuration")
	}
	expires, err := time.Parse(time.RFC3339Nano, binding.ArtifactExpiresAt)
	if err != nil || !expires.After(time.Now()) {
		return errors.New("installation write requires unexpired native artifact evidence")
	}
	return nil
}

func (s *revisionWorkflowScope) apply(ctx context.Context, _ map[string]any) (any, error) {
	r, err := s.writable(ctx)
	if err != nil {
		return nil, err
	}
	if r.State != "applying" {
		return nil, errors.New("installation effect must be durably started before an Argo write")
	}
	// A started journal row precedes the external request and does not prove
	// it was sent. Recover an already-applied intent with a read first. Any
	// possible new write still needs fresh configuration and artifact evidence,
	// even after a crash between begin and the first request.
	facts, err := s.client.Observe(ctx, r.Plan.Intent)
	if err != nil {
		if !errors.Is(err, argocd.ErrChanged) {
			return nil, errors.New("installation revision cannot be reconciled before applying")
		}
		if err := s.freshWriteEvidence(r); err != nil {
			return nil, err
		}
		facts, err = s.client.Apply(ctx, r.Plan.Intent)
		if err != nil {
			return nil, errors.New("installation revision effect is unconfirmed; reconcile its recorded intent")
		}
	}
	_, err = s.journal.observe(ctx, s.installation, s.key, r.ObservationVersion, facts)
	return nil, err
}

func (s *revisionWorkflowScope) observe(ctx context.Context, _ map[string]any) (any, error) {
	r, err := s.journal.get(ctx, s.installation, s.key)
	if err != nil {
		return nil, err
	}
	if r.State != "applying" {
		return nil, errors.New("installation observation requires a started intent")
	}
	facts, err := s.client.Observe(ctx, r.Plan.Intent)
	if err != nil {
		return nil, errors.New("installation revision cannot be confirmed from the receiving controller")
	}
	if _, err := s.journal.observe(ctx, s.installation, s.key, r.ObservationVersion, facts); err != nil {
		return nil, err
	}
	s.observed.Store(true)
	return map[string]any{"operationSucceeded": facts.OperationSucceeded, "healthy": facts.Healthy, "synced": facts.Synced}, nil
}
