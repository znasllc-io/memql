package calls

import "time"

// CallConfiguration is a server-quoted local simulation snapshot. A room keeps
// this rate version and capacity unchanged; old preview/pass rooms have none.
type CallConfiguration struct {
	ParticipationMode string `json:"participationMode,omitempty"`
	ParticipantLimit  int    `json:"participantLimit"`
	DurationMinutes   int    `json:"durationMinutes"`
	Insights          bool   `json:"insights"`
	Resources         bool   `json:"resources"`
	RateVersion       string `json:"rateVersion"`
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

// GuidanceRateVersion covers bounded session guidance, not an unconfigured
// transcription/model service. Existing rooms keep the original rate forever.
const GuidanceRateVersion = "preview-2026-11-20-guidance-v2"

func ValidCallConfiguration(c CallConfiguration) bool {
	if c.ParticipationMode != "" && c.ParticipationMode != "adults_only" && c.ParticipationMode != "supervised_family" {
		return false
	}
	if c.ParticipationMode == "supervised_family" && (c.Insights || c.Resources || c.ParticipantLimit < 2) {
		return false
	}
	rateValid := (!c.Insights && !c.Resources && c.RateVersion == PreviewRateVersion) || ((c.Insights || c.Resources) && c.RateVersion == GuidanceRateVersion)
	return c.ParticipantLimit >= 1 && c.ParticipantLimit <= 100 && rateValid && (c.DurationMinutes == 10 || c.DurationMinutes >= 15 && c.DurationMinutes <= 1440 && c.DurationMinutes%15 == 0)
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
	return (ConfigurationRate(c, c.ParticipantLimit)*int64(c.DurationMinutes) + 9999) / 10000
}

// ConfigurationRate applies the immutable, server-quoted service version. It
// shares the existing marginal participant bands and integer accounting. Rates
// remain illustrative local/sandbox values, not a measured production margin.
func ConfigurationRate(c CallConfiguration, participants int) int64 {
	if !ValidCallConfiguration(c) || participants < 0 || participants > 100 {
		return 0
	}
	rate := PreviewRate(participants)
	if c.Insights {
		rate = rate * 175 / 100
	}
	// A reserved source incurs its capacity charge while media is active, even
	// when the owner temporarily pauses publication.
	if c.Resources && participants > 0 {
		rate += 2500
	}
	return rate
}
