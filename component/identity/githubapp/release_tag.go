package githubapp

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

type releaseTagObject struct {
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

type releaseTagReference struct {
	Ref    string           `json:"ref"`
	Object releaseTagObject `json:"object"`
}

// ObserveReleaseTag distinguishes an absent tag from a failed commit lookup.
// GitHub's commits endpoint returns 422 for a missing tag, but that status also
// covers malformed requests. Only the exact Git-reference endpoint's 404 can
// mean absent, and the selected source must still be readable by the same grant
// before accepting it: a hidden repository or revoked contents access is not
// evidence that a release tag is available. The caller owns source/check proof.
func (c *Client) ObserveReleaseTag(ctx context.Context, bearer, owner, repo, tag, selected string) (string, bool, error) {
	if bearer == "" || owner == "" || repo == "" || strings.ContainsAny(owner+repo, "/\\\r\n\x00") || !branchChangeRef(tag) || !branchChangeSHA.MatchString(selected) {
		return "", false, errors.New("release tag observation requires its repository, tag and verified source")
	}
	ref := "refs/tags/" + tag
	endpoint := repoEndpoint(owner, repo) + "/git/ref/tags/" + escapePath(tag)
	var before releaseTagReference
	if _, err := c.call(ctx, http.MethodGet, endpoint, bearer, &before); err != nil {
		if StatusOf(err) != http.StatusNotFound {
			return "", false, err
		}
		sha, _, sourceErr := c.CommitForRef(ctx, bearer, owner, repo, selected)
		if sourceErr != nil {
			return "", false, sourceErr
		}
		if sha != selected {
			return "", false, errors.New("release source changed during absent-tag verification")
		}
		return "", false, ctx.Err()
	}
	if before.Ref != ref || !branchChangeSHA.MatchString(before.Object.SHA) || before.Object.Type != "commit" && before.Object.Type != "tag" {
		return "", false, errors.New("release tag reference is incomplete or names a different object")
	}
	// Peel the captured immutable object, never the mutable ref name. A name
	// can move away and back between reads (ABA) while resolving another commit.
	// Keep the publisher's eight-object/cycle/identity limits, then prove the
	// exact commit remains readable by this grant before the final ref read.
	sha, err := c.releaseTagCommit(ctx, bearer, owner, repo, before.Object)
	if err != nil {
		return "", false, err
	}
	var after releaseTagReference
	if _, err := c.call(ctx, http.MethodGet, endpoint, bearer, &after); err != nil {
		return "", false, err
	}
	if before != after {
		return "", false, errors.New("release tag changed during observation")
	}
	return sha, true, ctx.Err()
}

func (c *Client) releaseTagCommit(ctx context.Context, bearer, owner, repo string, object releaseTagObject) (string, error) {
	seen := map[string]bool{}
	for depth := 0; depth <= 8; depth++ {
		if !branchChangeSHA.MatchString(object.SHA) || seen[object.SHA] {
			return "", errors.New("release tag object is invalid or cyclic")
		}
		seen[object.SHA] = true
		if object.Type == "commit" {
			sha, _, err := c.CommitForRef(ctx, bearer, owner, repo, object.SHA)
			if err != nil {
				return "", err
			}
			if sha != object.SHA {
				return "", errors.New("release tag did not resolve to its observed commit")
			}
			return sha, ctx.Err()
		}
		if object.Type != "tag" || depth == 8 {
			return "", errors.New("unsupported or excessive annotated tag chain")
		}
		var tag struct {
			SHA    string           `json:"sha"`
			Object releaseTagObject `json:"object"`
		}
		if _, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/git/tags/"+object.SHA, bearer, &tag); err != nil {
			return "", err
		}
		if tag.SHA != object.SHA {
			return "", errors.New("annotated tag identity differs")
		}
		object = tag.Object
	}
	return "", errors.New("release tag did not resolve")
}
