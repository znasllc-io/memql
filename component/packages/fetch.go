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

	"github.com/znasllc-io/memql/component/packages/githubapp"
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

// RepoSource is what a repository fetch is told: the four facts off the package
// row, and nothing resolved.
//
// CredentialId is a NAME and OwnerUserId is whose name it is resolved under.
// The pair travels together because resolution is owner-scoped by
// construction (epic memql#4885, D10): the fetcher asks for the value at the
// moment of the fetch, under the PACKAGE OWNER's actor rather than the
// caller's, and never stores what comes back. A cluster owner deploying
// somebody's package fetches under that package's own credential, and a
// package naming another person's credential is refused by name.
type RepoSource struct {
	RepoUrl      string
	Ref          string
	CredentialId string
	OwnerUserId  string
}

// Fetcher fetches a package source. Its two implementations are the two source
// forms; everything downstream sees one SourceSnapshot either way.
type Fetcher interface {
	FetchRepo(ctx context.Context, src RepoSource, limits Limits) (*SourceSnapshot, error)
	FetchArtifact(ctx context.Context, artifactId string, limits Limits) (*SourceSnapshot, error)
}

// githubFetcher fetches through the GitHub tarball API.
//
// The tarball API rather than a git clone, and that is not an optimisation: a
// clone needs git in the image, a writable home, and a credential helper, and
// it fetches history nobody deploys. One authenticated GET returns exactly the
// tree at a ref.
type githubFetcher struct {
	http *http.Client
	// credentials unseals the package's credential at fetch time. Nil on a
	// node that cannot resolve credentials, which a private repository then
	// REFUSES against rather than fetching anonymously and reading 404.
	credentials CredentialResolver
	// artifactBytes reads a Library zip artifact's bytes. Separate from the
	// HTTP client because the two source forms share nothing but their output.
	artifactBytes func(ctx context.Context, artifactId string) ([]byte, string, error)
	// github is the cluster's GitHub App client (epic memql#4912). It is what
	// turns a grant into the INSTALLATION token this fetch carries, so a
	// deploy never depends on the person's own eight-hour token being alive
	// (C6). Nil on a node with no app configured, which a grant-sourced
	// package then refuses against by name rather than fetching under a token
	// this node could not renew.
	github *githubapp.Client
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
//
// The host refusal carries its OWN code (source_host_unsupported, epic
// memql#4885) rather than source_unreadable, because the repair is different:
// an unreadable source is fixed at the source, an unsupported host is fixed by
// choosing the other source form. The probe answers the same code as a typed
// reason, and the Source stop renders one repair for the condition however it
// was reached -- which is why the sentence is the one normalizeCredentialHost
// already uses.
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
	if host != credentialHostGitHub && host != "www."+credentialHostGitHub {
		return "", "", refuse(CodeSourceHostUnsupported,
			"%q is on %q, which is not a host this cluster fetches sources from -- only github.com today, or upload a zip of the tree instead; the two source forms are interchangeable.",
			raw, u.Hostname())
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", refuse(CodeSourceUnreadable,
			"%q does not name an owner and a repository (expected https://github.com/<owner>/<repo>)", raw)
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

func (f *githubFetcher) FetchRepo(ctx context.Context, src RepoSource, limits Limits) (*SourceSnapshot, error) {
	target, err := tarballURL(src.RepoUrl, src.Ref)
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

	// D10: resolved HERE, at the moment of the fetch, under the package
	// owner's actor, into a local that dies with this function. Nothing above
	// this line holds a token and nothing below it stores one -- and nothing
	// leaves the cluster before the credential has resolved, so a package
	// naming a credential its owner cannot read is refused with no request
	// made.
	grant := ResolvedCredential{}
	if id := strings.TrimSpace(src.CredentialId); id != "" {
		if f.credentials == nil {
			return nil, refuse(CodeSourceUnreadable,
				"this package fetches under credential %q, and this node cannot resolve credentials", id)
		}
		resolved, cerr := f.credentials(ctx, id, src.OwnerUserId)
		if cerr != nil {
			// A typed refusal (credential_not_found / credential_revoked /
			// reconnect_required) passes through untouched: the code is what
			// the Source stop renders its repair from.
			return nil, cerr
		}
		grant = resolved
		// UNDER A GRANT THE BEARER IS AN INSTALLATION TOKEN, not the person's
		// user token (C6). A deploy that ran under somebody's user token would
		// stop working the first time they went a day without signing in, and
		// an auto-deploy at three in the morning would be the first to notice.
		owner, repo, perr := parseGitHubRepo(src.RepoUrl)
		if perr != nil {
			return nil, perr
		}
		bearer, berr := installationBearer(ctx, f.github, resolved, owner, repo)
		if berr != nil {
			return nil, berr
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
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
	case resp.StatusCode == http.StatusUnauthorized && grant.IsGrant():
		// UNDER A GRANT A 401 IS A DIFFERENT FACT (epic memql#4912, D): the
		// authorization itself is refused, and the repair is one click of
		// Connect rather than choosing another credential. Reading it as
		// source_unreadable would send somebody hunting for a permission
		// problem that does not exist.
		return nil, refuse(CodeReconnectRequired, "%s", reconnectSentence(grant))
	case resp.StatusCode == http.StatusNotFound:
		// 404 is what GitHub answers for a private repository the token
		// cannot see, so the message names BOTH possibilities rather than
		// asserting the one it cannot distinguish. Under a grant this arm is
		// nearly unreachable -- installationBearer already refused a
		// repository the app is not installed on, by name -- and what is left
		// is a ref that does not exist, which the sentence covers.
		return nil, refuse(CodeSourceUnreadable,
			"GitHub answered 404 for this repository at ref %q. Either it does not exist, or it is private and this package's credential does not reach it.",
			refOrDefault(src.Ref))
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, refuse(CodeSourceUnreadable,
			"GitHub refused this cluster's access to the repository (HTTP %d). Check the credential this package fetches under.", resp.StatusCode)
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
