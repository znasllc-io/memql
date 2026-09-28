package release

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// versionfile.go -- the cut refuses when VERSION does not name the release.
//
// THE RULE. The repo-root VERSION file equals the tag of the commit a cut tags
// (VERSIONING.md). It was once "deliberately stale" and nothing here read it,
// and the lag that produced is measured rather than hypothetical: VERSION read
// 0.15.0 at every tag from v0.16.1 through v0.21.25, and 0.22.9 at v0.22.10 to
// v0.22.12, so the docs bundle built at v0.21.25 went out labelled as engine
// 0.15.0. A rule nobody checks does not hold, so the cut checks it.
//
// WHY A REFUSAL AND NOT A WRITE. The obvious fix -- the cut writes VERSION in a
// release commit and tags that -- is not available: `main` refuses direct
// pushes to every identity (a repository ruleset, the same fact pinbump.go is
// arranged around), and tagging a commit that is not on main would leave
// main's VERSION behind, build images from main's head rather than the tagged
// commit, and blind already_released_at_head. So VERSION reaches main the way
// every other change does: an ordinary "prepare vX.Y.Z" pull request, merged
// before the cut. The cut's job is to refuse when that step was skipped.
//
// READ AT THE SHA BEING TAGGED, NOT AT "main". The tag is created at the sha
// MainHeadSha returned, so the file is read at exactly that sha. Reading
// `main` instead would let a merge landing between the two calls answer for a
// commit the tag does not point at.
//
// BEFORE THE DRY-RUN RETURN, on purpose. cut.go's rule is that everything that
// can refuse has refused before the first irreversible step, and a dry run is
// the plan the card shows: a plan that would be refused has to say so, or the
// operator learns it only after confirming.

// versionFilePath is the file the check reads.
const versionFilePath = "VERSION"

// ReadVersionFile reads VERSION at ref, reporting found=false when GitHub
// answers 404.
//
// A 404 HERE MEANS THE FILE IS ABSENT, not that the repository is. GetFile
// sends a 404 through classify, which reads it as release_repo_unconfigured --
// right for a first call, wrong here: by the time this runs the same token
// has listed the repository's tags and read main's head, so the repository
// demonstrably exists and is visible. Mapping this 404 to the repository code
// would tell the operator to fix a setting that is correct.
func (c *Client) ReadVersionFile(ctx context.Context, token string, repo repoRef, ref string) (string, bool, error) {
	endpoint := fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), versionFilePath, url.QueryEscape(ref))
	status, body, err := c.do(ctx, token, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", false, err
	}
	if status == http.StatusNotFound {
		return "", false, nil
	}
	if err := classify(status, body, "read "+versionFilePath); err != nil {
		return "", false, err
	}
	content, _, err := decodeContents(versionFilePath, body)
	if err != nil {
		return "", false, err
	}
	return content, true, nil
}

// checkVersionFile refuses with version_file_stale unless VERSION at sha reads
// exactly next's bare version.
//
// The FIRST LINE is compared, trimmed, which is how every other reader takes
// the file (the Makefile, the docs bundle build). A leading `v` is a mismatch:
// VERSION is the unprefixed semver the image tag uses, and the `v` lives on
// the git tag only.
func (i *Integration) checkVersionFile(ctx context.Context, cfg settings, sha string, next version) error {
	raw, found, err := i.github.ReadVersionFile(ctx, cfg.token, cfg.repo, sha)
	if err != nil {
		return err
	}
	if !found {
		return refuse(CodeVersionFileStale,
			"there is no %s file at main's head (%s), so nothing in the tree says it is %s. VERSION must equal the tag a cut creates; land a pull request adding %s with the single line %s, then cut again.",
			versionFilePath, shortSha(sha), next.tag(), versionFilePath, next.bare())
	}
	got := firstLineOf(raw)
	if got == next.bare() {
		return nil
	}
	return refuse(CodeVersionFileStale,
		"%s at main's head (%s) reads %q, and this cut would tag %s. VERSION must equal the tag a cut creates, so every reader of the file -- the docs bundle among them -- names the release it belongs to. Land a pull request setting %s to %s, then cut again; if %q is the release you meant, choose the bump that reaches it.",
		versionFilePath, shortSha(sha), clampMessage(got), next.tag(), versionFilePath, next.bare(), clampMessage(got))
}

// firstLineOf returns the first line of s with surrounding whitespace removed.
func firstLineOf(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}
