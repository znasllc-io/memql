package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// probe.go -- the two compose probes (epic memql#4885, D11).
//
// A probe is a QUESTION the Source stop asks before a person commits to a
// source: can this cluster read that repository, and is that zip a package
// tree or a built site. Two things make it a probe rather than a fetch, and
// both have a test:
//
//   - IT WRITES NOTHING AND STAMPS NOTHING. No package row, no deployment row,
//     and -- the one that is easy to get wrong -- no lastUsedAt heartbeat on
//     the credential it resolves. The fetcher's resolver stamps that on every
//     unseal, because a fetch is what the heartbeat records; the probe
//     resolves through peekCredential, the same read under the same rule with
//     the stamp left out.
//   - IT ANSWERS A TYPED REASON, NEVER THE API'S OWN BODY. GitHub's 404 for a
//     private repository and for one that does not exist are one status, and
//     the reason says exactly that (not_found_or_private) rather than
//     forwarding a message that claims to know. The closed reason set is the
//     Source stop's copy table; anything outside it is an ERROR, which the
//     stop renders with the server's sentence and stays editable behind.
//
// The probe carries no more authority than a fetch (design section G): the
// credential resolves under the CALLER's actor -- the person composing is
// choosing among their own credentials -- and a credential they cannot read
// is answered as a reason before any request leaves the cluster.

// The probe's reasons (design D11). Three of them ARE refusal codes, spelled
// as the same string on purpose: the Source stop keys one sentence per
// condition, and a credential that does not resolve is the same condition
// whether the probe or the fetch found it.
const (
	ProbeReasonOK                    = "ok"
	ProbeReasonNotFoundOrPrivate     = "not_found_or_private"
	ProbeReasonCredentialCannotSeeIt = "credential_cannot_see_it"
	ProbeReasonCredentialNotFound    = CodeCredentialNotFound
	ProbeReasonCredentialRevoked     = CodeCredentialRevoked
	ProbeReasonSourceHostUnsupported = CodeSourceHostUnsupported
	ProbeReasonRateLimited           = "rate_limited"
)

// SourceProbeResult is what sourceProbe answers. Every field is JSON-tagged
// because the struct IS the wire shape the Source stop reads.
type SourceProbeResult struct {
	// Host is the repository's host as parsed -- github.com for either
	// spelling GitHub answers on, and the unsupported host verbatim when the
	// reason is source_host_unsupported.
	Host string `json:"host"`
	// Reachable is true only when GitHub answered the repository: the one
	// case the fetch would succeed in.
	Reachable bool `json:"reachable"`
	// Private and DefaultBranch come from GitHub's answer and are meaningful
	// only when Reachable is true; a repository the probe could not read has
	// no known visibility, and false is "not known", never "public".
	Private       bool   `json:"private"`
	DefaultBranch string `json:"defaultBranch"`
	// Reason is exactly one of the ProbeReason* values.
	Reason string `json:"reason"`
}

// ProbeSource asks GitHub whether repoUrl can be read, under the caller's
// credential when one is named.
func ProbeSource(ctx context.Context, d *Deps, repoUrl, credentialId string) (SourceProbeResult, error) {
	owner, repo, err := parseGitHubRepo(repoUrl)
	if err != nil {
		// A non-GitHub host is a REASON with the host it named: the stop
		// renders "only github.com today, or upload a zip" and the field
		// stays editable. An unparseable URL, or one naming no repository,
		// has no host to be unsupported and stays the error it is.
		if RefusalCode(err) == CodeSourceHostUnsupported {
			return SourceProbeResult{Host: hostOf(repoUrl), Reason: ProbeReasonSourceHostUnsupported}, nil
		}
		return SourceProbeResult{}, err
	}
	res := SourceProbeResult{Host: credentialHostGitHub}

	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo), nil)
	if rerr != nil {
		return SourceProbeResult{}, rerr
	}
	// The same headers the fetcher sends, so GitHub answers the probe and
	// the fetch the same way -- a probe that GitHub treated differently would
	// be a courtesy that lies.
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "memql-packages")

	credentialled := false
	if id := strings.TrimSpace(credentialId); id != "" {
		if d.PeekCredentials == nil {
			return SourceProbeResult{}, refuse(CodeSourceUnreadable,
				"this probe names credential %q, and this node cannot resolve credentials", id)
		}
		// Under the CALLER's actor: the person composing is choosing their
		// own credential, so the caller is the owner the sealed read runs
		// as. A credential they cannot read -- somebody else's, or none --
		// and a revoked one are the two typed reasons; any other failure
		// (a ciphertext this node cannot unseal) is an error, because the
		// repair is an operator's and the stop should say so.
		token, cerr := d.PeekCredentials(ctx, id, actorFromContext(ctx).UserId)
		if cerr != nil {
			switch RefusalCode(cerr) {
			case CodeCredentialNotFound:
				res.Reason = ProbeReasonCredentialNotFound
				return res, nil
			case CodeCredentialRevoked:
				res.Reason = ProbeReasonCredentialRevoked
				return res, nil
			}
			return SourceProbeResult{}, cerr
		}
		req.Header.Set("Authorization", "Bearer "+token)
		credentialled = true
	}

	resp, derr := d.httpClient().Do(req)
	if derr != nil {
		// A GitHub this cluster cannot reach IS an error, deliberately: the
		// stop says so and stays editable, and it never blocks Analyze on a
		// public repository, because the fetch is the authority and the
		// probe is a courtesy (design section H).
		return SourceProbeResult{}, refuse(CodeSourceUnreadable, "this cluster could not reach GitHub: %v", derr)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		var payload struct {
			Private       bool   `json:"private"`
			DefaultBranch string `json:"default_branch"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
			return SourceProbeResult{}, refuse(CodeSourceUnreadable,
				"GitHub answered the repository with a body this cluster could not read: %v", err)
		}
		res.Reachable = true
		res.Private = payload.Private
		res.DefaultBranch = strings.TrimSpace(payload.DefaultBranch)
		res.Reason = ProbeReasonOK
		return res, nil

	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusForbidden && strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")) == "0":
		// GitHub spells its primary limit as 403 with the remaining count at
		// zero and its secondary limits as 429; both are "ask again later",
		// and neither says anything about the repository.
		res.Reason = ProbeReasonRateLimited
		return res, nil

	case resp.StatusCode == http.StatusNotFound && !credentialled:
		// 404 is what GitHub answers for a private repository and for one
		// that does not exist alike, and the reason must not claim to know
		// which -- the stop offers a credential and asks again.
		res.Reason = ProbeReasonNotFoundOrPrivate
		return res, nil

	case credentialled && (resp.StatusCode == http.StatusNotFound ||
		resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden):
		// Under a credential the three refusals collapse to one fact: this
		// token does not reach this repository -- it cannot see it (404),
		// it is not a valid token (401), or it is forbidden from it (403).
		// The repair is the same for all three, which is why they are one
		// reason: choose another credential, or fix this one's grant.
		res.Reason = ProbeReasonCredentialCannotSeeIt
		return res, nil
	}

	// Everything else is a GitHub answer this cluster cannot type -- a 5xx,
	// an anonymous 403 that is not rate limiting -- and inventing a reason
	// for it would file a real fault as one of the person's mistakes.
	return SourceProbeResult{}, refuse(CodeSourceUnreadable,
		"GitHub answered HTTP %d for this repository", resp.StatusCode)
}

// hostOf reads the hostname out of a URL for the unsupported-host reason,
// verbatim, so the stop can name what was typed.
func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// ArtifactProbeResult is what artifactProbe answers, and the wire shape the
// Source stop reads to choose the package path or the hand-made one.
type ArtifactProbeResult struct {
	// IsPackage: memql-package.yaml sits at the root of the zip. The package
	// path takes it from here, and the analysis decides the rest.
	IsPackage bool `json:"isPackage"`
	// IsBuiltSite: index.html sits at the root and there is NO manifest. The
	// hand-made path takes it and asks for the kind. A manifest beside an
	// index.html is a package -- the manifest is the stronger claim.
	IsBuiltSite bool `json:"isBuiltSite"`
	// FileCount and TotalBytes are the What-it-is stop's two facts for a
	// built site, counted by walking the opened tree.
	FileCount  int   `json:"fileCount"`
	TotalBytes int64 `json:"totalBytes"`
}

// ProbeArtifact opens the caller's zip and reports what kind of tree it is.
//
// Through the same fetch the deploy uses -- the artifact's bytes under the
// CALLER's actor, expanded by OpenZip under the packages limits -- so a zip the
// deploy would refuse (too large, an entry escaping the root, not a zip) is
// refused here, by the same code, before a person commits to it. Nothing is
// written: the artifact stays exactly the Library row it was.
func ProbeArtifact(ctx context.Context, d *Deps, artifactId string) (ArtifactProbeResult, error) {
	if d.Fetcher == nil {
		return ArtifactProbeResult{}, refuse(CodeSourceUnreadable, "this node cannot read Library artifacts")
	}
	snapshot, err := d.Fetcher.FetchArtifact(ctx, artifactId, d.Limits)
	if err != nil {
		return ArtifactProbeResult{}, err
	}
	defer snapshot.Close()

	res := ArtifactProbeResult{
		IsPackage: hasFileIn(snapshot.Tree, ManifestName),
	}
	res.IsBuiltSite = !res.IsPackage && hasFileIn(snapshot.Tree, "index.html")

	werr := fs.WalkDir(snapshot.Tree, ".", func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, ierr := entry.Info()
		if ierr != nil {
			return ierr
		}
		res.FileCount++
		res.TotalBytes += info.Size()
		return nil
	})
	if werr != nil {
		return ArtifactProbeResult{}, refuse(CodeSourceUnreadable, "this zip could not be walked: %v", werr)
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// The two capabilities
// ---------------------------------------------------------------------------

func (i *Integration) handleSourceProbe(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	repoUrl := strings.TrimSpace(stringArg(args, "repoUrl"))
	if repoUrl == "" {
		return nil, refuse(CodeSourceUnreadable, "repoUrl is required")
	}
	res, perr := ProbeSource(ctx, deps, repoUrl, stringArg(args, "credentialId"))
	if perr != nil {
		return nil, perr
	}
	return resultNode(map[string]any{
		"host":          res.Host,
		"reachable":     res.Reachable,
		"private":       res.Private,
		"defaultBranch": res.DefaultBranch,
		"reason":        res.Reason,
	}), nil
}

func (i *Integration) handleArtifactProbe(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	artifactId := strings.TrimSpace(stringArg(args, "artifactId"))
	if artifactId == "" {
		return nil, refuse(CodeSourceUnreadable, "artifactId is required")
	}
	res, perr := ProbeArtifact(ctx, deps, artifactId)
	if perr != nil {
		return nil, perr
	}
	return resultNode(map[string]any{
		"isPackage":   res.IsPackage,
		"isBuiltSite": res.IsBuiltSite,
		"fileCount":   res.FileCount,
		"totalBytes":  res.TotalBytes,
	}), nil
}
