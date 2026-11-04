package reviewspack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// reads.go -- WHAT THE ADMIN PANEL READS: ONE REVIEW (reviewsGet), THE
// STORE'S DECISION LOG (reviewsDecisionLog), THE SETTINGS (reviewsSettings).
//
// The moderation list answers the queue. A moderator who opens one review
// needs its whole decision log and the acts they may take on it now; the
// store's log answers who decided what across every review, newest first;
// and the settings answer what is in force with the defaults applied.
//
// EACH READ SAYS WHAT THE VIEWER MAY DO, worked out here with the rules the
// writes enforce (ValidateDecision, moderatorOf, writeSettings), so the panel
// offers no act the engine would then refuse. Every read runs under the
// caller's own actor: a caller outside the store reads nothing.

// LoggedDecision is one entry of a decision log.
type LoggedDecision struct {
	ID        string `json:"id"`
	ReviewID  string `json:"reviewId"`
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
	Note      string `json:"note,omitempty"`
	DecidedBy string `json:"decidedBy"`
	At        string `json:"at"`
	Revision  int    `json:"revision"`
}

func loggedFrom(d Decision) LoggedDecision {
	return LoggedDecision{
		ID: d.ID, ReviewID: d.ReviewID, Action: d.Action, Reason: d.Reason, Note: d.Note,
		DecidedBy: d.DecidedBy, At: d.At.UTC().Format(time.RFC3339Nano), Revision: max(d.Revision, 1),
	}
}

// newestFirst orders decisions by time, newest first; a tie by id, so a page
// boundary never moves between two reads.
func newestFirst(ds []Decision) []Decision {
	out := append([]Decision(nil), ds...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].ID > out[j].ID
		}
		return out[i].At.After(out[j].At)
	})
	return out
}

// viewerOf is who is reading: their id, and whether they host the reviews
// (the cluster's operators) rather than moderate them.
func viewerOf(ctx context.Context) (userID string, operator bool) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(ac.UserId), auth.IsClusterOperator(ac.Role)
}

// actsFor are the decisions this viewer may record on a review in this state:
// the ones ValidateDecision accepts. A reason the act needs is assumed given,
// since the panel asks for one before it sends anything.
func actsFor(state State, moderates, approver bool) []string {
	acts := []string{}
	if !moderates {
		return acts
	}
	for _, action := range Actions {
		reason := ""
		switch action {
		case ActionDecline, ActionAskToFix, ActionRemove:
			reason = Reasons[0]
		}
		if ValidateDecision(action, reason, state, approver) == nil {
			acts = append(acts, action)
		}
	}
	return acts
}

// get answers reviewsGet: one review as the list shows it, its whole decision
// log newest first, and the acts the viewer may take on it now.
func (p *Provider) get(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	reviewID := strings.TrimSpace(asString(args["reviewId"]))
	if reviewID == "" {
		return nil, fmt.Errorf("reviews: reviewId is required")
	}
	row, err := p.readableReview(ctx, reviewID)
	if err != nil {
		return nil, err
	}
	decisions, err := p.decisionsForReview(ctx, reviewID)
	if err != nil {
		return nil, err
	}
	state := StateOf(VersionOf(row), decisions)
	review := map[string]any{}
	for k, v := range row {
		review[k] = v
	}
	review["state"] = string(state)
	review["contentHash"] = contentHashOf(row)
	if t := timeText(row["createdAt"]); t != "" {
		review["createdAt"] = t
	}
	log := make([]LoggedDecision, 0, len(decisions))
	for _, d := range newestFirst(decisions) {
		log = append(log, loggedFrom(d))
	}
	userID, operator := viewerOf(ctx)
	approver := false
	if !operator && userID != "" {
		if approver, err = p.isRemovalApprover(ctx, strings.TrimSpace(asString(row["storeId"])), userID); err != nil {
			return nil, err
		}
	}
	return receipt("v1:reviews:reviewRead", slugOf(canonicalReviewID(reviewID)), map[string]any{
		"review":    review,
		"decisions": log,
		"acts":      actsFor(state, !operator && userID != "", approver),
		"moderates": !operator && userID != "",
	})
}

// decisionLog answers reviewsDecisionLog: every decision on the store's
// reviews, newest first, paged. Each entry names the product and headline of
// its review and the state the review is in now -- never the writer, whose
// name and address are the list's and the review's to show.
func (p *Provider) decisionLog(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := strings.TrimSpace(asString(args["storeId"]))
	if storeID == "" {
		return nil, fmt.Errorf("reviews: storeId is required")
	}
	if err := p.requireSiteForStore(ctx, asString(args["siteId"]), storeID); err != nil {
		return nil, err
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
			return nil, fmt.Errorf("reviews: %q is not a cursor this log issued", after)
		}
		offset = n
	}
	rows, err := p.rowsFor(ctx, "reviewsInStore", map[string]string{"storeId": storeID})
	if err != nil {
		return nil, err
	}
	byID := map[string]map[string]any{}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		id := canonicalReviewID(asString(row["id"]))
		if id == "" {
			continue
		}
		byID[id] = row
		ids = append(ids, id)
	}
	decisions, err := p.decisionsByReview(ctx, storeID, ids)
	if err != nil {
		return nil, err
	}
	all := []Decision{}
	states := map[string]State{}
	for id, ds := range decisions {
		row, known := byID[id]
		if !known {
			continue
		}
		states[id] = StateOf(VersionOf(row), ds)
		all = append(all, ds...)
	}
	all = newestFirst(all)
	total := len(all)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	entries := make([]map[string]any, 0, end-offset)
	for _, d := range all[offset:end] {
		row := byID[canonicalReviewID(d.ReviewID)]
		logged := loggedFrom(d)
		raw, _ := json.Marshal(logged)
		entry := map[string]any{}
		_ = json.Unmarshal(raw, &entry)
		entry["reviewId"] = canonicalReviewID(d.ReviewID)
		entry["reviewState"] = string(states[canonicalReviewID(d.ReviewID)])
		setIfPresent(entry, "productHandle", strings.TrimSpace(asString(row["productHandle"])))
		setIfPresent(entry, "title", strings.TrimSpace(asString(row["title"])))
		entries = append(entries, entry)
	}
	next := ""
	if end < total {
		next = strconv.Itoa(end)
	}
	return receipt("v1:reviews:decisionLog", fmt.Sprintf("log-%s-%d", slugOf(storeID), offset), map[string]any{
		"storeId":   storeID,
		"decisions": entries,
		"count":     len(entries),
		"total":     total,
		"next":      next,
	})
}

// readSettings answers reviewsSettings: the store's settings with every
// default applied, whether the store has a settings row at all, and what this
// viewer may change -- writeSettings' two rules, read rather than tried.
func (p *Provider) readSettings(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := strings.TrimSpace(asString(args["storeId"]))
	if storeID == "" {
		return nil, fmt.Errorf("reviews: storeId is required; settings are per store")
	}
	if err := p.requireSiteForStore(ctx, asString(args["siteId"]), storeID); err != nil {
		return nil, err
	}
	row, err := p.settingsFor(ctx, storeID)
	if err != nil {
		return nil, err
	}
	eff := EffectiveSettings(row)
	userID, operator := viewerOf(ctx)
	exists := len(row) > 0
	approver := !operator && userID != "" && eff.RemovalApproverUserID != "" && eff.RemovalApproverUserID == userID
	return receipt("v1:reviews:settingsRead", SettingsRowID(storeID), map[string]any{
		"storeId": storeID,
		"exists":  exists,
		"settings": map[string]any{
			"publicDisplay":           eff.PublicDisplay,
			"linkLifetimeDays":        eff.LinkLifetimeDays,
			"requestDelayDays":        eff.RequestDelayDays,
			"requestsEnabled":         eff.RequestsEnabled,
			"requestTemplateId":       eff.RequestTemplateID,
			"requestSenderIdentityId": eff.RequestSenderIdentityID,
			"notifyEmail":             eff.NotifyEmail,
			"removalApproverUserId":   eff.RemovalApproverUserID,
			"criteriaText":            strings.TrimSpace(asString(row["criteriaText"])),
		},
		"may": map[string]any{
			"edit":         userID != "" && (exists || operator),
			"nameApprover": userID != "" && (operator || approver),
			"nameSender":   userID != "" && operator,
		},
		"viewer": map[string]any{
			"moderates":        !operator && userID != "",
			"approvesRemovals": approver,
		},
	})
}
