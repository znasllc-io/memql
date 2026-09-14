package memql

// workspace_language_lines.go -- a workspace's language lines, for the offline
// tool that shows a refused one on the files it refuses (epic memql#5356; the
// editor half of its final review, memql#5362).
//
// BuildOfflineSense fails as a whole when one mounted domain's line is
// refused: the strict Init refuses the tree, and the service falls back to the
// workspace graph alone, so hover and every name the registry would have
// supplied are gone across the whole workspace. The build's error does name
// the domain, but as one multi-line string, and an editor shows an error on a
// FILE. This file answers what the editor needs instead: which of the domains
// the build mounts are refused, why, and where each one's directory is.
//
// THE BUILD'S OWN ANSWER. BuildOfflineSenseWithLanguageLines hands back the
// lines its Init resolved -- Init's lines and the refusals Init recorded on
// its load report -- so the lines an editor shows and the failure it explains
// are one answer, even when a memql.toml changes while the build runs.
// ResolveWorkspaceLanguageLines answers the same question without a build.
//
// IT MOUNTS WHAT THE BUILD MOUNTS. Which directories are domains is decided by
// the one function that mounts them -- MountOverlayDomains, over the root
// resolveDSLRoot picks, the two calls the build makes -- and only those
// domains are kept. Restating the mount rule here instead (a directory holding
// a .memql file, not a core domain, ...) would be a second copy of it, and
// the day the copies disagreed an editor would flag a directory no build
// reads, or say nothing about one that refuses the build.

import (
	"io/fs"
	"strings"

	"github.com/znasllc-io/memql/component/memql/baseloader"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// WorkspaceLanguageLines is the language line of every domain an offline build
// mounts from one workspace, and the reason each refused one is refused.
type WorkspaceLanguageLines struct {
	// Root is the directory the domains sit in, relative to the workspace root
	// and slash-separated: "" when they sit at its top, "dsl" in the engine's
	// own repository and in a product repository. A domain's directory is
	// path.Join(Root, domain), and a LanguageLineProblem's Source is relative
	// to Root.
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
// runs, and without a build. A nil root, or one with nothing to mount,
// answers no lines. A caller that builds anyway takes the build's own lines
// from BuildOfflineSenseWithLanguageLines instead.
//
// It holds the build's lock while it mounts, because the overlay goes into the
// process-global tree exactly as the build's does, and it removes the overlay
// before returning.
func ResolveWorkspaceLanguageLines(root fs.FS) WorkspaceLanguageLines {
	dslRoot, prefix := resolveDSLRoot(root)
	if dslRoot == nil {
		return WorkspaceLanguageLines{Root: prefix, Lines: LanguageLines{}}
	}

	offlineSenseBuildMu.Lock()
	defer offlineSenseBuildMu.Unlock()

	mounted, _, unmount := memqldsl.MountOverlayDomains(nil, dslRoot)
	defer unmount()
	lines, problems := ResolveLanguageLines(memqldsl.Tree())
	return mountedLanguageLines(prefix, mounted, lines, problems)
}

// mountedLanguageLines keeps, of one resolution of the merged tree, the lines
// and refusals of the domains mounted from the workspace.
func mountedLanguageLines(prefix string, mounted []string, lines LanguageLines, problems []LanguageLineProblem) WorkspaceLanguageLines {
	out := WorkspaceLanguageLines{Root: prefix, Lines: LanguageLines{}}
	isMounted := make(map[string]bool, len(mounted))
	for _, d := range mounted {
		isMounted[d] = true
	}
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

// initLanguageLines is the language lines an engine's Init resolved, for the
// domains mounted from the workspace: the lines Init read (languageLines) and
// the refusals it recorded on its load report, one skip per refusal
// (languageLineSkip). The report keeps a refusal's message but not its code;
// the code is read back from the message, which the parser ends with
// " [<code>]" (LanguageLineProblem.Message).
func initLanguageLines(eng *MemQLEngine, prefix string, mounted []string) WorkspaceLanguageLines {
	var problems []LanguageLineProblem
	if report := eng.LoadReport(); report != nil {
		report.mu.Lock()
		skipped := append([]baseloader.Skip(nil), report.Skipped...)
		report.mu.Unlock()
		for _, s := range skipped {
			if s.Component != languageLineComponent || s.Phase != languageLinePhase {
				continue
			}
			problems = append(problems, LanguageLineProblem{
				Domain:  s.Name,
				Source:  s.File,
				Code:    languageLineCode(s.Err),
				Message: s.Err,
			})
		}
	}
	return mountedLanguageLines(prefix, mounted, eng.languageLines, problems)
}

// languageLineCode is the code a refusal's message ends with, "" for a message
// that ends with none.
func languageLineCode(message string) string {
	i := strings.LastIndex(message, " [")
	if i < 0 || !strings.HasSuffix(message, "]") {
		return ""
	}
	return message[i+2 : len(message)-1]
}
