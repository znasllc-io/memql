package appsession

import (
	"fmt"
	"path/filepath"
	"strings"
)

// protected.go keeps a session away from what it must never touch: this
// worker's own files -- its configuration holds the worker tokens, and
// policy.yaml's apps.allow is the app consent gate -- and this machine's
// fs.deny list.
//
// THE WORKSPACE IS THE WRITE GRANT. The app may edit any file in it and run
// shell commands that write anywhere in it (harness.claudeGrantArgs), so a
// workspace that CONTAINS a protected path hands the session that path: a
// delegation policy whose workspace root is "~" names the home directory,
// ~/.memql included, and CheckWorkspace (tools.Policy.CheckPath) refuses
// only a path at or under an fs.deny entry -- never one above it, never the
// worker's own directory, and under the default policy not even "/". So a
// workspace that is, contains or lies inside a protected path is refused
// here, before anything is written into it.
//
// That keeps the paths out of what the app may WRITE. What it may READ is the
// app's to enforce, so the same paths reach it as harness.Spec.DenyPaths:
// without them a sandboxed `cat ~/.memql/worker.yaml > out.txt` leaves the
// worker token in a new file, and the session pushes new files to the
// Library.

// protectedPath is one path a session may not touch, and whose it is, in the
// words a refusal names it with.
type protectedPath struct {
	path  string
	which string
}

// The two kinds of protected path, as a refusal completes "..., which ...".
const (
	protectedWorker = "is this worker's own (its tokens, and policy.yaml, whose apps.allow is the app consent gate)"
	protectedDeny   = "this machine's fs.deny lists"
)

// protectedPaths reads the paths this session may not touch: the worker's own
// first, then fs.deny as policy.yaml says it now.
//
// A relative entry names no place on this machine, and CheckPath never
// matches one either, so it is left out rather than resolved against
// whatever directory the worker happened to start in.
func (s *session) protectedPaths() []protectedPath {
	opts := s.manager.opts
	var out []protectedPath
	add := func(path, which string) {
		path = strings.TrimSpace(path)
		if path == "" || !filepath.IsAbs(path) {
			return
		}
		out = append(out, protectedPath{path: filepath.Clean(path), which: which})
	}
	for _, path := range opts.WorkerPaths {
		add(path, protectedWorker)
	}
	if opts.DenyPaths != nil {
		for _, path := range opts.DenyPaths() {
			add(path, protectedDeny)
		}
	}
	return out
}

// checkWorkspaceOverlap refuses a workspace that is, contains or lies inside
// a protected path, naming the first it meets.
func checkWorkspaceOverlap(workspace string, protected []protectedPath) error {
	for _, p := range protected {
		if overlaps(workspace, p.path) {
			return fmt.Errorf("app session: workspace %q overlaps %q, which %s: the app may write "+
				"anywhere in its workspace, so it may not run in, above or below that path",
				workspace, p.path, p.which)
		}
	}
	return nil
}

// appDenyPaths is the protected paths as the app is handed them: every form
// of each (pathForms), once, in order.
func appDenyPaths(protected []protectedPath) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range protected {
		for _, form := range pathForms(p.path) {
			if !seen[form] {
				seen[form] = true
				out = append(out, form)
			}
		}
	}
	return out
}

// overlaps reports whether either path is the other or lies under it, in any
// form either takes.
func overlaps(a, b string) bool {
	for _, x := range pathForms(a) {
		for _, y := range pathForms(b) {
			if within(x, y) || within(y, x) {
				return true
			}
		}
	}
	return false
}

// pathForms is a path as written and as the filesystem resolves it
// (resolvedPath), so a symlink cannot carry one past the other: on macOS /tmp
// and /var are both symlinks into /private, and the sandbox and the app see
// resolved paths.
func pathForms(path string) []string {
	path = filepath.Clean(path)
	if resolved := resolvedPath(path); resolved != path {
		return []string{path, resolved}
	}
	return []string{path}
}
