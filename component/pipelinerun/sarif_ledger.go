package pipelinerun

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// The identity binds one producer receipt to one repository/ref. It is never
// reused for another report, including after a rejected or uncertain upload.
type sarifEvidence struct {
	Owner, PipelineRunID, Repository, Commit, Ref, ReceiptDigest string
	InstallationID                                               int64
	Artifact                                                     pipelinesteps.StoredFileReceipt
}

type SARIFPublication struct {
	ID       string   `json:"publicationId"`
	State    string   `json:"state"`
	UploadID string   `json:"uploadId,omitempty"`
	SHA256   string   `json:"sha256"`
	Errors   []string `json:"errors,omitempty"`
}

type sarifLedger struct{ db *sql.DB }

func (l sarifLedger) evidence(ctx context.Context, owner, key string) (sarifEvidence, error) {
	var e sarifEvidence
	digest, err := hex.DecodeString(key)
	if err != nil || len(digest) != 32 || l.db == nil {
		return e, errors.New("SARIF publication journal needs an exact identity")
	}
	var body []byte
	if err := l.db.QueryRowContext(ctx, `SELECT evidence FROM pipeline_sarif_publications WHERE publication_id=$1 AND owner_user_id=$2`, key, owner).Scan(&body); err != nil {
		return e, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return e, err
	}
	actual, _, err := sarifIdentity(e)
	if err != nil || actual != key || e.Owner != owner {
		return e, errors.New("SARIF publication identity changed")
	}
	return e, nil
}

func sarifIdentity(e sarifEvidence) (string, []byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return "", nil, err
	}
	return string(id.NewUntracked().FromString("memql.pipeline-sarif.v1\x00" + string(b))), b, nil
}

func (l sarifLedger) reserve(ctx context.Context, e sarifEvidence) (SARIFPublication, error) {
	key, body, err := sarifIdentity(e)
	if err != nil {
		return SARIFPublication{}, err
	}
	if l.db == nil {
		return SARIFPublication{}, errors.New("SARIF publication journal is unavailable")
	}
	_, err = l.db.ExecContext(ctx, `INSERT INTO pipeline_sarif_publications(publication_id,owner_user_id,evidence,state,report_sha256)
VALUES($1,$2,$3::jsonb,'prepared',$4) ON CONFLICT(publication_id) DO NOTHING`, key, e.Owner, string(body), e.Artifact.SHA256)
	if err != nil {
		return SARIFPublication{}, err
	}
	return l.read(ctx, e)
}

func (l sarifLedger) read(ctx context.Context, e sarifEvidence) (SARIFPublication, error) {
	key, expected, err := sarifIdentity(e)
	if err != nil {
		return SARIFPublication{}, err
	}
	var got sarifEvidence
	var body, errs []byte
	r := SARIFPublication{ID: key}
	err = l.db.QueryRowContext(ctx, `SELECT evidence,state,upload_id,report_sha256,errors FROM pipeline_sarif_publications
WHERE publication_id=$1 AND owner_user_id=$2`, key, e.Owner).Scan(&body, &r.State, &r.UploadID, &r.SHA256, &errs)
	if err != nil {
		return r, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		return r, err
	}
	_, actual, err := sarifIdentity(got)
	if err != nil || !bytes.Equal(actual, expected) || r.SHA256 != e.Artifact.SHA256 {
		return r, errors.New("SARIF publication journal identity changed")
	}
	if err := json.Unmarshal(errs, &r.Errors); err != nil {
		return r, err
	}
	return r, nil
}

// A short compare-and-swap claims the only POST. No network request holds a
// transaction or a database lock. Another host sees uploading and may not POST.
func (l sarifLedger) claim(ctx context.Context, e sarifEvidence) (bool, error) {
	key, _, err := sarifIdentity(e)
	if err != nil {
		return false, err
	}
	r, err := l.db.ExecContext(ctx, `UPDATE pipeline_sarif_publications SET state='uploading',updated_at=clock_timestamp()
WHERE publication_id=$1 AND owner_user_id=$2 AND state='prepared'`, key, e.Owner)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (l sarifLedger) settle(ctx context.Context, e sarifEvidence, from, to, upload string, notes []string) (SARIFPublication, error) {
	if !((from == "uploading" && (to == "accepted" || to == "uncertain" || to == "rejected")) ||
		(from == "accepted" && (to == "complete" || to == "failed"))) {
		return SARIFPublication{}, errors.New("invalid SARIF journal transition")
	}
	if to == "accepted" && upload == "" {
		return SARIFPublication{}, errors.New("accepted SARIF requires an upload receipt")
	}
	key, _, err := sarifIdentity(e)
	if err != nil {
		return SARIFPublication{}, err
	}
	if notes == nil {
		notes = []string{}
	}
	body, err := json.Marshal(notes)
	if err != nil || len(body) > 16<<10 {
		return SARIFPublication{}, errors.New("SARIF errors exceed their bound")
	}
	_, err = l.db.ExecContext(ctx, `UPDATE pipeline_sarif_publications SET state=$3,upload_id=$4,errors=$5::jsonb,updated_at=clock_timestamp()
WHERE publication_id=$1 AND owner_user_id=$2 AND state=$6`, key, e.Owner, to, upload, string(body), from)
	if err != nil {
		return SARIFPublication{}, fmt.Errorf("recording SARIF receipt: %w", err)
	}
	return l.read(ctx, e)
}
