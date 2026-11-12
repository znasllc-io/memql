package memql

// account_rows.go -- the one reader that answers "which rows are this
// account's" (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section B, "The
// rule"). The export, the account inventory and the move act all read through
// it, so the three can never disagree about whose a row is.
//
// # The rule
//
// A row is one id with many versions. It is an account's when its NEWEST
// version names the account in `accountId`, in either spelling (bare `acme` or
// canonical `v1:accounts:account:acme`); an absent field and "" are no
// account. Older versions do not decide anything: the assignment job writes the
// account onto the newest version only, so older versions usually name none,
// and after a move they name the account the row left. Every version of a row
// travels with it, whatever account each names.
//
// The newest version is read across "MemoryNodes" and memory_liveness, the
// union "MemoryNodesObserved" is (memorynodes.ObservedRowsSQL): for the
// concepts memorynodes.HasLiveness lists, the newest version can be the row's
// current observation, and two of them are account-scoped
// (v1:platform:customDomain, v1:pipelines:run). The statements name the two
// tables rather than the view so each half can use its own index. Nothing
// writes an account-scoped concept to "SecretMemoryNodes"; a change that does
// extends this reader in the same push.
//
// # How it reads
//
// AccountRowIDs and AccountCensus never walk a concept to answer for one
// account. Candidates are the ids with ANY version naming the account, found
// through the partial index idx_memorynodes_account
// (component/database/20261112110000_account_index.go), whose predicate every
// such statement repeats so the planner can match it, plus the observation
// rows naming it (memory_liveness holds one row per record, so it has no such
// index and needs none). Each candidate is then decided by its newest version,
// a backward read of the primary key. A row whose newest version names the
// account has that version among the candidates, so nothing is missed.
//
// UnplacedRowIDs has no index to help it: "names no account" is exactly what
// the partial index leaves out. It walks the concept's newest versions in id
// order through (concept, id, "createdAt" DESC) and stops at its limit, so a
// concept with few such rows is read nearly whole to find them.
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
// Tests: account_rows_db_test.go; integrations/accounts/accounts_db_test.go for
// the inventory that counts through AccountCensus.

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

// AccountCensusLine is one kind's count: its rows whose newest version names
// the account, and every version of those rows.
type AccountCensusLine struct {
	Concept        string
	Rows, Versions int64
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
// concept c.concept names, across the row table and the observation store.
// On a createdAt tie the observation wins: the liveness trigger only ever
// stores an observation newer than the version it follows.
//
// staged-data: MUST-NOT-GATE -- deciding whose a row is; gating the newest version of a staged concept would place the row in no account and leave it out of its account's export.
const newestAccountSQL = `(SELECT v.account FROM (
          SELECT n.payload->>'accountId' AS account, n."createdAt", 0 AS observed
            FROM "MemoryNodes" n WHERE n.concept = c.concept AND n.id = c.id
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
// ascending, after afterID.
//
// Cost. A page is not independent of the pages before it. The account index
// keys on (account, concept) and holds no id, so `id > afterID` is a filter
// applied after each candidate's heap fetch, and every page re-reads and
// re-sorts all the remaining candidates of the kind before deciding the first
// `limit` of them. A walk over N rows in pages of P therefore costs about
// N*N/(2*P) candidate fetches, against N for one pass. A caller walking a whole
// kind (the export) should ask for large pages: for a kind of a few million
// rows, pages of 100,000 ids. At 3 million rows that is 30 pages and about 45
// million fetches, some fifteen passes over the account's index entries, while
// a page's ids stay a few megabytes in memory; halving the page doubles the
// fetches. Read the versions of a page in smaller batches through
// AccountRowVersions; the id page and the version batch need not be the same
// size.
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
// passes, and the limit is what bounds one call. A row's observation is joined
// by id, its key in memory_liveness, and decides only when it is at least as
// new as the newest versioned row; an observation never exists without a
// versioned row (the liveness trigger stores one only after a prior version,
// and pruning removes it with the history), so the walk misses no row.
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
// account, ascending, after afterID. It walks the concept (see
// unplacedRowIDsSQL); call it with a modest limit.
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

// accountRowVersionsSQL is every version of the given ids, in both stores.
//
// staged-data: MUST-NOT-GATE -- the versions of rows already chosen as an account's; the AccountRowIDs reason.
const accountRowVersionsSQL = `SELECT id, "createdAt", "createdBy", concept, type, schema, payload, metadata, provenance
  FROM "MemoryNodes" WHERE concept = $1 AND id = ANY($2::text[])
UNION ALL
SELECT id, "createdAt", "createdBy", concept, type, schema, payload, metadata, provenance
  FROM memory_liveness WHERE concept = $1 AND id = ANY($2::text[])
 ORDER BY 1, 2`

// AccountRowVersions calls each for every version of the given ids, ordered by
// id, then createdAt. A version's schema is the stored column as it is, usually
// a {"$ref": ...} into memory_node_schemas. The rows stream from one open
// statement, so each must not issue a statement on q itself; an error from
// each stops the read and is returned as it is.
func AccountRowVersions(ctx context.Context, q AccountQuerier, concept string, ids []string, each func(memorynodes.MemoryNode) error) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, accountRowVersionsSQL, concept, pq.Array(ids))
	if err != nil {
		return fmt.Errorf("account row versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n memorynodes.MemoryNode
		var schema, payload, metadata, provenance []byte
		if err := rows.Scan(&n.ID, &n.CreatedAt, &n.CreatedBy, &n.Concept, &n.Type, &schema, &payload, &metadata, &provenance); err != nil {
			return fmt.Errorf("account row versions: %w", err)
		}
		n.Schema, n.Payload = json.RawMessage(schema), json.RawMessage(payload)
		n.Metadata, n.Provenance = json.RawMessage(metadata), json.RawMessage(provenance)
		if err := each(n); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("account row versions: %w", err)
	}
	return nil
}

// accountCensusSQL counts, per concept, the rows whose newest version names the
// account and every version of them, by AccountRowIDs' rule but in one grouped
// statement of its own: an export checks what it wrote against this count, and
// a count made by re-running the walk would prove nothing.
//
// staged-data: MUST-NOT-GATE -- the AccountRowIDs reason, counted.
const accountCensusSQL = `SELECT c.concept, count(*),
       sum((SELECT count(*) FROM "MemoryNodes" n WHERE n.concept = c.concept AND n.id = c.id)
         + (SELECT count(*) FROM memory_liveness l WHERE l.concept = c.concept AND l.id = c.id))::bigint
  FROM (
    SELECT concept, id FROM "MemoryNodes"
     WHERE ` + accountNamedSQL + `
       AND (payload->>'accountId') IN ($1, $2) AND concept = ANY($3::text[])
    UNION
    SELECT concept, id FROM memory_liveness
     WHERE concept = ANY($3::text[]) AND (payload->>'accountId') IN ($1, $2)
  ) c
 WHERE ` + newestAccountSQL + ` IN ($1, $2)
 GROUP BY c.concept`

// AccountCensus counts rows and ALL their versions per concept, by the same
// rule as AccountRowIDs, sorted by concept. Only the given concepts are
// counted, and a concept with no rows has no line.
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
		if err := rows.Scan(&l.Concept, &l.Rows, &l.Versions); err != nil {
			return nil, fmt.Errorf("account census: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account census: %w", err)
	}
	// Sorted here, byte-wise, rather than by the database's collation.
	sort.Slice(out, func(a, b int) bool { return out[a].Concept < out[b].Concept })
	return out, nil
}
