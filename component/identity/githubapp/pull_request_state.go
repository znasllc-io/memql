package githubapp

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// PullRequestObservation is provider evidence used when promoting a reviewed
// repository change. A prospective GitHub merge commit is never a receipt.
type PullRequestObservation struct {
	Number                                  int
	State, HeadSHA, HeadRef, HeadRepository string
	BaseSHA, BaseRef, BaseRepository        string
	Merged                                  bool
	MergeCommitSHA, URL                     string
}

// PullRequestState reads one exact pull request. Missing or contradictory
// provider fields fail closed; a deleted head repository remains explicitly
// empty so callers cannot mistake it for the repository they control.
func (c *Client) PullRequestState(ctx context.Context, bearer, owner, repo string, number int) (PullRequestObservation, error) {
	if number <= 0 {
		return PullRequestObservation{}, fmt.Errorf("a pull request needs a positive number, not %d", number)
	}
	type ref struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	}
	var p struct {
		Number         int    `json:"number"`
		State          string `json:"state"`
		Head           ref    `json:"head"`
		Base           ref    `json:"base"`
		Merged         *bool  `json:"merged"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		URL            string `json:"html_url"`
	}
	if _, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/pulls/"+strconv.Itoa(number), bearer, &p); err != nil {
		return PullRequestObservation{}, err
	}
	validSHA := func(s string) bool {
		if len(s) != 40 {
			return false
		}
		_, err := hex.DecodeString(s)
		return err == nil
	}
	if p.Number != number || (p.State != "open" && p.State != "closed") || p.Merged == nil ||
		!validSHA(p.Head.SHA) || !validSHA(p.Base.SHA) || strings.TrimSpace(p.Head.Ref) == "" ||
		strings.TrimSpace(p.Base.Ref) == "" || p.Base.Repo == nil || !strings.EqualFold(p.Base.Repo.FullName, owner+"/"+repo) {
		return PullRequestObservation{}, errors.New("GitHub did not provide a complete, matching pull request observation")
	}
	out := PullRequestObservation{Number: p.Number, State: p.State, HeadSHA: p.Head.SHA, HeadRef: p.Head.Ref,
		BaseSHA: p.Base.SHA, BaseRef: p.Base.Ref, BaseRepository: p.Base.Repo.FullName, Merged: *p.Merged, URL: p.URL}
	if p.Head.Repo != nil {
		out.HeadRepository = p.Head.Repo.FullName
	}
	if out.Merged {
		if p.State != "closed" || !validSHA(p.MergeCommitSHA) {
			return PullRequestObservation{}, errors.New("GitHub reported a merge without a closed pull request and an exact merge commit")
		}
		out.MergeCommitSHA = p.MergeCommitSHA
	}
	return out, nil
}
