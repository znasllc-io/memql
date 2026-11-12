package memql

// account_rows.go -- the one reader that answers "which rows are this
// account's" (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section B, "The
// rule"). The export, the account inventory, the status panel's totals and the
// move act all read through it or through the rule written down here.
//
// # Two rules, and why there are two
//
// A row is one id with many versions, one physical row per version, and
// `accountId` is written onto its NEWEST version only (the assignment job) or
// copied forward by the write path. Both spellings of an account id match,
// bare `acme` and canonical `v1:accounts:account:acme`; an absent field and ""
// are no account. A row's versions live in "MemoryNodes" and, for the concepts
// memorynodes.HasLiveness lists, its newest version can be its current
// observation in memory_liveness (two of them are account-scoped,
// v1:platform:customDomain and v1:pipelines:run); every statement reads both,
// and an observation wins a createdAt tie (the liveness trigger never makes
// one). Nothing writes an account-scoped concept to "SecretMemoryNodes"; a
// change that does extends this reader in the same push.
//
//   - THE EXACT RULE: a row is an account's when its newest version names the
//     account. A row whose newest version names none is in no account, whatever
//     an older version named. This is what an export carries and what its
//     census proves. Deciding it needs every version of the kind, because the
//     newest version may name nothing and so is not in the account index, and
//     on an installation the cold tail of the row table is compressed, where
//     neither the account index nor an id index exists. So the exact rule is
//     read in ONE pass over the kind's versions, sorted or hashed by id, on the
//     export's snapshot session, never per candidate and never on a request:
//     AccountKindVersions (the export's rows), AccountCensus (its proof), and,
//     for a page of a list, AccountRowIDs with AccountRowVersions.
//
//   - THE COUNTING RULE: a row counts under the account its newest
//     ACCOUNT-NAMING version names. It is read from the versions that name an
//     account (the partial index idx_memorynodes_account, whose predicate every
//     such statement repeats, plus the observations naming one), with no read
//     of a version that names none, which is what lets a request afford it:
//     AccountRowCounts, behind the inventory, and the status panel's totals in
//     integrations/accounts. It differs from the exact rule in ONE case: a row
//     whose newest version names no account after an older one named an
//     account is not in the export, and is still counted here under the
//     account it last named. A move rewrites the newest version to name the new
//     account, so moved rows count the same under both rules.
//
// UnplacedRowIDs lists rows in no account by the exact rule; counting them is
// AccountCensus's InNoAccount.
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
// Measurements behind these shapes: the account export task report, round 2
// (a 26-chunk hypertable with its older half compressed). Tests:
// account_rows_db_test.go, account_rows_plan_db_test.go, and
// integrations/accounts/accounts_db_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

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

// AccountKindCount is one kind's rows counted by the counting rule, and the
// versions of those rows that name an account.
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

// newestKeySQL orders a row's versions inside one aggregate: the version's
// time to the microsecond in a fixed-width text, then the store (1 for an
// observation, so it wins a tie). max(newestKeySQL || account) is then the
// newest version's account after the first 21 characters, which lets a hash
// aggregate decide each row in one pass with no sort of the kind's versions.
// It reads `"createdAt"` and `observed` from the subquery it is applied to.
const newestKeySQL = `to_char("createdAt" AT TIME ZONE 'UTC', 'YYYYMMDDHH24MISSUS') || observed`

// newestKeyWidth is newestKeySQL's width: 20 characters of time, 1 of store.
const newestKeyWidth = 21

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
// ascending, after afterID: the exact rule, for one page of a list.
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
 ORDER BY 1, 2`

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

// accountKindVersionsSQL is every version of every row of one kind whose
// newest version names the account, in (id, createdAt) order: one pass over
// the kind's versions in both stores, a window that gives each version its
// row's newest account, and the filter. Measured against a materialized
// DISTINCT ON of the kind's ids joined back to its versions, which reads the
// kind twice: the window was faster for a large account and its first row
// arrives sooner (round 2 of the task report). Its sort is of the kind's
// whole versions and spills to temporary files for a large kind; it runs on
// the export's session, not on a request.
//
// staged-data: MUST-NOT-GATE -- the rows an export carries; leaving a staged concept's rows out would lose them on import.
const accountKindVersionsSQL = `SELECT ` + versionColumnsSQL + `, observed FROM (
  SELECT v.*, last_value(v.payload->>'accountId') OVER (
           PARTITION BY v.id ORDER BY v."createdAt", v.observed
           ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) AS newest
    FROM (
      SELECT ` + versionColumnsSQL + `, false AS observed FROM "MemoryNodes" WHERE concept = $3
      UNION ALL
      SELECT ` + versionColumnsSQL + `, true FROM memory_liveness WHERE concept = $3
    ) v
) w
 WHERE newest IN ($1, $2)
 ORDER BY id, "createdAt", observed`

// AccountKindVersions calls each for every version of every row of `concept`
// whose NEWEST version (across both stores) names `account`, ordered by id
// then createdAt, with observed true for a version held in the observation
// store. One statement, no probe per id: the export reads a kind with it on
// its snapshot session. The rows stream, so each must not issue a statement on
// q itself; an error from each stops the read and is returned as it is.
func AccountKindVersions(ctx context.Context, q AccountQuerier, account, concept string, each func(n memorynodes.MemoryNode, observed bool) error) error {
	bare, err := BareAccountID(account)
	if err != nil {
		return err
	}
	return scanVersions(ctx, q, "account kind versions", each, accountKindVersionsSQL, bare, accountCanonicalPrefix+bare, concept)
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

// accountCensusSQL is the exact rule counted: per kind, the rows whose newest
// version names the account, every version of them, and the rows whose newest
// version names none. One grouped statement over every version of the given
// kinds in both stores, each row decided by max(newestKeySQL || account) in a
// hash aggregate, so no row is probed and nothing is sorted by time.
//
// staged-data: MUST-NOT-GATE -- the count an export proves its file against; leaving a staged concept's rows out would make a file that dropped them look complete.
var accountCensusSQL = fmt.Sprintf(`SELECT concept,
       count(*) FILTER (WHERE newest IN ($1, $2)),
       COALESCE(sum(versions) FILTER (WHERE newest IN ($1, $2)), 0)::bigint,
       count(*) FILTER (WHERE newest = '')
  FROM (
    SELECT concept, id, count(*) AS versions,
           substr(max(%s || COALESCE(account, '')), %d) AS newest
      FROM (
        SELECT concept, id, "createdAt", payload->>'accountId' AS account, 0 AS observed
          FROM "MemoryNodes" WHERE concept = ANY($3::text[])
        UNION ALL
        SELECT concept, id, "createdAt", payload->>'accountId', 1
          FROM memory_liveness WHERE concept = ANY($3::text[])
      ) v
     GROUP BY concept, id
  ) r
 GROUP BY concept`, newestKeySQL, newestKeyWidth+1)

// AccountCensus is the exact count of what an export of the account carries,
// per kind: Rows and Versions by the exact rule, and InNoAccount, the kind's
// rows whose newest version names no account. Sorted by concept. A kind with
// neither rows of the account nor rows in no account has no line.
//
// It reads every version of the given kinds, the cold compressed tail
// included, and is for the export's snapshot, not for a request: a request
// counts with AccountRowCounts.
func AccountCensus(ctx context.Context, q AccountQuerier, account string, concepts []string) ([]AccountCensusLine, error) {
	bare, err := BareAccountID(account)
	if err != nil {
		return nil, err
	}
	out := []AccountCensusLine{}
	if len(concepts) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, accountCensusSQL, bare, accountCanonicalPrefix+bare, pq.Array(concepts))
	if err != nil {
		return nil, fmt.Errorf("account census: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l AccountCensusLine
		if err := rows.Scan(&l.Concept, &l.Rows, &l.Versions, &l.InNoAccount); err != nil {
			return nil, fmt.Errorf("account census: %w", err)
		}
		if l.Rows > 0 || l.InNoAccount > 0 {
			out = append(out, l)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account census: %w", err)
	}
	// Sorted here, byte-wise, rather than by the database's collation.
	sort.Slice(out, func(a, b int) bool { return out[a].Concept < out[b].Concept })
	return out, nil
}

// AccountNamingNewestSQL decides every row of the given kinds ($1) that has a
// version naming an account by the counting rule: the account its newest
// ACCOUNT-NAMING version names, raw (either spelling), with the count of its
// versions that name an account. Read only from versions that name an account,
// through idx_memorynodes_account's predicate, and the observations naming one,
// in one hash aggregate. The per-account count and the status panel's totals
// in integrations/accounts both group it, so the two cannot disagree.
//
// staged-data: MUST-NOT-GATE -- whose rows are whose for an inventory; a staged concept's rows are the account's all the same.
var AccountNamingNewestSQL = fmt.Sprintf(`SELECT concept, id, count(*) AS versions,
       substr(max(%s || account), %d) AS account
  FROM (
    SELECT concept, id, "createdAt", payload->>'accountId' AS account, 0 AS observed
      FROM "MemoryNodes" WHERE `+accountNamedSQL+` AND concept = ANY($1::text[])
    UNION ALL
    SELECT concept, id, "createdAt", payload->>'accountId', 1
      FROM memory_liveness WHERE `+accountNamedSQL+` AND concept = ANY($1::text[])
  ) v
 GROUP BY concept, id`, newestKeySQL, newestKeyWidth+1)

// accountRowCountsSQL is AccountNamingNewestSQL for one account.
//
// staged-data: MUST-NOT-GATE -- the AccountNamingNewestSQL reason, for one account.
var accountRowCountsSQL = `SELECT concept, count(*), sum(versions)::bigint
  FROM (` + AccountNamingNewestSQL + `) r
 WHERE account IN ($2, $3)
 GROUP BY concept`

// AccountRowCounts is, per kind, the rows counted under the account by the
// counting rule (the account a row's newest account-naming version names) and
// the versions of those rows that name an account. Sorted by concept; a kind
// with no rows has no line. For requests: it reads only versions that name an
// account. It is not what an export carries: a row whose newest version names
// no account after an older one named this account is counted here and is in
// no account by the exact rule (see the file header).
func AccountRowCounts(ctx context.Context, q AccountQuerier, account string, concepts []string) ([]AccountKindCount, error) {
	bare, err := BareAccountID(account)
	if err != nil {
		return nil, err
	}
	out := []AccountKindCount{}
	if len(concepts) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, accountRowCountsSQL, pq.Array(concepts), bare, accountCanonicalPrefix+bare)
	if err != nil {
		return nil, fmt.Errorf("account row counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c AccountKindCount
		if err := rows.Scan(&c.Concept, &c.Rows, &c.Versions); err != nil {
			return nil, fmt.Errorf("account row counts: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account row counts: %w", err)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Concept < out[b].Concept })
	return out, nil
}
