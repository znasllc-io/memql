package appsession

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// workspace.go settles WHERE a session runs.
//
// AN EMPTY AppSessionStart.workspace MEANS "THE MACHINE CHOOSES". The engine
// cannot know a path on somebody else's computer, and a delegation policy
// with no workspace root -- the state of a cluster that never set one --
// sends none. Refusing that (which this runner did) failed every session the
// engine opened without a policy, a structured app-door call included, with
// "no workspace in AppSessionStart" and nothing a person could act on. So the
// machine makes a directory of its own:
//
//	<fs.workspace_root>/<home>/<key>         when policy.yaml names a root
//	<platform data dir>/<home>/<key>         otherwise (scratchRootFor)
//
// The key is the RUN when the session belongs to one (AppSessionStart.run_id)
// and the SESSION otherwise. A run's sessions share one directory, so a later
// step finds what an earlier one made; a session with no run gets its own, and
// that one is removed when the session ends -- nothing else would ever clean
// it. A run's directory outlives the session: the session's end takes away its
// own scaffolding (the bearer's configuration, the transcript) and leaves the
// run's files.
//
// Two session-keyed directories are KEPT, and nothing collects them yet:
// one whose outputs did not reach the Library (it holds the only copy of what
// the app made; keepWorkspace says where), and an open session's, which a
// person may still be working in after the session ends.
//
// NEVER UNDER ~/.memql. That directory holds the worker's tokens and the
// consent file, and a session runs shell commands where it works.
//
// The chosen path goes through CheckWorkspace and the protected-path check
// (protected.go) like a path the engine named: this machine's fs.deny list
// binds its own choice as well.

// scratchSegmentMax bounds one directory name built from an id.
const scratchSegmentMax = 128

// scratchRootFor is where a machine keeps the workspaces it chooses when
// policy.yaml names no fs.workspace_root. A pure function of the platform
// facts, so every platform's answer is table-testable on any one of them.
//
// macOS: ~/Library/Application Support/MemQL/workspaces. Elsewhere, the XDG
// data directory: $XDG_DATA_HOME/memql/workspaces, or
// ~/.local/share/memql/workspaces when that is unset -- or RELATIVE, which
// the XDG specification says to ignore. Not the config directory: the
// default fs.deny list refuses ~/.config.
//
// Empty when there is nowhere to put it, which the session refuses by name.
func scratchRootFor(goos, home, xdgDataHome string) string {
	if goos == "darwin" {
		if home == "" {
			return ""
		}
		return filepath.Join(home, "Library", "Application Support", "MemQL", "workspaces")
	}
	base := ""
	switch {
	case filepath.IsAbs(xdgDataHome):
		base = xdgDataHome
	case home != "":
		base = filepath.Join(home, ".local", "share")
	default:
		return ""
	}
	return filepath.Join(base, "memql", "workspaces")
}

// defaultScratchRoot is scratchRootFor for this machine.
func defaultScratchRoot() string {
	home, _ := os.UserHomeDir()
	return scratchRootFor(runtime.GOOS, home, os.Getenv("XDG_DATA_HOME"))
}

// scratchSegment makes one directory name out of an id.
//
// The ids are server-minted (`v1:work:run:...`) and the home is a host name,
// so the dots a host name carries are kept and everything that is not a
// letter, a digit, '-', '_' or '.' becomes '_'. What comes out can neither
// climb out of its parent ("..", or a separator) nor hide in it (a leading
// dot).
func scratchSegment(id string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, strings.TrimSpace(id))
	if strings.Trim(cleaned, ".") == "" {
		return "unnamed"
	}
	if strings.HasPrefix(cleaned, ".") {
		cleaned = "_" + cleaned[1:]
	}
	if len(cleaned) > scratchSegmentMax {
		cleaned = cleaned[:scratchSegmentMax]
	}
	return cleaned
}

// chooseWorkspace is the directory this machine picks for a session whose
// start named none, and whether it belongs to the session alone.
func (s *session) chooseWorkspace() (path string, bySession bool, err error) {
	opts := s.manager.opts
	root, source := "", "fs.workspace_root"
	if opts.WorkspaceRoot != nil {
		root = strings.TrimSpace(opts.WorkspaceRoot())
	}
	if root == "" {
		root, source = strings.TrimSpace(opts.ScratchRoot), "the scratch directory"
	}
	if root == "" {
		root = defaultScratchRoot()
	}
	if root == "" {
		return "", false, errors.New("app session: no workspace in AppSessionStart, and this machine has " +
			"no home directory to make one under; set fs.workspace_root in policy.yaml")
	}
	if !filepath.IsAbs(root) {
		return "", false, fmt.Errorf("app session: no workspace in AppSessionStart, and %s %q "+
			"is not an absolute path to make one under", source, root)
	}
	home := strings.TrimSpace(opts.Home)
	if home == "" {
		home = "default"
	}
	key := strings.TrimSpace(s.start.GetRunId())
	if key == "" {
		key, bySession = s.id, true
	}
	return filepath.Join(root, scratchSegment(home), scratchSegment(key)), bySession, nil
}

// releaseWorkspace removes the directory this machine made for this session
// alone, once its outputs have been pushed. A workspace the engine named is
// never touched -- it can be a real project -- and neither is a run's or an
// open session's.
func (s *session) releaseWorkspace() {
	s.mu.Lock()
	path := ""
	if s.ownsWorkspace {
		path = s.workspace
	}
	s.ownsWorkspace = false
	s.mu.Unlock()
	if path == "" {
		return
	}
	if err := os.RemoveAll(path); err != nil {
		s.logger.Warn("app session: the workspace this machine made for it could not be removed",
			"workspace", path, "error", err)
	}
}

// keepWorkspace leaves the directory this machine made for this session on
// disk, and says where in the machine's log: its outputs are not in the
// Library, so it holds the only copy of what the app made. Nothing removes it
// afterwards; the owner does, once the files are recovered.
func (s *session) keepWorkspace(why string) {
	s.mu.Lock()
	path := ""
	if s.ownsWorkspace {
		path = s.workspace
	}
	s.ownsWorkspace = false
	s.mu.Unlock()
	if path == "" {
		return
	}
	s.logger.Warn("app session: keeping the workspace this machine made for it, because "+why+
		"; recover its files from there and remove it by hand", "workspace", path)
}
