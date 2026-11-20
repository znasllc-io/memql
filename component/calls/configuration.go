package calls

import "time"

// CallConfiguration is a server-quoted local simulation snapshot. A room keeps
// this rate version and capacity unchanged; old preview/pass rooms have none.
type CallConfiguration struct {
	ParticipantLimit int    `json:"participantLimit"`
	DurationMinutes  int    `json:"durationMinutes"`
	Insights         bool   `json:"insights"`
	RateVersion      string `json:"rateVersion"`
}
type CallQuote struct {
	CallConfiguration
	QuoteID                   string    `json:"quoteId"`
	RateMicroDollarsPerMinute int64     `json:"rateMicroDollarsPerMinute"`
	MaxCostCents              int64     `json:"maxCostCents"`
	ExpiresAt                 time.Time `json:"expiresAt"`
	ExpiresInMs               int64     `json:"expiresInMs"`
}

const PreviewRateVersion = "preview-2026-11-20-v1"

func ValidCallConfiguration(c CallConfiguration) bool {
	return c.ParticipantLimit >= 1 && c.ParticipantLimit <= 100 && !c.Insights && c.RateVersion == PreviewRateVersion && (c.DurationMinutes == 10 || c.DurationMinutes >= 15 && c.DurationMinutes <= 1440 && c.DurationMinutes%15 == 0)
}

// PreviewRate uses marginal participant bands, never a room's capacity in place
// of its connected participant count. Units retain fractional cents until the
// final settlement. This catalog authorizes simulation only.
func PreviewRate(participants int) int64 {
	if participants < 0 || participants > 100 {
		return 0
	}
	rate := int64(min(participants, 8)) * 2500
	n := max(0, participants-8)
	rate += int64(min(n, 17)) * 1500
	n = max(0, n-17)
	rate += int64(min(n, 25)) * 1000
	n = max(0, n-25)
	rate += int64(min(n, 50)) * 500
	return rate
}
func PreviewReservation(c CallConfiguration) int64 {
	return (PreviewRate(c.ParticipantLimit)*int64(c.DurationMinutes) + 9999) / 10000
}
