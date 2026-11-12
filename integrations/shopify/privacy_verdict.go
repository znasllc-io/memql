package shopify

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// privacy_verdict.go -- a privacy request that keeps failing is seen.
//
// A failed privacy job is run again at every pass of the runner
// (RunDueComplianceJobs). One that is still failing a day after it was due is
// not going to mend by itself, and its own row was the only place that said
// so. This is the read the store's verdict makes of those rows, and the
// reason it gives.

// ReasonPrivacyRequestFailing is the reason a store with a privacy request
// that could not be completed carries. It has no act: the job is tried again
// by itself, and nothing a person presses on the Store page mends it.
const ReasonPrivacyRequestFailing = "privacy_request_failing"

// privacyFailingAfter is how long a privacy job may have been due, and
// failing, before the store's verdict says so. A job that failed an hour ago
// has a pass still to come; the two erasure topics leave days to spare (thirty
// for a customer's, and the purge has already waited its hold).
const privacyFailingAfter = 24 * time.Hour

// privacyFailing is what a store's verdict knows of its privacy requests:
// how many have failed and were due more than privacyFailingAfter ago, and
// since when the oldest of them has been due.
type privacyFailing struct {
	Count int
	// AtLeast says the page that was read was full, so Count is a floor.
	AtLeast bool
	Since   time.Time
}

// privacyRequestsFailing reads a store's privacy jobs that are failed and were
// due more than a day ago: the first page of them, oldest due first, which is
// enough to say "at least this many, since then". It is asked on the server,
// by store, under the operator's identity, and answers a count and a time:
// nothing of a job's row -- which names the customer it is about -- leaves it.
func (c *Connector) privacyRequestsFailing(ctx context.Context, storeID string) (privacyFailing, error) {
	res, err := c.engine.Execute(memql.ContextWithFreshRead(operatorContext(ctx)), renderCall("complianceJobsFailingForStore", map[string]any{
		"storeId":   storeID,
		"dueBefore": c.now().UTC().Add(-privacyFailingAfter).Format(time.RFC3339),
	}))
	if err != nil {
		return privacyFailing{}, fmt.Errorf("shopify: read the store's failing privacy requests: %w", err)
	}
	rows := memql.MaterializeRows(res)
	out := privacyFailing{Count: len(rows)}
	for _, row := range rows {
		if due := rowTime(row, "dueAt"); !due.IsZero() && (out.Since.IsZero() || due.Before(out.Since)) {
			out.Since = due
		}
	}
	if meta := res.GetMeta(); meta != nil && meta.HasMore {
		out.AtLeast = true
	}
	return out, nil
}

// privacyReason is the verdict's reason for what the read answered, and
// whether there is one. It says how many requests and since when, and never
// whom they are about.
func privacyReason(p privacyFailing) (VerdictReason, bool) {
	if p.Count <= 0 {
		return VerdictReason{}, false
	}
	detail := "1 request"
	if p.Count > 1 {
		detail = fmt.Sprintf("%d requests", p.Count)
	}
	if p.AtLeast {
		detail = "At least " + detail
	}
	if !p.Since.IsZero() {
		if p.Count > 1 {
			detail += ", the oldest"
		}
		detail += " due since " + p.Since.UTC().Format(time.RFC3339)
	}
	return VerdictReason{
		Code:     ReasonPrivacyRequestFailing,
		Sentence: "A privacy request for this store could not be completed.",
		Detail:   detail + ". It is tried again at every run of the privacy jobs.",
		Count:    p.Count,
		AtLeast:  p.AtLeast,
	}, true
}
