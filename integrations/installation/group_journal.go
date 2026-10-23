package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/znasllc-io/memql/integrations/argocd"
)

type applicationObservation struct {
	ReconciledIntent *argocd.Intent `json:"reconciledIntent,omitempty"`
	Started          bool           `json:"started"`
	Version          int64          `json:"version"`
	Facts            *argocd.Facts  `json:"facts,omitempty"`
}

func (p preparedPlan) applicationMembers() []preparedApplication {
	if p.FormatVersion == 2 {
		return append([]preparedApplication(nil), p.Applications...)
	}
	return []preparedApplication{{Intent: p.Intent, CandidateCapture: "candidate", RollbackCapture: "rollback", RenderDigest: p.RenderDigest, RollbackRenderDigest: p.RollbackRenderDigest}}
}
func (p preparedPlan) application(key string) (preparedApplication, error) {
	for _, app := range p.applicationMembers() {
		if app.Key == key {
			return app, nil
		}
	}
	return preparedApplication{}, errors.New("application is outside the selected plan")
}
func applicationArgument(args map[string]any) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	value, ok := args["application"].(string)
	if !ok || len(args) != 1 {
		return "", errors.New("application effect accepts only its declared key")
	}
	return value, nil
}

func claimApplications(ctx context.Context, tx *sql.Tx, scope preparationScope, preparation string) error {
	apps := scope.applicationMembers()
	sort.Slice(apps, func(i, j int) bool {
		a, b := apps[i].Intent.Target, apps[j].Intent.Target
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	for _, app := range apps {
		target := app.Intent.Target
		if _, err := tx.ExecContext(ctx, `INSERT INTO installation_application_claims(namespace,name,uid,installation_id,preparation_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, target.Namespace, target.Name, target.UID, scope.InstallationID, preparation); err != nil {
			return err
		}
		var uid, installation, owner string
		if err := tx.QueryRowContext(ctx, `SELECT uid,installation_id,preparation_id FROM installation_application_claims WHERE namespace=$1 AND name=$2 FOR UPDATE`, target.Namespace, target.Name).Scan(&uid, &installation, &owner); err != nil {
			return err
		}
		if uid != target.UID || installation != scope.InstallationID || owner != preparation {
			return errors.New("application is already claimed by another installation attempt")
		}
	}
	return nil
}
func requireApplicationClaims(ctx context.Context, tx *sql.Tx, p preparedPlan) error {
	if p.Preparation == nil {
		if p.FormatVersion == 2 {
			return errors.New("application group has no admitted preparation")
		}
		return nil
	}
	for _, app := range p.applicationMembers() {
		var uid, installation, preparation string
		err := tx.QueryRowContext(ctx, `SELECT uid,installation_id,preparation_id FROM installation_application_claims WHERE namespace=$1 AND name=$2`, app.Intent.Target.Namespace, app.Intent.Target.Name).Scan(&uid, &installation, &preparation)
		if err != nil || uid != app.Intent.Target.UID || installation != p.InstallationID || preparation != p.Preparation.ID {
			return errors.New("application group no longer owns every target")
		}
	}
	return nil
}
func releaseApplicationClaims(ctx context.Context, tx *sql.Tx, installation, preparation string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM installation_application_claims WHERE installation_id=$1 AND preparation_id=$2`, installation, preparation)
	return err
}

func decodeApplicationObservations(body []byte, p preparedPlan) (map[string]applicationObservation, error) {
	out := map[string]applicationObservation{}
	if len(body) == 0 {
		return out, nil
	}
	if p.FormatVersion != 2 || len(body) > 128<<10 || receiverJSON(body, &out) != nil || len(out) > len(p.Applications) {
		return nil, errors.New("application observations are malformed")
	}
	for key, v := range out {
		if _, err := p.application(key); err != nil {
			return nil, err
		}
		if v.ReconciledIntent != nil || !v.Started || v.Version < 0 || (v.Version == 0) != (v.Facts == nil) {
			return nil, errors.New("application observation markers are inconsistent")
		}
	}
	return out, nil
}

func (j *revisionJournal) beginApplication(ctx context.Context, installation, key, application string, admission revisionWriteAdmission) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, actor string) error {
		if r.Plan.FormatVersion != 2 || r.State != "applying" || r.Rollback != nil || actor != r.Plan.RequestedBy || admission.planID != r.ID || admission.workflow != r.Plan.ExecutionWorkflowDigest || !admission.freshUntil.After(time.Now()) || admission.freshUntil.Sub(time.Now()) > time.Minute {
			return errors.New("application start requires the original fresh group admission")
		}
		if _, err := r.Plan.application(application); err != nil {
			return err
		}
		if r.Applications[application].Started {
			return nil
		}
		r.Applications[application] = applicationObservation{Started: true}
		body, _ := json.Marshal(r.Applications)
		_, err := tx.ExecContext(ctx, `UPDATE installation_revision_attempts SET application_observations=$2::jsonb,updated_at=clock_timestamp() WHERE plan_id=$1`, key, string(body))
		return err
	})
}
func (j *revisionJournal) observeApplication(ctx context.Context, installation, key, application string, version int64, facts argocd.Facts) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, _ string) error {
		if r.Plan.FormatVersion != 2 || r.State != "applying" || r.Rollback != nil {
			return errChanged
		}
		old, ok := r.Applications[application]
		if !ok || !old.Started || old.Version != version {
			return errChanged
		}
		old.Version++
		old.Facts = &facts
		r.Applications[application] = old
		body, _ := json.Marshal(r.Applications)
		if _, err := tx.ExecContext(ctx, `UPDATE installation_revision_attempts SET application_observations=$2::jsonb,updated_at=clock_timestamp() WHERE plan_id=$1`, key, string(body)); err != nil {
			return err
		}
		combined, complete := aggregateApplicationFacts(r.Plan, r.Applications)
		if !complete {
			return nil
		}
		observation, _ := json.Marshal(combined)
		_, err := tx.ExecContext(ctx, `UPDATE installation_revision_attempts SET observation=$2::jsonb,observation_version=observation_version+1 WHERE plan_id=$1`, key, string(observation))
		return err
	})
}
func aggregateApplicationFacts(plan preparedPlan, observations map[string]applicationObservation) (argocd.Facts, bool) {
	out := argocd.Facts{IntentObserved: true, OperationSucceeded: true, RevisionObserved: true, Healthy: true, Synced: true, OperationPhase: "Succeeded"}
	for _, app := range plan.Applications {
		member, ok := observations[app.Key]
		if !ok || !member.Started || member.Facts == nil {
			return argocd.Facts{}, false
		}
		f := member.Facts
		out.IntentObserved = out.IntentObserved && f.IntentObserved
		out.OperationPending = out.OperationPending || f.OperationPending
		out.OperationSucceeded = out.OperationSucceeded && f.OperationSucceeded
		out.RevisionObserved = out.RevisionObserved && f.RevisionObserved
		out.Healthy = out.Healthy && f.Healthy
		out.Synced = out.Synced && f.Synced
		if f.OperationPhase != "Succeeded" {
			out.OperationPhase = f.OperationPhase
		}
	}
	return out, true
}
