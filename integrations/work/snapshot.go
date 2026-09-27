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
// A READ THAT FAILS REFUSES THE ACT, it never shrinks the snapshot. A
// recording that could not be read is a set of files the snapshot silently
// lacks, which is exactly the divergence the refusal above exists to prevent;
// the person can try again, and a snapshot that says it is whole must be.
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
	target, err := resolve(key, head[key])
	if err != nil {
		return nil, err
	}
	session, err := i.recordingOf(ctx, run.owner, target)
	if err != nil || session == "" {
		return nil, err
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
		row, err := resolve(k, head[k])
		if err != nil {
			return nil, err
		}
		recording, err := i.recordingOf(ctx, run.owner, row)
		if err != nil {
			return nil, err
		}
		if recording == "" {
			continue
		}
		got, commands, err := i.recordedEffects(ctx, run.owner, recording, &order)
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
// read once and kept for the rest of the act. A version the head names and no
// row holds answers nil.
func (i *Integration) versionResolver(ctx context.Context, run actRun, own runVersions) func(key string, e work.HeadEntry) (map[string]any, error) {
	others := map[string]runVersions{}
	return func(key string, e work.HeadEntry) (map[string]any, error) {
		if e.Version <= 0 {
			return nil, nil
		}
		if e.RunId == "" || bareRunId(e.RunId) == bareRunId(run.id) {
			return own.row(key, e.Version), nil
		}
		v, ok := others[e.RunId]
		if !ok {
			read, err := i.readVersions(ctx, e.RunId, nil)
			if err != nil {
				return nil, fmt.Errorf("work: read run %s, which step %s of run %s is served from: %w", e.RunId, key, run.id, err)
			}
			v = read
			others[e.RunId] = v
		}
		return v.row(key, e.Version), nil
	}
}

// recordingOf is the recording run a step version handed its work to, or ""
// when it handed nothing to an app: no childRunId, or a child that is some
// other kind of subrun.
func (i *Integration) recordingOf(ctx context.Context, owner string, row map[string]any) (string, error) {
	child := rowString(row, "childRunId")
	if child == "" {
		return "", nil
	}
	childRun, err := i.store().runForOwner(ownerActor(ctx, owner), child)
	if err != nil {
		return "", fmt.Errorf("work: read the subrun %s a step handed its work to: %w", child, err)
	}
	if childRun == nil || rowString(childRun, "automationName") != appSessionTemplate {
		return "", nil
	}
	return child, nil
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
		return nil, 0, fmt.Errorf("work: read the recording %s: %w", recordingRunId, err)
	}
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
	workspace, err := i.recordingWorkspace(ctx, owner, recordingRunId, actions)
	if err != nil {
		return nil, 0, err
	}

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
			p, label, err := i.contentPath(ctx, owner, fileId, len(refs), argPaths, data)
			if err != nil {
				return nil, 0, err
			}
			effect := work.FileEffect{Order: *order, FileId: fileId, Path: work.WorkspaceRelative(workspace, p)}
			if p == "" {
				// A content the recording kept but cannot place: restoring it
				// anywhere would be a guess, so it refuses like an omitted one,
				// naming what the recording does know about it.
				effect = work.FileEffect{Order: *order, Path: label,
					Omitted: "the recording kept this content but not the path it belongs at"}
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
func (i *Integration) contentPath(ctx context.Context, owner, fileId string, contents int, argPaths []string, data map[string]any) (p, label string, err error) {
	if contents == 1 && len(argPaths) == 1 {
		return argPaths[0], argPaths[0], nil
	}
	row, err := one(i.store().query(ownerActor(ctx, owner), "query "+call("libraryFileById", map[string]any{"fileId": fileId})))
	if err != nil {
		return "", "", fmt.Errorf("work: read the recorded content %s: %w", fileId, err)
	}
	base, ok := workerservice.ContentBaseName(rowString(row, "name"), rowString(data, "sessionId"), rowString(data, "appActionId"))
	if ok {
		for _, candidate := range argPaths {
			if path.Base(candidate) == base {
				return candidate, candidate, nil
			}
		}
	}
	if len(argPaths) == 1 {
		return argPaths[0], argPaths[0], nil
	}
	if ok {
		return "", base, nil
	}
	return "", fileId, nil
}

// recordingWorkspace is the directory a recorded session ran in: the
// environment fingerprint its first action carried, or -- for a recording
// made before fingerprints -- the working directory its first action
// reported. Empty when neither was measured, and paths are then kept as the
// recording wrote them.
func (i *Integration) recordingWorkspace(ctx context.Context, owner, recordingRunId string, actions []map[string]any) (string, error) {
	steps, err := i.store().query(ownerActor(ctx, owner), "query "+call("workStepsForOwnerRun", map[string]any{"runId": recordingRunId}))
	if err != nil {
		return "", fmt.Errorf("work: read the recording %s's steps: %w", recordingRunId, err)
	}
	sort.SliceStable(steps, func(a, b int) bool { return rowInt(steps[a], "seq") < rowInt(steps[b], "seq") })
	for _, s := range steps {
		if cwd := rowString(rowMap(s, "fingerprint"), "cwd"); cwd != "" {
			return cwd, nil
		}
	}
	for _, o := range actions {
		if cwd := rowString(rowMap(o, "data"), "cwd"); cwd != "" {
			return cwd, nil
		}
	}
	return "", nil
}
