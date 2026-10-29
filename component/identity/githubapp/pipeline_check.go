package githubapp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// FindPipelineCheck observes a check-create receipt by its complete native
// identity. It grants no permission and does not infer a passing result: queued,
// running and completed checks all recover the same known ID. A zero answer is
// only a complete bounded observation of absence, never permission to repeat an
// uncertain create; that decision belongs to the caller's durable effect journal.
func (c *Client) FindPipelineCheck(ctx context.Context, bearer, owner, repo, head, name, external string, appID int64) (int64, error) {
	if !branchChangeSHA.MatchString(head) || strings.TrimSpace(name) == "" ||
		strings.TrimSpace(external) == "" || len(external) > 256 || appID <= 0 {
		return 0, errors.New("pipeline check observation requires an exact head, name, external identity and App")
	}
	found, total := int64(0), -1
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
		if out.Total == nil || *out.Total < 0 || *out.Total > 1000 || out.Runs == nil || len(out.Runs) > 100 || total >= 0 && total != *out.Total {
			return 0, errors.New("GitHub returned incomplete pipeline check history")
		}
		total = *out.Total
		for _, run := range out.Runs {
			if run.ID <= 0 || seen[run.ID] || run.HeadSHA != head || run.Name != name || run.App.ID <= 0 {
				return 0, errors.New("GitHub returned unbound or repeated pipeline check history")
			}
			seen[run.ID] = true
			if run.ExternalID != external || run.App.ID != appID {
				continue
			}
			if found != 0 {
				return 0, errors.New("multiple App-owned pipeline checks claim one native identity")
			}
			switch run.Status {
			case "queued", "in_progress", "completed":
				found = run.ID
			default:
				return 0, errors.New("existing pipeline check has an unrecognized state")
			}
		}
		if len(seen) == total {
			return found, nil
		}
		if len(out.Runs) < 100 || len(seen) > total {
			return 0, errors.New("GitHub pipeline check history changed while reading")
		}
	}
	return 0, errors.New("GitHub pipeline check history exceeds its bounded page count")
}
