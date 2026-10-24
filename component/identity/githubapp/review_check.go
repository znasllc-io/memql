package githubapp

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ReviewCheckEvidence binds the app-owned check to one immutable native review
// identity. It is an observation, never authorization to write a check.
type ReviewCheckEvidence struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	ExternalID string `json:"external_id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	App        struct {
		ID int64 `json:"id"`
	} `json:"app"`
}

func validReviewIdentity(head, name, external string, app int64) bool {
	_, digestErr := hex.DecodeString(strings.TrimPrefix(external, "memql-review:"))
	return digestErr == nil && branchChangeSHA.MatchString(head) && strings.TrimSpace(name) != "" && len(name) <= 100 && strings.HasPrefix(external, "memql-review:") && len(external) == len("memql-review:")+64 && app > 0
}
func (r ReviewCheckEvidence) matches(head, name, external string, app int64) bool {
	return r.ID > 0 && r.HeadSHA == head && r.Name == name && r.ExternalID == external && r.App.ID == app && r.Status == "completed" && r.Conclusion == "success"
}

// VerifyReviewCheck uses the provider's exact check ID, not a caller's success
// boolean or a matching title from another app. It never modifies the check.
func (c *Client) VerifyReviewCheck(ctx context.Context, bearer, owner, repo string, id int64, head, name, external string, app int64) error {
	if id <= 0 || !validReviewIdentity(head, name, external, app) {
		return errors.New("review check needs an exact native identity")
	}
	var out ReviewCheckEvidence
	_, err := c.callJSON(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/check-runs/"+strconv.FormatInt(id, 10), bearer, nil, &out)
	if err != nil {
		return err
	}
	if out.ID != id || !out.matches(head, name, external, app) {
		return errors.New("GitHub review check no longer proves the exact accepted review")
	}
	return nil
}

// findReviewCheck searches every bounded page, including previous runs. Native
// external identity recovers a successful create whose response was lost.
func (c *Client) findReviewCheck(ctx context.Context, bearer, owner, repo, head, name, external string, app int64) (int64, error) {
	found := int64(0)
	seen := map[int64]bool{}
	for page := 1; page <= 10; page++ {
		var out struct {
			Total *int                  `json:"total_count"`
			Runs  []ReviewCheckEvidence `json:"check_runs"`
		}
		endpoint := repoEndpoint(owner, repo) + "/commits/" + url.PathEscape(head) + "/check-runs?filter=all&per_page=100&page=" + strconv.Itoa(page) + "&check_name=" + url.QueryEscape(name)
		if _, err := c.callJSON(ctx, http.MethodGet, endpoint, bearer, nil, &out); err != nil {
			return 0, err
		}
		if out.Total == nil || *out.Total < 0 || *out.Total > 1000 || out.Runs == nil {
			return 0, errors.New("GitHub returned incomplete review check history")
		}
		for _, r := range out.Runs {
			if r.ID <= 0 || seen[r.ID] {
				return 0, errors.New("GitHub returned ambiguous review check history")
			}
			seen[r.ID] = true
			if r.ExternalID != external || r.App.ID != app {
				continue
			}
			if !r.matches(head, name, external, app) {
				return 0, errors.New("existing review check conflicts with exact accepted evidence")
			}
			if found != 0 {
				return 0, errors.New("multiple app-owned review checks claim one native identity")
			}
			found = r.ID
		}
		if len(seen) == *out.Total {
			return found, nil
		}
		if len(out.Runs) < 100 || len(seen) > *out.Total {
			return 0, errors.New("GitHub review check history changed while reading")
		}
	}
	return 0, errors.New("GitHub review check history exceeds its bounded page count")
}

// ReportReviewCheck is called only after native independent-review admission.
// It does not review code or accept a caller's verdict. Its fresh authorization
// callback returns only a repository-scoped installation checks writer.
func (c *Client) ReportReviewCheck(ctx context.Context, reader, owner, repo string, number int, head, base, name, external string, app int64, authorize func(context.Context) (string, error)) (int64, error) {
	if reader == "" || number <= 0 || base == "" || !validReviewIdentity(head, name, external, app) || authorize == nil {
		return 0, errors.New("review reporting needs an exact reviewed proposal and fresh authorization")
	}
	snapshot := func() error {
		p, answer, err := c.queueSnapshot(ctx, reader, owner, repo, number, head, base)
		if err != nil {
			return err
		}
		if answer.State == "attention" || answer.State == "merged" || p.State != "OPEN" {
			return errors.New("review reporting refused: pull request no longer matches its open reviewed proposal")
		}
		return nil
	}
	if err := snapshot(); err != nil {
		return 0, err
	}
	found, err := c.findReviewCheck(ctx, reader, owner, repo, head, name, external, app)
	if err != nil {
		return 0, err
	}
	if found > 0 {
		if err := c.VerifyReviewCheck(ctx, reader, owner, repo, found, head, name, external, app); err != nil {
			return 0, err
		}
		return found, nil
	}
	// Re-observe head after the initial grant refresh, then recheck native
	// authority immediately before the only mutable request.
	if _, err := authorize(ctx); err != nil {
		return 0, err
	}
	if err := snapshot(); err != nil {
		return 0, err
	}
	writer, err := authorize(ctx)
	if err != nil {
		return 0, err
	}
	if writer == "" {
		return 0, errors.New("review check writer is unavailable")
	}
	id, writeErr := c.CreateCheckRun(ctx, writer, owner, repo, CheckRun{Name: name, HeadSHA: head, Status: "completed", Conclusion: "success", ExternalID: external, Output: &CheckRunOutput{Title: "Independent review accepted", Summary: "An independent review accepted the exact proposed changes under the recorded repository authority. Required tests and merge-queue verification remain separate checks."}})
	if writeErr != nil {
		recovered, readErr := c.findReviewCheck(ctx, reader, owner, repo, head, name, external, app)
		if readErr != nil {
			return 0, errors.Join(writeErr, readErr)
		}
		if recovered == 0 {
			return 0, writeErr
		}
		id = recovered
	}
	if err := c.VerifyReviewCheck(ctx, reader, owner, repo, id, head, name, external, app); err != nil {
		return 0, fmt.Errorf("review check readback: %w", err)
	}
	if err := snapshot(); err != nil {
		return 0, err
	}
	return id, nil
}
