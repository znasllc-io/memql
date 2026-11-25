package memql

// memory_index_run.go -- the memory index job's run rows as stored: their
// payload, the fresh read, the fenced write that every writer of them goes
// through (the job, its flip, the owner's builtins), and the job's
// @serverOnly mutation calls. Why every write is fenced, and stamped after
// the version it read, is in memory_index_job.go's header and on
// writeMemoryIndexRun.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
)

// memoryIndexMutation runs one of the job's @serverOnly mutations.
func (e *MemQLEngine) memoryIndexMutation(ctx context.Context, name string, args map[string]any) error {
	_, err := e.memoryIndexMutationResult(ctx, name, args)
	return err
}

// memoryIndexMutationResult runs one of the job's @serverOnly mutations and
// answers what it wrote.
func (e *MemQLEngine) memoryIndexMutationResult(ctx context.Context, name string, args map[string]any) (*ExecuteResult, error) {
	call, err := parser.RenderCall(name, args)
	if err != nil {
		return nil, err
	}
	return e.Execute(ctx, "mutation "+call)
}

// memoryIndexWrittenVersion is the version a write of row id stored, as the
// write's own result reports it, or zero when the result does not name it.
func memoryIndexWrittenVersion(res *ExecuteResult, id string) time.Time {
	if res == nil || res.Bundle == nil {
		return time.Time{}
	}
	for _, n := range res.Bundle.Nodes {
		if n != nil && n.GetId() == id && n.GetCreatedAt() != nil {
			return n.GetCreatedAt().AsTime().UTC()
		}
	}
	return time.Time{}
}

// memoryIndexRunPayload is the row as stored.
type memoryIndexRunPayload struct {
	Kind             string            `json:"kind"`
	State            string            `json:"state"`
	TargetRef        string            `json:"targetRef"`
	TargetDimensions int               `json:"targetDimensions"`
	TargetTable      string            `json:"targetTable"`
	PreviousRef      string            `json:"previousRef"`
	IndexedBy        string            `json:"indexedBy"`
	StartedBy        string            `json:"startedBy"`
	StartedAt        string            `json:"startedAt"`
	FinishedAt       string            `json:"finishedAt"`
	Cursor           map[string]string `json:"cursor"`
	Counts           struct {
		Total     int `json:"total"`
		Done      int `json:"done"`
		Skipped   int `json:"skipped"`
		Unsourced int `json:"unsourced"`
		Failed    int `json:"failed"`
		Missing   int `json:"missing"`
	} `json:"counts"`
	WaitingReason string `json:"waitingReason"`
	LastPassAt    string `json:"lastPassAt"`
	Missing       int    `json:"missing"`
	MissingAt     string `json:"missingAt"`
	Counting      struct {
		Cursor  map[string]string `json:"cursor"`
		Missing int               `json:"missing"`
	} `json:"counting"`
}

func memoryIndexTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func memoryIndexTimeArg(args map[string]any, key string, t time.Time) {
	if !t.IsZero() {
		args[key] = t.UTC().Format(time.RFC3339Nano)
	}
}

// readMemoryIndexRun returns the newest version of one run row, when that
// version was written (the witness writeMemoryIndexRun fences on), and whether
// the row exists. Read straight off the store: it is the job's own state, read
// fresh by construction, and never answered from a result cache.
//
// staged-data: MUST-NOT-GATE -- the job's own state row (a core platform
// concept, never promoted). Read as staged, a switch would read as absent: the
// job would never see an owner's switch, or would start one over from its
// first item and embed everything again.
func (e *MemQLEngine) readMemoryIndexRun(ctx context.Context, kind string) (MemoryIndexRun, time.Time, bool, error) {
	db := e.database()
	if db == nil {
		return MemoryIndexRun{}, time.Time{}, false, fmt.Errorf("memory index: no database")
	}
	var node memorynodes.MemoryNode
	q := db.NewSelect().Model(&node).Column("createdAt", "payload").Where("concept = ?", MemoryIndexRunConceptID)
	err := versionsOfOneID(q, MemoryIndexRunConceptID+":"+kind, false).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return MemoryIndexRun{}, time.Time{}, false, nil
	}
	if err != nil {
		return MemoryIndexRun{}, time.Time{}, false, fmt.Errorf("memory index: reading the %s run: %w", kind, err)
	}
	var p memoryIndexRunPayload
	if err := json.Unmarshal(node.Payload, &p); err != nil {
		return MemoryIndexRun{}, time.Time{}, false, fmt.Errorf("memory index: the %s run does not decode: %w", kind, err)
	}
	run := MemoryIndexRun{
		Kind: p.Kind, State: p.State, TargetRef: p.TargetRef, TargetDimensions: p.TargetDimensions,
		TargetTable: p.TargetTable, PreviousRef: p.PreviousRef, IndexedBy: p.IndexedBy, StartedBy: p.StartedBy,
		WaitingReason: p.WaitingReason, StartedAt: memoryIndexTime(p.StartedAt), FinishedAt: memoryIndexTime(p.FinishedAt),
		LastPassAt: memoryIndexTime(p.LastPassAt), Cursor: p.Cursor,
		Counts: MemoryIndexCounts{Total: p.Counts.Total, Done: p.Counts.Done, Skipped: p.Counts.Skipped,
			Unsourced: p.Counts.Unsourced, Failed: p.Counts.Failed, Missing: p.Counts.Missing},
		Missing: p.Missing, MissingAt: memoryIndexTime(p.MissingAt),
		Counting: memoryIndexCounting{Cursor: p.Counting.Cursor, Missing: p.Counting.Missing},
	}
	if run.Kind == "" {
		run.Kind = kind
	}
	return cloneMemoryIndexRun(run), node.CreatedAt, true, nil
}

// writeMemoryIndexRun writes the WHOLE run row through recordMemoryIndexRun,
// fenced to version: the time of the newest version the caller read, or zero
// when it read none (the row must then still be absent). A write after anybody
// else's answers errMemoryIndexRunChanged and writes nothing; the caller reads
// again and decides again (memql#5431). It answers the version it stored, which
// is the witness for the caller's NEXT write of the row: a caller that wrote
// keeps that, and never a version read afterwards, which may be somebody
// else's (saveRun).
//
// EVERY WRITER OF THE RUN ROWS GOES THROUGH HERE -- the job, its flip, and the
// owner's builtins -- because the fence protects a row only when all its
// writers carry one: an unfenced write would silently revert a cancel, or a
// cancel would roll back a cursor.
//
// THE NEW VERSION IS STAMPED AFTER THE ONE READ (ruling R12, memql#5431's third
// part): VersionTimeAfter(version, now) is the write's versionTime, so a
// replica whose clock lags the last writer's still writes the newest version
// rather than one that reads as older and is lost. (The fence's own clock,
// fenceWriteClock, makes the same guarantee for a fenced write; the stamp here
// states it on the write, the way every other writer of memory search's rows
// does.)
func (e *MemQLEngine) writeMemoryIndexRun(ctx context.Context, run MemoryIndexRun, version time.Time) (time.Time, error) {
	if run.Kind != memoryIndexKindSwitch && run.Kind != memoryIndexKindBackfill {
		return time.Time{}, fmt.Errorf("memory index: no run of kind %q", run.Kind)
	}
	fenced := ContextWithRowVersionFence(auth.ContextWithInternalOrigin(ctx), MemoryIndexRunConceptID, run.Kind, version)
	res, err := e.memoryIndexMutationResult(fenced, "platform.recordMemoryIndexRun", memoryIndexRunArgs(run, VersionTimeAfter(version, time.Now())))
	if err != nil {
		if errors.Is(err, ErrRowVersionChanged) || strings.Contains(err.Error(), ErrRowVersionChanged.Error()) {
			return time.Time{}, errMemoryIndexRunChanged
		}
		return time.Time{}, err
	}
	if written := memoryIndexWrittenVersion(res, MemoryIndexRunConceptID+":"+run.Kind); !written.IsZero() {
		return written, nil
	}
	// The result did not name the version it stored: read it back, and take it
	// only as THIS write. Anything else is another writer landing after it,
	// and its version must not become this writer's witness (saveRun).
	stored, at, ok, err := e.readMemoryIndexRun(ctx, run.Kind)
	if err != nil {
		return time.Time{}, err
	}
	if !ok || !memoryIndexRunsEqual(stored, cloneMemoryIndexRun(run)) {
		return time.Time{}, errMemoryIndexRunChanged
	}
	return at, nil
}

// memoryIndexRunArgs is run as recordMemoryIndexRun's arguments, written as
// the row version versionTime. A blank field is left out, and the mutation
// stamps it blank: every write states the row.
func memoryIndexRunArgs(run MemoryIndexRun, versionTime time.Time) map[string]any {
	counts := map[string]any{"total": run.Counts.Total, "done": run.Counts.Done, "skipped": run.Counts.Skipped,
		"unsourced": run.Counts.Unsourced, "failed": run.Counts.Failed, "missing": run.Counts.Missing}
	cursor := map[string]any{}
	for k, v := range run.Cursor {
		cursor[k] = v
	}
	counting := map[string]any{}
	for k, v := range run.Counting.Cursor {
		counting[k] = v
	}
	args := map[string]any{"kind": run.Kind, "state": run.State, "cursor": cursor, "counts": counts, "missing": run.Missing,
		"counting": map[string]any{"cursor": counting, "missing": run.Counting.Missing}}
	for key, v := range map[string]string{"targetRef": run.TargetRef, "targetTable": run.TargetTable, "previousRef": run.PreviousRef,
		"indexedBy": run.IndexedBy, "startedBy": run.StartedBy, "waitingReason": run.WaitingReason} {
		if strings.TrimSpace(v) != "" {
			args[key] = v
		}
	}
	if run.TargetDimensions > 0 {
		args["targetDimensions"] = run.TargetDimensions
	}
	memoryIndexTimeArg(args, "startedAt", run.StartedAt)
	memoryIndexTimeArg(args, "finishedAt", run.FinishedAt)
	memoryIndexTimeArg(args, "lastPassAt", run.LastPassAt)
	memoryIndexTimeArg(args, "missingAt", run.MissingAt)
	memoryIndexTimeArg(args, "versionTime", versionTime)
	return args
}
