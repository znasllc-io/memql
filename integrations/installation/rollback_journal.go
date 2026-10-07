package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// rollbackPlan is the durable result of native rollback admission, never a
// request or a proof supplied by DSL. These digests name fresh verifier results;
// the scoped host must obtain those actual results before reserving this plan.
type rollbackPlan struct {
	ParentID            string        `json:"parentId"`
	RequestedBy         string        `json:"requestedBy"`
	WorkflowDigest      string        `json:"workflowDigest"`
	ConfigurationDigest string        `json:"configurationDigest"`
	ArtifactDigest      string        `json:"artifactDigest"`
	StorageDigest       string        `json:"storageDigest"`
	SensitiveDigest     string        `json:"sensitiveDigest"`
	ObservedAt          string        `json:"observedAt"`
	ArtifactExpiresAt   string        `json:"artifactExpiresAt"`
	Intent              argocd.Intent `json:"intent"`
}

type rollbackRecord struct {
	ID                 string
	Plan               rollbackPlan
	Started            bool
	ObservationVersion int64
	Observation        *argocd.Facts
}

func (p rollbackPlan) canonical(parent revisionRecord) ([]byte, string, error) {
	if p.ParentID != parent.ID || parent.Plan.Preparation == nil || p.RequestedBy != parent.Plan.RequestedBy {
		return nil, "", errors.New("rollback must belong to its admitted installation and requester")
	}
	for _, digest := range []string{p.ParentID, p.WorkflowDigest, p.ConfigurationDigest, p.ArtifactDigest, p.StorageDigest, p.SensitiveDigest} {
		if !internalDigest.MatchString(digest) {
			return nil, "", errors.New("rollback requires exact native evidence bindings")
		}
	}
	observed, err := time.Parse(time.RFC3339Nano, p.ObservedAt)
	expires, expiryErr := time.Parse(time.RFC3339Nano, p.ArtifactExpiresAt)
	if err != nil || expiryErr != nil || observed.IsZero() || !expires.After(observed) {
		return nil, "", errors.New("rollback evidence has invalid observation times")
	}
	if err := argocd.ValidateRollback(parent.Plan.Intent, p.Intent); err != nil {
		return nil, "", err
	}
	body, err := json.Marshal(p)
	if err != nil || len(body) > maxPlanBytes {
		return nil, "", errors.New("rollback plan exceeds its encoding bound")
	}
	body, err = canonicalJSON(body)
	if err != nil {
		return nil, "", err
	}
	return body, "memql-id:" + string(id.NewUntracked().FromString("installation-rollback-plan-v1:"+string(body))), nil
}

func (p rollbackPlan) fresh() error {
	observed, err := time.Parse(time.RFC3339Nano, p.ObservedAt)
	expires, expiryErr := time.Parse(time.RFC3339Nano, p.ArtifactExpiresAt)
	now := time.Now()
	if err != nil || expiryErr != nil || observed.After(now) || now.Sub(observed) > time.Minute || !expires.After(now) {
		return errors.New("rollback requires fresh configuration, preservation and artifact evidence")
	}
	return nil
}

// The lock order is always installation head, parent attempt, reversal row.
// Historical observations are data, never authority to release that head.
func readRollback(ctx context.Context, tx *sql.Tx, parent revisionRecord) (*rollbackRecord, error) {
	var r rollbackRecord
	var body, observation []byte
	var started sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT rollback_id,plan,started_at,observation_version,observation FROM installation_revision_rollbacks WHERE parent_plan_id=$1 FOR UPDATE`, parent.ID).
		Scan(&r.ID, &body, &started, &r.ObservationVersion, &observation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if parent.State != "applying" || len(body) > maxPlanBytes || json.Unmarshal(body, &r.Plan) != nil {
		return nil, errors.New("installation rollback journal is inconsistent")
	}
	expected, key, err := r.Plan.canonical(parent)
	actual, encodingErr := canonicalJSON(body)
	if err != nil || encodingErr != nil || key != r.ID || string(actual) != string(expected) {
		return nil, errors.New("installation rollback identity is inconsistent")
	}
	r.Started = started.Valid
	if r.ObservationVersion < 0 || (r.ObservationVersion == 0) != (len(observation) == 0) || (len(observation) != 0 && !r.Started) {
		return nil, errors.New("installation rollback effect markers are inconsistent")
	}
	if len(observation) != 0 {
		var facts argocd.Facts
		if json.Unmarshal(observation, &facts) != nil {
			return nil, errors.New("installation rollback observation is invalid")
		}
		body, _ := json.Marshal(facts)
		actual, err := canonicalJSON(observation)
		expected, _ := canonicalJSON(body)
		if err != nil || string(actual) != string(expected) {
			return nil, errors.New("installation rollback observation contains noncanonical fields")
		}
		r.Observation = &facts
	}
	return &r, nil
}

// reserveRollback is a journal operation, not a live verifier. The private
// host supplies one admitted plan before any Argo write. Once stored, even a
// not-yet-started rollback fences the forward workflow permanently.
func (j *revisionJournal) reserveRollback(ctx context.Context, installation, key string, plan rollbackPlan) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, parent *revisionRecord, actor string) error {
		if parent.State != "applying" || actor != parent.Plan.RequestedBy {
			return errors.New("rollback requires the active installation's requester")
		}
		body, rollbackID, err := plan.canonical(*parent)
		if err != nil {
			return err
		}
		if parent.Rollback != nil {
			if parent.Rollback.ID != rollbackID {
				return errChanged
			}
			return nil // Recover a lost journal reply without renewing evidence.
		}
		if err := plan.fresh(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO installation_revision_rollbacks(parent_plan_id,rollback_id,plan) VALUES($1,$2,$3::jsonb)`, key, rollbackID, string(body))
		return err
	})
}

func (j *revisionJournal) beginRollback(ctx context.Context, installation, key, workflow string) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, parent *revisionRecord, actor string) error {
		r := parent.Rollback
		if parent.State != "applying" || r == nil || actor != r.Plan.RequestedBy || workflow != r.Plan.WorkflowDigest {
			return errors.New("rollback authority or installed recipe changed")
		}
		if r.Started {
			return nil
		}
		// This is the durable start fence, not freshness authority. The host
		// must freshly verify the bound inputs before every possible Apply,
		// including a crash after this commit but before external dispatch.
		_, err := tx.ExecContext(ctx, `UPDATE installation_revision_rollbacks SET started_at=clock_timestamp(),updated_at=clock_timestamp() WHERE parent_plan_id=$1`, key)
		return err
	})
}

// A replacement engine may report current native facts. Neither a successful
// reverse sync nor a failed one releases the installation or discards evidence.
func (j *revisionJournal) observeRollback(ctx context.Context, installation, key string, version int64, facts argocd.Facts) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, parent *revisionRecord, _ string) error {
		r := parent.Rollback
		if parent.State != "applying" || r == nil || !r.Started || r.ObservationVersion != version {
			return errChanged
		}
		body, err := json.Marshal(facts)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE installation_revision_rollbacks SET observation=$2::jsonb,observation_version=observation_version+1,updated_at=clock_timestamp() WHERE parent_plan_id=$1`, key, string(body))
		return err
	})
}
