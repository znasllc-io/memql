package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/znasllc-io/memql/integrations/argocd"
)

func (r *rollbackRecord) application(key string) (rollbackApplicationState, bool) {
	if r == nil {
		return rollbackApplicationState{}, false
	}
	out := rollbackApplicationState{}
	found := false
	if r.Plan.FormatVersion != 2 {
		if key != "" {
			return out, false
		}
		out = rollbackApplicationState{Intent: r.Plan.Intent, Started: r.Started, BaselineFence: r.Plan.BaselineFence}
		found = true
	} else {
		for _, app := range r.Plan.Applications {
			if app.Key == key {
				out = rollbackApplicationState{Intent: app.Intent, Started: r.Applications[key].Started, BaselineFence: app.BaselineFence}
				found = true
			}
		}
	}
	if saved := r.Applications[key].ReconciledIntent; saved != nil {
		out.Intent = *saved
		out.BaselineFence = false
	}
	return out, found
}

func decodeRollbackObservations(body []byte, p rollbackPlan, parent preparedPlan) (map[string]applicationObservation, error) {
	out := map[string]applicationObservation{}
	if len(body) == 0 {
		return out, nil
	}
	if len(body) > 128<<10 || receiverJSON(body, &out) != nil || len(out) > len(parent.applicationMembers()) {
		return nil, errors.New("rollback application observations are invalid")
	}
	allowed := map[string]bool{}
	for _, app := range p.Applications {
		allowed[app.Key] = true
	}
	if p.FormatVersion != 2 {
		allowed[""] = true
	}
	for key, item := range out {
		if !allowed[key] || (!item.Started && item.ReconciledIntent == nil) || item.Version < 0 || (item.Version == 0) != (item.Facts == nil) {
			return nil, errors.New("rollback observation has no declared started application")
		}
		if item.ReconciledIntent != nil {
			forward, err := parent.application(key)
			if err != nil {
				return nil, err
			}
			baseline := p.BaselineFence
			if p.FormatVersion == 2 {
				baseline = false
				for _, app := range p.Applications {
					if app.Key == key {
						baseline = app.BaselineFence
					}
				}
			}
			if !baseline || argocd.ValidateRollback(forward.Intent, *item.ReconciledIntent) != nil {
				return nil, errors.New("reconciled reversal is outside its exact forward intent")
			}
		}
	}
	return out, nil
}
func (j *revisionJournal) beginRollbackApplication(ctx context.Context, installation, key, application, workflow string, admission revisionWriteAdmission) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, actor string) error {
		reverse := r.Rollback
		if !r.rollbackActive() || reverse == nil || reverse.Plan.FormatVersion != 2 || !reverse.Started || actor != r.Plan.RequestedBy || workflow != reverse.Plan.WorkflowDigest || admission.planID != key || admission.workflow != r.Plan.ExecutionWorkflowDigest || !admission.freshUntil.After(time.Now()) || admission.freshUntil.Sub(time.Now()) > time.Minute {
			return errors.New("rollback application requires fresh original-operator admission")
		}
		if _, ok := reverse.application(application); !ok {
			return errors.New("application is outside this reversal")
		}
		if reverse.Applications[application].Started {
			return nil
		}
		entry := reverse.Applications[application]
		entry.Started = true
		reverse.Applications[application] = entry
		body, _ := json.Marshal(reverse.Applications)
		_, err := tx.ExecContext(ctx, `UPDATE installation_revision_rollbacks SET application_observations=$2::jsonb,updated_at=clock_timestamp() WHERE parent_plan_id=$1`, key, string(body))
		return err
	})
}
func (j *revisionJournal) observeRollbackApplication(ctx context.Context, installation, key, application string, version int64, facts argocd.Facts) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, _ string) error {
		reverse := r.Rollback
		if !r.rollbackActive() || reverse == nil || reverse.Plan.FormatVersion != 2 || !reverse.Started {
			return errChanged
		}
		old, ok := reverse.Applications[application]
		if !ok || !old.Started || old.Version != version {
			return errChanged
		}
		old.Version++
		old.Facts = &facts
		reverse.Applications[application] = old
		body, _ := json.Marshal(reverse.Applications)
		if _, err := tx.ExecContext(ctx, `UPDATE installation_revision_rollbacks SET application_observations=$2::jsonb,updated_at=clock_timestamp() WHERE parent_plan_id=$1`, key, string(body)); err != nil {
			return err
		}
		plan := preparedPlan{}
		for _, app := range reverse.Plan.Applications {
			plan.Applications = append(plan.Applications, preparedApplication{Key: app.Key})
		}
		combined, complete := aggregateApplicationFacts(plan, reverse.Applications)
		if !complete {
			return nil
		}
		observation, _ := json.Marshal(combined)
		_, err := tx.ExecContext(ctx, `UPDATE installation_revision_rollbacks SET observation=$2::jsonb,observation_version=observation_version+1 WHERE parent_plan_id=$1`, key, string(observation))
		return err
	})
}

func (r *rollbackRecord) noStartedApplications() bool {
	return r != nil && r.Plan.FormatVersion == 2 && len(r.Plan.Applications) == 0
}

// A baseline-fence request and the uncertain forward request share the original
// CAS. Once the exact forward intent is observed, the old fence cannot land.
// Persist this single exact alternative before applying it; never rebind again.
func (j *revisionJournal) reconcileRollbackApplication(ctx context.Context, installation, key, application string, intent argocd.Intent) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, actor string) error {
		reverse := r.Rollback
		if !r.rollbackActive() || reverse == nil || actor != r.Plan.RequestedBy {
			return errors.New("reversal reconciliation requires its original operator")
		}
		effective, ok := reverse.application(application)
		if !ok {
			return errors.New("reversal application is not declared")
		}
		previous := reverse.Applications[application]
		if previous.ReconciledIntent != nil {
			if !sameIntent(*previous.ReconciledIntent, intent) {
				return errChanged
			}
			return nil
		}
		forward, err := r.Plan.application(application)
		if err != nil {
			return err
		}
		if !effective.BaselineFence || previous.Facts != nil || argocd.ValidateRollback(forward.Intent, intent) != nil {
			return errors.New("reversal alternative is not the exact recorded forward operation")
		}
		previous.ReconciledIntent = &intent
		reverse.Applications[application] = previous
		body, _ := json.Marshal(reverse.Applications)
		_, err = tx.ExecContext(ctx, `UPDATE installation_revision_rollbacks SET application_observations=$2::jsonb,updated_at=clock_timestamp() WHERE parent_plan_id=$1`, key, string(body))
		return err
	})
}
