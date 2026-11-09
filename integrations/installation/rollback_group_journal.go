package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/znasllc-io/memql/integrations/argocd"
)

// application is one member's reversal as it stands: its effective intent (a
// replanned intent over a reconciled one over the planned one) and whether a
// reverse write may have been sent for it (Started).
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
		started := r.Started
		if r.perApplyStarts() {
			started = r.Applications[""].Started
		}
		out = rollbackApplicationState{Intent: r.Plan.Intent, Started: started, BaselineFence: r.Plan.BaselineFence}
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
	if replanned, ok := r.Replanned[key]; ok {
		out.Intent = replanned
		out.BaselineFence = false
	}
	return out, found
}

// perApplyStarts reports whether a single-Application reversal records its
// own start immediately before each reverse write (Applications[""], set by
// beginRollbackApplication after every apply check), on top of the
// reversal-level start that begin commits before those checks. Before this a
// single-Application restoration had only the reversal-level start, so any
// apply check that refused left a restoration that had written nothing but
// could be neither withdrawn nor closed.
//
// Engines up to 0.31.28 wrote after the reversal-level start alone, so the
// per-apply start is trusted only while no intent such an engine could have
// written is in the reversal: the planned intent is history-backed (such an
// engine refuses to apply one) or was replanned because it was dead, and any
// reconciled replacement is history-backed as well. Otherwise the
// reversal-level start still counts as a possible write.
func (r *rollbackRecord) perApplyStarts() bool {
	if r == nil || r.Plan.FormatVersion == 2 {
		return false
	}
	if _, replanned := r.Replanned[""]; r.Plan.Intent.FormatVersion != 2 && !replanned {
		return false
	}
	if saved := r.Applications[""].ReconciledIntent; saved != nil && saved.FormatVersion != 2 {
		return false
	}
	return true
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

// beginRollbackApplication commits one member's reverse start, the marker
// that a reverse write for it may follow, under the plan's effect lock and
// after every apply check. A group reversal records one per member; a
// single-Application reversal records its one member ("") when it follows the
// per-apply protocol (perApplyStarts).
func (j *revisionJournal) beginRollbackApplication(ctx context.Context, installation, key, application, workflow string, admission revisionWriteAdmission) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, actor string) error {
		reverse := r.Rollback
		if !r.rollbackActive() || reverse == nil || (reverse.Plan.FormatVersion != 2 && !reverse.perApplyStarts()) || !reverse.Started || actor != r.Plan.RequestedBy || workflow != reverse.Plan.WorkflowDigest || admission.planID != key || admission.workflow != r.Plan.ExecutionWorkflowDigest || !admission.freshUntil.After(time.Now()) || admission.freshUntil.Sub(time.Now()) > time.Minute {
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

// replanRollbackApplication stores a member's reversal planned again
// (argocd.PlanRollback over the member's forward intent) in place of its
// effective intent, under the plan's effect lock its caller holds. A reserved
// reversal used to be planned once, so a member whose saved reversal could no
// longer be admitted held the installation for good: a history-backed one once
// Kubernetes compacted the watch history since its planning, a format 1 one
// (planned by engines up to 0.31.28) once Argo's status writes advanced the
// generation its strict fence tests.
//
// Replacing is safe only while nothing of the member's reversal can still
// reach the cluster. The caller establishes that: the member has no reverse
// start, or its effective intent is a format 1 intent the Application proves
// superseded (argocd.Superseded: no request for it can pass its
// compare-and-swap, and none landed). A started history-backed member is never
// replanned: the version its in-flight request tested is not retained.
// PlanRollback rechecks the forward intent's marker, spec, idle state and
// terminal outcome; this transaction rechecks the reversal's relation.
func (j *revisionJournal) replanRollbackApplication(ctx context.Context, installation, key, application string, intent argocd.Intent) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, actor string) error {
		reverse := r.Rollback
		if !r.rollbackActive() || reverse == nil || !reverse.Started || actor != r.Plan.RequestedBy {
			return errors.New("replanning a reversal requires its original operator and started reversal")
		}
		effective, ok := reverse.application(application)
		if !ok {
			return errors.New("application is outside this reversal")
		}
		if effective.Started && effective.Intent.FormatVersion == 2 {
			return errors.New("a started history-backed reversal is never replanned")
		}
		forward, err := r.Plan.application(application)
		if err != nil {
			return err
		}
		if err := argocd.ValidateRollback(forward.Intent, intent); err != nil {
			return err
		}
		next := map[string]argocd.Intent{}
		for member, planned := range reverse.Replanned {
			next[member] = planned
		}
		next[application] = intent
		body, err := json.Marshal(next)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE installation_revision_rollbacks SET replanned_intents=$2::jsonb,updated_at=clock_timestamp() WHERE parent_plan_id=$1`, key, string(body))
		return err
	})
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
