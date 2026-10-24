package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
)

// This native row is written before artifact pins. It is not editable graph
// state and conveys no approval. A second replica uses the same selection,
// while fresh installed authority is still required for every new effect.
type candidateProfileBinding struct {
	ID            string                    `json:"id"`
	Owner         string                    `json:"owner"`
	ProfileName   string                    `json:"profileName"`
	Configuration string                    `json:"configuration"`
	Runs          map[string]string         `json:"runs"`
	Facts         candidateProfileFacts     `json:"facts"`
	Snapshot      *workflowhost.Snapshot    `json:"snapshot"`
	Selection     candidateProfileSelection `json:"selection"`
	Digest        string                    `json:"digest"`
}

func profileRequestID(owner, name, configuration string, runs map[string]string) (string, error) {
	return profileDigest(map[string]any{"owner": owner, "profileName": name, "configuration": configuration, "runs": runs})
}
func (b candidateProfileBinding) digest() (string, error) { b.Digest = ""; return profileDigest(b) }

func decodeProfileBinding(body []byte, owner, key string) (candidateProfileBinding, error) {
	var b candidateProfileBinding
	if err := decodeCandidateObject(body, &b); err != nil {
		return b, err
	}
	id, err := profileRequestID(b.Owner, b.ProfileName, b.Configuration, b.Runs)
	digest, digestErr := b.digest()
	if err != nil || digestErr != nil || b.Owner != owner || b.ID != key || id != key || b.Digest != digest || b.Facts.ProfileName != b.ProfileName {
		return b, errors.New("release profile journal binding failed integrity verification")
	}
	if _, err := workflowhost.PrepareSnapshot(b.Snapshot, candidateProfileContract, workflowhost.Options{}); err != nil {
		return b, err
	}
	return b, nil
}

func (l *candidateLedger) profileBinding(ctx context.Context, key string, candidate bool) (candidateProfileBinding, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return candidateProfileBinding{}, err
	}
	db, err := l.database()
	if err != nil {
		return candidateProfileBinding{}, err
	}
	query := `SELECT binding_id, body FROM release_candidate_profile_bindings WHERE owner_user_id=$1 AND binding_id=$2`
	if candidate {
		query = `SELECT binding_id, body FROM release_candidate_profile_bindings WHERE owner_user_id=$1 AND candidate_id=$2`
	}
	var id string
	var body []byte
	if err := db.QueryRowContext(ctx, query, owner, key).Scan(&id, &body); err != nil {
		return candidateProfileBinding{}, err
	}
	return decodeProfileBinding(body, owner, id)
}

func (l *candidateLedger) saveProfileBinding(ctx context.Context, b candidateProfileBinding) (candidateProfileBinding, error) {
	owner, err := candidateOwner(ctx)
	if err != nil || owner != b.Owner {
		return b, errors.New("profile binding requires its original owner")
	}
	b.Digest, err = b.digest()
	if err != nil {
		return b, err
	}
	body, err := json.Marshal(b)
	if err != nil {
		return b, err
	}
	if _, err := decodeProfileBinding(body, owner, b.ID); err != nil {
		return b, err
	}
	db, err := l.database()
	if err != nil {
		return b, err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO release_candidate_profile_bindings(binding_id, owner_user_id, body) VALUES($1,$2,$3::jsonb) ON CONFLICT(binding_id) DO NOTHING`, b.ID, owner, string(body))
	if err != nil {
		return b, err
	}
	stored, err := l.profileBinding(ctx, b.ID, false)
	if err != nil {
		return b, err
	}
	if !reflect.DeepEqual(b, stored) {
		return b, errors.New("profile selection disagrees with its durable materialization")
	}
	return stored, nil
}

func (l *candidateLedger) attachProfileCandidate(ctx context.Context, b candidateProfileBinding, candidateID string) error {
	owner, err := candidateOwner(ctx)
	if err != nil || owner != b.Owner {
		return errors.New("profile candidate requires its original owner")
	}
	db, err := l.database()
	if err != nil {
		return err
	}
	// One immutable selection may produce exactly one candidate. This precedes
	// record-preparing, so interruption cannot lose the candidate's authority.
	result, err := db.ExecContext(ctx, `UPDATE release_candidate_profile_bindings SET candidate_id=$3 WHERE binding_id=$1 AND owner_user_id=$2 AND (candidate_id IS NULL OR candidate_id=$3)`, b.ID, owner, candidateID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return errors.New("profile selection already belongs to another candidate")
	}
	return nil
}

func (i *Integration) configuredCandidateFor(ctx context.Context, key string) (*candidatePreparer, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	p, err := i.candidateStorage(ctx)
	if err != nil {
		return nil, err
	}
	b, err := p.ledger.profileBinding(ctx, key, true)
	if errors.Is(err, sql.ErrNoRows) {
		return i.configuredCandidate(ctx)
	}
	if err != nil {
		return nil, err
	}
	profile, _, digest, err := i.candidateProfile(ctx, b.ProfileName)
	if err != nil || digest != b.Configuration {
		return nil, errors.New("release profile authority or installed resolver changed")
	}
	facts, err := i.profileFacts(ctx, profile, b.Runs)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(facts, b.Facts) {
		return nil, errors.New("release profile native facts changed")
	}
	return i.profilePreparer(ctx, p, profile, b)
}
