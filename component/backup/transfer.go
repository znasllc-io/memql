package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/uptrace/bun"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Options controls what an export carries.
type Options struct {
	EngineVersion string
	Domain        string
	// MasterKey is used ONLY to fingerprint itself into the manifest. It is
	// never written to the stream and never used to decrypt anything: a backup
	// carries secret payloads exactly as they sit in the database, still
	// encrypted.
	MasterKey string
	// IncludeSecrets carries SecretMemoryNodes rows. Default TRUE, because a
	// backup that silently drops the identity rows restores a cluster nobody
	// can sign into -- the failure would surface long after the restore, as an
	// unexplained empty account list.
	IncludeSecrets bool
	// Now is injectable so a test's output is deterministic.
	Now func() time.Time
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Export streams every graph row to w, manifest first.
//
// TWO PASSES, NOT ONE, and the reason is the manifest. Counts belong in the
// header where a reader sees them before deciding anything, but they are only
// known once the rows are out. Buffering every row in memory to count them
// first would make a backup's memory cost scale with the database. So the rows
// go to a temporary spill first, then the manifest and the spill are
// concatenated into w. Constant memory, complete header.
func Export(ctx context.Context, db bun.IDB, w io.Writer, opts Options) (Manifest, error) {
	if db == nil {
		return Manifest{}, errors.New("backup: export: nil database")
	}

	spill, err := newSpill()
	if err != nil {
		return Manifest{}, err
	}
	defer spill.Close()

	rows := newWriter(spill.file)

	tables := []string{TableMemoryNodes}
	if opts.IncludeSecrets {
		tables = append(tables, TableSecretMemoryNodes)
	}
	for _, table := range tables {
		if err := eachRow(ctx, db, table, func(r Row) error { return rows.WriteRow(r) }); err != nil {
			return Manifest{}, err
		}
	}
	if err := rows.flush(); err != nil {
		return Manifest{}, err
	}

	manifest := ManifestFor(
		opts.EngineVersion, opts.Domain, KeyFingerprint(opts.MasterKey),
		opts.IncludeSecrets, rows.Counts(), opts.now(),
	)
	if err := WriteManifest(w, manifest); err != nil {
		return Manifest{}, err
	}
	if err := spill.copyTo(w); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// eachRow streams one table, ORDERED, and in pages.
//
// Ordered by (createdAt, id) so two exports of an unchanged database are
// byte-identical -- which is what makes a backup diffable and a fixture stable.
// Paged because a cluster's row count is unbounded and a single materialised
// result set is not.
//
// staged-data: MUST-NOT-GATE -- gating this DESTROYS DATA, silently and
// permanently, and it is the worst instance in the set memql#3984 inventoried.
// The full argument is the paragraph below; it is worth adding only that this
// gate does not act alone -- CountRows is the compounding half, and one sweep
// that "adds the predicate everywhere" produces both.
//
// UNFILTERED, AND THAT IS LOAD-BEARING (memql#3985). Whole table, every concept,
// every version -- there is no WHERE clause here and none may be added. In
// particular do NOT teach this the staged-DATA tier (epic memql#3974): a backup
// is not a read, it is the copy the rows survive in, and a row omitted from an
// export is a row DESTROYED by the next restore. The staged tier withholds rows
// from callers asking what exists; this is not that question.
//
// The tier needs nothing here anyway, and the reason is worth stating because it
// looks like an omission. Staging is marked CONCEPT-grain (memql#3977): the flag
// is `conceptDataStaged` on a v1:authoring:construct row, and construct rows are
// ordinary MemoryNodes rows, so this unfiltered read already carries the marker
// along with everything it marks. A restore therefore brings the concept back
// STAGED and cannot resurrect its rows as live -- satisfied by the marking model
// rather than by code. Pinned by
// TestBackupCarriesTheStagedFlagSoARestoreCannotResurrectStagedRowsAsLive and
// TestExportIsUnfilteredAcrossConcepts in staged_data_db_test.go.
//
// One clarification the marking model does not cover, because it is about the
// EXPORT rather than the restore: ModelTableExpr parameterises this scan over
// SecretMemoryNodes as well, so any predicate anyone did add here would have to
// be valid on both tables. That is a complication, not the argument. The
// argument is that the rows do not come back.
func eachRow(ctx context.Context, db bun.IDB, table string, fn func(Row) error) error {
	const page = 2000
	offset := 0
	for {
		var batch []memoryNodes.MemoryNode
		q := db.NewSelect().Model(&batch).
			ModelTableExpr("? AS mn", bun.Ident(table)).
			// OrderExpr, not Order (memql#3985). Order() treats each argument
			// as an identifier and quotes it, so `mn."createdAt" ASC` came out
			// as ORDER BY "mn"."""createdAt""" ASC -- a column no table has, and
			// every Export against a real database died on it with
			// `column mn."createdAt" does not exist`. It survived because no
			// test in this package had ever run Export against a database:
			// format_test.go and compatibility_test.go both exercise the STREAM,
			// building []Row by hand, so the one statement that had to be right
			// was the one statement nothing executed. staged_data_db_test.go now
			// runs it for real.
			OrderExpr(`mn."createdAt" ASC, mn.id ASC`).
			Limit(page).Offset(offset)
		if err := q.Scan(ctx); err != nil {
			return fmt.Errorf("backup: read %s at offset %d: %w", table, offset, err)
		}
		if len(batch) == 0 {
			return nil
		}
		for i := range batch {
			if err := fn(rowFrom(table, batch[i])); err != nil {
				return err
			}
		}
		if len(batch) < page {
			return nil
		}
		offset += len(batch)
	}
}

func rowFrom(table string, n memoryNodes.MemoryNode) Row {
	return Row{
		Kind:       KindRow,
		Table:      table,
		ID:         n.ID,
		CreatedAt:  n.CreatedAt.UTC(),
		CreatedBy:  n.CreatedBy,
		Concept:    n.Concept,
		Type:       n.Type,
		Schema:     n.Schema,
		Payload:    n.Payload,
		Metadata:   n.Metadata,
		Provenance: n.Provenance,
	}
}

// RestoreReport is what a restore did, so the caller can tell the operator
// something true rather than "done".
type RestoreReport struct {
	Manifest Manifest
	Inserted map[string]int
	// SecretsUnreadable is set when the backup's secret rows were encrypted
	// under a DIFFERENT master key than this cluster holds. The rows are
	// restored either way -- deciding to drop somebody's data on a fingerprint
	// mismatch would be worse -- but they cannot be decrypted here, and saying
	// so is the whole point of the fingerprint.
	SecretsUnreadable bool
}

// Restore reads a backup into the database.
//
// INSERT-ONLY, and deliberately so. memQL rows are a time series keyed by
// (id, createdAt): a row is a VERSION, not a mutable record. So a restore adds
// versions and never overwrites one, and re-running it is a conflict on the
// primary key rather than a silent double-write. The caller decides whether a
// target must be empty; this function does not guess.
func Restore(ctx context.Context, db bun.IDB, r io.Reader, masterKey string) (RestoreReport, error) {
	if db == nil {
		return RestoreReport{}, errors.New("backup: restore: nil database")
	}
	reader, err := NewReader(r)
	if err != nil {
		return RestoreReport{}, err
	}
	m := reader.Manifest()

	report := RestoreReport{Manifest: m, Inserted: map[string]int{}}
	if m.IncludesSecrets && m.SecretKeyFingerprint != "" {
		if KeyFingerprint(masterKey) != m.SecretKeyFingerprint {
			report.SecretsUnreadable = true
		}
	}

	const batchSize = 500
	byTable := map[string][]memoryNodes.MemoryNode{}

	flush := func(table string) error {
		batch := byTable[table]
		if len(batch) == 0 {
			return nil
		}
		if _, err := db.NewInsert().Model(&batch).
			ModelTableExpr("? AS mn", bun.Ident(table)).Exec(ctx); err != nil {
			return fmt.Errorf("backup: restore into %s: %w", table, err)
		}
		report.Inserted[table] += len(batch)
		byTable[table] = batch[:0]
		return nil
	}

	for {
		row, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return report, err
		}
		byTable[row.Table] = append(byTable[row.Table], memoryNodes.MemoryNode{
			ID:         row.ID,
			CreatedAt:  row.CreatedAt,
			CreatedBy:  row.CreatedBy,
			Concept:    row.Concept,
			Type:       row.Type,
			Schema:     row.Schema,
			Payload:    row.Payload,
			Metadata:   row.Metadata,
			Provenance: row.Provenance,
		})
		if len(byTable[row.Table]) >= batchSize {
			if err := flush(row.Table); err != nil {
				return report, err
			}
		}
	}
	for table := range byTable {
		if err := flush(table); err != nil {
			return report, err
		}
	}
	return report, nil
}

// CountRows reports how many graph rows the cluster already holds, across both
// tables.
//
// Used by restore to refuse a non-empty target. Cheap on purpose -- it answers
// "is there anything here" rather than producing a census, and a restore that
// asked a more expensive question would be slower for no better decision.
//
// staged-data: MUST-NOT-GATE -- and this is the half that makes the pair worse
// than either site alone (memql#3984's inventory).
//
// UNFILTERED, for a sharper reason than eachRow's (memql#3985). This is the
// guard, so narrowing it does not merely lose data -- it removes the thing that
// would have stopped the loss. A concept filter added here (say, one skipping
// staged concepts to match a staged read gate) makes a cluster whose only rows
// are staged report ZERO; restore then concludes the target is empty and
// proceeds to overwrite exactly those rows. One change, causing the loss and
// disabling the check that catches it. Pinned by TestCountRowsIsUnfiltered.
func CountRows(ctx context.Context, db bun.IDB) (int, error) {
	total := 0
	for _, table := range []string{TableMemoryNodes, TableSecretMemoryNodes} {
		n, err := db.NewSelect().
			ModelTableExpr("? AS mn", bun.Ident(table)).
			ColumnExpr("1").
			Count(ctx)
		if err != nil {
			return 0, fmt.Errorf("backup: count %s: %w", table, err)
		}
		total += n
	}
	return total, nil
}
