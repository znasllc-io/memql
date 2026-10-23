package installation

import (
	"context"
	"errors"
	"time"

	"github.com/znasllc-io/memql/integrations/argocd"
)

// readSelectedReceiver authenticates every member at its original baseline or
// exact durably started intent. Own progress may change; foreign changes cannot
// be hidden by excluding Application specs from the configuration invariant.
func readSelectedReceiver(ctx context.Context, h *preparationHost, r revisionRecord) (*receiverSnapshot, error) {
	client, err := argocd.New(h.api)
	if err != nil {
		return nil, err
	}
	active := map[string]argocd.Intent{}
	for _, app := range r.Plan.applicationMembers() {
		started := r.State == "applying" || r.State == "reverting" || r.State == "complete" || r.State == "rolled_back"
		if r.Plan.FormatVersion == 2 {
			started = r.Applications[app.Key].Started
		}
		intent := app.Intent
		if r.Rollback != nil {
			if reverse, ok := r.Rollback.application(app.Key); ok && reverse.Started {
				if reverseFacts, e := client.Observe(ctx, reverse.Intent); e == nil {
					if !reverseFacts.IntentObserved {
						return nil, errors.New("rollback intent is unconfirmed")
					}
					active[app.Key] = reverse.Intent
					continue
				} else if !errors.Is(e, argocd.ErrChanged) {
					return nil, e
				}
			}
		}
		facts, observeErr := client.Observe(ctx, intent)
		if observeErr == nil {
			if !started || !facts.IntentObserved {
				return nil, errors.New("application intent has no durable native start")
			}
			active[app.Key] = intent
		} else if !errors.Is(observeErr, argocd.ErrChanged) {
			return nil, errors.New("application intent cannot be reconciled")
		}
	}
	snapshot, err := readReceiverGroupInputs(ctx, h.api, h.namespace, h.configurationName, h.catalog, active)
	if err != nil {
		return nil, err
	}
	if snapshot.configuration.InstallationID != r.Plan.InstallationID || r.Plan.Preparation == nil || snapshot.invariantDigest != r.Plan.Preparation.ConfigurationInvariantDigest || len(snapshot.applicationMembers()) != len(r.Plan.applicationMembers()) {
		return nil, errors.New("installation configuration or membership changed")
	}
	for _, app := range r.Plan.applicationMembers() {
		observed, err := snapshot.applicationByKey(app.Key)
		if err != nil {
			return nil, err
		}
		if observed.target != app.Intent.Target {
			return nil, errors.New("application identity changed")
		}
		if _, owned := active[app.Key]; !owned {
			expected, err := argocd.PlanRevision(observed.application, app.Intent.RequestID, app.Intent.Revision, app.Intent.Prune)
			if err != nil || !sameIntent(expected, app.Intent) {
				return nil, errors.New("unapplied application differs from its immutable baseline")
			}
		}
	}
	if h.rendererAddressOverride != "" {
		snapshot.rendererAddress = h.rendererAddressOverride
	}
	return snapshot, nil
}

func (s *revisionWorkflowScope) applications(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.journal.get(ctx, s.installation, s.key)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, app := range r.Plan.applicationMembers() {
		out = append(out, map[string]any{"key": app.Key})
	}
	return out, nil
}
func (s *revisionWorkflowScope) sourceKey(ctx context.Context, args map[string]any) (string, string, preparedApplication, error) {
	role, ok := args["role"].(string)
	key, _ := args["application"].(string)
	if !ok || (role != "candidate" && role != "rollback") || len(args) > 2 {
		return "", "", preparedApplication{}, errors.New("source requires one declared application and role")
	}
	for name := range args {
		if name != "role" && name != "application" {
			return "", "", preparedApplication{}, errors.New("source accepts no caller evidence")
		}
	}
	r, err := s.journal.get(ctx, s.installation, s.key)
	if err != nil {
		return "", "", preparedApplication{}, err
	}
	app, err := r.Plan.application(key)
	if err != nil {
		return "", "", app, err
	}
	return applicationCaptureKey(key, role), role, app, nil
}
func (s *revisionWorkflowScope) inventory(role string) argocd.RenderInventory {
	return renderInventory(s.preparation.Scope, s.renders, role)
}

func (s *revisionWorkflowScope) readGroup(ctx context.Context, r revisionRecord) (any, error) {
	needs := false
	s.memberNeeds = map[string]bool{}
	for _, app := range r.Plan.Applications {
		facts, err := s.client.Observe(ctx, app.Intent)
		if err == nil {
			if !r.Applications[app.Key].Started || !facts.IntentObserved {
				return nil, errors.New("application intent preceded its durable start")
			}
		} else if errors.Is(err, argocd.ErrChanged) {
			needs = true
			s.memberNeeds[app.Key] = true
		} else {
			return nil, errors.New("group application cannot be reconciled")
		}
	}
	s.needsWrite = needs
	return map[string]any{"mayApply": s.mode == revisionStart, "needsWrite": needs, "state": r.State}, nil
}
func (s *revisionWorkflowScope) applyGroup(ctx context.Context, r revisionRecord, key string) (any, error) {
	app, err := r.Plan.application(key)
	if err != nil {
		return nil, err
	}
	if !s.memberNeeds[key] {
		return nil, nil
	}
	admission, err := s.freshWriteEvidence(r)
	if err != nil {
		return nil, err
	}
	// The complete group is freshly authenticated before each member effect.
	if _, err = readSelectedReceiver(ctx, s.host, r); err != nil {
		return nil, err
	}
	r, err = s.journal.beginApplication(ctx, s.installation, s.key, key, admission)
	if err != nil {
		return nil, err
	}
	if _, err = s.freshWriteEvidence(r); err != nil {
		return nil, err
	}
	effect, cancel := context.WithDeadline(ctx, admission.freshUntil)
	defer cancel()
	if _, err = s.client.Apply(effect, app.Intent); err != nil {
		return nil, errors.New("application effect is unconfirmed; resume the exact selected group")
	}
	s.memberNeeds[key] = false
	return nil, nil
}
func (s *revisionWorkflowScope) observeGroup(ctx context.Context, r revisionRecord, key string) (any, error) {
	app, err := r.Plan.application(key)
	if err != nil {
		return nil, err
	}
	old := r.Applications[key]
	if !old.Started {
		return nil, errors.New("application has not started")
	}
	facts, err := s.client.Observe(ctx, app.Intent)
	if err != nil {
		return nil, err
	}
	r, err = s.journal.observeApplication(ctx, s.installation, s.key, key, old.Version, facts)
	if err != nil {
		return nil, err
	}
	if s.memberObserved == nil {
		s.memberObserved = map[string]bool{}
	}
	s.memberObserved[key] = true
	s.observed = len(s.memberObserved) == len(r.Plan.Applications)
	return map[string]any{"operationSucceeded": facts.OperationSucceeded, "healthy": facts.Healthy, "synced": facts.Synced}, nil
}

func configurationMatchesPlan(snapshot *receiverSnapshot, plan preparedPlan) bool {
	if snapshot == nil || plan.Preparation == nil {
		return false
	}
	return snapshot.invariantDigest == plan.Preparation.ConfigurationInvariantDigest && (plan.FormatVersion == 2 || snapshot.digest == plan.Preparation.ConfigurationDigest) && freshConfigurationObservation(snapshot.observed, time.Now())
}
