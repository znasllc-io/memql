package githubapp

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// LandChange fast-forwards an explicitly selected commit with exactly the
// reviewed base as its sole parent. It neither merges nor rebases. An uncertain
// update is recovered by proving that exact commit is already on the branch.
func (c *Client) LandChange(ctx context.Context, bearer, owner, repo, branch, expectedBase, proposed string, authorize func(context.Context) (string, error)) error {
	if !branchChangeRef(branch) || !branchChangeSHA.MatchString(expectedBase) || !branchChangeSHA.MatchString(proposed) || proposed == expectedBase || authorize == nil {
		return errors.New("landing requires an exact reviewed branch, base and commit")
	}
	var commit struct{ Parents []struct{ SHA string } }
	if _, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/git/commits/"+proposed, bearer, &commit); err != nil {
		return err
	}
	if len(commit.Parents) != 1 || commit.Parents[0].SHA != expectedBase {
		return errors.New("proposed commit does not have the reviewed base as its sole parent")
	}
	observe := func(cctx context.Context) (bool, string, error) {
		head, _, err := c.BranchHead(cctx, bearer, owner, repo, branch)
		if err != nil {
			return false, "", err
		}
		if head == proposed {
			return true, head, nil
		}
		if head == expectedBase {
			return false, head, nil
		}
		ancestry, err := c.CompareAncestry(cctx, bearer, owner, repo, proposed, head)
		return err == nil && ancestry.Ancestor, head, err
	}
	landed, head, err := observe(ctx)
	if err != nil {
		return err
	}
	if landed {
		return nil
	}
	if head != expectedBase {
		return errors.New("default branch moved since review; prepare a new proposal")
	}
	bearer, err = authorize(ctx)
	if err != nil {
		return err
	}
	if bearer == "" {
		return errors.New("owner landing has no current grant")
	}
	// force:false plus the checked single parent prevents overwriting a racing
	// owner push. The provider, not a client-side permission flag, enforces rules.
	var response any
	_, writeErr := c.callJSON(ctx, http.MethodPatch, repoEndpoint(owner, repo)+"/git/refs/heads/"+escapePath(branch), bearer, map[string]any{"sha": proposed, "force": false}, &response)
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	landed, _, err = observe(readCtx)
	if err == nil && landed {
		return nil
	}
	if writeErr != nil {
		return errors.New("owner landing was not confirmed; retry the same reviewed commit to reconcile")
	}
	return errors.New("owner landing read-back could not confirm the reviewed commit")
}
