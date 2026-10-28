package githubapp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// checks.go -- the check run a pipeline reports on a commit (epic memql#5477,
// design record D4): created `queued`, moved to `in_progress`, completed with
// the stage table as its summary.
//
// Both calls run under an INSTALLATION token, the only bearer GitHub accepts
// for a check run: the API reserves them to apps. An app installed before
// pipelines does not hold `checks: write`, and every write then answers 403 --
// a StatusError with Status 403 the caller keys on to record that the check
// could not be written, and carry on with the run.

// CheckRunOutput is what a check run shows: a title, a summary in markdown,
// and optional detail below it.
type CheckRunOutput struct {
	Title, Summary, Text string
}

// CheckRun is the state a check run is created with or moved to. An empty
// string and a zero time are OMITTED rather than sent: a queued run has no
// conclusion, and GitHub refuses a conclusion of "".
type CheckRun struct {
	Name, HeadSHA, Status, Conclusion, DetailsURL, ExternalID string
	StartedAt, CompletedAt                                    time.Time // zero = omitted
	Output                                                    *CheckRunOutput
}

// checkRunWire is CheckRun as GitHub's check-runs endpoints name its fields.
type checkRunWire struct {
	Name        string          `json:"name,omitempty"`
	HeadSHA     string          `json:"head_sha,omitempty"`
	Status      string          `json:"status,omitempty"`
	Conclusion  string          `json:"conclusion,omitempty"`
	DetailsURL  string          `json:"details_url,omitempty"`
	ExternalID  string          `json:"external_id,omitempty"`
	StartedAt   string          `json:"started_at,omitempty"`
	CompletedAt string          `json:"completed_at,omitempty"`
	Output      *checkRunOutput `json:"output,omitempty"`
}

type checkRunOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Text    string `json:"text,omitempty"`
}

// wire renders run for one request. head_sha is sent on create only: a run's
// commit is fixed when it is created, and the update endpoint takes none.
func (run CheckRun) wire(create bool) checkRunWire {
	w := checkRunWire{
		Name:        run.Name,
		Status:      run.Status,
		Conclusion:  run.Conclusion,
		DetailsURL:  run.DetailsURL,
		ExternalID:  run.ExternalID,
		StartedAt:   isoTime(run.StartedAt),
		CompletedAt: isoTime(run.CompletedAt),
	}
	if create {
		w.HeadSHA = run.HeadSHA
	}
	if run.Output != nil {
		w.Output = &checkRunOutput{Title: run.Output.Title, Summary: run.Output.Summary, Text: run.Output.Text}
	}
	return w
}

// isoTime is GitHub's timestamp form, and "" for the zero time.
func isoTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// CreateCheckRun creates a check run on owner/repo and answers its id.
//
// A run with no name or no head SHA is refused before any request: GitHub
// would answer 422, and the caller's row would carry a status nobody can act
// on.
func (c *Client) CreateCheckRun(ctx context.Context, bearer, owner, repo string, run CheckRun) (int64, error) {
	if strings.TrimSpace(run.Name) == "" || strings.TrimSpace(run.HeadSHA) == "" {
		return 0, errors.New("a check run needs a name and a head SHA")
	}
	var payload struct {
		Id int64 `json:"id"`
	}
	if _, err := c.callJSON(ctx, http.MethodPost, repoEndpoint(owner, repo)+"/check-runs", bearer, run.wire(true), &payload); err != nil {
		return 0, err
	}
	if payload.Id == 0 {
		return 0, errors.New("GitHub created a check run and named no id for it")
	}
	return payload.Id, nil
}

// UpdateCheckRun moves check run id on owner/repo to run's state.
func (c *Client) UpdateCheckRun(ctx context.Context, bearer, owner, repo string, id int64, run CheckRun) error {
	if c == nil {
		return ErrNotConfigured
	}
	if id <= 0 {
		return errors.New("a check run update needs the run's id")
	}
	endpoint := repoEndpoint(owner, repo) + "/check-runs/" + strconv.FormatInt(id, 10)
	_, err := c.callJSON(ctx, http.MethodPatch, endpoint, bearer, run.wire(false), nil)
	return err
}

// CheckRunRef is one check run as a commit's listing names it.
type CheckRunRef struct {
	ID         int64
	Name       string
	ExternalID string
	Status     string
	AppID      int64
}

// checkRunListPages bounds a commit's listing: 1000 check runs, GitHub's own
// limit for a check suite listing, in pages of 100.
const checkRunListPages = 10

// CheckRunsForCommit lists every check run on sha, every attempt included
// (filter=all), with each one's external id: how a pipeline finds the step
// checks a run already has -- a driver taking the run over on another
// replica, a cancel, recovery -- instead of creating them again.
func (c *Client) CheckRunsForCommit(ctx context.Context, bearer, owner, repo, sha string) ([]CheckRunRef, error) {
	if strings.TrimSpace(sha) == "" {
		return nil, errors.New("listing a commit's check runs needs its SHA")
	}
	var out []CheckRunRef
	for page := 1; page <= checkRunListPages; page++ {
		var result struct {
			TotalCount int `json:"total_count"`
			CheckRuns  []struct {
				ID         int64  `json:"id"`
				Name       string `json:"name"`
				ExternalID string `json:"external_id"`
				Status     string `json:"status"`
				App        *struct {
					ID int64 `json:"id"`
				} `json:"app"`
			} `json:"check_runs"`
		}
		endpoint := repoEndpoint(owner, repo) + "/commits/" + url.PathEscape(sha) + "/check-runs?filter=all&per_page=100&page=" + strconv.Itoa(page)
		if _, err := c.call(ctx, http.MethodGet, endpoint, bearer, &result); err != nil {
			return nil, err
		}
		for _, r := range result.CheckRuns {
			ref := CheckRunRef{ID: r.ID, Name: r.Name, ExternalID: r.ExternalID, Status: r.Status}
			if r.App != nil {
				ref.AppID = r.App.ID
			}
			out = append(out, ref)
		}
		if len(result.CheckRuns) < 100 || len(out) >= result.TotalCount {
			break
		}
	}
	return out, nil
}
