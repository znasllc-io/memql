package work

// snapshot.go -- the workspace a session step's branch starts against (epic
// memql#5414; design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// D19).
//
// When the step a person branches from, or re-runs, was answered by an app
// session, the new session must start against the workspace AS IT WAS BEFORE
// that step -- not the directory the previous version's later steps have
// since changed. The recording holds the files the earlier sessions read or
// wrote, content-addressed in the Library, so the workspace is REBUILT from
// them: for every path, the newest recorded content wins.
//
// A PARTIAL SNAPSHOT IS REFUSED, NAMING THE FILE. A recorded action whose
// content was omitted -- above the per-file cap, or a Library write that
// failed -- leaves that path unknown, and a branch started without it would
// diverge from the run it claims to branch without saying so. An `exec` whose
// file effects the harness never reported is different: nothing recorded it
// at all, so there is no file to name. That is COUNTED (UnrecordedCommands)
// and shown, never silently treated as complete and never refused -- refusing
// it would refuse almost every branch, since almost every session runs a
// command.

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// FileEffect is one recorded read or write of one file, in action order.
type FileEffect struct {
	// Order is the action's position across the recordings, oldest first.
	Order int
	// Path is workspace-relative.
	Path string
	// FileId is the v1:library:file holding the content; empty when omitted.
	FileId string
	// Omitted says why the content is missing; empty when it is present.
	Omitted string
}

// SnapshotFile is one file of a rebuilt workspace.
type SnapshotFile struct {
	Path   string `json:"path"`
	FileId string `json:"fileId"`
}

// Snapshot is a rebuilt workspace.
type Snapshot struct {
	// Files are sorted by path.
	Files []SnapshotFile `json:"files"`
	// UnrecordedCommands counts earlier shell commands whose file effects no
	// recording reported: the snapshot cannot contain what they changed.
	UnrecordedCommands int `json:"unrecordedCommands"`
}

// SnapshotOmittedError refuses a snapshot whose newest content for a path was
// never recorded.
type SnapshotOmittedError struct {
	Path string
	Why  string
}

func (e *SnapshotOmittedError) Error() string {
	why := strings.TrimSpace(e.Why)
	if why == "" {
		why = "its content was not recorded"
	}
	return fmt.Sprintf("snapshot_content_omitted: the workspace before this step cannot be rebuilt: %s (%s)", e.Path, why)
}

// SnapshotOf rebuilds the workspace from the recorded effects: the newest
// effect per path wins, and a path whose newest effect has no content refuses
// the whole snapshot, naming the first such path in path order. An earlier
// omitted content that a later action rewrote with recorded content is fine:
// what a branch needs is the file's LAST state.
func SnapshotOf(effects []FileEffect, unrecordedCommands int) (Snapshot, error) {
	sorted := append([]FileEffect(nil), effects...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Order < sorted[j].Order })
	latest := map[string]FileEffect{}
	for _, e := range sorted {
		p := cleanWorkspacePath(e.Path)
		if p == "" {
			continue
		}
		e.Path = p
		latest[p] = e
	}
	paths := make([]string, 0, len(latest))
	for p := range latest {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	snap := Snapshot{Files: []SnapshotFile{}, UnrecordedCommands: unrecordedCommands}
	for _, p := range paths {
		e := latest[p]
		if e.FileId == "" || e.Omitted != "" {
			return Snapshot{}, &SnapshotOmittedError{Path: p, Why: e.Omitted}
		}
		snap.Files = append(snap.Files, SnapshotFile{Path: p, FileId: e.FileId})
	}
	return snap, nil
}

// cleanWorkspacePath normalizes a recorded path so ./a/../b and b are one
// file. A path that climbs out of the workspace is kept as written: it is not
// the workspace's, but refusing it here would hide it from the person reading
// the snapshot.
func cleanWorkspacePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	c := path.Clean(p)
	if c == "." {
		return ""
	}
	return strings.TrimPrefix(c, "./")
}

// Object is the stored form, the shape run.rerun.snapshot declares.
func (s Snapshot) Object() map[string]any {
	files := make([]any, 0, len(s.Files))
	for _, f := range s.Files {
		files = append(files, map[string]any{"path": f.Path, "fileId": f.FileId})
	}
	return map[string]any{"files": files, "unrecordedCommands": s.UnrecordedCommands}
}

// ParseSnapshot reads a stored snapshot; nil when absent.
func ParseSnapshot(v any) *Snapshot {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	raw, _ := m["files"].([]any)
	snap := &Snapshot{Files: []SnapshotFile{}, UnrecordedCommands: intOf(m["unrecordedCommands"])}
	for _, r := range raw {
		f, _ := r.(map[string]any)
		p, _ := f["path"].(string)
		id, _ := f["fileId"].(string)
		if p != "" && id != "" {
			snap.Files = append(snap.Files, SnapshotFile{Path: p, FileId: id})
		}
	}
	return snap
}
