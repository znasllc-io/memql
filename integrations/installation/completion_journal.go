package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type completionReceipt struct {
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
	if p.PlanID != key || plan.CompletionWorkflow == nil || p.Workflow != plan.CompletionWorkflow.Version || p.Health.validate() != nil || plan.HealthBaseline == nil || p.Health.UserID != plan.HealthBaseline.UserID || p.Health.IdentityID != plan.HealthBaseline.IdentityID {
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
		if r.State != "applying" || r.Rollback != nil || actor != r.Plan.RequestedBy || e.operator != actor || e.plan != key || !e.expires.After(time.Now()) || e.expires.Sub(time.Now()) > time.Minute || r.ObservationVersion != version || r.Observation == nil || !completedController(*r.Observation) {
			return errors.New("installation completion requires fresh original-operator native evidence")
		}
		if err := e.receipt.validate(r.Plan, key); err != nil {
			return err
		}
		body, err := json.Marshal(e.receipt)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE installation_revision_attempts SET state='complete',completion_receipt=$2::jsonb,completed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE plan_id=$1`, key, string(body))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE installation_revision_heads SET active_plan_id=NULL WHERE installation_id=$1 AND active_plan_id=$2 AND slot_epoch=$3`, installation, key, r.SlotEpoch)
		return err
	})
}
