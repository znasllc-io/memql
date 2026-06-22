package telephony

import "math"

// Per-minute cost estimates in USD (June 2026 model). OpenAI Realtime audio is
// ~90% of per-minute cost; the carrier trunk is the cheap part. These back the
// per-call costEstimate attribution on v1:telephony:call.
//
// IMPORTANT: this is an UPPER-BOUND estimate from wall-clock duration. A
// VAD-gated silent call costs ~$0 in reality (no audio tokens are billed while
// the input gate is closed and the model is not speaking), so a quiet call is
// over-counted here. Treat costEstimate as a ceiling for alerting + rough
// attribution; reconcile against provider usage for exact billing.
const (
	// defaultModelCostPerMinute is the OpenAI Realtime audio cost for a minute
	// of active two-way speech with prompt caching on (~$0.096; rounded).
	defaultModelCostPerMinute = 0.10

	carrierInboundLocalPerMinute    = 0.0032
	carrierInboundTollFreePerMinute = 0.015
	carrierOutboundPerMinute        = 0.005
)

// carrierCostPerMinute returns the carrier leg cost for a call.
func carrierCostPerMinute(direction string, numberType NumberType) float64 {
	if direction == "outbound" {
		return carrierOutboundPerMinute
	}
	if numberType == NumberTypeTollFree {
		return carrierInboundTollFreePerMinute
	}
	return carrierInboundLocalPerMinute
}

// EstimateCallCost returns an upper-bound USD cost estimate for a call of the
// given wall-clock duration, blending the model audio cost with the carrier
// leg. modelPerMinute lets the caller override the model rate (0 uses the
// default).
func EstimateCallCost(durationSeconds int, direction string, numberType NumberType, modelPerMinute float64) float64 {
	if durationSeconds <= 0 {
		return 0
	}
	if modelPerMinute <= 0 {
		modelPerMinute = defaultModelCostPerMinute
	}
	minutes := float64(durationSeconds) / 60.0
	total := minutes * (modelPerMinute + carrierCostPerMinute(direction, numberType))
	// Round to cents-ish precision (4 dp) for stable storage.
	return math.Round(total*10000) / 10000
}
