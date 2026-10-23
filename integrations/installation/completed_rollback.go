package installation

import (
	"context"
	"database/sql"
	"errors"
)

func (r revisionRecord) rollbackActive() bool { return r.State == "applying" || r.State == "reverting" }

// Eligibility is a fresh review projection, never an effect grant. The same
// predicate is checked under the installation head lock when reclaiming it.
func rollbackEligibility(ctx context.Context, tx *sql.Tx, r revisionRecord, actor string) (bool, error) {
	if actor != r.Plan.RequestedBy || r.Plan.Preparation == nil || r.Plan.HealthBaseline == nil || r.Plan.RollbackWorkflow == nil || r.Plan.CompletionWorkflow == nil {
		return false, nil
	}
	var active, preparation sql.NullString
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT active_plan_id,active_preparation_id,slot_epoch FROM installation_revision_heads WHERE installation_id=$1`, r.Plan.InstallationID).Scan(&active, &preparation, &epoch); err != nil {
		return false, err
	}
	if epoch != r.SlotEpoch || preparation.Valid {
		return false, nil
	}
	if r.rollbackActive() {
		return active.Valid && active.String == r.ID, nil
	}
	if r.State != "complete" || active.Valid || r.Completion == nil || r.Rollback != nil {
		return false, nil
	}
	for _, app := range r.Plan.applicationMembers() {
		var occupied bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM installation_application_claims WHERE namespace=$1 AND name=$2)`, app.Intent.Target.Namespace, app.Intent.Target.Name).Scan(&occupied); err != nil {
			return false, err
		}
		if occupied {
			return false, nil
		}
	}
	return true, nil
}

// reacquireRollback reserves the completed attempt itself, retaining its exact
// forward completion receipt and time. A newer preparation (even cancelled),
// plan or target claim permanently prevents this historical attempt taking over.
// This reservation grants no external effect: the captured rollback recipe must
// reopen all evidence and revalidate controller, runtime and migration state.
func (j *revisionJournal) reacquireRollback(ctx context.Context, installation, key string) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, actor string) error {
		allowed, err := rollbackEligibility(ctx, tx, *r, actor)
		if err != nil {
			return err
		}
		if !allowed {
			return errors.New("rollback requires the original operator's latest unchanged installation attempt")
		}
		if r.rollbackActive() {
			return nil // Another replica already admitted this exact request.
		}
		preparation, err := readPreparation(ctx, tx, installation, r.Plan.Preparation.ID)
		if err != nil {
			return err
		}
		if preparation.State != "promoted" || preparation.PromotedPlanID != key || preparation.SlotEpoch+1 != r.SlotEpoch || preparation.Scope.RequestedBy != actor {
			return errors.New("completed rollback lost its original preparation")
		}
		if _, err := promotedRevision(ctx, tx, preparation); err != nil {
			return err
		}
		if err := claimApplications(ctx, tx, preparation.Scope, preparation.ID); err != nil {
			return err
		}
		if err := requireApplicationClaims(ctx, tx, r.Plan); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE installation_revision_heads SET active_plan_id=$2 WHERE installation_id=$1`, installation, key); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE installation_revision_attempts SET state='reverting',updated_at=clock_timestamp() WHERE plan_id=$1`, key)
		return err
	})
}
