package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
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
//
// ONE SNAPSHOT ACROSS EVERY PAGE OF EVERY TABLE (memql#4043). eachRow pages
// with LIMIT/OFFSET, and each page is a SEPARATE statement -- so without a
// snapshot the pages are read from different states of the table and a
// concurrent write before the offset shifts every later row. The two directions
// are NOT symmetric, and only one of them is loud:
//
//	an INSERT before the offset shifts later rows FORWARD, so the next page
//	re-emits a row the previous page already sent. A DUPLICATE -- caught at
//	restore time, because Restore inserts with no ON CONFLICT against the
//	(id, "createdAt") primary key and aborts.
//
//	a DELETE before the offset shifts later rows BACKWARD, so the next page's
//	OFFSET steps over one. A SILENT OMISSION: exit 0, no error, and a manifest
//	whose counts match the short stream because they are counted FROM it. A
//	named row, present before the export began and never itself deleted, simply
//	absent from the backup.
//
// A delete looks impossible in an append-only store, which is why the real
// trigger is worth naming: integrations/knowledge/seed.go purges
// v1:knowledge:documentChunk rows whenever a knowledge domain is re-seeded, and
// those rows carry OLD createdAt values -- so they sit EARLY in this ordering,
// exactly where the damage is done. `memql backup` does no quiescing and takes
// no lock: it runs against a fully live cluster on a 30-minute timeout.
//
// The snapshot covers the READS and nothing else. Writing the manifest and
// concatenating the spill into w happen after it is released, because w may be
// a slow or remote sink and a transaction held open across it would pin the
// oldest xmin -- blocking vacuum cluster-wide for as long as the write takes.
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
	if err := readTablesUnderOneSnapshot(ctx, db, tables, func(r Row) error { return rows.WriteRow(r) }); err != nil {
		return Manifest{}, err
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

// readTablesUnderOneSnapshot runs fn over every row of every named table inside
// a SINGLE repeatable-read snapshot (memql#4043). See Export for what the
// snapshot buys and why the pages need it.
//
// Two branches, because bun.IDB is satisfied by both a pool and a transaction
// and they cannot be treated alike:
//
//   - a POOL (*bun.DB) -- open the transaction here, at REPEATABLE READ. Also
//     READ ONLY: an export writes nothing, and saying so lets Postgres refuse a
//     write rather than leaving "reads only" as a property of the code above.
//
//   - a TRANSACTION the caller already holds -- it cannot be reopened, and this
//     must NOT quietly accept whatever isolation it happens to have. bun's
//     Tx.RunInTx DISCARDS its *sql.TxOptions (its parameter is literally named
//     `_`, bun@v1.2.18 db.go:780) and opens a SAVEPOINT instead, so asking a Tx
//     for REPEATABLE READ is accepted and ignored. Under a READ COMMITTED caller
//     the export would page with no snapshot at all and every line of this fix
//     would be decoration. So ask the DATABASE what is actually in effect, and
//     refuse what cannot hold a snapshot.
//
// That refusal is the fail-CLOSED half and it is the point: a wrong backup is
// worth less than no backup, because only one of the two is discovered at the
// moment it is taken.
func readTablesUnderOneSnapshot(ctx context.Context, db bun.IDB, tables []string, fn func(Row) error) error {
	if tx, ok := db.(bun.Tx); ok {
		if err := requireSnapshotIsolation(ctx, tx); err != nil {
			return err
		}
		return eachTable(ctx, tx, tables, fn)
	}
	return db.RunInTx(ctx,
		&sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true},
		func(ctx context.Context, tx bun.Tx) error { return eachTable(ctx, tx, tables, fn) },
	)
}

func eachTable(ctx context.Context, db bun.IDB, tables []string, fn func(Row) error) error {
	for _, table := range tables {
		if err := eachRow(ctx, db, table, fn); err != nil {
			return err
		}
	}
	return nil
}

// requireSnapshotIsolation refuses a caller transaction that cannot hold one
// snapshot for its whole life.
//
// current_setting('transaction_isolation') reports what the SERVER is running
// this transaction under, which is the only source that cannot be fooled by a
// TxOptions somebody handed to a call that throws it away. Inside a savepoint it
// reports the enclosing transaction's level -- which is exactly the case being
// guarded, so the probe answers the question actually being asked.
//
// SERIALIZABLE is accepted alongside REPEATABLE READ: in Postgres it is
// repeatable read plus predicate locking, so it holds the same single snapshot
// and is strictly stronger for this purpose.
func requireSnapshotIsolation(ctx context.Context, tx bun.Tx) error {
	var level string
	if err := tx.NewRaw(`SELECT current_setting('transaction_isolation')`).Scan(ctx, &level); err != nil {
		return fmt.Errorf("backup: export: reading the caller transaction's isolation level: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "repeatable read", "serializable":
		return nil
	}
	return fmt.Errorf(
		"backup: export: the caller's transaction runs at %q, which takes a new snapshot per statement; "+
			"an export pages with LIMIT/OFFSET, so a concurrent delete before the offset would silently "+
			"omit rows from the backup. Pass the pool (*bun.DB) and let Export open its own transaction, "+
			"or begin yours with sql.LevelRepeatableRead",
		level)
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
// NO SNAPSHOT OF ITS OWN, and that is deliberate rather than an omission
// (memql#4043). The snapshot belongs one level up, in readTablesUnderOneSnapshot,
// because it has to span every page of every TABLE -- a per-call transaction here
// would give MemoryNodes and SecretMemoryNodes two different views of the same
// instant, which is the bug in a smaller shape. Leaving this function bare is
// also what lets TestUnsnapshottedPagingSilentlyOmitsARow reproduce the omission
// as a standing positive control; adding a transaction here would keep that test
// green while quietly making it measure nothing.
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
