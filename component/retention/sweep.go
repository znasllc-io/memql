// Package retention enforces opt-in concept version lifetimes. Log lines keep
// their dedicated chunk-aware sweeper; all graph concepts use this bounded path.
package retention

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
)

const pageSize = 100
const archiveBudget = 32 << 20

// Archive verifies the bytes from object storage, not only an upload receipt.
type Archive interface {
	Upload(context.Context, string, string, []byte, string) (string, error)
	DownloadWithLimit(context.Context, string, string, int64) ([]byte, error)
}

type Sweeper struct {
	DB         *bun.DB
	Concepts   memorynodes.Registry
	Admit      func(context.Context, memorynodes.MemoryNode) bool
	Invalidate func(string)
	Archive    Archive
	Container  string
	Now        func() time.Time
}

type Report struct {
	Scanned   int      `json:"scanned"`
	Eligible  int      `json:"eligible"`
	Archived  int      `json:"archived"`
	Deleted   int      `json:"deleted"`
	Oversized int      `json:"oversized"`
	Objects   []string `json:"objects,omitempty"`
}

type cursor struct {
	At         sql.NullTime
	ID         string
	Generation int64
}

type candidate struct {
	memorynodes.MemoryNode
	Newer bool `bun:"newer" json:"-"`
}

type archiveRow struct {
	Table string `json:"table"`
	memorynodes.MemoryNode
}

// Run divides a bounded pass fairly among declarations. Policy-specific cursor
// keys include the policy contents, so changing TTL cannot strand older rows.
func (s *Sweeper) Run(ctx context.Context, dry bool) (Report, error) {
	var report Report
	if s.DB == nil || s.Concepts == nil || s.Admit == nil || s.Invalidate == nil {
		return report, fmt.Errorf("retention requires a database, live concepts, row admission and cache invalidation")
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	var concepts []*memorynodes.Concept
	for _, c := range s.Concepts.List() {
		if c.Retention != nil && c.Name != "v1:observability:logLine" {
			if c.Retention.TTL < time.Second || c.Retention.TTL > 36500*24*time.Hour || (c.Retention.Archive != "required" && c.Retention.Archive != "none") {
				return report, fmt.Errorf("%s: invalid retention declaration; records preserved", c.Name)
			}
			concepts = append(concepts, c)
		}
	}
	sort.Slice(concepts, func(i, j int) bool { return concepts[i].Name < concepts[j].Name })
	pass, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	var failures []error
	for n, c := range concepts {
		deadline, _ := pass.Deadline()
		share := time.Until(deadline) / time.Duration(len(concepts)-n)
		policyCtx, done := context.WithTimeout(pass, share)
		err := s.sweepConcept(policyCtx, c, now.Add(-c.Retention.TTL), dry, &report)
		done()
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", c.Name, err))
		}
		if pass.Err() != nil {
			break
		}
	}
	return report, errors.Join(failures...)
}

func (s *Sweeper) sweepConcept(ctx context.Context, c *memorynodes.Concept, cutoff time.Time, dry bool, report *Report) error {
	for _, table := range []string{memorynodes.MemoryNodesTable, memorynodes.SecretMemoryNodesTable} {
		raw, _ := json.Marshal(c.Retention)
		key := table + "/" + c.Name + "/" + string(id.NewUntracked().FromString(string(raw)))
		for ctx.Err() == nil {
			batchCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			complete, err := s.batch(batchCtx, key, table, c, cutoff, dry, report)
			cancel()
			if err != nil {
				return err
			}
			if complete || dry {
				break
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

func (s *Sweeper) claim(ctx context.Context, key string) (*cursor, error) {
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO concept_retention_progress(policy) VALUES (?) ON CONFLICT DO NOTHING`, key); err != nil {
		return nil, err
	}
	var c cursor
	err := s.DB.QueryRowContext(ctx, `UPDATE concept_retention_progress SET generation=generation+1,lease_until=clock_timestamp()+interval '30 seconds' WHERE policy=? AND lease_until<=clock_timestamp() RETURNING cursor_at,cursor_id,generation`, key).Scan(&c.At, &c.ID, &c.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// Physical retirement must preserve staged rows
// in its archive too. Hiding them loses recovery evidence; each row still passes
// the caller's row-admission gate before it is archived or deleted.
func (s *Sweeper) batch(ctx context.Context, key, table string, c *memorynodes.Concept, cutoff time.Time, dry bool, report *Report) (bool, error) {
	progress := &cursor{}
	if !dry {
		var err error
		progress, err = s.claim(ctx, key)
		if err != nil || progress == nil {
			return true, err
		}
		// Release a failed claim without advancing its cursor. Expiry handles a
		// crashed process; the generation predicate protects a replacement owner.
		defer func() {
			release, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			_, _ = s.DB.ExecContext(release, `UPDATE concept_retention_progress SET lease_until=clock_timestamp() WHERE policy=? AND generation=?`, key, progress.Generation)
		}()
	}
	var rows []candidate
	q := s.DB.NewSelect().TableExpr("? AS n", bun.Ident(table)).ColumnExpr("n.*").
		ColumnExpr(`EXISTS(SELECT 1 FROM ? newer WHERE newer.concept=n.concept AND newer.id=n.id AND newer."createdAt">n."createdAt") AS newer`, bun.Ident(table)).
		Where(`n.concept=? AND n."createdAt"<?`, c.Name, cutoff).OrderExpr(`n."createdAt",n.id`).Limit(pageSize)
	if progress.At.Valid {
		q = q.Where(`(n."createdAt",n.id)>(?,?)`, progress.At.Time, progress.ID)
	}
	if err := q.Scan(ctx, &rows); err != nil {
		return false, err
	}
	report.Scanned += len(rows)
	complete := len(rows) < pageSize
	expanded := make([]memorynodes.MemoryNode, len(rows))
	for n, row := range rows {
		expanded[n] = row.MemoryNode
	}
	if c.Retention.Archive == "required" {
		if err := memorynodes.HydrateSchemas(ctx, s.DB, expanded); err != nil {
			return false, err
		}
	}
	var selected, snapshots []memorynodes.MemoryNode
	size := 0
	for n, row := range rows {
		if c.Retention.KeepLatest && !row.Newer {
			continue
		}
		if !s.Admit(ctx, row.MemoryNode) {
			return false, fmt.Errorf("row admission refused; records preserved")
		}
		full := expanded[n]
		rowSize := len(full.Schema) + len(full.Payload) + len(full.Metadata) + len(full.Provenance)
		if rowSize > archiveBudget {
			report.Oversized++ // Preserve it and let smaller records get a turn.
			continue
		}
		if size+rowSize > archiveBudget {
			rows = rows[:n] // Resume at this unprocessed row in the next batch.
			complete = false
			break
		}
		size += rowSize
		selected = append(selected, full)
		snapshots = append(snapshots, row.MemoryNode)
	}
	report.Eligible += len(selected)
	if dry {
		return complete, nil
	}
	// snapshots retain the physical encoding for the final compare-and-delete.
	if c.Retention.Archive == "required" && len(selected) > 0 {
		object, err := s.archive(ctx, table, selected)
		if err != nil {
			return false, err
		}
		report.Archived += len(selected)
		report.Objects = append(report.Objects, object)
	}
	deleted := 0
	err := s.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var generation int64
		if err := tx.QueryRowContext(ctx, `SELECT generation FROM concept_retention_progress WHERE policy=? FOR UPDATE`, key).Scan(&generation); err != nil {
			return err
		}
		if generation != progress.Generation {
			return fmt.Errorf("retention claim changed; records preserved")
		}
		current, err := s.Concepts.Get(c.Name)
		if err != nil || current.Retention == nil || *current.Retention != *c.Retention {
			return fmt.Errorf("retention declaration changed; records preserved")
		}
		for _, row := range snapshots {
			del := tx.NewDelete().TableExpr("? AS n", bun.Ident(table)).Where(`n.id=? AND n.concept=? AND n."createdAt"=?`, row.ID, row.Concept, row.CreatedAt).
				Where(`n.payload=?::jsonb AND n.schema=?::jsonb AND n.metadata=?::jsonb AND n.provenance=?::jsonb AND n."createdBy"=? AND n.type=?`, string(row.Payload), string(row.Schema), string(row.Metadata), string(row.Provenance), row.CreatedBy, row.Type)
			if c.Retention.KeepLatest {
				del = del.Where(`EXISTS(SELECT 1 FROM ? newer WHERE newer.concept=n.concept AND newer.id=n.id AND newer."createdAt">n."createdAt")`, bun.Ident(table))
			}
			result, err := del.Exec(ctx)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			deleted += int(count) // each exact primary key matches at most one row
		}
		var at any
		lastID := ""
		if !complete && len(rows) > 0 {
			at = rows[len(rows)-1].CreatedAt
			lastID = rows[len(rows)-1].ID
		}
		_, err = tx.ExecContext(ctx, `UPDATE concept_retention_progress SET cursor_at=?,cursor_id=?,lease_until=clock_timestamp() WHERE policy=? AND generation=?`, at, lastID, key, progress.Generation)
		return err
	})
	if err != nil {
		return false, err
	}
	report.Deleted += deleted
	if deleted > 0 {
		s.Invalidate(c.Name)
	}
	return complete, nil
}

func (s *Sweeper) archive(ctx context.Context, table string, rows []memorynodes.MemoryNode) (string, error) {
	if s.Archive == nil || s.Container == "" {
		return "", fmt.Errorf("verified archive unavailable; records preserved")
	}
	if err := memorynodes.HydrateSchemas(ctx, s.DB, rows); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	encoder := json.NewEncoder(gz)
	total := 0
	for _, row := range rows {
		total += len(row.Schema) + len(row.Payload) + len(row.Metadata) + len(row.Provenance)
		if total > archiveBudget {
			return "", fmt.Errorf("expanded schemas exceed archive budget; records preserved")
		}
		if err := encoder.Encode(archiveRow{table, row}); err != nil {
			return "", err
		}
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	blob := buf.Bytes()
	object := "retention/concepts/" + string(id.NewUntracked().FromString(string(blob))) + ".ndjson.gz"
	if _, err := s.Archive.Upload(ctx, s.Container, object, blob, "application/gzip"); err != nil {
		return "", fmt.Errorf("archive upload failed; records preserved: %w", err)
	}
	restored, err := s.Archive.DownloadWithLimit(ctx, s.Container, object, int64(len(blob))+1)
	if err != nil {
		return "", fmt.Errorf("archive verification failed; records preserved: %w", err)
	}
	if !bytes.Equal(blob, restored) {
		return "", fmt.Errorf("archive verification mismatch; records preserved")
	}
	return object, nil
}
