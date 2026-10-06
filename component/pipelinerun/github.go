package pipelinerun

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	// testing/fstest in production code, deliberately, as component/packages'
	// probe does: MapFS is the smallest fs.FS over bytes already in memory --
	// it synthesizes the directories a kept file implies, which is what the
	// Go graph scan walks -- and the package imports neither `testing` nor
	// `flag`, so nothing of a test binary comes with it.
	"testing/fstest"

	"github.com/znasllc-io/memql/component/identity/githubapp"
)

// PackagesAccess is what the production GitHub port borrows from Deployables:
// the node's one GitHub App client and the verified grant path.
// *packages.Integration satisfies it.
type PackagesAccess interface {
	GitHub() *githubapp.Client
	InstallationToken(ctx context.Context, credentialID, ownerUserID, owner, repo string) (token string, installationID int64, err error)
}

// NewGitHub is the production GitHub port over Deployables.
//
// The client is asked for AT EACH CALL and never held: packages builds it
// once, from what app/ installed by the time of the first call, and asking
// while wiring would freeze this node's client to the environment alone
// (packages.Integration.GitHub's own warning).
func NewGitHub(pkgs PackagesAccess) GitHub { return &packagesGitHub{pkgs: pkgs} }

type packagesGitHub struct {
	pkgs PackagesAccess
}

func (g *packagesGitHub) client() (*githubapp.Client, error) {
	if g == nil || g.pkgs == nil {
		return nil, githubapp.ErrNotConfigured
	}
	c := g.pkgs.GitHub()
	if c == nil {
		return nil, githubapp.ErrNotConfigured
	}
	return c, nil
}

func (g *packagesGitHub) Configured() bool {
	c, err := g.client()
	return err == nil && c.Configured()
}

func (g *packagesGitHub) InstallationToken(ctx context.Context, credentialID, ownerUserID, repository string) (string, int64, error) {
	if g == nil || g.pkgs == nil {
		return "", 0, githubapp.ErrNotConfigured
	}
	owner, name, err := splitRepository(repository)
	if err != nil {
		return "", 0, err
	}
	return g.pkgs.InstallationToken(ctx, credentialID, ownerUserID, owner, name)
}

func (g *packagesGitHub) CreateCheckRun(ctx context.Context, token, repository string, run githubapp.CheckRun) (int64, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return 0, err
	}
	return c.CreateCheckRun(ctx, token, owner, name, run)
}

func (g *packagesGitHub) UpdateCheckRun(ctx context.Context, token, repository string, id int64, run githubapp.CheckRun) error {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return err
	}
	return c.UpdateCheckRun(ctx, token, owner, name, id, run)
}

func (g *packagesGitHub) Repository(ctx context.Context, token, repository string) (githubapp.RepositoryInfo, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return githubapp.RepositoryInfo{}, err
	}
	return c.Repository(ctx, token, owner, name)
}

func (g *packagesGitHub) BranchHead(ctx context.Context, token, repository, branch string) (string, string, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return "", "", err
	}
	return c.BranchHead(ctx, token, owner, name, branch)
}

func (g *packagesGitHub) OpenPullRequests(ctx context.Context, token, repository string) ([]githubapp.PullRequestHead, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return nil, err
	}
	return c.OpenPullRequests(ctx, token, owner, name)
}

func (g *packagesGitHub) PullRequestHead(ctx context.Context, token, repository string, number int) (githubapp.PullRequestHead, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return githubapp.PullRequestHead{}, err
	}
	return c.PullRequest(ctx, token, owner, name, number)
}

func (g *packagesGitHub) Compare(ctx context.Context, token, repository, base, head string) ([]string, bool, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return nil, false, err
	}
	return c.Compare(ctx, token, owner, name, base, head)
}

func (g *packagesGitHub) CommitForRef(ctx context.Context, token, repository, ref string) (string, string, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return "", "", err
	}
	return c.CommitForRef(ctx, token, owner, name, ref)
}

func (g *packagesGitHub) Tree(ctx context.Context, token, repository, sha string, keep func(string) bool, maxBytes int64) (fs.FS, error) {
	c, owner, name, err := g.target(repository)
	if err != nil {
		return nil, err
	}
	body, err := c.Tarball(ctx, token, owner, name, sha)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return ReadTree(body, keep, maxBytes)
}

func (g *packagesGitHub) Installations(ctx context.Context) ([]githubapp.AppInstallation, error) {
	c, err := g.client()
	if err != nil {
		return nil, err
	}
	return c.Installations(ctx)
}

func (g *packagesGitHub) target(repository string) (*githubapp.Client, string, string, error) {
	c, err := g.client()
	if err != nil {
		return nil, "", "", err
	}
	owner, name, err := splitRepository(repository)
	if err != nil {
		return nil, "", "", err
	}
	return c, owner, name, nil
}

// splitRepository splits "owner/name".
func splitRepository(repository string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(repository), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("pipelines: %q is not an owner/name repository", repository)
	}
	return owner, name, nil
}

// ReadTree reads a gzipped GitHub tarball and keeps only the regular files
// keep admits, by repository-relative path.
//
// GitHub wraps the whole repository in ONE synthesized top-level directory,
// `<owner>-<repo>-<sha>/`, which is not part of the tree the author wrote; it
// is stripped, so keep sees "go.mod", never "acme-shop-0123abc/go.mod". The
// prefix is learned from the archive's first member, and a member outside it
// keeps its own path rather than losing its first directory to a guess.
//
// Nothing but what keep admits is read into memory, and more than maxBytes of
// it is ErrTreeTooLarge: a repository is somebody else's size. Symlinks and
// every other non-regular member are dropped -- a link's own name is a clean
// path while its target is arbitrary -- and a member whose path would escape
// the tree is refused rather than read.
func ReadTree(r io.Reader, keep func(path string) bool, maxBytes int64) (fs.FS, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("pipelines: the repository archive is not gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	tree := fstest.MapFS{}
	var (
		total       int64
		prefix      string
		prefixKnown bool
	)
	for {
		hdr, nerr := tr.Next()
		if errors.Is(nerr, io.EOF) {
			break
		}
		if nerr != nil {
			return nil, fmt.Errorf("pipelines: the repository archive could not be read: %w", nerr)
		}
		// A PAX global header describes the archive, not a member of it;
		// every GitHub tarball starts with one (git archive's comment=<sha>).
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name, perr := cleanMember(hdr.Name)
		if perr != nil {
			return nil, perr
		}
		if name == "" {
			continue
		}
		if !prefixKnown {
			prefixKnown = true
			if first, _, nested := strings.Cut(name, "/"); nested || hdr.Typeflag == tar.TypeDir {
				prefix = first + "/"
			}
		}
		if prefix != "" {
			if name+"/" == prefix {
				continue // the synthesized directory itself
			}
			if rel, under := strings.CutPrefix(name, prefix); under {
				name = rel
			}
		}
		if hdr.Typeflag != tar.TypeReg || keep == nil || !keep(name) {
			continue
		}
		if hdr.Size < 0 || total+hdr.Size > maxBytes {
			return nil, fmt.Errorf("%w (more than %d bytes kept, at %q)", ErrTreeTooLarge, maxBytes, name)
		}
		// LimitReader as well as the header check: the size is the archive's
		// claim, and a lying header must not read past the cap.
		data, rerr := io.ReadAll(io.LimitReader(tr, maxBytes-total+1))
		if rerr != nil {
			return nil, fmt.Errorf("pipelines: reading %q from the repository archive: %w", name, rerr)
		}
		total += int64(len(data))
		if total > maxBytes {
			return nil, fmt.Errorf("%w (more than %d bytes kept, at %q)", ErrTreeTooLarge, maxBytes, name)
		}
		tree[name] = &fstest.MapFile{Data: data, Mode: 0o644}
	}
	return tree, nil
}

// cleanMember is an archive member's path, cleaned, or "" for the root; a
// path that is absolute, carries a NUL, or climbs out of the tree is refused.
func cleanMember(raw string) (string, error) {
	name := strings.ReplaceAll(strings.TrimSpace(raw), `\`, "/")
	if strings.ContainsRune(name, 0) {
		return "", errors.New("pipelines: the repository archive holds a member with a NUL in its name")
	}
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("pipelines: the repository archive holds an absolute path (%q)", raw)
	}
	name = strings.TrimSuffix(name, "/")
	if name == "" || name == "." {
		return "", nil
	}
	cleaned := path.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || !fs.ValidPath(cleaned) {
		return "", fmt.Errorf("pipelines: the repository archive holds a member outside the tree (%q)", raw)
	}
	return cleaned, nil
}
