package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

type savedOverlay struct {
	Record                                     overlayRecord
	BranchCommit, PullRequestURL, MergedCommit string
	PullRequest                                int
}

func (r savedOverlay) summary() InstallationProposal {
	state := "prepared"
	if r.PullRequest > 0 {
		state = "proposed"
	}
	if r.MergedCommit != "" {
		state = "merged"
	}
	return InstallationProposal{r.Record.ID, r.Record.Request.InstallationID, r.Record.Request.CandidateID, r.Record.Request.CatalogDigest, state, r.PullRequestURL, r.MergedCommit}
}
func (s *overlayScope) load(ctx context.Context, key string) (savedOverlay, error) {
	var r savedOverlay
	var body []byte
	var branch, url, merged sql.NullString
	var number sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT record,branch_commit,pull_request,pull_request_url,merged_commit FROM release_installation_proposals WHERE proposal_id=$1 AND owner_user_id=$2`, key, s.owner).Scan(&body, &branch, &number, &url, &merged)
	if err != nil {
		return r, err
	}
	if decodeCandidateObject(body, &r.Record) != nil || r.Record.ID != key || r.Record.Owner != s.owner || r.Record.Snapshot == nil || r.Record.Snapshot.Contract != overlayContract || r.Record.Snapshot.CheckArgs(nil) != nil || r.Record.CreatedAt.IsZero() || validateOverlayRecordFiles(r.Record) != nil || r.Record.ID != overlayHash([]string{r.Record.Owner, r.Record.Request.InstallationID, r.Record.Request.RequestID}) {
		return r, errors.New("stored installation overlay intent is invalid")
	}
	r.BranchCommit, r.PullRequestURL, r.MergedCommit = branch.String, url.String, merged.String
	if number.Valid {
		if number.Int64 < 1 || number.Int64 > 2147483647 {
			return r, errors.New("stored installation proposal number is invalid")
		}
		r.PullRequest = int(number.Int64)
	}
	if (branch.Valid && !installationCommit.MatchString(branch.String)) || (merged.Valid && !installationCommit.MatchString(merged.String)) || (number.Valid && (!branch.Valid || !url.Valid)) || (merged.Valid && !number.Valid) {
		return r, errors.New("installation overlay journal contains incomplete effect history")
	}
	return r, nil
}
func (s *overlayScope) reserve(ctx context.Context, args map[string]any) (any, error) {
	if err := s.gate(ctx, args); err != nil {
		return nil, err
	}
	if len(s.replacements) == 0 || s.source == nil || s.snapshot == nil {
		return nil, errors.New("installation overlay reserve requires verified exact bytes")
	}
	r := overlayRecord{ID: s.id, Owner: s.owner, Request: s.request, Configuration: s.configuration, CreatedAt: time.Now().UTC().Truncate(time.Second), Snapshot: s.snapshot}
	if s.configuration.FormatVersion == 1 {
		r.Replacement = s.replacements[0]
	} else {
		r.Replacements = s.replacements
	}
	if s.configuration.FormatVersion == 3 {
		r.NativeEvidence = s.nativeBound
		r.FilePreconditions = s.filePreconditions
	}
	if err := validateOverlayRecordFiles(r); err != nil {
		return nil, err
	}
	if s.record != nil {
		r.CreatedAt = s.record.CreatedAt
	}
	body, err := json.Marshal(r)
	if err != nil || len(body) > 1<<20 {
		return nil, errors.New("installation overlay journal exceeds its size bound")
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO release_installation_proposals(proposal_id,owner_user_id,installation_id,record) VALUES($1,$2,$3,$4::jsonb) ON CONFLICT (proposal_id) DO NOTHING`, s.id, s.owner, s.request.InstallationID, string(body))
	if err != nil {
		return nil, err
	}
	saved, err := s.load(ctx, s.id)
	if err != nil {
		return nil, err
	}
	// A concurrent first call may choose the timestamp. That durable value owns
	// all retry Git objects; no loser invents a second branch or commit identity.
	r.CreatedAt = saved.Record.CreatedAt
	if overlayHash(r) != overlayHash(saved.Record) {
		return nil, errors.New("installation overlay request was already reserved for different bytes")
	}
	s.record = &saved.Record
	return nil, nil
}

func (i *Integration) installationProposals(ctx context.Context) ([]InstallationProposal, error) {
	out := []InstallationProposal{}
	// No configured proposal policy is a valid installation using reviewed
	// external choices. Malformed configured policy is a refusal, not fallback.
	if i.resolver.systemVariable == nil {
		return out, nil
	}
	raw, err := i.resolver.systemVariable(ctx, InstallationOverlayVariable)
	if err != nil && !memql.IsVariableNotFound(err) {
		return nil, err
	}
	if raw == "" {
		return out, nil
	}
	cfg, err := i.overlayConfig(ctx)
	if err != nil {
		return nil, err
	}
	db, err := i.overlayDatabase()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT proposal_id FROM release_installation_proposals WHERE owner_user_id=$1 AND installation_id=$2 ORDER BY created_at DESC,proposal_id LIMIT 50`, cfg.OwnerUserID, cfg.InstallationID)
	if err != nil {
		return nil, err
	}
	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	s := &overlayScope{integration: i, db: db, owner: cfg.OwnerUserID, configuration: cfg}
	for _, key := range keys {
		r, err := s.load(ctx, key)
		if err != nil {
			return nil, err
		}
		if overlayHash(r.Record.Configuration) == overlayHash(cfg) {
			out = append(out, r.summary())
		}
	}
	return out, nil
}
