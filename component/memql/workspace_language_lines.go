package memql

// workspace_language_lines.go -- a workspace's language lines, for the offline
// tool that shows a refused one on the files it refuses (epic memql#5356; the
// editor half of its final review, memql#5362).
//
// BuildOfflineSense fails as a whole when one mounted domain's line is
// refused: the strict Init refuses the tree, and the service falls back to the
// workspace graph alone, so completion and hover go dark across the whole
// workspace. The build's error does name the domain, but as one multi-line
// string, and an editor shows an error on a FILE. This file answers what the
// editor needs instead: which of the domains the build mounts are refused,
// why, and where each one's directory is.
//
// IT MOUNTS WHAT THE BUILD MOUNTS. Which directories are domains is decided by
// the one function that mounts them -- MountOverlayDomains, over the root
// resolveDSLRoot picks, the two calls buildOfflineSenseAdapter makes -- and the
// lines are resolved over memqldsl.Tree() while that overlay is up, which is
// the tree Init's first step resolves. Restating the mount rule here instead (a
// directory holding a .memql file, not a core domain, ...) would be a second
// copy of it, and the day the copies disagreed an editor would flag a
// directory no build reads, or say nothing about one that refuses the build.

import (
	"io/fs"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// WorkspaceLanguageLines is the language line of every domain an offline build
// mounts from one workspace, and the reason each refused one is refused.
type WorkspaceLanguageLines struct {
	// Root is the directory the domains sit in, relative to the workspace root
	// and slash-separated: "" when they sit at its top, "dsl" in the engine's
	// own repository. A domain's directory is path.Join(Root, domain), and a
	// LanguageLineProblem's Source is relative to Root.
	Root string
	// Lines is each mounted domain's line, by domain. A domain the embedded
	// tree owns is never mounted -- it speaks the engine's own line -- so it
	// is not here, and neither is a directory the build does not mount.
	Lines LanguageLines
	// Problems is every refusal of a line in Lines, in domain order.
	Problems []LanguageLineProblem
}

// ResolveWorkspaceLanguageLines resolves the language lines of the domains
// BuildOfflineSense mounts from root, with the resolver the engine's Init
// runs. A nil root, or one with nothing to mount, answers no lines.
//
// It holds the build's lock while it mounts, because the overlay goes into the
// process-global tree exactly as the build's does, and it removes the overlay
// before returning.
func ResolveWorkspaceLanguageLines(root fs.FS) WorkspaceLanguageLines {
	dslRoot, prefix := resolveDSLRoot(root)
	out := WorkspaceLanguageLines{Root: prefix, Lines: LanguageLines{}}
	if dslRoot == nil {
		return out
	}

	offlineSenseBuildMu.Lock()
	defer offlineSenseBuildMu.Unlock()

	mounted, _, unmount := memqldsl.MountOverlayDomains(nil, dslRoot)
	defer unmount()
	if len(mounted) == 0 {
		return out
	}
	isMounted := make(map[string]bool, len(mounted))
	for _, d := range mounted {
		isMounted[d] = true
	}

	lines, problems := ResolveLanguageLines(memqldsl.Tree())
	for d, line := range lines {
		if isMounted[d] {
			out.Lines[d] = line
		}
	}
	for _, p := range problems {
		if isMounted[p.Domain] {
			out.Problems = append(out.Problems, p)
		}
	}
	return out
}
