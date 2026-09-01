package packages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// SourceSnapshot is one fetched package source, expanded and validated.
type SourceSnapshot struct {
	// Tree is the package tree with the manifest at its root -- the same FS
	// shape for both source forms (D1), which is what makes every rule below
	// the fetch apply exactly once.
	Tree fs.FS
	// Version is the commit SHA (repo) or content hash (zip) this snapshot is.
	Version string
	// Bytes is the archive as fetched, for storing as the content-addressed
	// Library snapshot (D8). Nil when the source WAS a stored artifact --
	// re-storing bytes the Library already holds would give one snapshot two
	// identities.
	Bytes []byte
	// Root is the on-disk directory when the fetch expanded to disk, empty
	// when the tree is read straight out of an archive. The build stage needs
	// real files; analysis does not.
	Root string
	// cleanup releases Root. Always non-nil.
	cleanup func()
}

// Close releases anything the fetch expanded to disk.
func (s *SourceSnapshot) Close() {
	if s != nil && s.cleanup != nil {
		s.cleanup()
	}
}

// SecretResolver turns a globalSecret NAME into its value.
//
// The whole D14 shape is in this signature: the pipeline holds a NAME, asks
// for the value at the moment of the fetch, and never stores what comes back.
// The value never lands on a row, a snapshot, or a log line.
type SecretResolver func(ctx context.Context, name string) (string, error)

// Fetcher fetches a package source. Its two implementations are the two source
// forms; everything downstream sees one SourceSnapshot either way.
type Fetcher interface {
	FetchRepo(ctx context.Context, repoUrl, ref, tokenRef string, limits Limits) (*SourceSnapshot, error)
	FetchArtifact(ctx context.Context, artifactId string, limits Limits) (*SourceSnapshot, error)
}

// githubFetcher fetches through the GitHub tarball API.
//
// The tarball API rather than a git clone, and that is not an optimisation: a
// clone needs git in the image, a writable home, and a credential helper, and
// it fetches history nobody deploys. One authenticated GET returns exactly the
// tree at a ref.
type githubFetcher struct {
	http    *http.Client
	secrets SecretResolver
	// artifactBytes reads a Library zip artifact's bytes. Separate from the
	// HTTP client because the two source forms share nothing but their output.
	artifactBytes func(ctx context.Context, artifactId string) ([]byte, string, error)
	// tempDir is the expansion root; empty means os.TempDir.
	tempDir string
}

// tarballURL builds the GitHub codeload URL for owner/repo at ref.
//
// An EMPTY ref means the repository's default branch, and the API expresses
// that by leaving the ref off the path entirely -- which is why the empty case
// is a different URL rather than a placeholder string. Resolving a default to
// "main" here would be a guess that is wrong for every repository that never
// renamed, and freezing it onto the row would be worse.
func tarballURL(repoUrl, ref string) (string, error) {
	owner, repo, err := parseGitHubRepo(repoUrl)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(ref) == "" {
		return fmt.Sprintf("https://api.github.com/repos/%s/%s/tarball", owner, repo), nil
	}
	return fmt.Sprintf("https://api.github.com/repos/%s/%s/tarball/%s",
		owner, repo, url.PathEscape(strings.TrimSpace(ref))), nil
}

// parseGitHubRepo pulls owner and repo out of a repository URL.
//
// Refused rather than best-effort for a host that is not GitHub: the fetch
// speaks GitHub's tarball API and its auth header, so a GitLab URL would
// produce an authenticated request to an endpoint that does not exist and a
// refusal naming the wrong thing.
func parseGitHubRepo(repoUrl string) (owner, repo string, err error) {
	raw := strings.TrimSpace(repoUrl)
	if raw == "" {
		return "", "", refuse(CodeSourceUnreadable, "this package declares no repository URL")
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", "", refuse(CodeSourceUnreadable, "%q is not a URL this cluster can read: %v", raw, perr)
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && host != "www.github.com" {
		return "", "", refuse(CodeSourceUnreadable,
			"this cluster fetches packages from github.com, and %q is on %q. Upload the tree as a zip instead -- the two source forms are interchangeable.",
			raw, u.Hostname())
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", refuse(CodeSourceUnreadable,
			"%q does not name an owner and a repository (expected https://github.com/<owner>/<repo>)", raw)
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

func (f *githubFetcher) FetchRepo(ctx context.Context, repoUrl, ref, tokenRef string, limits Limits) (*SourceSnapshot, error) {
	target, err := tarballURL(repoUrl, ref)
	if err != nil {
		return nil, err
	}

	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if rerr != nil {
		return nil, rerr
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "memql-packages")

	// D14: resolved HERE, at the moment of the fetch, into a local that dies
	// with this function. Nothing above this line holds a token and nothing
	// below it stores one.
	if name := strings.TrimSpace(tokenRef); name != "" {
		if f.secrets == nil {
			return nil, refuse(CodeSourceUnreadable,
				"this package names the secret %q for its repository, and this node cannot resolve secrets", name)
		}
		token, serr := f.secrets(ctx, name)
		if serr != nil || strings.TrimSpace(token) == "" {
			return nil, refuse(CodeSourceUnreadable,
				"this package names the secret %q for its repository, and this cluster has no usable value stored under that name. Add it under Settings, or clear the field if the repository is public.",
				name)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := f.http
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, derr := client.Do(req)
	if derr != nil {
		return nil, refuse(CodeSourceUnreadable, "this cluster could not reach %s: %v", target, derr)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// 404 is what GitHub answers for a private repository the token
		// cannot see, so the message names BOTH possibilities rather than
		// asserting the one it cannot distinguish.
		return nil, refuse(CodeSourceUnreadable,
			"GitHub answered 404 for this repository at ref %q. Either it does not exist, or it is private and this package's access token does not reach it.",
			refOrDefault(ref))
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, refuse(CodeSourceUnreadable,
			"GitHub refused this cluster's access to the repository (HTTP %d). Check the token this package names.", resp.StatusCode)
	case resp.StatusCode >= 300:
		return nil, refuse(CodeSourceUnreadable, "GitHub answered HTTP %d for this repository", resp.StatusCode)
	}

	// The commit SHA is the snapshot's identity, and GitHub puts it in the
	// tarball's own top-level directory name (<owner>-<repo>-<sha>) as well as
	// in ETag. ExtractTarGz reports the stripped root, so the SHA is read from
	// the directory name it returns -- one source rather than a header that
	// several proxies rewrite.
	dir, mkErr := os.MkdirTemp(f.tempDir, "memql-package-*")
	if mkErr != nil {
		return nil, mkErr
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	root, xerr := ExtractTarGz(io.LimitReader(resp.Body, limits.normalized().MaxSourceBytes+1), dir, limits)
	if xerr != nil {
		cleanup()
		return nil, xerr
	}

	return &SourceSnapshot{
		Tree:    os.DirFS(root),
		Version: versionFromTarballRoot(root, resp.Header.Get("ETag")),
		Root:    root,
		cleanup: cleanup,
	}, nil
}

func refOrDefault(ref string) string {
	if strings.TrimSpace(ref) == "" {
		return "the default branch"
	}
	return ref
}

// versionFromTarballRoot recovers the commit SHA from the directory GitHub
// synthesized, falling back to the ETag when the shape is unfamiliar.
//
// It returns whatever it found rather than an error: a snapshot with an
// unrecognised version still deploys, it just cannot be compared against
// upstream -- and refusing the deploy over a naming convention would be
// refusing over the wrong thing.
func versionFromTarballRoot(root, etag string) string {
	base := root
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.LastIndexByte(base, '-'); i >= 0 && i+1 < len(base) {
		if candidate := base[i+1:]; len(candidate) >= 7 && isHex(candidate) {
			return candidate
		}
	}
	return strings.Trim(strings.TrimPrefix(strings.TrimSpace(etag), "W/"), `"`)
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return len(s) > 0
}

// FetchArtifact reads a zip already stored in the Library (D8).
//
// It sets Bytes to nil: the artifact IS the stored snapshot, so writing a
// second copy would give one source two Library identities and make the
// provenance link ambiguous. Re-analysis of an artifact-sourced package
// therefore costs nothing but a read, which is section I's "repeatable from
// the stored snapshot without refetching" in its cheapest form.
func (f *githubFetcher) FetchArtifact(ctx context.Context, artifactId string, limits Limits) (*SourceSnapshot, error) {
	if f.artifactBytes == nil {
		return nil, refuse(CodeSourceUnreadable, "this node cannot read Library artifacts")
	}
	raw, _, err := f.artifactBytes(ctx, artifactId)
	if err != nil {
		return nil, err
	}
	tree, zerr := OpenZip(readerAtBytes(raw), int64(len(raw)), limits)
	if zerr != nil {
		return nil, zerr
	}
	return &SourceSnapshot{
		Tree:    tree,
		Version: contentHash(raw),
		cleanup: func() {},
	}, nil
}

// contentHash is a zip source's identity: the digest of its bytes. Content
// addressing means re-uploading the same tree produces the same version, so a
// redeploy of unchanged bytes is visibly a redeploy of the same thing.
func contentHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type readerAtBytes []byte

func (b readerAtBytes) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
