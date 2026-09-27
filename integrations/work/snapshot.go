package work

// snapshot.go -- the workspace a session step's re-run or branch starts
// against (epic memql#5414, #5415; design D19).
//
// A SESSION STEP is a step whose current version handed its work to an app:
// the version carries a childRunId, and the child is a recording run
// (automationName "appSession") holding one step and one tool_result
// observation per action the app took. Re-running or branching such a step
// starts a NEW session, and it must start against the workspace as it was
// before that step -- not the directory the replaced version, and everything
// after it, has since changed. The recordings of the EARLIER session steps on
// the head hold the files they read and wrote, content-addressed in the
// Library, so the workspace is rebuilt from them; component/work.SnapshotOf
// decides what the rebuilt workspace holds and refuses a partial one, naming
// the file.
//
// A version with no childRunId is not treated as a session step, even when it
// was one: runs recorded before the delegate stamped the real step row carry
// no back-pointer, and there is nothing to rebuild from. Such a step re-runs
// in the default workspace, as it always did.

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/work"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// sessionSnapshot is the snapshot the step at key starts from, or nil when
// its current version is not a session step.
//
// head is the head the act runs against: the source's for a branch, the run's
// own for a re-run. An entry pointing into another run (a branch of a branch)
// is followed there, so an earlier session step served by reference still
// contributes the files its recording holds.
func (i *Integration) sessionSnapshot(ctx context.Context, run actRun, versions runVersions, head work.Head, key string) (*work.Snapshot, error) {
	resolve := i.versionResolver(ctx, run, versions)
	target := resolve(key, head[key])
	if target == nil || !i.isSessionVersion(ctx, run.owner, target) {
		return nil, nil
	}

	var (
		effects    []work.FileEffect
		unrecorded int
		order      int
	)
	for _, k := range run.order {
		if k == key {
			break
		}
		row := resolve(k, head[k])
		if row == nil || !i.isSessionVersion(ctx, run.owner, row) {
			continue
		}
		got, commands, err := i.recordedEffects(ctx, run.owner, rowString(row, "childRunId"), &order)
		if err != nil {
			return nil, err
		}
		effects = append(effects, got...)
		unrecorded += commands
	}
	snapshot, err := work.SnapshotOf(effects, unrecorded)
	if err != nil {
		// SnapshotOf's refusal already begins with its code and names the
		// file; the prefix is this package's, and errors.As still reaches it.
		return nil, fmt.Errorf("work: %w", err)
	}
	return &snapshot, nil
}

// versionResolver finds the row of the version a head entry names: in this
// run's own versions, or -- for an entry naming another run -- in that run's,
// read once and kept for the rest of the act.
func (i *Integration) versionResolver(ctx context.Context, run actRun, own runVersions) func(key string, e work.HeadEntry) map[string]any {
	others := map[string]runVersions{}
	return func(key string, e work.HeadEntry) map[string]any {
		if e.Version <= 0 {
			return nil
		}
		if e.RunId == "" || bareRunId(e.RunId) == bareRunId(run.id) {
			return own.row(key, e.Version)
		}
		v, ok := others[e.RunId]
		if !ok {
			read, err := i.readVersions(ctx, e.RunId, nil)
			if err != nil {
				i.log().Warn("work: could not read the run a head entry points into; its version contributes nothing to the snapshot",
					"component", "work.snapshot", "run", run.id, "points_into", e.RunId, "err", err)
			}
			v = read
			others[e.RunId] = v
		}
		return v.row(key, e.Version)
	}
}

// isSessionVersion reports whether a step version handed its work to an app:
// its childRunId names a recording run.
func (i *Integration) isSessionVersion(ctx context.Context, owner string, row map[string]any) bool {
	child := rowString(row, "childRunId")
	if child == "" {
		return false
	}
	childRun, err := i.store().runForOwner(ownerActor(ctx, owner), child)
	if err != nil || childRun == nil {
		return false
	}
	return rowString(childRun, "automationName") == appSessionTemplate
}

// recordedEffects reads one recording's file effects, in action order, and
// counts its shell commands.
//
// Every `exec` is counted as a command whose effects nobody recorded, whether
// or not the harness reported contents for it: a command's reported contents
// are what the harness chose to report, never a claim that nothing else
// changed. The count is shown, never treated as complete and never refused.
func (i *Integration) recordedEffects(ctx context.Context, owner, recordingRunId string, order *int) ([]work.FileEffect, int, error) {
	st := i.store()
	scoped := ownerActor(ctx, owner)
	observations, err := st.query(scoped, "query "+call("workObservationsForOwnerRun", map[string]any{"runId": recordingRunId}))
	if err != nil {
		return nil, 0, err
	}
	workspace := i.recordingWorkspace(ctx, owner, recordingRunId)

	actions := make([]map[string]any, 0, len(observations))
	for _, o := range observations {
		if rowString(o, "kind") == observationKindToolResult {
			actions = append(actions, o)
		}
	}
	sort.SliceStable(actions, func(a, b int) bool {
		sa, sb := rowInt(rowMap(actions[a], "data"), "seq"), rowInt(rowMap(actions[b], "data"), "seq")
		if sa != sb {
			return sa < sb
		}
		ta, _ := rowTime(actions[a], "createdAt")
		tb, _ := rowTime(actions[b], "createdAt")
		return ta.Before(tb)
	})

	var (
		effects  []work.FileEffect
		commands int
	)
	for _, o := range actions {
		data := rowMap(o, "data")
		if workerservice.StepTypeForTool(rowString(data, "tool")) == work.StepTypeExec {
			commands++
		}
		args, _ := work.RecordedArgs(data)
		argPaths := work.RecordedPaths(args)
		refs := rowStringSlice(data, "contentRefs")
		for _, fileId := range refs {
			fileId = strings.TrimSpace(fileId)
			if fileId == "" {
				continue
			}
			p, label := i.contentPath(ctx, owner, fileId, len(refs), argPaths, data)
			effect := work.FileEffect{Order: *order, FileId: fileId}
			if p == "" {
				// A content the recording kept but cannot place: restoring it
				// anywhere would be a guess, so it refuses like an omitted one,
				// naming what the recording does know about it.
				effect.Path = label
				effect.FileId = ""
				effect.Omitted = "the recording kept this content but not the path it belongs at"
			} else {
				effect.Path = work.WorkspaceRelative(workspace, p)
			}
			effects = append(effects, effect)
			*order++
		}
		for _, entry := range rowStringSlice(data, "contentOmitted") {
			p, why, _ := work.ParseOmitted(entry)
			if p == "" {
				continue
			}
			if why == "" {
				why = "its content was not recorded"
			}
			effects = append(effects, work.FileEffect{Order: *order, Path: work.WorkspaceRelative(workspace, p), Omitted: why})
			*order++
		}
	}
	return effects, commands, nil
}

// contentPath names the file one stored content belongs to, from the SAME
// action's arguments. The Library row keeps only a name (the file's base name,
// suffixed with the session and the action), so when the action named exactly
// one path and stored exactly one content the two are the same file, and
// otherwise the row's base name picks among the action's paths. label is what
// a refusal names when no path can be recovered.
func (i *Integration) contentPath(ctx context.Context, owner, fileId string, contents int, argPaths []string, data map[string]any) (p, label string) {
	if contents == 1 && len(argPaths) == 1 {
		return argPaths[0], argPaths[0]
	}
	name := i.libraryFileName(ctx, owner, fileId)
	base, ok := workerservice.ContentBaseName(name, rowString(data, "sessionId"), rowString(data, "appActionId"))
	if ok {
		for _, candidate := range argPaths {
			if path.Base(candidate) == base {
				return candidate, candidate
			}
		}
	}
	if len(argPaths) == 1 {
		return argPaths[0], argPaths[0]
	}
	if ok {
		return "", base
	}
	return "", fileId
}

// libraryFileName is a stored content's Library row name under the owner's
// actor; "" when the row is not readable.
func (i *Integration) libraryFileName(ctx context.Context, owner, fileId string) string {
	row, err := one(i.store().query(ownerActor(ctx, owner), "query "+call("libraryFileById", map[string]any{"fileId": fileId})))
	if err != nil || row == nil {
		return ""
	}
	return rowString(row, "name")
}

// recordingWorkspace is the directory a recorded session ran in: the
// environment fingerprint its first action carried. Empty when nobody
// measured it, and paths are then kept as the recording wrote them.
func (i *Integration) recordingWorkspace(ctx context.Context, owner, recordingRunId string) string {
	steps, err := i.store().query(ownerActor(ctx, owner), "query "+call("workStepsForOwnerRun", map[string]any{"runId": recordingRunId}))
	if err != nil {
		return ""
	}
	sort.SliceStable(steps, func(a, b int) bool { return rowInt(steps[a], "seq") < rowInt(steps[b], "seq") })
	for _, s := range steps {
		if cwd := rowString(rowMap(s, "fingerprint"), "cwd"); cwd != "" {
			return cwd
		}
	}
	return ""
}
