package work

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

const EnvSystemRunRetentionDays = "MEMQL_WORK_SYSTEM_RUN_RETENTION_DAYS"
const DefaultSystemRunRetentionDays = 30
const retirementBatchSize = 100
const retirementMaxVersions = 10000
const retirementMaxBytes = 32 << 20

// Age never outranks a live parent. Check the latest run via its indexed id,
// so a previous terminal version cannot make current work look disposable.
// staged-data: MUST-NOT-GATE -- hiding a staged active parent makes its detail
// appear orphaned and permits retirement while that parent is still running.
const retentionInactiveParentSQL = `NOT EXISTS (SELECT 1 FROM "MemoryNodes" parent WHERE parent.concept='v1:work:run' AND parent.id=n.payload->>'runId' AND COALESCE(parent.payload->>'status','') NOT IN ('succeeded','failed','cancelled','abandoned') AND NOT EXISTS (SELECT 1 FROM "MemoryNodes" newerParent WHERE newerParent.concept=parent.concept AND newerParent.id=parent.id AND newerParent."createdAt">parent."createdAt"))`

// This is a closed list of operational records, never a default lifetime for
// arbitrary concepts. Business records, goals, templates and active work are
// not eligible. A terminal system run owns its step/approval archive as a unit.
type operationalPolicy struct {
	concept, env string
	days         int
	predicate    string
}

var operationalPolicies = []operationalPolicy{
	{"v1:identity:auditEvent", "MEMQL_IDENTITY_AUDIT_LOG_RETENTION_DAYS", 365, "TRUE"},
	{"v1:safety:classification", "MEMQL_SAFETY_CLASSIFICATION_RETENTION_DAYS", 90, "TRUE"},
	{"v1:safety:outputScreening", "MEMQL_SAFETY_OUTPUT_SCREENING_RETENTION_DAYS", 90, "TRUE"},
	{"v1:worker:invocation", "MEMQL_WORKER_INVOCATION_RETENTION_DAYS", 90, `COALESCE(n.payload->>'outcome','') IN ('success','failure','cancelled','timeout','denied_by_scope','denied_by_policy','denied_by_classifier','kill_switch_engaged','no_worker_available','rerouted')`},
	{runConcept, EnvSystemRunRetentionDays, DefaultSystemRunRetentionDays, `n.payload->>'ownerUserId' = '' AND COALESCE(n.payload->>'goalId','') = '' AND n.payload->>'triggeredBy' = 'schedule' AND n.payload->>'status' IN ('succeeded','failed','cancelled','abandoned')`},
}

type OperationalRetentionResult struct {
	Concept          string   `json:"concept"`
	RetentionDays    int      `json:"retentionDays"`
	Candidates       int      `json:"candidates"`
	ArchivedVersions int      `json:"archivedVersions"`
	DeletedVersions  int      `json:"deletedVersions"`
	Objects          []string `json:"objects,omitempty"`
}

// staged-data: MUST-NOT-GATE -- retention is physical retirement, including
// staged historical versions. Every fetched row still passes AdmitSourceRow.
func (i *Integration) operationalRetention(ctx context.Context, dry bool) ([]OperationalRetentionResult, error) {
	if err := requireClusterOwner(ctx); err != nil {
		return nil, err
	}
	var results []OperationalRetentionResult
	windows, err := i.operationalRetentionWindows(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range operationalPolicies {
		days := windows[p.concept]
		cutoff := i.clock().UTC().AddDate(0, 0, -days)
		// The age applies to the TRUE LATEST version. An old terminal version of
		// recently updated/active work must never qualify on its own.
		predicate := p.predicate
		params := []any{p.concept, cutoff}
		if p.concept == runConcept {
			// Apply child eligibility before LIMIT as well as after loading: a
			// large population awaiting longer detail retention cannot starve
			// later, eligible system runs every night.
			predicate += ` AND NOT EXISTS (SELECT 1 FROM "MemoryNodes" child WHERE child.concept IN ('v1:work:step','v1:work:approval','v1:work:modelCall','v1:work:observation') AND child.payload->>'runId'=n.id AND (child.concept IN ('v1:work:modelCall','v1:work:observation') OR child."createdAt">=? OR COALESCE(child.payload->>'ownerUserId','missing')!='' OR (child.concept='v1:work:approval' AND COALESCE(child.payload->>'decision','')='')) AND NOT EXISTS (SELECT 1 FROM "MemoryNodes" newerChild WHERE newerChild.concept=child.concept AND newerChild.id=child.id AND newerChild."createdAt">child."createdAt"))`
			params = append(params, cutoff)
		}
		query := `SELECT n.id,n."createdAt",n.payload FROM "MemoryNodes" n WHERE n.concept=? AND n."createdAt" < ? AND (` + predicate + `) AND NOT EXISTS (SELECT 1 FROM "MemoryNodes" newer WHERE newer.concept=n.concept AND newer.id=n.id AND newer."createdAt">n."createdAt") AND ` + retentionInactiveParentSQL + ` ORDER BY n."createdAt",n.id LIMIT 20000`
		rows, err := i.selectAdmitted(ctx, p.concept, query, params...)
		if err != nil {
			return results, err
		}
		result := OperationalRetentionResult{Concept: p.concept, RetentionDays: days, Candidates: len(rows)}
		for start := 0; start < len(rows); start += retirementBatchSize {
			end := min(start+retirementBatchSize, len(rows))
			archived, deleted, object, err := i.retireVerified(ctx, p.concept, rows[start:end], p.concept == runConcept, cutoff, dry)
			if err != nil {
				return append(results, result), err
			}
			result.ArchivedVersions += archived
			result.DeletedVersions += deleted
			if object != "" {
				result.Objects = append(result.Objects, object)
			}
		}
		results = append(results, result)
	}
	return results, nil
}

// Retain existing global-variable settings. Explicit process configuration
// overrides a stored policy; absent configuration uses the documented default.
// staged-data: MUST-NOT-GATE -- hiding an active stored retention policy can
// silently substitute a shorter default and prematurely retire its records.
// These maintenance-only configuration reads still pass selectAdmitted.
func (i *Integration) operationalRetentionWindows(ctx context.Context) (map[string]int, error) {
	names := []string{"WORKER_INVOCATION_RETENTION_DAYS"}
	for _, p := range operationalPolicies {
		names = append(names, p.env)
	}
	rows, err := i.selectAdmitted(ctx, "v1:platform:globalVariable", `WITH latest AS (SELECT DISTINCT ON(id) id,"createdAt" FROM "MemoryNodes" WHERE concept=? ORDER BY id,"createdAt" DESC) SELECT n.id,n."createdAt",n.payload FROM latest l JOIN "MemoryNodes" n ON n.id=l.id AND n."createdAt"=l."createdAt" WHERE n.payload->>'name' IN (?) AND n.payload->>'active'='true'`, "v1:platform:globalVariable", bun.In(names))
	if err != nil {
		return nil, err
	}
	stored := map[string]string{}
	for _, row := range rows {
		stored[rowString(row, "name")] = fmt.Sprint(row["value"])
	}
	if _, ok := stored["MEMQL_WORKER_INVOCATION_RETENTION_DAYS"]; !ok {
		stored["MEMQL_WORKER_INVOCATION_RETENTION_DAYS"] = stored["WORKER_INVOCATION_RETENTION_DAYS"]
	}
	windows := map[string]int{}
	for _, p := range operationalPolicies {
		days := retentionDays(p.env, p.days)
		if strings.TrimSpace(os.Getenv(p.env)) == "" {
			if n, err := strconv.Atoi(strings.TrimSpace(stored[p.env])); err == nil && n > 0 {
				days = n
			}
		}
		windows[p.concept] = days
	}
	return windows, nil
}

type retirementKey struct {
	ID      string    `json:"id"`
	Concept string    `json:"concept"`
	At      time.Time `json:"at"`
}

// retireVerified archives ALL versions, including schema/provenance, with a
// content-addressed object name. Read-back must match before an exact-key
// transactional delete. Bounded batches cap both memory and database work.
// A concurrent revision or a new child leaves the entire batch in place.
// staged-data: MUST-NOT-GATE -- hiding staged versions loses their archive
// evidence, and hiding staged revisions/children defeats the concurrent-write
// guards. Every archived version passes admitRow before any deletion.
func (i *Integration) retireVerified(ctx context.Context, concept string, candidates []map[string]any, withChildren bool, cutoff time.Time, dry bool) (int, int, string, error) {
	if len(candidates) == 0 {
		return 0, 0, "", nil
	}
	if i.admitRow == nil || i.bunDB == nil || i.bunDB() == nil {
		return 0, 0, "", fmt.Errorf("retention requires a database and row admission")
	}
	db := i.bunDB()
	ids := make([]string, 0, len(candidates))
	expected := map[string]time.Time{}
	for _, r := range candidates {
		at, ok := rowTime(r, "createdAt")
		if !ok {
			return 0, 0, "", fmt.Errorf("retention candidate has no version key")
		}
		id := rowString(r, "id")
		ids = append(ids, id)
		expected[id] = at
	}
	where := `concept=? AND id IN (?)`
	args := []any{concept, bun.In(ids)}
	if withChildren {
		where = `(` + where + `) OR (concept IN ('v1:work:step','v1:work:approval','v1:work:modelCall','v1:work:observation') AND payload->>'runId' IN (?))`
		args = append(args, bun.In(ids))
	}
	rows, err := db.QueryContext(ctx, `SELECT row_to_json(n) FROM "MemoryNodes" n WHERE `+where+` ORDER BY concept,id,"createdAt" LIMIT ?`, append(args, retirementMaxVersions+1)...)
	if err != nil {
		return 0, 0, "", err
	}
	var nodes []memorynodes.MemoryNode
	size := 0
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			break
		}
		size += len(raw)
		if size > retirementMaxBytes || len(nodes) == retirementMaxVersions {
			err = fmt.Errorf("retention batch exceeds archive budget; records preserved")
			break
		}
		var n memorynodes.MemoryNode
		if err = json.Unmarshal(raw, &n); err != nil {
			break
		}
		if !i.admitRow(ctx, n) {
			err = fmt.Errorf("retention row admission refused; records preserved")
			break
		}
		nodes = append(nodes, n)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return 0, 0, "", err
	}
	// Group system children by their parent, and preserve parents with detail
	// under its longer policy, a live approval, a new child, or an owned child.
	blocked := map[string]bool{}
	latest := map[string]memorynodes.MemoryNode{}
	for _, n := range nodes {
		latest[n.ID] = n
		if n.Concept == concept {
			if !n.CreatedAt.After(expected[n.ID]) {
				continue
			}
			blocked[n.ID] = true
		}
	}
	for _, n := range latest {
		if n.Concept == concept {
			continue
		}
		var p map[string]any
		if err = json.Unmarshal(n.Payload, &p); err != nil {
			return 0, 0, "", err
		}
		parent := rowString(p, "runId")
		if n.Concept == modelCallConcept || n.Concept == observationConcept || !n.CreatedAt.Before(cutoff) || p["ownerUserId"] != "" || (n.Concept == "v1:work:approval" && rowString(p, "decision") == "") {
			blocked[parent] = true
		}
	}
	var kept []memorynodes.MemoryNode
	var parentIDs []string
	for _, id := range ids {
		if !blocked[id] {
			parentIDs = append(parentIDs, id)
		}
	}
	for _, n := range nodes {
		parent := n.ID
		if n.Concept != concept {
			var p map[string]any
			_ = json.Unmarshal(n.Payload, &p)
			parent = rowString(p, "runId")
		}
		if !blocked[parent] {
			kept = append(kept, n)
		}
	}
	if len(kept) == 0 || dry {
		return 0, 0, "", nil
	}
	archiver := i.archiverRef()
	container := archiveContainer()
	if archiver == nil || container == "" {
		return 0, 0, "", fmt.Errorf("retention cannot archive: configure %s or %s; records preserved", EnvArchiveContainer, envBlobContainer)
	}
	verifier, ok := archiver.(interface {
		DownloadWithLimit(context.Context, string, string, int64) ([]byte, error)
	})
	if !ok {
		return 0, 0, "", fmt.Errorf("archive does not support bounded verification; records preserved")
	}
	encoded := make([]map[string]any, 0, len(kept))
	keys := make([]retirementKey, 0, len(kept))
	for _, n := range kept {
		raw, e := json.Marshal(n)
		if e != nil {
			return 0, 0, "", e
		}
		var row map[string]any
		if e = json.Unmarshal(raw, &row); e != nil {
			return 0, 0, "", e
		}
		encoded = append(encoded, row)
		keys = append(keys, retirementKey{n.ID, n.Concept, n.CreatedAt})
	}
	blob, err := ndjsonGzip(encoded)
	if err != nil {
		return 0, 0, "", err
	}
	digest := sha256.Sum256(blob)
	object := fmt.Sprintf("retention/%s/%x.ndjson.gz", i.clock().UTC().Format("2006-01-02"), digest)
	if _, err = archiver.Upload(ctx, container, object, blob, archiveContentType); err != nil {
		return 0, 0, "", fmt.Errorf("archive upload failed; records preserved: %w", err)
	}
	restored, err := verifier.DownloadWithLimit(ctx, container, object, int64(len(blob))+1)
	if err != nil {
		return 0, 0, "", fmt.Errorf("archive verification failed; records preserved: %w", err)
	}
	if !bytes.Equal(restored, blob) {
		return 0, 0, "", fmt.Errorf("archive checksum mismatch; records preserved")
	}
	rawKeys, err := json.Marshal(keys)
	if err != nil {
		return len(kept), 0, object, err
	}
	var deleted int
	err = db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Only archived exact keys can be removed. The whole batch is retained if
		// ANY logical row advanced since the archive snapshot.
		guard := ` AND NOT EXISTS (SELECT 1 FROM keys k JOIN "MemoryNodes" n ON n.id=k.id AND n.concept=k.concept AND n."createdAt"=k.at WHERE NOT (` + retentionInactiveParentSQL + `))`
		var params []any
		params = append(params, string(rawKeys))
		if withChildren {
			guard += ` AND NOT EXISTS (SELECT 1 FROM "MemoryNodes" child WHERE child.concept IN ('v1:work:step','v1:work:approval','v1:work:modelCall','v1:work:observation') AND child.payload->>'runId' IN (?) AND NOT EXISTS (SELECT 1 FROM keys k WHERE k.id=child.id AND k.concept=child.concept AND k.at=child."createdAt"))`
			params = append(params, bun.In(parentIDs))
		}
		result, e := tx.ExecContext(ctx, `WITH keys AS MATERIALIZED (SELECT * FROM jsonb_to_recordset(?::jsonb) AS k(id text,concept text,at timestamptz)), latest AS (SELECT id,concept,max(at) AS at FROM keys GROUP BY id,concept) DELETE FROM "MemoryNodes" n USING keys k WHERE n.id=k.id AND n.concept=k.concept AND n."createdAt"=k.at AND NOT EXISTS (SELECT 1 FROM latest l JOIN "MemoryNodes" newer ON newer.id=l.id AND newer.concept=l.concept AND newer."createdAt">l.at)`+guard, params...)
		if e != nil {
			return e
		}
		count, e := result.RowsAffected()
		if e != nil {
			return e
		}
		deleted = int(count)
		// Keep vectors for a concurrent/new version; otherwise remove them in the
		// SAME transaction so a failed vector deletion cannot leave half a retire.
		_, e = tx.ExecContext(ctx, `WITH keys AS (SELECT * FROM jsonb_to_recordset(?::jsonb) AS k(id text,concept text,at timestamptz)) DELETE FROM node_vectors v WHERE v.id IN (SELECT id FROM keys) AND NOT EXISTS (SELECT 1 FROM "MemoryNodes" n WHERE n.id=v.id)`, string(rawKeys))
		return e
	})
	if err != nil {
		return len(kept), 0, object, err
	}
	return len(kept), deleted, object, nil
}
