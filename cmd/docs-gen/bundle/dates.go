package bundle

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ErrShallowClone refuses to date pages in a shallow clone. At depth 1 every
// page's last commit is the tip, so every lastUpdated and every sitemap
// lastmod would read as the release day -- dates that look right and say
// nothing.
var ErrShallowClone = errors.New("the repository is a shallow clone: every page would date to the tip commit; fetch the full history (actions/checkout fetch-depth: 0, or git fetch --unshallow)")

// Dater answers when a page last changed.
type Dater interface {
	// LastUpdated returns the committer date of the last commit that touched
	// the repository-relative path.
	LastUpdated(repoPath string) (time.Time, error)
}

// GitDater dates pages from git history: the committer date of
// `git log -1 --format=%cI <ref> -- <path>`.
type GitDater struct {
	Root string
	Ref  string
}

// NewGitDater returns a dater for the repository at root, reading history
// from ref (HEAD when empty). It refuses a shallow clone.
func NewGitDater(root, ref string) (*GitDater, error) {
	if ref == "" {
		ref = "HEAD"
	}
	out, err := git(root, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTree, err)
	}
	if out == "true" {
		return nil, ErrShallowClone
	}
	return &GitDater{Root: root, Ref: ref}, nil
}

// LastUpdated implements Dater.
func (d *GitDater) LastUpdated(repoPath string) (time.Time, error) {
	out, err := git(d.Root, "log", "-1", "--format=%cI", d.Ref, "--", repoPath)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %v", ErrTree, err)
	}
	if out == "" {
		return time.Time{}, fmt.Errorf("%w: %s is in no commit at %s; commit it before building the bundle", ErrUndated, repoPath, d.Ref)
	}
	return time.Parse(time.RFC3339, out)
}

// Meta is what the manifest says about the build itself.
type Meta struct {
	Version string
	// Tag is the release tag at the built commit (v-prefixed or bare), or ""
	// when the commit carries none -- a local build between releases.
	Tag        string
	Commit     string
	ReleasedAt time.Time // the built commit's committer date
	SiteURL    string
}

// GitMeta reads the commit, its date and its release tag at HEAD of the
// repository at root. The tag is the one named v<version> or <version> that
// points at HEAD; release tags come in both spellings (bare through 0.14.0).
func GitMeta(root, version, siteURL string) (Meta, error) {
	m := Meta{Version: version, SiteURL: siteURL}
	commit, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		return m, fmt.Errorf("%w: %v", ErrTree, err)
	}
	m.Commit = commit
	date, err := git(root, "log", "-1", "--format=%cI", "HEAD")
	if err != nil {
		return m, fmt.Errorf("%w: %v", ErrTree, err)
	}
	if m.ReleasedAt, err = time.Parse(time.RFC3339, date); err != nil {
		return m, err
	}
	for _, name := range []string{"v" + version, version} {
		at, err := git(root, "rev-parse", "-q", "--verify", "refs/tags/"+name+"^{commit}")
		if err == nil && at == commit {
			m.Tag = name
			break
		}
	}
	return m, nil
}

func git(root string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
