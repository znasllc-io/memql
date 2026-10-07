package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/argocd"
)

var (
	errBusy    = errors.New("installation already has an active revision attempt")
	errChanged = errors.New("installation revision journal changed; read the current attempt")
)

type revisionJournal struct{ db func() *sql.DB }

type revisionRecord struct {
	ID                 string
	Plan               preparedPlan
	State              string
	SlotEpoch          int64
	ObservationVersion int64
	Observation        *argocd.Facts
	Rollback           *rollbackRecord
}

// The role is from the current resolved request, not a persisted claim or DSL
// argument. An installation update is separate from owner-only publication.
func installationActor(ctx context.Context) (string, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac.IsAnonymous || ac.Synthetic || ac.RoleStandIn ||
		(ac.Role != auth.RoleOwner && ac.Role != auth.RoleAdmin && ac.Role != auth.RoleDeveloper) {
		return "", errors.New("installation revision requires an identified developer, admin or owner")
	}
	actor := memql.BareShortId(ac.UserId)
	if !identifier.MatchString(actor) {
		return "", errors.New("installation revision requires an identified operator")
	}
	return actor, nil
}

func (j *revisionJournal) database() (*sql.DB, error) {
	if j == nil || j.db == nil {
		return nil, errors.New("installation revision journal is unavailable")
	}
	db := j.db()
	if db == nil {
		return nil, errors.New("installation revision journal is unavailable")
	}
	return db, nil
}

// reserve persists the complete reviewed native scope before any Argo write.
// All replicas serialize on one installation head, including the first insert.
// A started attempt keeps that head until a future verified completion/rollback
// operation releases it; timeouts, process death and Healthy/Synced do not.
func (j *revisionJournal) reserve(ctx context.Context, plan preparedPlan) (revisionRecord, error) {
	actor, err := installationActor(ctx)
	if err != nil {
		return revisionRecord{}, err
	}
	if actor != plan.RequestedBy {
		return revisionRecord{}, errors.New("installation plan belongs to another requester")
	}
	if plan.Preparation != nil {
		return revisionRecord{}, errors.New("prepared source evidence requires atomic preparation promotion")
	}
	body, key, err := plan.canonical()
	if err != nil {
		return revisionRecord{}, err
	}
	db, err := j.database()
	if err != nil {
		return revisionRecord{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return revisionRecord{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO installation_revision_heads(installation_id) VALUES($1) ON CONFLICT DO NOTHING`, plan.InstallationID)
	if err != nil {
		return revisionRecord{}, err
	}
	active, epoch, err := lockHead(ctx, tx, plan.InstallationID)
	if err != nil {
		return revisionRecord{}, err
	}
	var preparation sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT active_preparation_id FROM installation_revision_heads WHERE installation_id=$1`, plan.InstallationID).Scan(&preparation); err != nil {
		return revisionRecord{}, err
	}
	if preparation.Valid {
		return revisionRecord{}, errBusy
	}
	record, err := readRevision(ctx, tx, plan.InstallationID, key)
	if err == nil {
		if record.State == "cancelled" {
			return revisionRecord{}, errors.New("installation plan was permanently cancelled")
		}
		if active != key || epoch != record.SlotEpoch {
			return revisionRecord{}, errChanged
		}
		if err := tx.Commit(); err != nil {
			return revisionRecord{}, err
		}
		return record, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return revisionRecord{}, err
	}
	if active != "" {
		return revisionRecord{}, errBusy
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO installation_revision_attempts(plan_id,installation_id,requested_by,slot_epoch,plan,state)
VALUES($1,$2,$3,$4,$5::jsonb,'prepared')`, key, plan.InstallationID, actor, epoch+1, string(body))
	if err != nil {
		return revisionRecord{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE installation_revision_heads SET active_plan_id=$2,slot_epoch=$3 WHERE installation_id=$1`, plan.InstallationID, key, epoch+1)
	if err != nil {
		return revisionRecord{}, err
	}
	record, err = readRevision(ctx, tx, plan.InstallationID, key)
	if err != nil {
		return revisionRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return revisionRecord{}, err
	}
	return record, nil
}

func lockHead(ctx context.Context, tx *sql.Tx, installation string) (string, int64, error) {
	var active sql.NullString
	var epoch int64
	err := tx.QueryRowContext(ctx, `SELECT active_plan_id,slot_epoch FROM installation_revision_heads WHERE installation_id=$1 FOR UPDATE`, installation).Scan(&active, &epoch)
	return active.String, epoch, err
}

func readRevision(ctx context.Context, tx *sql.Tx, installation, key string) (revisionRecord, error) {
	r := revisionRecord{ID: key}
	var plan, observation []byte
	var requester string
	var started sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT plan,requested_by,slot_epoch,state,observation,observation_version,started_at
FROM installation_revision_attempts WHERE installation_id=$1 AND plan_id=$2 FOR UPDATE`, installation, key).
		Scan(&plan, &requester, &r.SlotEpoch, &r.State, &observation, &r.ObservationVersion, &started)
	if err != nil {
		return revisionRecord{}, err
	}
	var actual string
	r.Plan, actual, err = decodePlan(plan)
	if err != nil || actual != key || r.Plan.InstallationID != installation || r.Plan.RequestedBy != requester || r.SlotEpoch < 1 {
		return revisionRecord{}, errors.New("installation revision journal identity is inconsistent")
	}
	if r.State != "prepared" && r.State != "applying" && r.State != "cancelled" {
		return revisionRecord{}, errors.New("installation revision journal state is invalid")
	}
	if (r.State == "applying") != started.Valid || r.ObservationVersion < 0 || (r.ObservationVersion == 0) != (len(observation) == 0) {
		return revisionRecord{}, errors.New("installation revision effect markers are inconsistent")
	}
	if len(observation) != 0 {
		var facts argocd.Facts
		if json.Unmarshal(observation, &facts) != nil || r.State != "applying" {
			return revisionRecord{}, errors.New("installation observation is invalid")
		}
		encoded, _ := json.Marshal(facts)
		canonical, err := canonicalJSON(observation)
		want, _ := canonicalJSON(encoded)
		if err != nil || string(canonical) != string(want) {
			return revisionRecord{}, errors.New("installation observation contains noncanonical fields")
		}
		r.Observation = &facts
	}
	r.Rollback, err = readRollback(ctx, tx, r)
	return r, err
}

// withRecord always locks head then attempt, including reads. A historical
// cancelled attempt may be read, but can never capture a successor's slot.
func (j *revisionJournal) withRecord(ctx context.Context, installation, key string, update func(*sql.Tx, *revisionRecord, string) error) (revisionRecord, error) {
	actor, err := installationActor(ctx)
	if err != nil {
		return revisionRecord{}, err
	}
	if !identifier.MatchString(installation) || !internalDigest.MatchString(key) {
		return revisionRecord{}, errors.New("exact installation and plan identities are required")
	}
	db, err := j.database()
	if err != nil {
		return revisionRecord{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return revisionRecord{}, err
	}
	defer tx.Rollback()
	active, epoch, err := lockHead(ctx, tx, installation)
	if err != nil {
		return revisionRecord{}, err
	}
	r, err := readRevision(ctx, tx, installation, key)
	if err != nil {
		return revisionRecord{}, err
	}
	if r.State != "cancelled" && (active != key || epoch != r.SlotEpoch) {
		return revisionRecord{}, errChanged
	}
	if update != nil {
		if err := update(tx, &r, actor); err != nil {
			return revisionRecord{}, err
		}
		r, err = readRevision(ctx, tx, installation, key)
		if err != nil {
			return revisionRecord{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return revisionRecord{}, err
	}
	return r, nil
}

func (j *revisionJournal) get(ctx context.Context, installation, key string) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, nil)
}

// begin commits the started fence before an external write. The caller must
// await this result before applying the exact persisted Argo intent. A lost
// commit reply recovers the same plan; it never mints another effect identity.
// A changed executable workflow can still observe, but cannot reuse authority
// to perform a write under a different recipe.
func (j *revisionJournal) begin(ctx context.Context, installation, key, workflow string) (revisionRecord, error) {
	return j.beginWithAdmission(ctx, installation, key, workflow, nil)
}

// beginAdmitted accepts only a short-lived native proof handle from the
// revision workflow. It permits requalification after the promoted artifact
// observation expired, while still requiring the exact plan/recipe and a
// currently live proof deadline in the transaction that fences the write.
func (j *revisionJournal) beginAdmitted(ctx context.Context, installation, key, workflow string, admission revisionWriteAdmission) (revisionRecord, error) {
	return j.beginWithAdmission(ctx, installation, key, workflow, &admission)
}

func (j *revisionJournal) beginWithAdmission(ctx context.Context, installation, key, workflow string, admission *revisionWriteAdmission) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, actor string) error {
		if actor != r.Plan.RequestedBy || workflow != r.Plan.ExecutionWorkflowDigest || r.Rollback != nil {
			return errors.New("installation request authority or workflow changed")
		}
		if admission != nil && (admission.planID != key || admission.workflow != workflow || !admission.freshUntil.After(time.Now()) || admission.freshUntil.Sub(time.Now()) > time.Minute) {
			return errors.New("installation start requires a live exact-plan native requalification")
		}
		if r.State == "applying" {
			return nil
		}
		if r.State != "prepared" {
			return errors.New("installation plan cannot start")
		}
		if binding := r.Plan.Preparation; binding != nil && admission == nil {
			expires, err := time.Parse(time.RFC3339Nano, binding.ArtifactExpiresAt)
			if err != nil || !expires.After(time.Now()) {
				return errors.New("installation artifact evidence expired before start; fresh qualification is required")
			}
		}
		if admission != nil && !admission.freshUntil.After(time.Now()) {
			return errors.New("installation start requalification expired before the durable fence")
		}
		_, err := tx.ExecContext(ctx, `UPDATE installation_revision_attempts SET state='applying',started_at=clock_timestamp(),updated_at=clock_timestamp() WHERE plan_id=$1`, key)
		return err
	})
}

// cancel is safe only before begin. There is no cancellation path that guesses
// whether an uncertain Argo write happened, and no deletion of its history.
func (j *revisionJournal) cancel(ctx context.Context, installation, key string) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, actor string) error {
		if actor != r.Plan.RequestedBy {
			return errors.New("only the requester may cancel this prepared installation")
		}
		if r.State == "cancelled" {
			return nil
		}
		if r.State != "prepared" {
			return errors.New("installation effect may have started; observe or explicitly roll back")
		}
		_, err := tx.ExecContext(ctx, `UPDATE installation_revision_attempts SET state='cancelled',updated_at=clock_timestamp() WHERE plan_id=$1`, key)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE installation_revision_heads SET active_plan_id=NULL WHERE installation_id=$1 AND active_plan_id=$2 AND slot_epoch=$3`, installation, key, r.SlotEpoch)
		return err
	})
}

// observe records native adapter facts, not a workflow verdict. The version is
// captured BEFORE the external read. A late observer cannot overwrite newer
// facts. A lost journal reply is recovered by reading, never by inventing a new
// expected version for the old result. Healthy/Synced does not release the slot.
func (j *revisionJournal) observe(ctx context.Context, installation, key string, expectedVersion int64, facts argocd.Facts) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, _ string) error {
		if r.State != "applying" || r.Rollback != nil || r.ObservationVersion != expectedVersion {
			return errChanged
		}
		body, err := json.Marshal(facts)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE installation_revision_attempts SET observation=$2::jsonb,observation_version=observation_version+1,updated_at=clock_timestamp() WHERE plan_id=$1`, key, string(body))
		return err
	})
}
