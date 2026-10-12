package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// The recipient digest sweep: put v1:campaigns:recipient.emailDigest on the rows
// written without it (memql#6022, F9).
//
// ===========================================================================
// WHAT IT DOES, AND WHY
// ===========================================================================
// addRecipient stamps every recipient it writes with emailDigest, the SHA-256 of
// the address. A newsletter signup and an email rule's row-address send find the
// rows a mailbox holds by that key (recipientInAudienceByDigest) instead of
// walking the audience's whole roster. A recipient written without one -- before
// the field existed, by a replica still running the previous release, or by a raw
// write -- is invisible to that lookup, so the same callers also read the
// audience's rows that have none (recipientsWithoutDigestInAudience) and compare
// addresses. This sweep is what empties that second set. Until it has, a signup
// costs a second read and, in an audience holding a page of such rows, the
// roster; it never costs a missed opt-out.
//
// ===========================================================================
// WHY A SWEEP AFTER THE ROLLOUT, AND NOT A MIGRATION BEFORE IT
// ===========================================================================
// The recipient concept is closed: a payload carrying a field the concept does
// not declare is refused. A replica still running the release BEFORE emailDigest
// does not declare it, so it cannot write any recipient that carries one -- an
// unsubscribe it receives answers the retry page, a bounce or a complaint it
// handles is not recorded, an operator's by-hand change through it errors -- for
// as long as that replica serves. A migration runs in the pre-deploy Job, before
// the new pods roll, so stamping there would put the field on every recipient in
// the cluster while the previous release is still serving: the whole rollout would
// fail those writes, and a rollback would leave the rows stamped for good.
//
// So nothing here runs before the rollout. The campaigns worker
// (component/campaigns, digest_sweep.go) runs it ten minutes after a node boots
// and hourly after that. By the first pass a rollout is normally over, and the rows
// the previous release wrote meanwhile are caught by the next. Two limits, stated
// plainly:
//
//   - the ten minutes is a guess about the rollout, not a check of it: a pass that
//     starts while a previous-release replica still serves stamps rows that replica
//     cannot then update, the failure above, limited to the recipients the pass
//     reached and ending with the rollout;
//   - every NEW recipient is stamped by addRecipient from the new release's first
//     request, which no sweep can postpone.
//
// ===========================================================================
// THE NORMALIZATION: TRIM AND LOWER-CASE, NOTHING ELSE
// ===========================================================================
// RecipientEmailStamp is what addRecipient stamps (hash(lower(trim(args.email)))
// in the DSL): the SHA-256, as lower-case hex, of the text trimmed of surrounding
// whitespace and lower-cased -- whatever the text is. The sweep gives a recipient
// the digest addRecipient would have given it, so a row it fills in and a row
// addRecipient wrote are the same.
//
// RecipientEmailDigest is the key a LOOKUP uses (campaigns.EmailDigest): the same
// value for an address, and "" for text that cannot be a mailbox -- no "@", an
// empty local part or domain, whitespace inside -- because nobody looks that up.
// Nothing else is rewritten: sub-addressing and dots in the local part are the
// person's own distinctions, and an internationalized domain is hashed as given.
//
// Text that cannot be a mailbox is stamped too. A scrubbed recipient (the deletion
// path zeroes the email) and a malformed import hold such text, and stamping it is
// what takes the row out of the set of rows without a digest: left alone, every
// one of them would stay in the page a signup reads for as long as it exists, and
// an audience holding a page of them would read its roster on every signup. The
// digest of text that is not an address matches no lookup, because a lookup is
// only made for an address and two different texts do not share a digest.
//
// The rule is a copy because this package cannot import component/campaigns, and
// it is hashed in Go rather than in SQL because Postgres's lower() follows the
// database's locale and btrim() trims spaces only, while the engine folds with
// Go's Unicode rules; a digest the SQL computed could differ from the one the
// lookup asks for, silently. Tests hold the places that write it together:
// TestRecipientDigestBackfillMatchesEmailDigest (component/campaigns) holds
// RecipientEmailDigest to the original, and
// TestAddRecipientStampsTheDigestTheGoFunctionComputes holds the DSL stamp to
// RecipientEmailStamp.
//
// ===========================================================================
// WHICH ROWS, AND WHAT IS APPENDED
// ===========================================================================
// Selection is by concept (`v1:campaigns:recipient`) and reads each id's NEWEST
// version -- `DISTINCT ON (id) ... ORDER BY id, "createdAt" DESC`, the collapse
// every engine read makes -- so an older version without a digest behind a
// newer one with it is not a candidate, and an id whose newest version lacks one
// is, whatever an older version carried.
//
// MemoryNodes is append-only, so the repair is ONE NEW VERSION per row: the
// newest version copied whole -- createdBy, type, schema, metadata, every payload
// key -- with emailDigest added and nothing else changed, under a `system`
// provenance named for this sweep.
//
// The new version is stamped ONE MICROSECOND after the version it copies.
// (id, "createdAt") is the primary key and the newest version is the one every
// read returns, so it has to sort after its source -- and no later than that,
// because a write that lands while the statement runs (a hand unsubscribe, a
// bounce) carries a createdAt of its own, later than the source by far more than a
// microsecond, and must stay the newest. A version stamped at the clock's "now"
// would sort after such a write and shadow it.
//
// ===========================================================================
// SAFE TO RUN ON TWO REPLICAS AT ONCE, AND EVERY PASS IS CHEAP WHEN IDLE
// ===========================================================================
// Every batch takes one advisory lock for its transaction and reads its
// candidates AFTER holding it, and the insert re-checks, in the same statement,
// that the newest version still has no digest and still has the address that was
// hashed. Two passes therefore take turns batch by batch, and the second finds the
// first's work already stamped; each recipient ends with exactly one new version,
// however many passes ran. Nothing is cached: every read is of the table.
//
// A pass over recipients that all carry a digest writes nothing. It reads the
// newest version of every recipient in the cluster, recipientEmailDigestBatch ids
// per statement, on the latest-row index (concept, id, "createdAt" DESC) that
// 20260913000000 and 20260914000000 ensure.
//
// ===========================================================================
// BOUNDED IN MEMORY, IN STATEMENTS AND IN TIME
// ===========================================================================
// The walk is keyset-paginated by id: at most one batch of candidates is held at a
// time, and each batch is two statements of bounded size in one transaction. Each
// transaction runs under a transaction-local statement_timeout, so a statement that
// runs long is cancelled by the SERVER with an error the client receives, rather
// than abandoned by the client over a backend that is still running. A step runs on
// a context of its own -- the pass's values without its cancellation -- so a node
// shutting down cannot cut one off midway; the pass's context is read BETWEEN
// steps, and a cancelled pass ends there. The next pass walks again from the
// start: recipients already stamped cost a read and nothing else.
//
// ===========================================================================
// ROWS WRITTEN WHILE IT RUNS
// ===========================================================================
// A replica still running the previous release can add a recipient with no digest
// after the walk has passed its id. The next pass stamps it, and until then every
// lookup also reads the audience's rows that have none (campaigns
// Store.MailboxMembers), so the gap costs a read and never a missed opt-out.
// Updates do not close it -- a read-merge keeps the payload it read, digest or
// not -- but the walk is idempotent, and an addRecipient for the same id stamps
// its row.

const (
	// recipientEmailDigestProvenanceName is what every version this sweep appends
	// is attributed to: the engine itself repairing derived data, which the
	// provenance enum calls `system`, as the other boot-time reconcilers do.
	recipientEmailDigestProvenanceName = "reconcile:campaigns-recipient-digest"

	// recipientEmailDigestConcept is the one concept this sweep reads or writes.
	recipientEmailDigestConcept = "v1:campaigns:recipient"

	// recipientEmailDigestBatch is how many recipient ids one transaction
	// examines, and so the most candidates held in memory at once.
	recipientEmailDigestBatch = 500

	// recipientEmailDigestLockKey is the advisory lock every batch's transaction
	// takes, so concurrent passes take turns. A fixed 64-bit key, "recipdig" in
	// ASCII, distinct from the other advisory locks in this repository.
	recipientEmailDigestLockKey int64 = 0x7265636970646967

	// recipientEmailDigestStatementTimeout is the server-side bound on every
	// statement of a step, kept well under pgdriver's 10 s read deadline.
	recipientEmailDigestStatementTimeout = 5 * time.Second

	// recipientEmailDigestStepTimeout is the client-side bound on a whole step:
	// the statement bound plus room for its other round trips, still under the
	// read deadline.
	recipientEmailDigestStepTimeout = recipientEmailDigestStatementTimeout + 2*time.Second
)

// recipientEmailDigestReport is what one pass of the walk did.
type recipientEmailDigestReport struct {
	// Stamped is how many recipients received a new version carrying the digest.
	Stamped int
	// NotAddresses is how many of those held text that cannot be a mailbox (a
	// scrubbed recipient, a malformed import) and were stamped with the digest of
	// that text, as addRecipient stamps it.
	NotAddresses int
	// Changed is how many recipients were candidates when their batch was read
	// and no longer were when the insert ran: the newest version gained a digest
	// from another writer, or its address changed.
	Changed int
	// Batches is how many transactions the walk ran.
	Batches int
}

// RecipientEmailStamp is the digest addRecipient stamps on a recipient and this
// sweep gives the ones that have none: the SHA-256, as lower-case hex, of the text
// trimmed of surrounding whitespace and lower-cased. Whatever the text is; see the
// header for why text that is not an address is stamped too.
func RecipientEmailStamp(text string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(text))))
	return hex.EncodeToString(sum[:])
}

// RecipientEmailDigest is the key a lookup of an address uses: the stamp of the
// address, or "" when the text cannot be a mailbox (no "@", an empty local part or
// domain, whitespace inside). It is campaigns.EmailDigest, kept here because this
// package cannot import that one; see the header for what holds the two together.
func RecipientEmailDigest(addr string) string {
	normalized := strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndex(normalized, "@")
	if at <= 0 || at == len(normalized)-1 || strings.ContainsAny(normalized, " \t\r\n") {
		return ""
	}
	return RecipientEmailStamp(normalized)
}

// recipientEmailDigestSelectSQL reads the next page of recipient ids after the
// cursor, each with its NEWEST version's address and whether that version
// already carries a digest. The LIMIT is on ids, not on versions: DISTINCT ON
// collapses before it applies, and the index (concept, id, "createdAt" DESC)
// answers it in order without sorting.
//
// staged-data: MUST-NOT-GATE -- a maintenance pass must see every row, by
// definition (epic memql#3974, task memql#3984): gated, a staged concept's
// recipients are invisible to the walk and keep a membership no signup can find.
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
// that was hashed: the newest version copied whole, with the digest added, one
// microsecond after the version it copies (see the header for why that and not
// the clock). The two guards in the WHERE are what make a walk that lost a race
// -- to another walk, or to the engine writing the same row -- append nothing.
//
// staged-data: MUST-NOT-GATE -- a maintenance pass must see every row, by
// definition (epic memql#3974, task memql#3984).
const recipientEmailDigestInsertSQL = `
INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", concept, type, schema, payload, metadata, provenance)
SELECT latest.id,
       latest."createdAt" + interval '1 microsecond',
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
   AND COALESCE(latest.payload->>'email', '') = want.email
ON CONFLICT (id, "createdAt") DO NOTHING`

// recipientEmailDigestProvenance is what every version this sweep appends
// carries.
func recipientEmailDigestProvenance() string {
	return fmt.Sprintf(`{"kind":"system","name":%q}`, recipientEmailDigestProvenanceName)
}

// BackfillRecipientEmailDigests stamps the digest onto every recipient whose
// newest version has none, and returns how many it stamped. Safe to call any
// number of times, and from any number of replicas at once. db is the pool the
// caller owns.
//
// A pass whose context is cancelled ends between two batches, after the batches
// that committed, with an error wrapping the context's; the next call carries on.
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
		return recipientEmailDigestReport{}, fmt.Errorf("recipient email digest sweep: batch size %d, want at least 1", batch)
	}
	w := &recipientEmailDigestWalk{db: db, batch: batch, started: time.Now()}
	if err := w.run(ctx); err != nil {
		return w.report, err
	}
	// An idle pass is the common one and says nothing above debug: the worker runs
	// it hourly, and a line an hour that reports nothing is how a log stops being read.
	level := slog.LevelDebug
	if w.report.Stamped > 0 || w.report.Changed > 0 {
		level = slog.LevelInfo
	}
	logger.Log(ctx, level, "recipient email digest sweep: done",
		"stamped", w.report.Stamped, "notAddresses", w.report.NotAddresses, "changedUnderIt", w.report.Changed,
		"batches", w.report.Batches, "duration", time.Since(w.started).Round(time.Millisecond).String())
	return w.report, nil
}

// recipientEmailDigestWalk is one pass of the walk.
type recipientEmailDigestWalk struct {
	db      *bun.DB
	batch   int
	started time.Time
	report  recipientEmailDigestReport
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
		// The one place the pass's own context is read: between steps, so a
		// cancelled pass stops with nothing in flight.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("recipient email digest sweep: stopped after %d recipients stamped; the next pass walks again from the start: %w",
				w.report.Stamped, err)
		}
		last, examined, err := w.step(ctx, cursor)
		if err != nil {
			return fmt.Errorf("recipient email digest sweep: the batch after %q failed, after %d recipients stamped by this pass "+
				"(every committed batch stands and the next pass walks again): %w", cursor, w.report.Stamped, err)
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
func (w *recipientEmailDigestWalk) step(ctx context.Context, cursor string) (last string, examined int, err error) {
	stepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recipientEmailDigestStepTimeout)
	defer cancel()
	var stamped, notAddresses, changed int
	err = w.db.RunInTx(stepCtx, nil, func(ctx context.Context, tx bun.Tx) error {
		stamped, notAddresses, changed = 0, 0, 0
		// The statement bound first, so the wait for the lock is under it too.
		if _, err := tx.ExecContext(ctx, `SELECT set_config('statement_timeout', ?, true)`,
			strconv.FormatInt(recipientEmailDigestStatementTimeout.Milliseconds(), 10)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, recipientEmailDigestLockKey); err != nil {
			return err
		}
		// The candidates are read AFTER the lock: a pass that waited for it sees
		// what the pass it waited for committed.
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
			want = append(want, wanted{ID: c.id, Email: c.email, Digest: RecipientEmailStamp(c.email)})
			if RecipientEmailDigest(c.email) == "" {
				notAddresses++
			}
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
	w.report.NotAddresses += notAddresses
	w.report.Changed += changed
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
