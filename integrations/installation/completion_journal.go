package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type completionReceipt struct {
	Direction string            `json:"direction,omitempty"`
	PlanID    string            `json:"planId"`
	Workflow  string            `json:"workflow"`
	Artifacts string            `json:"artifacts"`
	Workloads string            `json:"workloads"`
	Storage   string            `json:"storage"`
	Protected string            `json:"protected"`
	Health    HealthObservation `json:"health"`
}

// This value is built from private native proof objects, never decoded from a request.
type completionEvidence struct {
	plan, operator string
	expires        time.Time
	receipt        completionReceipt
}

func (p completionReceipt) validate(plan preparedPlan, key string) error {
	if (p.Direction != "" && p.Direction != "rollback") || (p.Direction == "rollback" && (plan.HealthBaseline == nil || p.Health.EngineCommit != plan.HealthBaseline.EngineCommit || p.Health.MigrationsDigest != plan.HealthBaseline.MigrationsDigest)) || p.PlanID != key || plan.CompletionWorkflow == nil || p.Workflow != plan.CompletionWorkflow.Version || p.Health.validate() != nil || plan.HealthBaseline == nil || p.Health.UserID != plan.HealthBaseline.UserID || p.Health.IdentityID != plan.HealthBaseline.IdentityID {
		return errors.New("installation completion receipt differs from selected plan")
	}
	for _, digest := range []string{p.Artifacts, p.Workloads, p.Storage, p.Protected} {
		if !internalDigest.MatchString(digest) {
			return errors.New("installation completion receipt is incomplete")
		}
	}
	if plan.Preparation == nil || p.Storage != plan.Preparation.StorageDigest || p.Protected != plan.Preparation.SensitiveDigest {
		return errors.New("installation completion preservation identity changed")
	}
	return nil
}
func (j *revisionJournal) complete(ctx context.Context, installation, key string, version int64, e completionEvidence) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, actor string) error {
		converged := r.Observation != nil && completedController(*r.Observation)
		currentVersion := r.ObservationVersion
		state := "complete"
		if r.Rollback != nil {
			converged = r.Rollback.Started && ((r.Rollback.Observation != nil && completedController(*r.Rollback.Observation)) || r.Rollback.noStartedApplications())
			currentVersion = r.Rollback.ObservationVersion
			state = "rolled_back"
			if e.receipt.Direction != "rollback" {
				return errors.New("rollback requires its own terminal proof")
			}
		} else if e.receipt.Direction != "" {
			return errors.New("forward completion cannot claim rollback")
		}
		if (r.State == "reverting" && r.Rollback == nil) || !r.rollbackActive() || actor != r.Plan.RequestedBy || e.operator != actor || e.plan != key || !e.expires.After(time.Now()) || e.expires.Sub(time.Now()) > time.Minute || currentVersion != version || !converged {
			return errors.New("installation completion requires fresh original-operator native evidence")
		}

		if err := e.receipt.validate(r.Plan, key); err != nil {
			return err
		}
		body, err := json.Marshal(e.receipt)
		if err != nil {
			return err
		}
		if state == "rolled_back" {
			_, err = tx.ExecContext(ctx, `UPDATE installation_revision_attempts SET state=$3,rollback_completion_receipt=$2::jsonb,rollback_completed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE plan_id=$1`, key, string(body), state)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE installation_revision_attempts SET state=$3,completion_receipt=$2::jsonb,completed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE plan_id=$1`, key, string(body), state)
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE installation_revision_heads SET active_plan_id=NULL WHERE installation_id=$1 AND active_plan_id=$2 AND slot_epoch=$3`, installation, key, r.SlotEpoch)
		if err == nil && r.Plan.Preparation != nil {
			err = releaseApplicationClaims(ctx, tx, installation, r.Plan.Preparation.ID)
		}
		return err
	})
}
