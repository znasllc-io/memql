package installation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"time"

	"github.com/znasllc-io/memql/integrations/argocd"
)

// ownedSuccessRecord is the journal's record that this attempt's own Argo
// operation for one intent was observed finishing Succeeded: the member it
// belongs to, the revision and canonical source that sync applied, and when
// it was observed. It is keyed by the intent's digest in owned_successes.
//
// Why it exists: Argo keeps only the LAST operation in status.operationState.
// A later sync of the same Application (production's Applications are manual
// sync, so any hand sync; self-heal elsewhere) replaces the plan's operation
// there, and completion and restoration, which counted success only from that
// field, then refused forever although the Application still ran exactly the
// plan's revision. With this record argocd.Observe may report the success
// again, under the conditions in argocd's observe: the plan's marker and
// complete desired spec, nothing pending or running, Argo comparing the plan's
// revision with the plan's source, and any later sync's result naming that
// same revision and source. A later sync to another revision or spec still
// refuses.
//
// It is written only from what argocd reported for the exact intent, only for
// an intent this attempt durably started (startedIntents), only while the
// attempt is active, and never removed. A plan that never observed its own
// success (the operation was replaced before any read) has no record and keeps
// refusing; its operator can close it.
type ownedSuccessRecord struct {
	Application string          `json:"application"`
	Revision    string          `json:"revision"`
	Source      json.RawMessage `json:"source"`
	ObservedAt  string          `json:"observedAt"`
}

// At most one forward intent, one planned reversal and one reconciled
// replacement per member, for at most eight members.
const maxOwnedSuccesses = 24

func decodeOwnedSuccesses(body []byte, r revisionRecord) (map[string]ownedSuccessRecord, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var out map[string]ownedSuccessRecord
	if receiverJSON(body, &out) != nil || len(out) == 0 || len(out) > maxOwnedSuccesses {
		return nil, errors.New("installation owned-success record is malformed")
	}
	started := r.startedIntents()
	for digest, record := range out {
		item, ok := started[digest]
		if !ok || item.application != record.Application {
			return nil, errors.New("installation owned success names no started intent of this attempt")
		}
		if err := record.matches(item.intent); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// matches reports whether the record states exactly the revision and source
// its intent's own sync applies, and a valid observation time.
func (o ownedSuccessRecord) matches(intent argocd.Intent) error {
	revision, source, err := intent.Synced()
	if err != nil {
		return err
	}
	actual, actualErr := canonicalJSON(o.Source)
	expected, expectedErr := canonicalJSON(source)
	observed, timeErr := time.Parse(time.RFC3339Nano, o.ObservedAt)
	if actualErr != nil || expectedErr != nil || timeErr != nil || observed.IsZero() || o.Revision != revision || !bytes.Equal(actual, expected) {
		return errors.New("installation owned success differs from its intent's revision and source")
	}
	return nil
}

// recordOwnedSuccess keeps the observation that an intent's own operation
// finished Succeeded. The caller passes an intent whose argocd facts reported
// OperationSucceeded; with a record already present this is a no-op, so a
// success Observe reported from the record itself never rewrites it. Like any
// observation it needs a currently authorized operator, not the requester.
func (j *revisionJournal) recordOwnedSuccess(ctx context.Context, installation, key, application string, intent argocd.Intent) (revisionRecord, error) {
	digest, err := intent.Digest()
	if err != nil {
		return revisionRecord{}, err
	}
	revision, source, err := intent.Synced()
	if err != nil {
		return revisionRecord{}, err
	}
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, _ string) error {
		if _, recorded := r.OwnedSuccesses[digest]; recorded {
			return nil
		}
		if !r.rollbackActive() {
			return errChanged
		}
		item, ok := r.startedIntents()[digest]
		if !ok || item.application != application {
			return errors.New("installation owned success names no started intent of this attempt")
		}
		next := maps.Clone(r.OwnedSuccesses)
		if next == nil {
			next = map[string]ownedSuccessRecord{}
		}
		var observed time.Time
		if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&observed); err != nil {
			return err
		}
		next[digest] = ownedSuccessRecord{Application: application, Revision: revision, Source: source, ObservedAt: observed.UTC().Format(time.RFC3339Nano)}
		if len(next) > maxOwnedSuccesses {
			return errors.New("installation owned-success record exceeds its bound")
		}
		body, err := json.Marshal(next)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE installation_revision_attempts SET owned_successes=$2::jsonb,updated_at=clock_timestamp() WHERE plan_id=$1`, key, string(body))
		return err
	})
}

// recordedSuccessContext lets argocd.Observe and PlanRollback, in every native
// operation of this call, accept the success of each intent of r whose own
// operation is known to have succeeded: those recorded in OwnedSuccesses, and
// every forward member once the attempt has a forward completion receipt,
// because completion required each member's own operation to have succeeded
// (journal.complete checks completedController over every member). The
// receipt covers attempts completed before owned successes were recorded.
//
// A started reversal member's effective intent may also be proved from Argo's
// deployment history from the reversal's start (argocd.WithDeploymentHistory).
// A reversal whose own sync was replaced before any read saw it could
// otherwise neither complete nor be closed, since its writes reached the
// cluster; a forward plan in that state can be closed, so forward intents get
// no history evidence. Without any of these the context is unchanged and
// Argo's operationState alone decides, as before.
func recordedSuccessContext(ctx context.Context, r revisionRecord) context.Context {
	intents := []argocd.Intent{}
	for digest, item := range r.startedIntents() {
		if _, recorded := r.OwnedSuccesses[digest]; recorded {
			intents = append(intents, item.intent)
		}
	}
	if r.Completion != nil {
		for _, app := range r.Plan.applicationMembers() {
			intents = append(intents, app.Intent)
		}
	}
	evidence, err := argocd.WithRecordedSuccess(ctx, intents...)
	if err != nil {
		// A journal intent that no longer validates carries no evidence; the
		// stricter operationState-only observation applies.
		return ctx
	}
	if r.Rollback == nil || !r.Rollback.Started {
		return evidence
	}
	reversals := []argocd.Intent{}
	for _, app := range r.Plan.applicationMembers() {
		if reverse, ok := r.Rollback.application(app.Key); ok && reverse.Started {
			reversals = append(reversals, reverse.Intent)
		}
	}
	if deployed, err := argocd.WithDeploymentHistory(evidence, r.Rollback.StartedAt, reversals...); err == nil {
		return deployed
	}
	return evidence
}

// observedSuccess reports whether facts show an intent's own operation
// Succeeded on evidence a journal may record: operationState itself, or a
// record it already holds. A success read from Argo's deployment history is
// not recorded as observed; the history keeps proving it.
func observedSuccess(facts argocd.Facts) bool {
	return facts.OperationSucceeded && facts.SuccessEvidence != argocd.SuccessDeployed
}

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
