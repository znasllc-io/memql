package memql

// account_rows.go -- the one reader that answers "which rows are this
// account's" (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section B, "The
// rule"). The export, the account inventory, the status panel's totals and the
// move act all read through it.
//
// # The rule
//
// A row is one id with many versions, one physical row per version. It is an
// account's when its NEWEST version names the account in `accountId`, in
// either spelling (bare `acme` or canonical `v1:accounts:account:acme`); an
// absent field and "" are no account. A row whose newest version names none is
// in no account, whatever an older version named, and is counted nowhere.
// Every version of an account's row travels with it, whatever account each
// names. The newest version is read across "MemoryNodes" and memory_liveness:
// for the concepts memorynodes.HasLiveness lists (two are account-scoped,
// v1:platform:customDomain and v1:pipelines:run) the newest version can be the
// row's current observation, which decides when it is at least as new as the
// newest versioned row. Nothing writes an account-scoped concept to
// "SecretMemoryNodes"; a change that does extends this reader in the same push.
//
// # Deciding it without reading payloads
//
// On an installation a version's payload is about 3 KB and stored out of line,
// so evaluating payload->>'accountId' on a heap tuple fetches and unpacks the
// whole payload. The decision (accountDecisionSQL) therefore reads narrow
// columns only: per row its newest createdAt and version count from the
// (concept, id, "createdAt" DESC) index, and the newest createdAt among its
// versions that name the account, found through idx_memorynodes_account's own
// condition so the account is matched by the index, not by evaluating the
// expression. The row is the account's when the two are equal (or the
// observation decides). Payloads are then read once, and only for the
// account's rows. account_rows_plan_db_test.go holds that plan.
//
// # What each function costs
//
//   - AccountKindRowIDs: the decision for one kind; the account's versions
//     through the account index and its candidate rows' version counts. Holds
//     the kind's ids in memory.
//   - AccountKindVersions: those ids, then every version of them in batches of
//     AccountKindVersionBatch ids; no statement sorts more than a batch. The
//     export's read of a kind, on its snapshot session.
//   - AccountCensus: the exact count the export proves its file against, plus
//     the rows in no account; reads every row of the given kinds through narrow
//     columns. For the export, not a request.
//   - AccountRowCounts: the census's Rows and Versions from the account's rows
//     only; for requests (the inventory, the status totals).
//   - AccountRowIDs and AccountRowVersions: a page of a list, a probe per row
//     (newestAccountSQL) and an id lookup in every chunk; not for a whole kind.
//   - UnplacedRowIDs: a page of the rows in no account, walking the kind.
//
// A compressed chunk has neither the account index nor an id index; every
// statement here decompresses the kind's segment of it. No installation built
// from this tree is compressed today (memory_nodes_compression.go).
//
// # Who may call
//
// These functions read rows directly and apply no per-row admission, and a
// staged concept's rows are the account's all the same. Every caller sits
// behind a capability (the inventory's app:accounts/inventory, the export's
// own), which is the wall. AccountQuerier is the only handle taken, so a read
// runs on the request pool, on the export's snapshot session, or in a test's
// transaction alike.
//
// Measurements behind these shapes: the account export task report, rounds 2
// and 3. Tests: account_rows_db_test.go, account_rows_plan_db_test.go, and
// integrations/accounts/accounts_db_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// accountCanonicalPrefix is the account concept's id prefix.
const accountCanonicalPrefix = "v1:accounts:account:"

// AccountCensusLine is one kind's exact count: its rows whose newest version
// names the account and every version of those rows, and the kind's rows whose
// newest version names no account.
type AccountCensusLine struct {
	Concept        string
	Rows, Versions int64
	InNoAccount    int64
}

// AccountKindCount is one kind's rows whose newest version names the account,
// and every version of those rows.
type AccountKindCount struct {
	Concept  string
	Rows     int64
	Versions int64
}

// AccountKinds is the sorted names of the account-scoped concepts; travelOnly
// drops kinds that stay.
func AccountKinds(concepts []*memorynodes.Concept, travelOnly bool) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, c := range concepts {
		if !c.AccountScoped() || (travelOnly && !c.Travels()) || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

// BareAccountID accepts either spelling and refuses an id of another concept.
func BareAccountID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("accountId is empty")
	}
	if strings.Contains(raw, ":") && !strings.HasPrefix(raw, accountCanonicalPrefix) {
		return "", fmt.Errorf("%q is not an account id", raw)
	}
	bare := BareShortId(strings.TrimPrefix(raw, accountCanonicalPrefix))
	if bare == "" || strings.Contains(bare, ":") {
		return "", fmt.Errorf("%q is not an account id", raw)
	}
	return bare, nil
}

// accountNamedSQL is idx_memorynodes_account's predicate, repeated in every
// statement that reads through the index so the planner can match it.
const accountNamedSQL = `(payload->>'accountId') IS NOT NULL AND (payload->>'accountId') <> ''`

// newestAccountSQL is the account the newest version of the row c.id of
// concept c.concept names, across the row table and the observation store: a
// probe of one row, for a page of a list, never for a whole kind.
//
// The row table's branch matches the id with = ANY and sorts by the id first,
// the shape versionsOfOneID (executor.go) gives the engine's own newest-version
// read: "createdAt" alone in the ORDER BY of a LIMIT lets TimescaleDB's
// ordered append walk the chunks newest first, costed by the first chunk
// alone, which on an installation read a whole chunk for one id. With the id
// leading the sort every chunk is read through an index the id leads. The
// observation store holds one row per id and is matched by its key. The newer
// of the two answers; the observation wins a tie.
// account_rows_plan_db_test.go holds the plan.
//
// staged-data: MUST-NOT-GATE -- deciding whose a row is; gating the newest version of a staged concept would place the row in no account and leave it out of its account's export.
const newestAccountSQL = `(SELECT v.account FROM (
          (SELECT n.payload->>'accountId' AS account, n."createdAt", 0 AS observed
             FROM "MemoryNodes" n WHERE n.concept = c.concept AND n.id = ANY(ARRAY[c.id])
            ORDER BY n.id, n."createdAt" DESC LIMIT 1)
          UNION ALL
          SELECT l.payload->>'accountId', l."createdAt", 1
            FROM memory_liveness l WHERE l.concept = c.concept AND l.id = c.id
        ) v ORDER BY v."createdAt" DESC, v.observed DESC LIMIT 1)`

// accountRowIDsSQL pages one concept's rows of one account. The candidates are
// sorted in a subquery and decided after, so a page decides only as many
// candidates as it needs to fill its limit.
//
// staged-data: MUST-NOT-GATE -- which rows are an account's; a staged concept's rows are the account's all the same, and the caller's capability is the wall.
const accountRowIDsSQL = `SELECT c.id FROM (
  SELECT $3::text AS concept, id FROM (
    SELECT id FROM "MemoryNodes"
     WHERE ` + accountNamedSQL + `
       AND (payload->>'accountId') IN ($1, $2) AND concept = $3 AND id > $4
    UNION
    SELECT id FROM memory_liveness
     WHERE concept = $3 AND (payload->>'accountId') IN ($1, $2) AND id > $4
  ) k ORDER BY id
) c
 WHERE ` + newestAccountSQL + ` IN ($1, $2)
 ORDER BY c.id
 LIMIT $5`

// AccountRowIDs is the ids of `concept` whose NEWEST version names `account`,
// ascending, after afterID: the rule, for one page of a list.
//
// It finds candidates through the account index and decides each by a probe of
// its newest version (newestAccountSQL), so a page costs a probe per row it
// decides, and every page re-reads the kind's remaining candidates, because the
// index holds no id. That is fine for a page of a list and wrong for a whole
// kind: the export does not use it, it reads AccountKindVersions.
func AccountRowIDs(ctx context.Context, q AccountQuerier, account, concept, afterID string, limit int) ([]string, error) {
	bare, err := BareAccountID(account)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("account rows: limit %d must be positive", limit)
	}
	return scanIDs(ctx, q, accountRowIDsSQL, bare, accountCanonicalPrefix+bare, concept, afterID, limit)
}

// unplacedRowIDsSQL walks the concept's newest versions in id order and keeps
// those naming no account. No index serves "no account" (the partial index
// leaves those versions out by design), so it streams the concept through
// (concept, id, "createdAt" DESC), reading every version of every row it
// passes (a compressed chunk has no such index and is sorted instead), and the
// limit is what bounds one call. A row's observation is joined by id, its key
// in memory_liveness, and decides only when it is at least as new as the
// newest versioned row; an observation never exists without a versioned row
// (the liveness trigger stores one only after a prior version, and pruning
// removes it with the history), so the walk misses no row.
//
// staged-data: MUST-NOT-GATE -- which rows are in no account; a staged concept's rows count the same, and the caller's capability is the wall.
const unplacedRowIDsSQL = `SELECT n.id FROM (
  SELECT DISTINCT ON (id) id, "createdAt", payload->>'accountId' AS account
    FROM "MemoryNodes" WHERE concept = $1 AND id > $2
   ORDER BY id, "createdAt" DESC
) n
LEFT JOIN memory_liveness l ON l.id = n.id AND l."createdAt" >= n."createdAt"
 WHERE COALESCE(CASE WHEN l.id IS NULL THEN n.account ELSE l.payload->>'accountId' END, '') = ''
 ORDER BY n.id
 LIMIT $3`

// UnplacedRowIDs is the ids of `concept` whose newest version names no
// account, ascending, after afterID: a page of a list. It walks the concept
// (see unplacedRowIDsSQL); call it with a modest limit, and count with
// AccountCensus's InNoAccount instead.
func UnplacedRowIDs(ctx context.Context, q AccountQuerier, concept, afterID string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("account rows: limit %d must be positive", limit)
	}
	return scanIDs(ctx, q, unplacedRowIDsSQL, concept, afterID, limit)
}

func scanIDs(ctx context.Context, q AccountQuerier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("account rows: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("account rows: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account rows: %w", err)
	}
	return out, nil
}

// versionColumnsSQL is the columns every version read returns, in the order
// scanVersions reads them.
const versionColumnsSQL = `id, "createdAt", "createdBy", concept, type, schema, payload, metadata, provenance`

// accountRowVersionsSQL is every version of the given ids, in both stores.
//
// staged-data: MUST-NOT-GATE -- the versions of rows already chosen as an account's; the AccountRowIDs reason.
const accountRowVersionsSQL = `SELECT ` + versionColumnsSQL + `, false AS observed
  FROM "MemoryNodes" WHERE concept = $1 AND id = ANY($2::text[])
UNION ALL
SELECT ` + versionColumnsSQL + `, true
  FROM memory_liveness WHERE concept = $1 AND id = ANY($2::text[])
 ORDER BY 1, 2, 10`

// AccountRowVersions calls each for every version of the given ids, ordered by
// id, then createdAt, with observed true for a version held in the observation
// store. It is for a handful of ids (a page of a list): every id is looked up
// in every chunk, and a compressed chunk has no id index. The export reads
// AccountKindVersions. The rows stream from one open statement, so each must
// not issue a statement on q itself; an error from each stops the read and is
// returned as it is.
func AccountRowVersions(ctx context.Context, q AccountQuerier, concept string, ids []string, each func(n memorynodes.MemoryNode, observed bool) error) error {
	if len(ids) == 0 {
		return nil
	}
	return scanVersions(ctx, q, "account row versions", each, accountRowVersionsSQL, concept, pq.Array(ids))
}

// accountKindBatchSQL is AccountRowVersions' statement bounded by the batch's
// first and last id ($3, $4). Without the bound the planner read each batch by
// scanning the kind's concept index and filtering on the ids, the whole kind
// once per batch (64 s for a 212,500-row kind on the bench); with it each
// batch is a range of the (concept, id, "createdAt" DESC) index, and a kind is
// read about once (21 s, nearly all of it the payloads themselves). The
// bounds are the batch's own ends in the order the database returned the ids,
// so they compare in the database's collation.
//
// staged-data: MUST-NOT-GATE -- the versions of rows already chosen as an account's; leaving a staged concept's out would lose them on import.
const accountKindBatchSQL = `SELECT ` + versionColumnsSQL + `, false AS observed
  FROM "MemoryNodes" WHERE concept = $1 AND id >= $3 AND id <= $4 AND id = ANY($2::text[])
UNION ALL
SELECT ` + versionColumnsSQL + `, true
  FROM memory_liveness WHERE concept = $1 AND id >= $3 AND id <= $4 AND id = ANY($2::text[])
 ORDER BY 1, 2, 10`

// AccountKindVersionBatch is how many ids AccountKindVersions reads the
// versions of in one statement. A variable so a test can cross a batch
// boundary with a small fixture. Chosen on the bench (round 3 of the task
// report): one batch's statement sorts only that batch's versions.
var AccountKindVersionBatch = 5000

// AccountKindVersions calls each for every version of every row of `concept`
// whose NEWEST version (across both stores) names `account`, ordered by id
// then createdAt then observed, with observed true for a version held in the
// observation store. The export reads a kind with it on its snapshot session.
//
// The rows are AccountKindRowIDs' (decided from narrow columns, the kind's ids
// held in memory); their versions are then read AccountKindVersionBatch ids at
// a time, so payloads are read once and
// only for the account's rows, and no statement sorts more than a batch (see
// accountKindBatchSQL).
// Because each batch's statement is closed before the next is opened, the
// read is consistent across batches only inside one transaction, which is how
// the export runs it. each must not issue a statement on q itself; an error
// from each stops the read and is returned as it is.
func AccountKindVersions(ctx context.Context, q AccountQuerier, account, concept string, each func(n memorynodes.MemoryNode, observed bool) error) error {
	ids, err := AccountKindRowIDs(ctx, q, account, concept)
	if err != nil {
		return fmt.Errorf("account kind versions: %w", err)
	}
	batch := AccountKindVersionBatch
	if batch <= 0 {
		batch = 1
	}
	for start := 0; start < len(ids); start += batch {
		end := min(start+batch, len(ids))
		if err := scanVersions(ctx, q, "account kind versions", each, accountKindBatchSQL, concept, pq.Array(ids[start:end]), ids[start], ids[end-1]); err != nil {
			return err
		}
	}
	return nil
}

func scanVersions(ctx context.Context, q AccountQuerier, what string, each func(memorynodes.MemoryNode, bool) error, query string, args ...any) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	for rows.Next() {
		var n memorynodes.MemoryNode
		var observed bool
		var schema, payload, metadata, provenance []byte
		if err := rows.Scan(&n.ID, &n.CreatedAt, &n.CreatedBy, &n.Concept, &n.Type, &schema, &payload, &metadata, &provenance, &observed); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		n.Schema, n.Payload = json.RawMessage(schema), json.RawMessage(payload)
		n.Metadata, n.Provenance = json.RawMessage(metadata), json.RawMessage(provenance)
		if err := each(n, observed); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// accountCensusSQL counts accountDecisionSQL's rule per kind, from the same
// narrow sources, plus the rows in no account. Beside accountDecisionSQL's
// three it reads, for every row of the kinds, its newest createdAt and version
// count from the (concept, id, "createdAt" DESC) index (latest, not restricted
// to candidates), and its newest createdAt among the versions that name ANY
// account, through idx_memorynodes_account's predicate with no account
// condition (anyNamed). Its "greater than the empty string" condition is that
// index's leading column read as a range (every non-empty text is greater than
// the empty one), so the planner reads the account index; without it the
// planner filtered another index's heap tuples on the expression. A row is in
// no account when that is not its newest
// version, or when an observation that decides names none.
//
// staged-data: MUST-NOT-GATE -- the count an export proves its file against; leaving a staged concept's rows out would make a file that dropped them look complete.
const accountCensusSQL = `WITH named AS MATERIALIZED (
    SELECT concept, id, max("createdAt") AS at FROM "MemoryNodes"
     WHERE ` + accountNamedSQL + ` AND (payload->>'accountId') IN ($1, $2) AND concept = ANY($3::text[])
     GROUP BY concept, id
), anyNamed AS MATERIALIZED (
    SELECT concept, id, max("createdAt") AS at FROM "MemoryNodes"
     WHERE ` + accountNamedSQL + ` AND (payload->>'accountId') > '' AND concept = ANY($3::text[])
     GROUP BY concept, id
), observed AS MATERIALIZED (
    SELECT concept, id, "createdAt" AS at, COALESCE(payload->>'accountId', '') AS account
      FROM memory_liveness WHERE concept = ANY($3::text[])
), latest AS (
    SELECT concept, id, max("createdAt") AS at, count(*) AS versions FROM "MemoryNodes"
     WHERE concept = ANY($3::text[])
     GROUP BY concept, id
)
SELECT concept,
       count(*) FILTER (WHERE mine),
       COALESCE(sum(versions) FILTER (WHERE mine), 0)::bigint,
       count(*) FILTER (WHERE nobody)
  FROM (
    SELECT l.concept, l.versions + CASE WHEN o.id IS NULL THEN 0 ELSE 1 END AS versions,
           CASE WHEN o.at >= l.at THEN o.account IN ($1, $2) ELSE n.at IS NOT DISTINCT FROM l.at END AS mine,
           CASE WHEN o.at >= l.at THEN o.account = '' ELSE a.at IS DISTINCT FROM l.at END AS nobody
      FROM latest l
      LEFT JOIN named n ON n.concept = l.concept AND n.id = l.id
      LEFT JOIN anyNamed a ON a.concept = l.concept AND a.id = l.id
      LEFT JOIN observed o ON o.concept = l.concept AND o.id = l.id
  ) x
 GROUP BY concept`

// AccountCensus is the exact count of what an export of the account carries,
// per kind: Rows and Versions by the rule, and InNoAccount, the kind's
// rows whose newest version names no account. Sorted by concept. A kind with
// neither rows of the account nor rows in no account has no line.
//
// It reads every row of the given kinds through narrow columns (no payload of
// a versioned row), the cold compressed tail decompressed, and is for the
// export's snapshot, not for a request: a request counts with
// AccountRowCounts, which reads the account's rows only.
func AccountCensus(ctx context.Context, q AccountQuerier, account string, concepts []string) ([]AccountCensusLine, error) {
	bare, err := BareAccountID(account)
	if err != nil {
		return nil, err
	}
	out := []AccountCensusLine{}
	if len(concepts) == 0 {
		return out, nil
	}
	err = withIndexPlans(ctx, q, func() error {
		rows, err := q.QueryContext(ctx, accountCensusSQL, bare, accountCanonicalPrefix+bare, pq.Array(concepts))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l AccountCensusLine
			if err := rows.Scan(&l.Concept, &l.Rows, &l.Versions, &l.InNoAccount); err != nil {
				return err
			}
			if l.Rows > 0 || l.InNoAccount > 0 {
				out = append(out, l)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("account census: %w", err)
	}
	// Sorted here, byte-wise, rather than by the database's collation.
	sort.Slice(out, func(a, b int) bool { return out[a].Concept < out[b].Concept })
	return out, nil
}

// AccountRowCounts is, per kind, the rows whose newest version names the
// account and every version of them: the census's Rows and Versions, without
// its rows in no account, read from the account's rows only (accountDecisionSQL)
// so a request can afford it. Sorted by concept; a kind with no rows has no
// line.
func AccountRowCounts(ctx context.Context, q AccountQuerier, account string, concepts []string) ([]AccountKindCount, error) {
	bare, err := BareAccountID(account)
	if err != nil {
		return nil, err
	}
	out := []AccountKindCount{}
	if len(concepts) == 0 {
		return out, nil
	}
	err = withIndexPlans(ctx, q, func() error {
		few, err := accountDecisionShape(ctx, q, bare, concepts)
		if err != nil {
			return err
		}
		query := accountRowCountsManySQL
		if few {
			query = accountRowCountsFewSQL
		}
		rows, err := q.QueryContext(ctx, query, bare, accountCanonicalPrefix+bare, pq.Array(concepts))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c AccountKindCount
			if err := rows.Scan(&c.Concept, &c.Rows, &c.Versions); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("account row counts: %w", err)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Concept < out[b].Concept })
	return out, nil
}

// accountDecisionSQL is the rule decided from narrow columns, for the kinds in
// $3 and the account spelled $1 (bare) or $2 (canonical): one row per row of
// those kinds whose newest version names the account, with every version of
// it counted (the observation included). No payload of a versioned row is
// read:
//
//   - named: per row, the newest createdAt among its versions that name the
//     account, read through idx_memorynodes_account's own condition (its
//     predicate repeated), so the account is matched by the index and the
//     expression is never evaluated on a heap tuple; only id and createdAt
//     come from the heap.
//   - latest: per row, its newest createdAt and version count, from the
//     (concept, id, "createdAt" DESC) index.
//   - observed: the observation store's row, at least as new as the newest
//     versioned row, decides by its own account (memory_liveness holds one
//     small row per record, so its payload is read there).
//
// A row is the account's when its newest version is one of those that name the
// account: named.at = latest.at, unless the observation decides.
//
// It comes in two shapes, because no one plan serves a small account and the
// big one (round 3 of the task report). FEW restricts latest to the account's
// candidate rows, which the planner answers with a probe of the id index per
// candidate per chunk: a few milliseconds for a small account, 40 s for one
// holding 85% of a 2-million-version bench. MANY reads latest for the whole kind in one pass of
// the index and joins: the right plan for a large account and wasteful for a
// small one. accountDecisionShape chooses with a bounded count of the
// account's index entries; the planner's own estimate of the account's share
// was measured off by a factor of thirty, so it is not trusted to choose.
//
// staged-data: MUST-NOT-GATE -- deciding whose a row is; gating a staged concept's versions would place its rows in no account and leave them out of the account's export.
func accountDecisionSQL(few bool) string {
	restrict := ""
	if few {
		restrict = `
       AND (concept, id) IN (SELECT concept, id FROM named UNION ALL SELECT concept, id FROM observed WHERE names)`
	}
	return `WITH named AS MATERIALIZED (
    SELECT concept, id, max("createdAt") AS at FROM "MemoryNodes"
     WHERE ` + accountNamedSQL + ` AND (payload->>'accountId') IN ($1, $2) AND concept = ANY($3::text[])
     GROUP BY concept, id
), observed AS MATERIALIZED (
    SELECT concept, id, "createdAt" AS at, COALESCE(payload->>'accountId', '') IN ($1, $2) AS names
      FROM memory_liveness WHERE concept = ANY($3::text[])
), latest AS (
    SELECT concept, id, max("createdAt") AS at, count(*) AS versions FROM "MemoryNodes"
     WHERE concept = ANY($3::text[])` + restrict + `
     GROUP BY concept, id
)
SELECT l.concept, l.id, l.versions + CASE WHEN o.id IS NULL THEN 0 ELSE 1 END AS versions
  FROM latest l
  LEFT JOIN named n ON n.concept = l.concept AND n.id = l.id
  LEFT JOIN observed o ON o.concept = l.concept AND o.id = l.id
 WHERE CASE WHEN o.at >= l.at THEN o.names ELSE n.at IS NOT DISTINCT FROM l.at END`
}

// accountDecisionFewLimit is the count of the account's versions in the
// given kinds at and above which the MANY shape is read. On the bench, with
// payloads of installation size and 26 chunks, a candidate cost about 0.7 ms
// in the FEW shape (a probe of each chunk) and the MANY shape about one second
// for a kind of 250,000 rows, so they cross near 1,500 candidates, about 3,000
// versions; the limit stays below that, and the count stops at it.
const accountDecisionFewLimit = 2000

// accountDecisionProbeSQL counts the account's versions in the kinds, up to
// $4, through the account index's condition.
//
// staged-data: MUST-NOT-GATE -- choosing how to read an account's rows; a staged concept's rows are the account's all the same.
const accountDecisionProbeSQL = `SELECT count(*) FROM (
    SELECT 1 FROM "MemoryNodes"
     WHERE ` + accountNamedSQL + ` AND (payload->>'accountId') IN ($1, $2) AND concept = ANY($3::text[])
     LIMIT $4) k`

// accountDecisionShape is true when the account has few enough versions in the
// kinds for the FEW shape.
func accountDecisionShape(ctx context.Context, q AccountQuerier, bare string, kinds []string) (bool, error) {
	rows, err := q.QueryContext(ctx, accountDecisionProbeSQL, bare, accountCanonicalPrefix+bare, pq.Array(kinds), accountDecisionFewLimit)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var n int
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return false, err
		}
	}
	return n < accountDecisionFewLimit, rows.Err()
}

// staged-data: MUST-NOT-GATE -- deciding whose a row is; gating a staged concept's versions would place its rows in no account and leave them out of the account's export.
var (
	accountKindRowIDsFewSQL  = `SELECT d.id FROM (` + accountDecisionSQL(true) + `) d ORDER BY d.id`
	accountKindRowIDsManySQL = `SELECT d.id FROM (` + accountDecisionSQL(false) + `) d ORDER BY d.id`
	accountRowCountsFewSQL   = `SELECT d.concept, count(*), sum(d.versions)::bigint FROM (` + accountDecisionSQL(true) + `) d GROUP BY d.concept`
	accountRowCountsManySQL  = `SELECT d.concept, count(*), sum(d.versions)::bigint FROM (` + accountDecisionSQL(false) + `) d GROUP BY d.concept`
)

// withIndexPlans runs read with sequential and bitmap scans turned off for its
// statements, then puts both settings back. A sequential scan of a kind that
// matches the account's versions by evaluating payload->>'accountId' unpacks
// every payload it passes; a bitmap scan rechecks the expression on a lossy
// page. The planner prefers both for an account that holds most of a kind,
// because it does not price the unpacking. The settings are transaction-local
// (set_config(..., true)): inside a transaction, which is how the export and
// the inventory read, they hold for read's statements alone. On a pool or a
// connection outside a transaction each statement is its own transaction and
// the settings do nothing, and the statements run as the planner chooses.
func withIndexPlans(ctx context.Context, q AccountQuerier, read func() error) error {
	var seq, bitmap string
	if err := queryOne(ctx, q, `SELECT current_setting('enable_seqscan'), current_setting('enable_bitmapscan')`, nil, &seq, &bitmap); err != nil {
		return err
	}
	var ignore1, ignore2 string
	if err := queryOne(ctx, q, `SELECT set_config('enable_seqscan', 'off', true), set_config('enable_bitmapscan', 'off', true)`, nil, &ignore1, &ignore2); err != nil {
		return err
	}
	readErr := read()
	if err := queryOne(ctx, q, `SELECT set_config('enable_seqscan', $1, true), set_config('enable_bitmapscan', $2, true)`, []any{seq, bitmap}, &ignore1, &ignore2); err != nil && readErr == nil {
		readErr = err
	}
	return readErr
}

// queryOne scans the one row a statement answers into dest.
func queryOne(ctx context.Context, q AccountQuerier, query string, args []any, dest ...any) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return errors.New("account rows: no row")
	}
	if err := rows.Scan(dest...); err != nil {
		return err
	}
	return rows.Err()
}

// AccountKindRowIDs is the ids of every row of `concept` whose newest version
// (across both stores) names `account`, ascending in the database's order.
// Exact, decided from narrow columns (accountDecisionSQL), and the whole kind's
// ids are held in memory: the export reads a kind's ids with it and then its
// versions in batches.
func AccountKindRowIDs(ctx context.Context, q AccountQuerier, account, concept string) ([]string, error) {
	bare, err := BareAccountID(account)
	if err != nil {
		return nil, err
	}
	kinds := []string{concept}
	var ids []string
	err = withIndexPlans(ctx, q, func() error {
		few, err := accountDecisionShape(ctx, q, bare, kinds)
		if err != nil {
			return err
		}
		query := accountKindRowIDsManySQL
		if few {
			query = accountKindRowIDsFewSQL
		}
		ids, err = scanIDs(ctx, q, query, bare, accountCanonicalPrefix+bare, pq.Array(kinds))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("account kind row ids: %w", err)
	}
	return ids, nil
}

// accountRowHeadsLimit is the most ids AccountRowHeads takes: a page of a list.
const accountRowHeadsLimit = 500

// accountRowHeadsSQL is, per id of $2, its newest version and its oldest
// createdAt. The span (oldest and newest createdAt) comes from narrow columns
// grouped by id, the id matched with = ANY, so no older version's payload is
// read and no LIMIT over "createdAt" invites an ordered walk of the chunks
// (versionsOfOneID in executor.go has that history). The newest version is
// then read by its key, with the ids matched again by = ANY: the planner
// misjudged the span's size and, joining on the key alone, hashed the whole
// kind (4 s and 2 GB of temporary files for 50 ids on the bench). An
// observation at least as new as the newest versioned row is the newest
// version instead.
//
// staged-data: MUST-NOT-GATE -- a page of an account's rows for the operator who lists them; a staged concept's rows are the account's all the same.
const accountRowHeadsSQL = `WITH span AS MATERIALIZED (
    SELECT id, min("createdAt") AS first, max("createdAt") AS last FROM "MemoryNodes"
     WHERE concept = $1 AND id = ANY($2::text[])
     GROUP BY id
)
SELECT n.id, n."createdAt", n."createdBy", n.concept, n.type, n.schema, n.payload, n.metadata, n.provenance, false, s.first
  FROM span s
  JOIN "MemoryNodes" n ON n.concept = $1 AND n.id = ANY($2::text[]) AND n.id = s.id AND n."createdAt" = s.last
 WHERE NOT EXISTS (SELECT 1 FROM memory_liveness l WHERE l.concept = $1 AND l.id = s.id AND l."createdAt" >= s.last)
UNION ALL
SELECT l.id, l."createdAt", l."createdBy", l.concept, l.type, l.schema, l.payload, l.metadata, l.provenance, true, s.first
  FROM span s
  JOIN memory_liveness l ON l.concept = $1 AND l.id = s.id AND l."createdAt" >= s.last
 ORDER BY 1`

// AccountRowHeads calls each once per id of `concept` that exists, in the
// ids' database order, with the row's newest version across both stores, true
// when that version is the observation, and the row's oldest createdAt. It is
// for a page of a list and refuses more than 500 ids; it reads one payload per
// row (the newest), never the older versions'.
func AccountRowHeads(ctx context.Context, q AccountQuerier, concept string, ids []string, each func(newest memorynodes.MemoryNode, observed bool, firstCreatedAt time.Time) error) error {
	if len(ids) > accountRowHeadsLimit {
		return fmt.Errorf("account row heads: %d ids is more than a page (%d)", len(ids), accountRowHeadsLimit)
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, accountRowHeadsSQL, concept, pq.Array(ids))
	if err != nil {
		return fmt.Errorf("account row heads: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n memorynodes.MemoryNode
		var observed bool
		var first time.Time
		var schema, payload, metadata, provenance []byte
		if err := rows.Scan(&n.ID, &n.CreatedAt, &n.CreatedBy, &n.Concept, &n.Type, &schema, &payload, &metadata, &provenance, &observed, &first); err != nil {
			return fmt.Errorf("account row heads: %w", err)
		}
		n.Schema, n.Payload = json.RawMessage(schema), json.RawMessage(payload)
		n.Metadata, n.Provenance = json.RawMessage(metadata), json.RawMessage(provenance)
		if err := each(n, observed, first); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("account row heads: %w", err)
	}
	return nil
}
