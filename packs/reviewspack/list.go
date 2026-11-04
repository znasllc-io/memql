package reviewspack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// list.go -- THE MODERATION LIST (reviewsList).
//
// One store's reviews, each with the state its decision log gives it
// (state.go) and the last decision recorded on it. It is a builtin rather than
// a query for the reason the public read is: a review's state is a fact about
// a different concept's rows, and a filter cannot make that read.
//
// The queue -- state waiting, the default -- is OLDEST FIRST, because how long
// a review has waited is what the store's own timeline is measured against.
// Every other view is newest first. A review's time is its row's createdAt,
// the version clock, so an edit through the writer's link re-enters the queue
// when it arrives.
//
// The list is read under the caller's own actor: a caller outside the store
// reads nothing. Unlike the public read it projects the writer's email,
// because a moderator may need to reply.

const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// ListedDecision is the last decision on a listed review.
type ListedDecision struct {
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
	Note      string `json:"note,omitempty"`
	DecidedBy string `json:"decidedBy"`
	At        string `json:"at"`
}

// list answers reviewsList.
func (p *Provider) list(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := strings.TrimSpace(asString(args["storeId"]))
	if storeID == "" {
		return nil, fmt.Errorf("reviews: storeId is required")
	}
	want := strings.TrimSpace(asString(args["state"]))
	if want == "" {
		want = string(StateWaiting)
	}
	if want != "all" && !knownState(want) {
		return nil, fmt.Errorf("reviews: %q is not a review state", want)
	}
	limit := asInt(args["limit"])
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	offset := 0
	if after := strings.TrimSpace(asString(args["after"])); after != "" {
		n, err := strconv.Atoi(after)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("reviews: %q is not a cursor this list issued", after)
		}
		offset = n
	}

	rows, err := p.rowsFor(ctx, "reviewsInStore", map[string]string{"storeId": storeID})
	if err != nil {
		return nil, err
	}
	reviewIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		if id := strings.TrimSpace(asString(row["id"])); id != "" {
			reviewIDs = append(reviewIDs, id)
		}
	}
	decisions, err := p.decisionsByReview(ctx, storeID, reviewIDs)
	if err != nil {
		return nil, err
	}

	type entry struct {
		row map[string]any
		at  time.Time
	}
	matched := make([]entry, 0, len(rows))
	for _, row := range rows {
		id := strings.TrimSpace(asString(row["id"]))
		if id == "" {
			continue
		}
		state := StateOf(asInt(row["revision"]), decisions[id])
		if want != "all" && string(state) != want {
			continue
		}
		out := map[string]any{}
		for k, v := range row {
			out[k] = v
		}
		out["state"] = string(state)
		if last := lastDecision(decisions[id]); last != nil {
			out["lastDecision"] = last
		}
		out["decisionCount"] = len(decisions[id])
		at, _ := time.Parse(time.RFC3339Nano, asString(row["createdAt"]))
		matched = append(matched, entry{row: out, at: at})
	}
	oldestFirst := want == string(StateWaiting)
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].at.Equal(matched[j].at) {
			return asString(matched[i].row["id"]) < asString(matched[j].row["id"])
		}
		if oldestFirst {
			return matched[i].at.Before(matched[j].at)
		}
		return matched[i].at.After(matched[j].at)
	})

	total := len(matched)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	page := make([]map[string]any, 0, end-offset)
	for _, e := range matched[offset:end] {
		page = append(page, e.row)
	}
	next := ""
	if end < total {
		next = strconv.Itoa(end)
	}
	payload, err := json.Marshal(map[string]any{
		"storeId": storeID,
		"state":   want,
		"reviews": page,
		"count":   len(page),
		"total":   total,
		"next":    next,
	})
	if err != nil {
		return nil, fmt.Errorf("reviews: marshal list: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("reviews:list:%s:%s:%d", storeID, want, offset),
		Concept:   "v1:reviews:list",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

func knownState(s string) bool {
	for _, st := range States {
		if string(st) == s {
			return true
		}
	}
	return false
}

// lastDecision is the most recent decision of any kind, or nil.
func lastDecision(ds []Decision) *ListedDecision {
	if len(ds) == 0 {
		return nil
	}
	latest := ds[0]
	for _, d := range ds[1:] {
		if d.At.After(latest.At) || (d.At.Equal(latest.At) && d.ID > latest.ID) {
			latest = d
		}
	}
	return &ListedDecision{
		Action: latest.Action, Reason: latest.Reason, Note: latest.Note,
		DecidedBy: latest.DecidedBy, At: latest.At.UTC().Format(time.RFC3339Nano),
	}
}
