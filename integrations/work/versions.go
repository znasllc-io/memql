package work

// versions.go -- every version of every step of a run (epic memql#5414,
// #5415; design D18).
//
// A step's versions are row-versions of ONE row id (the journal's
// runId-stepKey id), and every standard read collapses a row to its newest
// row-version -- which is exactly why the timeline shows each step once, and
// exactly why no DSL read can answer "what were the other versions".
// dsl/deployment/queries.memql names a builtin as the prior art for that
// question, and this is that builtin for the work spine: a hand-rolled read of
// every row-version, folded to one entry per (row, version), each gated
// through row admission.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// stepVersionsSQL reads every row-version of one run's steps. The run id is
// matched in every form the journal writes -- bare for a trigger run and after
// a resume, canonical for an adopted goal run -- and
// memory_nodes_work_journal_lookup_idx covers ((payload->>'runId'), concept,
// id, "createdAt" DESC) for exactly this shape.
//
// staged-data: MUST-NOT-GATE -- the version numbers a re-run and a head move
// compute come from EVERY recorded version: a version withheld here would be
// issued again, and with it the idempotency key its side effect already ran
// under. Row admission still applies, to each folded row as it leaves.
const stepVersionsSQL = `
SELECT id, "createdAt", payload
FROM "MemoryNodes"
WHERE (payload->>'runId') IN (?) AND concept = ?
ORDER BY id, "createdAt" DESC
LIMIT ?
`

// stepVersionsMaxRows bounds the read. A run's step row-versions are its
// steps times their versions times two (intent and receipt), so this is far
// above any run a person re-runs by hand -- and a read that reaches it is
// REFUSED rather than answered: a version history missing its tail would hand
// a re-run a version number already used.
const stepVersionsMaxRows = 20000

// StepVersions is every version of every step of one run, one entry per (step
// row, version), each the NEWEST row-version of that version -- the receipt
// when there is one, the intent while it runs, the re-assertion after a head
// move. A row written before versions were a field takes its attempt as its
// version, and the entry carries it as `version`. Sorted by seq, then row id,
// then version. Nested steps are included; the head decisions keep to the
// top-level ones.
//
// The actor in ctx decides which rows are admitted, exactly as a query's would:
// the caller's own for the version-history builtin, the owner's borrowed
// authority for a Go caller acting on the owner's behalf. Exported for the
// dispatcher and the other Go callers that need a run's full history.
func (i *Integration) StepVersions(ctx context.Context, runId string) ([]map[string]any, error) {
	runId = trim(runId)
	if runId == "" {
		return nil, fmt.Errorf("work: StepVersions needs a run id")
	}
	// The admission gate is checked FIRST, for selectAdmitted's reason: "we
	// cannot tell who may see this row" must never be resolved by proceeding.
	if i.admitRow == nil {
		return nil, fmt.Errorf("work: no row-admission gate is wired; refusing a hand-rolled read rather than admitting every row")
	}
	if i.bunDB == nil || i.bunDB() == nil {
		return nil, fmt.Errorf("work: the version history needs a database handle")
	}

	rows, err := i.bunDB().QueryContext(ctx, stepVersionsSQL,
		bun.In(runIdForms(runId)), memorynodes.ConceptWorkStep, stepVersionsMaxRows+1)
	if err != nil {
		return nil, fmt.Errorf("work: read the versions of run %s: %w", runId, err)
	}
	defer func() { _ = rows.Close() }()

	type folded struct {
		node   memorynodes.MemoryNode
		fields map[string]any
	}
	latest := map[string]*folded{}
	read := 0
	for rows.Next() {
		var (
			idv       string
			createdAt time.Time
			payload   []byte
		)
		if err := rows.Scan(&idv, &createdAt, &payload); err != nil {
			return nil, fmt.Errorf("work: scan the versions of run %s: %w", runId, err)
		}
		read++
		if read > stepVersionsMaxRows {
			return nil, fmt.Errorf("work: run %s has more than %d step row-versions; its version history cannot be read whole, and a partial one would reissue a version number", runId, stepVersionsMaxRows)
		}
		fields := map[string]any{}
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &fields); err != nil {
				return nil, fmt.Errorf("work: decode a step row-version of run %s: %w", runId, err)
			}
		}
		fold := idv + "\x00" + strconv.Itoa(rowVersion(fields))
		// THE FOLD COMES BEFORE THE GATE (AdmitSourceRow's ordering rule):
		// gating first would let a denied newest row-version fall through to
		// an admitted older one, handing the caller a stale version instead of
		// none. Compared on createdAt rather than trusting the ORDER BY, so the
		// fold holds whatever order the rows arrive in.
		if prev, ok := latest[fold]; ok && !createdAt.After(prev.node.CreatedAt) {
			continue
		}
		latest[fold] = &folded{
			node: memorynodes.MemoryNode{
				ID:        idv,
				Concept:   memorynodes.ConceptWorkStep,
				Type:      memorynodes.NodeTypeObject,
				CreatedAt: createdAt,
				Payload:   payload,
			},
			fields: fields,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("work: read the versions of run %s: %w", runId, err)
	}

	out := make([]map[string]any, 0, len(latest))
	for _, f := range latest {
		if !i.admitRow(ctx, f.node) {
			continue
		}
		row := map[string]any{
			"id":        f.node.ID,
			"concept":   f.node.Concept,
			"createdAt": f.node.CreatedAt.UTC().Format(time.RFC3339Nano),
		}
		for k, v := range f.fields {
			if k == "id" || k == "concept" || k == "createdAt" {
				continue
			}
			row[k] = v
		}
		row["version"] = rowVersion(f.fields)
		out = append(out, row)
	}
	sort.Slice(out, func(a, b int) bool {
		if sa, sb := rowInt(out[a], "seq"), rowInt(out[b], "seq"); sa != sb {
			return sa < sb
		}
		if ia, ib := rowString(out[a], "id"), rowString(out[b], "id"); ia != ib {
			return ia < ib
		}
		return rowVersion(out[a]) < rowVersion(out[b])
	})
	return out, nil
}

// runIdForms is every spelling of one run id a step row may carry: as given,
// bare, and canonical, each once.
func runIdForms(runId string) []string {
	bare := memqlengine.BareShortId(runId)
	var forms []string
	seen := map[string]bool{}
	for _, f := range []string{runId, bare, runConcept + ":" + bare} {
		if f != "" && !seen[f] {
			seen[f] = true
			forms = append(forms, f)
		}
	}
	return forms
}

// handleStepVersions is workStepVersions: every version of every step of one
// of the caller's runs, each marked `current` when the run's head names it.
//
// ONE NODE PER VERSION, each with its own id (`<stepId>@v<version>`, the step
// row's short id), because a builtin reply crosses the wire as one map keyed by
// node id and two entries sharing an id collapse into one.
//
// CURRENT is the head's word for a top-level step. A step the head does not
// name -- a nested statement, or any step of a run written before the head
// existed -- is current at its newest version, which is what every collapsed
// read shows. A head entry pointing into ANOTHER run (a branch's shared
// prefix) marks none of this run's own rows current, because the version it
// names lives in the run it forked.
func (i *Integration) handleStepVersions(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requirePrincipal(ctx); err != nil {
		return nil, err
	}
	runId := argString(args, "runId")
	if runId == "" {
		return nil, fmt.Errorf("work: workStepVersions needs a runId")
	}
	run, err := i.readActRun(ctx, runId)
	if err != nil {
		return nil, err
	}
	rows, err := i.StepVersions(ctx, run.id)
	if err != nil {
		return nil, err
	}
	versions := shapeVersions(rows, run.order)
	head := headOf(run, versions)

	entries := make([]replyEntry, 0, len(rows))
	for _, row := range rows {
		key := rowString(row, "key")
		version := rowVersion(row)
		current := version == versions.newest(key)
		if e, named := head[key]; named {
			current = e.RunId == "" && e.Version == version
		}
		payload := make(map[string]any, len(row)+1)
		for k, v := range row {
			payload[k] = v
		}
		payload["current"] = current
		at, _ := rowTime(row, "createdAt")
		entries = append(entries, replyEntry{
			id:      memqlengine.BareShortId(rowString(row, "id")) + "@v" + strconv.Itoa(version),
			at:      at,
			payload: payload,
		})
	}
	return i.replyNodes(entries), nil
}
