package emailstats

import "time"

// statistics_payload.go -- the emailStatistics answer as the node carries it
// (statistics.go reads it, this file renders it).

// Payload is the answer as the node carries it, which the OS parses
// (parseEmailStatistics): lowerCamel keys, instants in RFC3339, measuredSince
// as YYYY-MM-DD, and every figure nothing measured PRESENT AND NULL, so a
// client can tell "not measured" from a key it does not know -- the website
// read's payload (component/sitetraffic, Statistics.Payload). Without a
// previous period, previous, previousSeries, prevStart and prevEnd are null.
func (s EmailStatistics) Payload() map[string]any {
	p := map[string]any{
		"period":         string(s.Period),
		"bucket":         s.Bucket,
		"start":          instant(s.Start),
		"end":            instant(s.End),
		"hasPrevious":    s.HasPrevious,
		"prevStart":      nil,
		"prevEnd":        nil,
		"measuredSince":  nil,
		"totals":         s.Totals.payload(),
		"previous":       nil,
		"series":         pointsPayload(s.Series),
		"previousSeries": nil,
		"campaigns":      figuresPayload(s.Campaigns),
		"automations":    automationsPayload(s.Automations),
		"audiences":      audiencesPayload(s.Audiences),
		"accounts":       figuresPayload(s.Accounts),
	}
	if s.HasPrevious {
		p["prevStart"], p["prevEnd"] = instant(s.PrevStart), instant(s.PrevEnd)
		p["previous"] = s.Previous.payload()
		p["previousSeries"] = pointsPayload(s.PreviousSeries)
	}
	if s.MeasuredSince != nil {
		p["measuredSince"] = s.MeasuredSince.UTC().Format(time.DateOnly)
	}
	return p
}

func (t EmailTotals) payload() map[string]any {
	return map[string]any{
		"sent": countOrNull(t.Sent), "delivered": countOrNull(t.Delivered),
		"undelivered": countOrNull(t.Undelivered), "failed": countOrNull(t.Failed),
		"skipped": countOrNull(t.Skipped), "suppressed": countOrNull(t.Suppressed),
		"opens": countOrNull(t.Opens), "clicks": countOrNull(t.Clicks),
		"uniqueOpens": countOrNull(t.UniqueOpens), "uniqueClicks": countOrNull(t.UniqueClicks),
		"unsubscribes": countOrNull(t.Unsubscribes), "bounces": countOrNull(t.Bounces),
		"complaints": countOrNull(t.Complaints), "subscribes": countOrNull(t.Subscribes),
		"openRate": rateOrNull(t.OpenRate), "clickRate": rateOrNull(t.ClickRate),
	}
}

func pointsPayload(points []EmailPoint) []any {
	out := make([]any, len(points))
	for i, p := range points {
		out[i] = map[string]any{
			"at":   instant(p.At),
			"sent": countOrNull(p.Sent), "uniqueOpens": countOrNull(p.UniqueOpens),
			"uniqueClicks": countOrNull(p.UniqueClicks), "unsubscribes": countOrNull(p.Unsubscribes),
			"subscribes": countOrNull(p.Subscribes), "bounces": countOrNull(p.Bounces),
			"complaints": countOrNull(p.Complaints), "failed": countOrNull(p.Failed),
			"suppressed": countOrNull(p.Suppressed),
			"openRate":   rateOrNull(p.OpenRate), "clickRate": rateOrNull(p.ClickRate),
		}
	}
	return out
}

func figuresPayload(rows []CampaignFigures) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = map[string]any{
			"id": r.ID, "sent": r.Sent, "uniqueOpens": r.UniqueOpens, "uniqueClicks": r.UniqueClicks,
			"openRate": rateOrNull(r.OpenRate), "clickRate": rateOrNull(r.ClickRate),
		}
	}
	return out
}

func automationsPayload(rows []AutomationFigures) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = map[string]any{"id": r.ID, "kind": r.Kind, "sent": r.Sent, "failed": r.Failed, "suppressed": r.Suppressed}
	}
	return out
}

func audiencesPayload(rows []AudienceFigures) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = map[string]any{"id": r.ID, "subscribes": r.Subscribes, "unsubscribes": r.Unsubscribes, "net": r.Net}
	}
	return out
}

// countOrNull and rateOrNull spell an unmeasured figure as an untyped nil,
// which JSON writes as null.
func countOrNull(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func rateOrNull(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// instant renders a bound in RFC3339, UTC; a zero time is no bound at all.
func instant(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}
