package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

// Backfill v1:campaigns:recipient.emailDigest onto the rows written before the
// field existed (memql#6022, F9).
//
// ===========================================================================
// WHAT IT DOES, AND WHY
// ===========================================================================
// Every recipient now carries emailDigest, the SHA-256 of its address, stamped
// by addRecipient. A newsletter signup and an email rule's row-address send use
// it to find an existing membership in ONE keyed read
// (recipientInAudienceByDigest), where they used to walk the audience's whole
// roster. That only works for rows that HAVE a digest, so this stamps the ones
// that predate it.
//
// ===========================================================================
// THE NORMALIZATION: TRIM AND LOWER-CASE, NOTHING ELSE
// ===========================================================================
// RecipientEmailDigest is campaigns.EmailDigest: the address trimmed of
// surrounding whitespace and lower-cased, hashed with SHA-256, as lower-case
// hex. No other rewriting -- sub-addressing and dots in the local part are the
// person's own distinctions, and an internationalized domain is hashed as
// given. An address that cannot be a mailbox (no "@", an empty local part or
// domain, whitespace inside) has no digest: "".
//
// It is a copy because this package cannot import component/campaigns, and it
// is hashed in Go rather than in SQL because Postgres's lower() follows the
// database's locale and btrim() trims spaces only, while the engine folds with
// Go's Unicode rules; a digest the SQL computed could differ from the one the
// lookup asks for, silently. TestRecipientDigestMigrationMatchesEmailDigest
// (component/campaigns) holds this function to the original, and the DSL stamp
// hash(lower(trim(args.email))) is held to it by
// TestAddRecipientStampsTheDigestTheGoFunctionComputes.
//
// ===========================================================================
// WHICH ROWS, AND WHAT IS APPENDED
// ===========================================================================
// Selection is by concept (`v1:campaigns:recipient`) and reads each id's NEWEST
// version -- `DISTINCT ON (id) ... ORDER BY id, "createdAt" DESC`, the collapse
// every engine read makes -- so an older version without a digest behind a
// newer one with it is not a candidate, and an id whose newest version lacks one
// is, whatever an older version carried. A row whose address normalizes to
// nothing (an erased recipient, a malformed import) is left alone and counted.
//
// MemoryNodes is append-only, so the repair is ONE NEW VERSION per row: the
// newest version copied whole -- createdBy, type, schema, metadata, every payload
// key -- with emailDigest added and nothing else changed, under a `migration`
// provenance naming this file. The new version is stamped after the one it
// copies, since (id, "createdAt") is the primary key and the newest version is
// the one every read returns. The down direction deletes exactly the versions
// this migration appended, found by that provenance.
//
// ===========================================================================
// SAFE TO RUN ON TWO REPLICAS AT ONCE
// ===========================================================================
// The migration runs once per cluster, on whichever replica boots first, and
// bun's migration lock keeps a second replica's runner from starting it. This
// function does not rely on that, because it is also called directly: every
// batch takes one advisory lock for its transaction and reads its candidates
// AFTER holding it, and the insert re-checks, in the same statement, that the
// newest version still has no digest and still has the address that was
// hashed. Two walks therefore take turns batch by batch, and the second finds
// the first's work already stamped. Each recipient ends with exactly one new
// version, however many walks ran.
//
// ===========================================================================
// BOUNDED IN MEMORY, IN STATEMENTS AND IN TIME
// ===========================================================================
// The walk is keyset-paginated by id, recipientEmailDigestBatch (500) ids per
// transaction: at most that many candidates are held at once, and each batch is
// two statements of bounded size on the latest-row index (concept, id,
// "createdAt" DESC), which 20260913000000 and 20260914000000 ensure.
//
// Each transaction runs under a transaction-local statement_timeout below
// pgdriver's 10 s read deadline, so a statement that runs long is cancelled by
// the SERVER with an error the client receives: the runner sees an acknowledged
// failure and releases its lock, rather than a client timeout over a backend
// that is still running (migrationMayStillBeRunning; the readiness collapse's
// memql#5604). A step runs on a context of its own -- the attempt's values
// without its cancellation -- so a SIGTERM at a node's restart cannot cut one
// off midway; the attempt's context is read BETWEEN steps, and an attempt that
// was cancelled or has too little time left ends there with ErrMigrationDeferred
// instead of a context error. The next attempt walks again from the start:
// recipients already stamped cost a read and nothing else.
//
// ===========================================================================
// ROWS WRITTEN WHILE IT RUNS
// ===========================================================================
// A replica still running the previous release can add a recipient with no
// digest after the walk has passed its id, and the migration, which is recorded
// as applied, does not look again. The membership is then not found by digest,
// and a signup for that mailbox adds a second one. That window is the length of
// a rolling deploy. Updates do not close it -- a read-merge keeps the payload it
// read, digest or not -- but the walk is idempotent, so running it again
// (BackfillRecipientEmailDigests, or the down and up of this migration) stamps
// whatever the window left, and an addRecipient for the same id stamps its row.
//
// bun names a Go migration after the FILE its Register call sits in, so the
// call must stay in this file.

const (
	// recipientEmailDigestMigrationName is this file's name, which is the
	// version and comment bun derives for the migration, and the provenance name
	// stamped on every version it appends.
	recipientEmailDigestMigrationName = "20261011000000_recipient_email_digest"

	// recipientEmailDigestConcept is the one concept this migration reads or
	// writes.
	recipientEmailDigestConcept = "v1:campaigns:recipient"

	// recipientEmailDigestBatch is how many recipient ids one transaction
	// examines, and so the most candidates held in memory at once.
	recipientEmailDigestBatch = 500

	// recipientEmailDigestLockKey is the advisory lock every batch's transaction
	// takes, so concurrent walks take turns. A fixed 64-bit key, "recipdig" in
	// ASCII, distinct from the other advisory locks in this repository.
	recipientEmailDigestLockKey int64 = 0x7265636970646967

	// recipientEmailDigestStatementTimeout is the server-side bound on every
	// statement of a step, kept well under pgdriver's 10 s read deadline.
	recipientEmailDigestStatementTimeout = 5 * time.Second

	// recipientEmailDigestStepTimeout is the client-side bound on a whole step:
	// the statement bound plus room for its other round trips, still under the
	// read deadline.
	recipientEmailDigestStepTimeout = recipientEmailDigestStatementTimeout + 2*time.Second

	// recipientEmailDigestBookkeeping is the end of the attempt the runner needs
	// once the migration returns (MarkApplied runs on the attempt's context).
	recipientEmailDigestBookkeeping = 2 * time.Second

	// recipientEmailDigestMinStep is the least time a step is started with.
	recipientEmailDigestMinStep = time.Second

	// recipientEmailDigestReserve is what the walk leaves of a full attempt for
	// the migrations after it and the runner: one step's bound and the
	// bookkeeping. A walk keeps less when its attempt is short; see reserve.
	recipientEmailDigestReserve = recipientEmailDigestStepTimeout + recipientEmailDigestBookkeeping

	// recipientEmailDigestLoggedIDs bounds how many ids of unusable rows the
	// summary names.
	recipientEmailDigestLoggedIDs = 10
)

// errRecipientEmailDigestDeferred is this migration's ErrMigrationDeferred: an
// attempt that stopped with nothing of its own outstanding. Neither a context
// nor a transport error, on purpose; see the header.
var errRecipientEmailDigestDeferred = fmt.Errorf("recipient email digest backfill %w", ErrMigrationDeferred)

// recipientEmailDigestReport is what one run of the walk did.
type recipientEmailDigestReport struct {
	// Stamped is how many recipients received a new version carrying the digest.
	Stamped int
	// Unusable is how many recipients had no digest and an address that
	// normalizes to nothing, so were left as they were.
	Unusable int
	// Changed is how many recipients were candidates when their batch was read
	// and no longer were when the insert ran: the newest version gained a digest
	// from another writer, or its address changed.
	Changed int
	// Batches is how many transactions the walk ran.
	Batches int
}

// RecipientEmailDigest is the digest a recipient row carries for an address:
// the SHA-256, as lower-case hex, of the address trimmed of surrounding
// whitespace and lower-cased -- nothing else is rewritten -- or "" when the text
// cannot be a mailbox (no "@", an empty local part or domain, whitespace
// inside). It is campaigns.EmailDigest, kept here because this package cannot
// import that one; see the header for what holds the two together.
func RecipientEmailDigest(addr string) string {
	normalized := strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndex(normalized, "@")
	if at <= 0 || at == len(normalized)-1 || strings.ContainsAny(normalized, " \t\r\n") {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// registerRecipientEmailDigest adds the migration to the set the SQL files are
// discovered into. It MUST be called from this file; see the header.
func registerRecipientEmailDigest(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, recipientEmailDigestMigrationName) {
		return
	}
	m.MustRegister(
		func(ctx context.Context, db *bun.DB) error {
			_, err := BackfillRecipientEmailDigests(ctx, db, logger)
			return err
		},
		func(ctx context.Context, db *bun.DB) error {
			return RevertRecipientEmailDigests(ctx, db, logger)
		},
	)
}

// recipientEmailDigestSelectSQL reads the next page of recipient ids after the
// cursor, each with its NEWEST version's address and whether that version
// already carries a digest. The LIMIT is on ids, not on versions: DISTINCT ON
// collapses before it applies, and the index (concept, id, "createdAt" DESC)
// answers it in order without sorting.
//
// staged-data: MUST-NOT-GATE -- a MIGRATION must see every row, by definition
// (epic memql#3974, task memql#3984): gated, a staged concept's recipients are
// invisible to the walk and keep a membership no signup can find.
const recipientEmailDigestSelectSQL = `
SELECT id,
       COALESCE(payload->>'email', ''),
       COALESCE(payload->>'emailDigest', '') <> ''
  FROM (SELECT DISTINCT ON (id) id, payload
          FROM "MemoryNodes"
         WHERE concept = ? AND id > ?
         ORDER BY id, "createdAt" DESC
         LIMIT ?) AS newest
 ORDER BY id`

// recipientEmailDigestInsertSQL appends the stamped version of every wanted
// recipient whose newest version is STILL unstamped and STILL has the address
// that was hashed: the newest version copied whole, with the digest added.
// Strictly later than the version it copies, since (id, "createdAt") is the
// primary key. The two guards in the WHERE are what make a walk that lost a race
// -- to another walk, or to the engine writing the same row -- append nothing.
//
// staged-data: MUST-NOT-GATE -- a MIGRATION must see every row, by definition
// (epic memql#3974, task memql#3984).
const recipientEmailDigestInsertSQL = `
INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", concept, type, schema, payload, metadata, provenance)
SELECT latest.id,
       GREATEST(clock_timestamp(), latest."createdAt" + interval '1 millisecond'),
       latest."createdBy", latest.concept, latest.type, latest.schema,
       jsonb_set(latest.payload, '{emailDigest}', to_jsonb(want.digest), true),
       latest.metadata,
       ?::jsonb
  FROM jsonb_to_recordset(?::jsonb) AS want(id text, email text, digest text)
 CROSS JOIN LATERAL (SELECT *
                       FROM "MemoryNodes"
                      WHERE concept = ? AND id = want.id
                      ORDER BY "createdAt" DESC
                      LIMIT 1) AS latest
 WHERE COALESCE(latest.payload->>'emailDigest', '') = ''
   AND latest.payload->>'email' = want.email
ON CONFLICT (id, "createdAt") DO NOTHING`

// recipientEmailDigestRevertSQL deletes one bounded batch of the versions this
// migration appended, found by provenance (the containment test is answered by
// the provenance index).
//
// staged-data: MUST-NOT-GATE -- a MIGRATION must see every row, by definition
// (epic memql#3974, task memql#3984).
const recipientEmailDigestRevertSQL = `
DELETE FROM "MemoryNodes"
 WHERE (id, "createdAt") IN (SELECT id, "createdAt"
                               FROM "MemoryNodes"
                              WHERE concept = ? AND provenance @> ?::jsonb
                              LIMIT ?)`

// recipientEmailDigestProvenance is what every version this migration appends
// carries, and what the down direction finds them by.
func recipientEmailDigestProvenance() string {
	return fmt.Sprintf(`{"kind":"migration","name":%q}`, recipientEmailDigestMigrationName)
}

// BackfillRecipientEmailDigests stamps the digest onto every recipient whose
// newest version has none, and returns how many it stamped. Safe to call any
// number of times, and from any number of replicas at once. db is the migration
// pool at boot; a caller passes its own.
//
// An attempt that is cancelled or runs short of time ends with a deferral
// (errors.Is(err, ErrMigrationDeferred)) after the batches that committed, and
// the next call carries on.
func BackfillRecipientEmailDigests(ctx context.Context, db *bun.DB, logger *slog.Logger) (int, error) {
	report, err := backfillRecipientEmailDigests(ctx, db, logger, recipientEmailDigestBatch)
	return report.Stamped, err
}

// backfillRecipientEmailDigests is the walk, with the batch size injected so a
// test crosses batch boundaries with a handful of rows.
func backfillRecipientEmailDigests(ctx context.Context, db *bun.DB, logger *slog.Logger, batch int) (recipientEmailDigestReport, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if batch < 1 {
		return recipientEmailDigestReport{}, fmt.Errorf("recipient email digest backfill: batch size %d, want at least 1", batch)
	}
	w := &recipientEmailDigestWalk{db: db, logger: logger, batch: batch, started: time.Now(), reserve: recipientEmailDigestReserve}
	if deadline, ok := ctx.Deadline(); ok {
		w.reserve = min(w.reserve, time.Until(deadline)/3)
	}
	if err := w.run(ctx); err != nil {
		return w.report, err
	}
	logger.Info("recipient email digest: done",
		"migration", recipientEmailDigestMigrationName,
		"stamped", w.report.Stamped, "unusable", w.report.Unusable, "changedUnderIt", w.report.Changed,
		"batches", w.report.Batches, "duration", time.Since(w.started).Round(time.Millisecond).String())
	if w.report.Unusable > 0 {
		logger.Warn("recipient email digest: recipients with no usable address were left without a digest",
			"migration", recipientEmailDigestMigrationName, "unusable", w.report.Unusable, "firstIds", w.unusableIDs)
	}
	return w.report, nil
}

// recipientEmailDigestWalk is one run of the walk.
type recipientEmailDigestWalk struct {
	db      *bun.DB
	logger  *slog.Logger
	batch   int
	started time.Time

	// reserve is what this run leaves of its attempt: the full reserve, or a
	// third of what the attempt had left when the run began if that is less, so
	// a short attempt still spends most of itself on the walk. steps counts the
	// steps it started.
	reserve time.Duration
	steps   int

	report      recipientEmailDigestReport
	unusableIDs []string
}

// recipientEmailDigestCandidate is one recipient the select returned.
type recipientEmailDigestCandidate struct {
	id      string
	email   string
	stamped bool
}

func (w *recipientEmailDigestWalk) run(ctx context.Context) error {
	cursor := ""
	for {
		limit, why, ok := w.stepLimit(ctx)
		if !ok {
			return w.deferred(cursor, why)
		}
		w.steps++
		last, examined, err := w.step(ctx, cursor, limit)
		if err != nil {
			return w.fail(err, cursor)
		}
		w.report.Batches++
		if examined < w.batch {
			return nil // a page that is not full is the last
		}
		cursor = last
	}
}

// step runs one batch in its own transaction on a context of its own. It
// returns the last id it examined and how many ids that was.
func (w *recipientEmailDigestWalk) step(ctx context.Context, cursor string, limit time.Duration) (last string, examined int, err error) {
	stepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), limit)
	defer cancel()
	var stamped, unusable, changed int
	var unusableIDs []string
	err = w.db.RunInTx(stepCtx, nil, func(ctx context.Context, tx bun.Tx) error {
		stamped, unusable, changed, unusableIDs = 0, 0, 0, nil
		// The statement bound first, so the wait for the lock is under it too.
		if _, err := tx.ExecContext(ctx, `SELECT set_config('statement_timeout', ?, true)`,
			strconv.FormatInt(recipientEmailDigestStatementTimeout.Milliseconds(), 10)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, recipientEmailDigestLockKey); err != nil {
			return err
		}
		// The candidates are read AFTER the lock: a walk that waited for it sees
		// what the walk it waited for committed.
		candidates, err := w.candidates(ctx, tx, cursor)
		if err != nil {
			return err
		}
		examined = len(candidates)
		if examined > 0 {
			last = candidates[examined-1].id
		}
		type wanted struct {
			ID     string `json:"id"`
			Email  string `json:"email"`
			Digest string `json:"digest"`
		}
		var want []wanted
		for _, c := range candidates {
			if c.stamped {
				continue
			}
			digest := RecipientEmailDigest(c.email)
			if digest == "" {
				unusable++
				unusableIDs = append(unusableIDs, c.id)
				continue
			}
			want = append(want, wanted{ID: c.id, Email: c.email, Digest: digest})
		}
		if len(want) == 0 {
			return nil
		}
		encoded, err := json.Marshal(want)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, recipientEmailDigestInsertSQL,
			recipientEmailDigestProvenance(), string(encoded), recipientEmailDigestConcept)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		stamped = int(n)
		changed = len(want) - stamped
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	w.report.Stamped += stamped
	w.report.Unusable += unusable
	w.report.Changed += changed
	for _, id := range unusableIDs {
		if len(w.unusableIDs) < recipientEmailDigestLoggedIDs {
			w.unusableIDs = append(w.unusableIDs, id)
		}
	}
	return last, examined, nil
}

// candidates reads the page after cursor and closes the rows before anything
// else is sent on the transaction.
func (w *recipientEmailDigestWalk) candidates(ctx context.Context, tx bun.Tx, cursor string) ([]recipientEmailDigestCandidate, error) {
	rows, err := tx.QueryContext(ctx, recipientEmailDigestSelectSQL, recipientEmailDigestConcept, cursor, w.batch)
	if err != nil {
		return nil, fmt.Errorf("selecting the next recipients: %w", err)
	}
	defer rows.Close()
	out := make([]recipientEmailDigestCandidate, 0, w.batch)
	for rows.Next() {
		var c recipientEmailDigestCandidate
		if err := rows.Scan(&c.id, &c.email, &c.stamped); err != nil {
			return nil, fmt.Errorf("scanning a recipient: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the next recipients: %w", err)
	}
	return out, nil
}

// stepLimit says whether the next step may start and, if so, how long it may
// run. It is the one place the attempt's own context is read: between steps, so
// an attempt that was cancelled or has run out stops here, with nothing in
// flight, and what the runner receives is the deferral rather than a context
// error.
//
// A step starts only while the attempt has more left than the run's reserve, and
// never less than one step and the runner's bookkeeping; its bound is cut so that
// it ends before the bookkeeping does.
func (w *recipientEmailDigestWalk) stepLimit(ctx context.Context) (time.Duration, string, bool) {
	if ctx.Err() != nil {
		return 0, "the migration attempt ended", false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return recipientEmailDigestStepTimeout, "", true
	}
	left := time.Until(deadline)
	least := recipientEmailDigestBookkeeping + recipientEmailDigestMinStep
	if keep := max(w.reserve, least); left < keep {
		return 0, fmt.Sprintf("%s was left before the migration deadline, under the %s this attempt keeps for the migrations after it and the runner",
			left.Round(time.Millisecond), keep.Round(time.Millisecond)), false
	}
	return min(recipientEmailDigestStepTimeout, left-recipientEmailDigestBookkeeping), "", true
}

// deferred ends the attempt with nothing outstanding. Every batch that committed
// stands; the next attempt walks again, and a recipient already stamped costs it
// a read and nothing more.
func (w *recipientEmailDigestWalk) deferred(cursor, why string) error {
	w.logger.Info("recipient email digest: stopping with nothing outstanding; the next migration attempt carries on",
		"migration", recipientEmailDigestMigrationName, "why", why, "cursor", cursor,
		"stampedThisAttempt", w.report.Stamped, "batches", w.report.Batches, "duration", time.Since(w.started).Round(time.Millisecond).String())
	return fmt.Errorf("%w: %s; %d recipients stamped by this attempt, and the next attempt walks again from the start",
		errRecipientEmailDigestDeferred, why, w.report.Stamped)
}

// fail says what the walk was doing and how far it got. A deferral passes through
// as it is: it already says both, and it must stay recognisable as itself.
func (w *recipientEmailDigestWalk) fail(err error, cursor string) error {
	if errors.Is(err, ErrMigrationDeferred) {
		return err
	}
	return fmt.Errorf("recipient email digest backfill: the batch after %q failed, after %d recipients stamped by this attempt "+
		"(every committed batch stands and the next attempt walks again): %w", cursor, w.report.Stamped, err)
}

// RevertRecipientEmailDigests is the down direction: delete exactly the versions
// this migration appended, identified by their provenance, in bounded batches.
// Every other version of every recipient is left as it was, so the rows read as
// they did before the up ran.
func RevertRecipientEmailDigests(ctx context.Context, db *bun.DB, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	var deleted int64
	for {
		res, err := db.ExecContext(ctx, recipientEmailDigestRevertSQL,
			recipientEmailDigestConcept, recipientEmailDigestProvenance(), recipientEmailDigestBatch)
		if err != nil {
			return fmt.Errorf("recipient email digest: reverting: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("recipient email digest: reverting: %w", err)
		}
		deleted += n
		if n < recipientEmailDigestBatch {
			break
		}
	}
	logger.Info("recipient email digest: reverted", "migration", recipientEmailDigestMigrationName, "deleted", deleted)
	return nil
}
