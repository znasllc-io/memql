package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/znasllc-io/memql/component/accountexport"
)

// account_handover.go -- email blocks in an account export
// (docs/superpowers/specs/2026-11-12-account-export-design.md, section B,
// "Outside the row table").
//
// A cluster-wide block (a v1:campaigns:suppression row with no account, whose
// id is the address digest) belongs to the cluster, so the row never travels.
// But a target that imports the account's recipients without it would mail an
// address this cluster refuses to. So for each address the account itself
// holds that is blocked here, the file carries the digest, the reason and the
// date, marked as coming from the cluster.
//
// Only the account's own addresses are asked about: the digests come from the
// carried rows, and a block no carried row names is never read. That is what
// keeps the file from saying anything about another account's recipients. An
// account's own opt-outs are rows of the account and travel as rows; another
// account's are never looked at, because their ids are keyed by that account.

const emailBlocksHandOver = "email-blocks"

const suppressionConcept = "v1:campaigns:suppression"

// addressKinds are the kinds whose rows name an address by emailDigest.
// recipient is the one whose older rows may have no digest yet (the digest
// sweep fills it later); its address decides for those.
var addressKinds = []string{
	"v1:campaigns:consentEvent",
	"v1:campaigns:journeyEnrollment",
	"v1:campaigns:newsletterSignup",
	"v1:campaigns:recipient",
}

// emailBlockLine is one line of blocks.jsonl.
type emailBlockLine struct {
	EmailDigest string `json:"emailDigest"`
	Reason      string `json:"reason"`
	BlockedAt   string `json:"blockedAt"`
	Source      string `json:"source"`
}

func init() {
	accountexport.RegisterHandOver(emailBlocksHandOver, handOverEmailBlocks)
}

func handOverEmailBlocks(ctx context.Context, q accountexport.Querier, in accountexport.HandOverInput, out accountexport.HandOverSink) error {
	digests := map[string]bool{}
	for _, kind := range addressKinds {
		if err := carriedDigests(ctx, q, kind, in.RowIDs(kind), digests); err != nil {
			return fmt.Errorf("%s: %w", kind, err)
		}
	}
	if len(digests) == 0 {
		return nil
	}
	lines, err := clusterBlocks(ctx, q, digests)
	if err != nil {
		return err
	}
	for _, l := range lines {
		if err := out.Line(accountexport.MemberBlocks, l); err != nil {
			return err
		}
	}
	return nil
}

// bothSpellings is the ids as a JSON array, bare and canonical: a stored id
// is canonical, and a row written before ids were canonicalized may be bare.
func bothSpellings(concept string, ids []string) string {
	all := make([]string, 0, 2*len(ids))
	for _, id := range ids {
		all = append(all, id, concept+":"+id)
	}
	b, _ := json.Marshal(all)
	return string(b)
}

// bareIDSQL is a row's id without its concept prefix ($1), so the versions
// of one row group together whichever way each spelled its id.
const bareIDSQL = `CASE WHEN id LIKE $1 || ':%' THEN substr(id, length($1) + 2) ELSE id END`

// carriedAddressSQL reads the newest version of each carried row of a kind:
// the address it names now, not one an older version named.
//
// staged-data: MUST-NOT-GATE -- the rows are the ones the file already carries, chosen by the export; a staged kind's addresses are the account's all the same, and leaving them out would let the target mail an address this cluster blocks.
const carriedAddressSQL = `SELECT COALESCE(payload->>'emailDigest', ''), COALESCE(payload->>'email', '')
  FROM (SELECT DISTINCT ON (` + bareIDSQL + `) payload
          FROM "MemoryNodes"
         WHERE concept = $1
           AND id IN (SELECT jsonb_array_elements_text($2::jsonb))
         ORDER BY ` + bareIDSQL + `, "createdAt" DESC) newest`

func carriedDigests(ctx context.Context, q accountexport.Querier, kind string, ids []string, into map[string]bool) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, carriedAddressSQL, kind, bothSpellings(kind, ids))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var digest, email string
		if err := rows.Scan(&digest, &email); err != nil {
			return err
		}
		// The address is read only to digest it, as the sweep would; it is
		// never written anywhere.
		if digest == "" {
			digest = EmailDigest(email)
		}
		if digest != "" {
			into[digest] = true
		}
	}
	return rows.Err()
}

// clusterBlockSQL reads the newest version of the suppression rows whose id
// is one of the digests. The newest version decides: one that names an
// account is an account's opt-out, and one that is lifted no longer blocks.
//
// staged-data: MUST-NOT-GATE -- a cluster-wide block is the cluster's own row, read for the account's own addresses only; it is not carried as a row, and missing it would let the target mail an address this cluster refuses.
const clusterBlockSQL = `SELECT digest, COALESCE(payload->>'reason', ''), COALESCE(payload->>'suppressedAt', '')
  FROM (SELECT DISTINCT ON (` + bareIDSQL + `) ` + bareIDSQL + ` AS digest, payload
          FROM "MemoryNodes"
         WHERE concept = $1
           AND id IN (SELECT jsonb_array_elements_text($2::jsonb))
         ORDER BY ` + bareIDSQL + `, "createdAt" DESC) newest
 WHERE COALESCE(payload->>'accountId', '') = ''
   AND COALESCE(payload->>'liftedAt', '') = ''`

func clusterBlocks(ctx context.Context, q accountexport.Querier, digests map[string]bool) ([]emailBlockLine, error) {
	ids := make([]string, 0, len(digests))
	for d := range digests {
		ids = append(ids, d)
	}
	rows, err := q.QueryContext(ctx, clusterBlockSQL, suppressionConcept, bothSpellings(suppressionConcept, ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []emailBlockLine
	for rows.Next() {
		var digest, reason, at string
		if err := rows.Scan(&digest, &reason, &at); err != nil {
			return nil, err
		}
		lines = append(lines, emailBlockLine{EmailDigest: digest, Reason: reason, BlockedAt: at, Source: "cluster"})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].EmailDigest < lines[j].EmailDigest })
	return lines, nil
}
