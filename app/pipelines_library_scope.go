package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

var _ pipelinesteps.LibraryArtifactScopeRetirement = (*pipelinesLibraryStore)(nil)

var errPipelineScopeFenced = errors.New("artifact producer scope was permanently fenced")

const pipelineScopeIntentPageSize = 128

func pipelineScopeKey(scope pipelinesteps.RunFileReceiptScope) (string, []byte) {
	payload, _ := json.Marshal(scope)
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:]), payload
}

func readPipelineScopeFence(ctx context.Context, tx *sql.Tx, scope pipelinesteps.RunFileReceiptScope) (bool, error) {
	key, _ := pipelineScopeKey(scope)
	var owner string
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT owner_user_id,scope FROM pipeline_artifact_scope_fences WHERE scope_key=$1`, key).Scan(&owner, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var stored pipelinesteps.RunFileReceiptScope
	if json.Unmarshal(payload, &stored) != nil || stored != scope || owner != scope.OwnerUserID {
		return false, errors.New("artifact producer fence has an inconsistent immutable scope")
	}
	return true, nil
}

// FenceRunFileScope closes admissions before cleanup inventories artifacts,
// including when a producer never returned an intent receipt. The same owner
// lock serializes this permanent record with every new/recovered reservation.
// Calls admitted earlier may still contact storage; each inventoried intent
// must also pass RetireRunFile's permanent provider fence before cleanup ends.
func (s *pipelinesLibraryStore) FenceRunFileScope(ctx context.Context, scope pipelinesteps.RunFileReceiptScope) error {
	var err error
	if scope, err = validatePipelineReceiptScope(ctx, scope, nil); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db, err := s.pipelineReceiptDB()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockPipelineUploadOwner(ctx, tx, scope.OwnerUserID); err != nil {
		return err
	}
	fenced, err := readPipelineScopeFence(ctx, tx, scope)
	if err != nil {
		return err
	}
	if !fenced {
		key, payload := pipelineScopeKey(scope)
		if _, err := tx.ExecContext(ctx, `INSERT INTO pipeline_artifact_scope_fences (scope_key,owner_user_id,scope)
VALUES ($1,$2,$3::jsonb)`, key, scope.OwnerUserID, string(payload)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReadFencedRunFileIntents returns a stable, bounded inventory after the
// admissions fence committed. Reserved and retired intents are both included;
// ready receipts alone cannot discover a lost response or interrupted upload.
// The caller owns pagination and chooses when to release pins and retire bytes.
func (s *pipelinesLibraryStore) ReadFencedRunFileIntents(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, afterID string) ([]string, error) {
	var cursorIDs []string
	if afterID != "" {
		cursorIDs = []string{afterID}
	}
	var err error
	if scope, err = validatePipelineReceiptScope(ctx, scope, cursorIDs); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db, err := s.pipelineReceiptDB()
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	fenced, err := readPipelineScopeFence(ctx, tx, scope)
	if err != nil {
		return nil, err
	}
	if !fenced {
		return nil, errors.New("artifact inventory requires a committed producer scope fence")
	}
	rows, err := tx.QueryContext(ctx, `SELECT intent_id,identity FROM pipeline_artifact_uploads
WHERE owner_user_id=$1 AND identity->>'WorkRunID'=$2 AND identity->>'StepKey'=$3 AND identity->>'Attempt'=$4
  AND intent_id COLLATE "C" > $5
ORDER BY intent_id COLLATE "C" LIMIT $6`, scope.OwnerUserID, scope.WorkRunID, scope.StepKey, strconv.Itoa(scope.Attempt), afterID, pipelineScopeIntentPageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]string, 0, pipelineScopeIntentPageSize)
	for rows.Next() {
		var intent pipelineUploadIntent
		var payload []byte
		if err := rows.Scan(&intent.ID, &payload); err != nil {
			return nil, err
		}
		if json.Unmarshal(payload, &intent.Identity) != nil || !pipelineIntentInScope(intent, scope) {
			return nil, errors.New("artifact inventory has an inconsistent immutable intent")
		}
		result = append(result, intent.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
