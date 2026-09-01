package campaigns

import (
	"context"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// stats.go -- the outcome breakdown for one campaign (memql#4823, design
// D12).
//
// # What this replaces, and why it had to be server-side
//
// The portal counted delivery rows in the browser, off a page-capped read. So
// every campaign past the page bound was under-reported, by an amount nobody
// could see, in a panel whose whole job is to say how a send went. That is
// the same measurement mistake memql#3460 removed from the send path -- a
// bounded read of an unbounded set is a truncation -- reached a second time
// through a different door.
//
// # Every bucket that CAN be a count IS one
//
// The engine has no group-by, so an aggregate is either one `count` query per
// bucket or a Go fold over rows. A count is exact at any audience size and
// costs one round trip; a fold is bounded by whatever page the read came back
// on. The buckets here are therefore mostly counts, and the two that are not
// say so.
//
// # The two figures that are honest about their limits
//
// UNIQUE opens and clicks are folded in Go, because the engine has no
// DISTINCT. The read behind them is bounded, and a fold over a truncated page
// gives a unique count that is LOWER than the truth and indistinguishable
// from a correct one. So when the read comes back at its bound the figure is
// reported as UNMEASURED -- an ABSENT key, never a zero -- and a client
// renders a dash. Zero and "not measured" are different answers, and the
// whole point of replacing the browser's count was to stop reporting the
// first when the second is true.
//
// SOFT BOUNCES are absent from the reply entirely, and that is a decision
// rather than an omission. Nothing measures them per campaign: a soft bounce
// is transient, deliberately does not suppress, and the feedback path records
// it against the sending identity's reputation counters rather than against
// the campaign. Emitting `soft: 0` would be a claim, and a reader has no way
// to tell a claim from a count. The field is missing so the question has to
// be asked somewhere that can answer it.

// campaignStatsResult is the shape the builtin returns. Built as a map rather
// than a struct because two of its keys are conditionally ABSENT, and
// `omitempty` cannot express "absent because unmeasured" separately from
// "absent because zero".
type campaignStatsResult map[string]any

// suppressedSkipReasons are the skip reasons that mean "the cluster-wide list
// refused this address".
//
// It carries FIVE values for a reason worth stating: the worker writes the
// SUPPRESSION ROW'S reason into skipReason for a cluster-list skip
// (hard_bounce / complaint / manual), and the RECIPIENT ROW'S subscription
// status for a per-audience skip (bounced / complained / unsubscribed). Those
// two enums overlap in meaning and not in spelling, and a list carrying only
// one family would silently file half the suppressions under "other".
var suppressedSkipReasons = []string{"hard_bounce", "complaint", "manual", "bounced", "complained"}

// unsubscribedSkipReason is reported separately from the rest, because "they
// asked us to stop" and "the address is dead" are different facts about a
// list and an operator acts on them differently.
const unsubscribedSkipReason = "unsubscribed"

// handleStats computes the breakdown.
//
// AUTHORIZATION IS THE FIRST READ, as everywhere else in this package:
// campaignById is composite-tier, so a caller who cannot read the campaign
// gets "not found" and nothing further happens. Every subsequent read runs
// under the SAME caller context -- not the owner's, not the engine's -- so
// the counts are computed with exactly the authority the asker holds.
func (w *Worker) handleStats(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	campaignID := memql.BareShortId(strings.TrimSpace(argString(args, "campaignId")))
	if campaignID == "" {
		return nil, fmt.Errorf("campaigns.stats: campaignId is required")
	}
	campaign, found, err := w.store.CampaignByID(ctx, campaignID)
	if err != nil {
		return nil, fmt.Errorf("campaigns.stats: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("campaigns.stats: campaign %q not found", campaignID)
	}

	out, err := w.campaignStats(ctx, campaign)
	if err != nil {
		return nil, fmt.Errorf("campaigns.stats: %w", err)
	}
	return resultNode("campaignStats", out)
}

func (w *Worker) campaignStats(ctx context.Context, campaign Campaign) (campaignStatsResult, error) {
	recipients, err := w.store.RosterSize(ctx, campaign.AudienceID)
	if err != nil {
		return nil, err
	}

	// The delivery buckets. `skipped` is read as a whole and then split,
	// rather than derived by subtraction from the parts: the parts are what
	// might be wrong (a skipReason nobody thought of), and taking the total
	// from the same source keeps `other` an honest remainder instead of a
	// bucket that silently absorbs a query mistake.
	pending, err := w.store.DeliveryCountByStatus(ctx, campaign.ID, "pending")
	if err != nil {
		return nil, err
	}
	sent, err := w.store.DeliveryCountByStatus(ctx, campaign.ID, "sent")
	if err != nil {
		return nil, err
	}
	failed, err := w.store.DeliveryCountByStatus(ctx, campaign.ID, "failed")
	if err != nil {
		return nil, err
	}
	skippedTotal, err := w.store.DeliveryCountByStatus(ctx, campaign.ID, "skipped")
	if err != nil {
		return nil, err
	}
	suppressed, err := w.store.SkipCountByReason(ctx, campaign.ID, suppressedSkipReasons)
	if err != nil {
		return nil, err
	}
	unsubscribedSkips, err := w.store.SkipCountByReason(ctx, campaign.ID, []string{unsubscribedSkipReason})
	if err != nil {
		return nil, err
	}
	other := skippedTotal - suppressed - unsubscribedSkips
	if other < 0 {
		// Only reachable if the reason lists overlap, which they must not.
		// Clamping rather than reporting a negative: a negative count in a
		// panel is a defect the reader cannot interpret, and the total plus
		// the two named buckets are all still exact and visible beside it.
		other = 0
	}

	// The consent buckets. A bounce arrives AFTER the transport accepted the
	// message, so the delivery row says `sent` and stays that way; only the
	// consent stream can answer these.
	hardBounces, err := w.store.ConsentCountByKind(ctx, campaign.ID, ConsentBounce)
	if err != nil {
		return nil, err
	}
	complaints, err := w.store.ConsentCountByKind(ctx, campaign.ID, ConsentComplaint)
	if err != nil {
		return nil, err
	}
	unsubscribes, err := w.store.ConsentCountByKind(ctx, campaign.ID, ConsentWithdraw)
	if err != nil {
		return nil, err
	}

	opens, err := w.engagementStats(ctx, campaign.ID, EngagementOpen)
	if err != nil {
		return nil, err
	}
	clicks, err := w.engagementStats(ctx, campaign.ID, EngagementClick)
	if err != nil {
		return nil, err
	}

	return campaignStatsResult{
		"campaignId": campaign.ID,
		"recipients": recipients,
		"pending":    pending,
		"sent":       sent,
		"failed":     failed,
		"skipped": map[string]any{
			"total":        skippedTotal,
			"suppressed":   suppressed,
			"unsubscribed": unsubscribedSkips,
			"other":        other,
		},
		// A nested object with ONE key, deliberately. `bounces.soft` is
		// absent because nothing measures it per campaign, and the nesting is
		// what makes that absence legible: a flat `hardBounces` would say
		// nothing about the missing half, while `bounces: {hard: n}` reads as
		// the incomplete pair it is.
		"bounces":      map[string]any{"hard": hardBounces},
		"complaints":   complaints,
		"unsubscribed": unsubscribes,
		"opens":        opens,
		"clicks":       clicks,
	}, nil
}

// engagementStats returns {total, unique} for one engagement kind, with
// `unique` ABSENT when it could not be measured.
func (w *Worker) engagementStats(ctx context.Context, campaignID, kind string) (map[string]any, error) {
	total, err := w.store.EngagementCountByKind(ctx, campaignID, kind)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"total": total}

	refs, atBound, err := w.store.EngagementDeliveryRefs(ctx, campaignID, kind)
	if err != nil {
		return nil, err
	}
	if atBound {
		// UNMEASURED. The key is left out entirely and a companion flag says
		// why, so a client renders a dash and an explanation rather than a
		// number that is quietly too low. Emitting 0 or the truncated fold
		// would both be wrong in the same undetectable direction.
		out["uniqueUnmeasured"] = true
		return out, nil
	}
	seen := make(map[string]struct{}, len(refs))
	for _, id := range refs {
		seen[id] = struct{}{}
	}
	out["unique"] = len(seen)
	return out, nil
}
