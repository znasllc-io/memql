package packages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	// testing/fstest in production code, deliberately: MapFS is the smallest
	// fs.FS over bytes already in memory, and the package imports neither
	// `testing` nor `flag`, so nothing about a test binary comes with it. The
	// alternative -- a second one-file fs.FS written here -- would be a second
	// implementation for no gain.
	"testing/fstest"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/packages/githubapp"
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

	// The two reasons a GRANT can produce and a pasted token cannot (epic
	// memql#4912, D). Under a token, 401 and 404 are both
	// credential_cannot_see_it because the cluster cannot tell them apart;
	// under a grant it can, and each names a different repair.
	ProbeReasonReconnectRequired      = CodeReconnectRequired
	ProbeReasonRepositoryNotInstalled = CodeRepositoryNotInstalled
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

	// Branches is every branch the repository has, DEFAULT BRANCH FIRST, and
	// it is answered only under a grant (epic memql#4912). It fills the ref
	// picker on the Source stop, so a person chooses a branch that exists
	// instead of typing one that does not.
	Branches []string `json:"branches"`
	// Manifest is what memql-package.yaml says about itself, read through the
	// contents API before anything is fetched. It is a PREVIEW: the What-it-is
	// stop shows it so a person recognises the package they picked, and
	// Analyze over the real snapshot remains the authority on every question
	// it answers.
	Manifest ManifestSummary `json:"manifest"`
}

// ManifestSummary is the probe's preview of a package's own manifest.
//
// EMPTY IS A VALID ANSWER and never a refusal. A repository with no manifest,
// a manifest that does not parse, a manifest this cluster's format version does
// not read -- all of them answer an empty summary and let Analyze report the
// real problem against the real snapshot. Refusing here would turn a courtesy
// into a gate, and it would report a manifest problem twice with two different
// sentences.
type ManifestSummary struct {
	Name        string                      `json:"name"`
	Deployables []ManifestSummaryDeployable `json:"deployables"`
	// DslDomains is the directory names directly under dsl/, which IS the
	// declaration (analyze.go's DslRoot -- domains are discovered, never
	// declared). Empty when the repository has no dsl/ at all, which is the
	// ordinary SPAs-only package.
	DslDomains []string `json:"dslDomains"`
}

// ManifestSummaryDeployable is one declared deployable, in the three facts the
// preview shows. Deliberately not the build plan or the binding: those are
// Analyze's to report against a tree it has actually read.
type ManifestSummaryDeployable struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// emptyManifestSummary is what a probe answers when there is nothing to
// preview. Non-nil slices, so the wire shape is `[]` rather than `null` and a
// client can iterate without a guard.
func emptyManifestSummary() ManifestSummary {
	return ManifestSummary{Deployables: []ManifestSummaryDeployable{}, DslDomains: []string{}}
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
	res := SourceProbeResult{Host: credentialHostGitHub, Branches: []string{}, Manifest: emptyManifestSummary()}

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
	credential, cerr := resolveProbeCredential(ctx, d, credentialId)
	if cerr != nil {
		switch RefusalCode(cerr) {
		case CodeCredentialNotFound:
			res.Reason = ProbeReasonCredentialNotFound
			return res, nil
		case CodeCredentialRevoked:
			res.Reason = ProbeReasonCredentialRevoked
			return res, nil
		case CodeReconnectRequired:
			// The refresh already failed, so the authorization is over and
			// the repair is one click. A probe that reported this as "the
			// credential cannot see it" would send somebody looking for a
			// permission problem that does not exist.
			res.Reason = ProbeReasonReconnectRequired
			return res, nil
		}
		return SourceProbeResult{}, cerr
	}
	if credential.Bearer != "" {
		// UNDER A GRANT THE PROBE PRESENTS THE USER TOKEN, not an
		// installation token, and the difference is what the probe is FOR: the
		// question is whether the PERSON can see this repository, which is
		// the question the picker and the Source stop are asking. The fetcher
		// asks a different question -- whether the APP can read it in the
		// background -- and carries a different bearer for it.
		req.Header.Set("Authorization", "Bearer "+credential.Bearer)
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
		// The prefill, and only under a grant (epic memql#4912): the branch
		// list and the manifest preview. BEST EFFORT throughout -- a failure
		// here leaves the fields empty and the reason `ok`, because the
		// repository really is reachable and the preview is a courtesy on top
		// of that answer, not part of it.
		if credential.IsGrant() {
			res.Branches = probeBranches(ctx, d, credential.Bearer, owner, repo, res.DefaultBranch)
			res.Manifest = probeManifest(ctx, d, credential.Bearer, owner, repo)
		}
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

	case credentialled && credential.IsGrant() && resp.StatusCode == http.StatusUnauthorized:
		// UNDER A GRANT, 401 IS THE AUTHORIZATION being refused rather than
		// the repository being out of reach, and the two have different
		// repairs. Never collapsed into credential_cannot_see_it: that
		// sentence tells somebody to choose another credential, and there is
		// no other credential to choose.
		res.Reason = ProbeReasonReconnectRequired
		return res, nil

	case credentialled && credential.IsGrant() && resp.StatusCode == http.StatusNotFound:
		// THE ONE PLACE THE PROBE ASKS A SECOND QUESTION, because under a
		// grant it can. The person's token answering 404 leaves two readings
		// open -- they cannot see the repository, or the app is not installed
		// on it -- so the app asks about ITSELF, under its own JWT: a 404
		// there means the app is not installed, which is a link away and not a
		// credential problem at all. Anything else leaves the honest
		// collapsed answer standing.
		if notInstalled(ctx, d, owner, repo) {
			res.Reason = ProbeReasonRepositoryNotInstalled
			return res, nil
		}
		res.Reason = ProbeReasonCredentialCannotSeeIt
		return res, nil

	case credentialled && (resp.StatusCode == http.StatusNotFound ||
		resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden):
		// Under a PASTED TOKEN the three refusals collapse to one fact: this
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

// resolveProbeCredential answers what the probe presents to GitHub.
//
// TWO WAYS IN, and the second one is the whole point of Connect. A NAMED
// credential resolves exactly as it always has -- under the CALLER's actor,
// because the person composing is choosing among their own credentials. An
// EMPTY credentialId falls back to the caller's own active GRANT when they
// hold one, so a connected person's picker prefills without anybody having to
// name a credential; with no grant it answers nothing, and the probe is
// anonymous, which is what a public repository needs.
//
// The fallback is deliberately silent about failure: a grant lookup that
// errors leaves the probe anonymous rather than refusing, because the caller
// asked about a repository and named no credential, and a public repository
// must not stop being probeable because a grant read went wrong.
func resolveProbeCredential(ctx context.Context, d *Deps, credentialId string) (ResolvedCredential, error) {
	if id := strings.TrimSpace(credentialId); id != "" {
		if d.PeekCredentials == nil {
			return ResolvedCredential{}, refuse(CodeSourceUnreadable,
				"this probe names credential %q, and this node cannot resolve credentials", id)
		}
		// A credential the caller cannot read -- somebody else's, or none --
		// and a revoked one are typed reasons; any other failure (a ciphertext
		// this node cannot unseal) is an error, because the repair is an
		// operator's and the stop should say so.
		return d.PeekCredentials(ctx, id, actorFromContext(ctx).UserId)
	}
	if d.Store == nil || d.PeekCredentials == nil {
		return ResolvedCredential{}, nil
	}
	grant, err := d.Store.githubAppGrantForCaller(ctx)
	if err != nil || grant == nil {
		return ResolvedCredential{}, nil
	}
	// Through the resolver rather than unsealing the row just read, even
	// though that row already carries the sealed fields: the resolver is the
	// one place a grant is unsealed AND refreshed, and a second unseal here
	// would be a second place to keep the expiry rule in step with. One extra
	// owner-scoped read is the honest price of one code path.
	resolved, rerr := d.PeekCredentials(ctx, rowString(grant, "id"), actorFromContext(ctx).UserId)
	if rerr != nil {
		// The one exception to "silent": a grant whose authorization GitHub
		// has ended is worth saying so about even though nobody named it,
		// because the person IS connected and the repair is theirs. Every
		// other failure leaves the probe anonymous.
		if RefusalCode(rerr) == CodeReconnectRequired {
			return ResolvedCredential{}, rerr
		}
		return ResolvedCredential{}, nil
	}
	return resolved, nil
}

// notInstalled asks the APP whether it is installed on owner/repo.
//
// It answers a BOOLEAN rather than an error because it is used to choose
// between two readings of somebody else's 404, and every way of failing to
// find out -- no app configured, GitHub unreachable, a status nobody expected
// -- means the same thing here: this cluster does not know, so it keeps the
// collapsed answer it already had rather than asserting the more specific one.
func notInstalled(ctx context.Context, d *Deps, owner, repo string) bool {
	if d.GitHubApp == nil || !d.GitHubApp.Configured() {
		return false
	}
	_, err := d.GitHubApp.InstallationForRepo(ctx, owner, repo)
	return errors.Is(err, githubapp.ErrNotInstalled)
}

// probeBranches lists the repository's branches with the DEFAULT FIRST.
//
// Default first because the ref picker's first entry is what a person takes
// when they do not care, and an alphabetical list would offer them whatever
// branch happens to sort first -- which for most repositories is somebody's
// half-finished feature.
//
// Best effort: a failure answers an empty list. The repository is reachable --
// that is the answer the probe already gave -- and a branch list nobody could
// read must not turn a reachable repository into an error.
func probeBranches(ctx context.Context, d *Deps, bearer, owner, repo, defaultBranch string) []string {
	if d.GitHubApp == nil {
		return []string{}
	}
	names, err := d.GitHubApp.Branches(ctx, bearer, owner, repo)
	if err != nil || len(names) == 0 {
		return []string{}
	}
	hasDefault := false
	if defaultBranch != "" {
		for _, n := range names {
			if n == defaultBranch {
				hasDefault = true
				break
			}
		}
	}
	out := make([]string, 0, len(names))
	if hasDefault {
		out = append(out, defaultBranch)
	}
	for _, n := range names {
		if hasDefault && n == defaultBranch {
			continue
		}
		out = append(out, n)
	}
	return out
}

// probeManifest previews memql-package.yaml through the contents API.
//
// IT READS THE MANIFEST WITH ReadManifest, the same function Analyze uses,
// over an fstest.MapFS holding the one fetched file. That is the whole reason
// the preview is trustworthy: a second YAML reader here would drift from the
// analyser, and the preview's entire job is to say in advance what Analyze
// will say -- a preview that disagreed with the run it previews is worse than
// no preview.
//
// Every failure -- no manifest, unparseable YAML, a format version this
// cluster does not read, GitHub refusing the contents call -- answers the
// EMPTY summary. The analysis is the authority (design section A.4), and
// reporting a manifest problem here would report it twice, in two sentences,
// one of them before the tree was even fetched.
func probeManifest(ctx context.Context, d *Deps, bearer, owner, repo string) ManifestSummary {
	out := emptyManifestSummary()
	if d.GitHubApp == nil {
		return out
	}
	raw, err := d.GitHubApp.FileContents(ctx, bearer, owner, repo, "", ManifestName)
	if err != nil || len(raw) == 0 {
		return out
	}
	manifest, merr := ReadManifest(fstest.MapFS{ManifestName: &fstest.MapFile{Data: raw}})
	if merr != nil || manifest == nil {
		return out
	}
	out.Name = manifest.Name
	for _, dep := range manifest.Deployables {
		out.Deployables = append(out.Deployables, ManifestSummaryDeployable{
			Name: dep.Name, Kind: dep.Kind, Path: dep.Path,
		})
	}
	// The DSL domains are the DIRECTORY NAMES under dsl/, because that is
	// what a domain IS in this model -- discovered, never declared (D2). A
	// repository with no dsl/ answers a 404 here and an empty list, which is
	// the ordinary SPAs-only package rather than a problem.
	if domains, derr := d.GitHubApp.DirectoryNames(ctx, bearer, owner, repo, "", DslRoot); derr == nil {
		out.DslDomains = append(out.DslDomains, domains...)
	}
	return out
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
	// SEVEN KEYS: the five the Source stop has always read, plus the two the
	// grant makes answerable -- `branches` (the ref picker) and `manifest`
	// (the What-it-is preview). Both are always PRESENT and empty when there
	// is nothing to say, so a client iterates without a guard and an absent
	// key never has to be told apart from an empty one.
	return resultNode(map[string]any{
		"host":          res.Host,
		"reachable":     res.Reachable,
		"private":       res.Private,
		"defaultBranch": res.DefaultBranch,
		"reason":        res.Reason,
		"branches":      res.Branches,
		"manifest":      res.Manifest,
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
