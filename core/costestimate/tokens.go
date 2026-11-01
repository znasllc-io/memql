// Package costestimate computes reproducible estimates, never billing receipts.
// Prices and the assumptions used at calculation time travel with each result.
package costestimate

import (
	"embed"
	"encoding/json"
	"math"
	"time"
)

//go:embed prices.json
var catalogFS embed.FS

// Quote is a dated observation of an official public price. EffectiveFrom is
// optional: the day we checked a page is not the day the vendor changed prices.
type Quote struct {
	ID                   string   `json:"id"`
	Vendor               string   `json:"vendor"`
	Model                string   `json:"model"`
	Currency             string   `json:"currency"`
	SourceURLs           []string `json:"sourceUrls"`
	CheckedAt            string   `json:"checkedAt"`
	RefreshAfter         string   `json:"refreshAfter"`
	EffectiveFrom        *string  `json:"effectiveFrom"`
	EffectiveDateReason  string   `json:"effectiveDateReason,omitempty"`
	Input                float64  `json:"inputPerMillion"`
	Output               float64  `json:"outputPerMillion"`
	CacheRead            float64  `json:"cacheReadPerMillion"`
	CacheWriteShort      float64  `json:"cacheWriteShortPerMillion"`
	CacheWriteLong       float64  `json:"cacheWriteLongPerMillion"`
	CacheDurations       []string `json:"cacheDurations"`
	LongContextAbove     int64    `json:"longContextAbove,omitempty"`
	LongInputMultiplier  float64  `json:"longInputMultiplier,omitempty"`
	LongOutputMultiplier float64  `json:"longOutputMultiplier,omitempty"`
}

// Usage normalizes input to ALL input tokens, including cache reads/writes.
// Nil category counts mean unknown; zero means reported zero. Aggregated usage
// must say so: session totals cannot select a per-request context price bracket.
type Usage struct {
	Input           int64  `json:"inputTokens"`
	Output          int64  `json:"outputTokens"`
	CacheRead       *int64 `json:"cacheReadTokens"`
	CacheWriteShort *int64 `json:"cacheWriteShortTokens"`
	CacheWriteLong  *int64 `json:"cacheWriteLongTokens"`
	Known           bool   `json:"known"`
	Estimated       bool   `json:"estimated"`
	Aggregate       bool   `json:"aggregate"`
}

type TokenEstimate struct {
	Kind         string             `json:"kind"`
	Status       string             `json:"status"`
	Reason       string             `json:"reason,omitempty"`
	Currency     string             `json:"currency"`
	Amount       *float64           `json:"amount"`
	Minimum      *float64           `json:"minimum"`
	Maximum      *float64           `json:"maximum"`
	ServedModel  string             `json:"servedModel"`
	ModelMapping string             `json:"modelMapping"`
	CalculatedAt string             `json:"calculatedAt"`
	Freshness    string             `json:"freshness"`
	Quote        *Quote             `json:"quote"`
	Usage        Usage              `json:"usage"`
	Breakdown    map[string]float64 `json:"breakdown,omitempty"`
	Assumptions  []string           `json:"assumptions"`
}

func Catalog() []Quote {
	data, _ := catalogFS.ReadFile("prices.json")
	var quotes []Quote
	// Embedded, validated by tests; do not silently use a partial catalog.
	if err := json.Unmarshal(data, &quotes); err != nil {
		panic(err)
	}
	return quotes
}

func EstimateTokens(vendor, servedModel string, usage Usage, at time.Time) TokenEstimate {
	var quote *Quote
	for _, q := range Catalog() {
		if q.Vendor == vendor && q.Model == servedModel {
			quote = &q
			break
		}
	}
	return Tokens(quote, servedModel, usage, at)
}

// Tokens uses exact model IDs only. Prefix/family guesses would turn an unknown
// app reroute into a plausible, wrong price. A missing mapping remains visible.
func Tokens(q *Quote, servedModel string, u Usage, at time.Time) TokenEstimate {
	e := TokenEstimate{Kind: "api-equivalent", Status: "unavailable", Currency: "USD", ServedModel: servedModel,
		ModelMapping: "unavailable", CalculatedAt: at.UTC().Format(time.RFC3339), Freshness: "unavailable", Usage: u,
		Assumptions: []string{"Standard global API text-token equivalent; not an invoice or subscription allocation.", "Excludes tools, storage, taxes, region premiums and service-tier premiums."}}
	if servedModel == "" {
		e.Reason = "Actual served model was not reported."
		return e
	}
	if q == nil || q.Model != servedModel {
		e.Reason = "No verified official price mapping for the served model."
		return e
	}
	e.Quote = q
	e.Currency = q.Currency
	e.ModelMapping = "exact served-model ID"
	checked, err := time.Parse(time.RFC3339, q.CheckedAt)
	refresh, refreshErr := time.Parse(time.RFC3339, q.RefreshAfter)
	if err != nil || refreshErr != nil || checked.After(at) || !refresh.After(checked) || !validQuote(q) {
		e.Reason = "Price evidence is invalid or postdates this estimate."
		return e
	}
	e.Freshness = "current"
	if !at.Before(refresh) {
		e.Freshness = "stale"
		e.Assumptions = append(e.Assumptions, "Price refresh is overdue; retained quote has not been reverified.")
	}
	if !u.Known {
		e.Reason = "Token usage was not reported or explicitly estimated."
		return e
	}
	if u.Input < 0 || u.Output < 0 {
		e.Reason = "Invalid negative token usage."
		return e
	}
	remaining := u.Input
	counts := []*int64{u.CacheRead, u.CacheWriteShort, u.CacheWriteLong}
	rates := []float64{q.CacheRead, q.CacheWriteShort, q.CacheWriteLong}
	names := []string{"cacheRead", "cacheWriteShort", "cacheWriteLong"}
	minUnknown, maxUnknown := q.Input, q.Input
	exact := true
	e.Breakdown = map[string]float64{}
	knownInputCost := 0.0
	for n, count := range counts {
		if count == nil {
			exact = false
			minUnknown = math.Min(minUnknown, rates[n])
			maxUnknown = math.Max(maxUnknown, rates[n])
			continue
		}
		if *count < 0 || *count > remaining {
			e.Reason = "Cache categories exceed total input tokens."
			e.Breakdown = nil
			return e
		}
		remaining -= *count
		part := float64(*count) * rates[n] / 1e6
		knownInputCost += part
		e.Breakdown[names[n]] = part
	}
	inputMin, inputMax := knownInputCost+float64(remaining)*minUnknown/1e6, knownInputCost+float64(remaining)*maxUnknown/1e6
	outputMin, outputMax := float64(u.Output)*q.Output/1e6, float64(u.Output)*q.Output/1e6
	if q.LongContextAbove > 0 && u.Input > q.LongContextAbove {
		inputMax *= q.LongInputMultiplier
		outputMax *= q.LongOutputMultiplier
		if !u.Aggregate {
			inputMin *= q.LongInputMultiplier
			outputMin *= q.LongOutputMultiplier
			for key, value := range e.Breakdown {
				e.Breakdown[key] = value * q.LongInputMultiplier
			}
		} else {
			exact = false
			e.Assumptions = append(e.Assumptions, "Aggregate usage lacks per-request context brackets; range includes short and long context pricing.")
		}
	}
	if u.CacheRead == nil || u.CacheWriteShort == nil || u.CacheWriteLong == nil {
		e.Assumptions = append(e.Assumptions, "Unreported cache categories are bounded by eligible input/cache rates, not assumed zero.")
	}
	if u.Aggregate {
		e.Assumptions = append(e.Assumptions, "Aggregate estimate assumes all reported tokens used the reported served model; per-request model attribution is unavailable.")
	}
	if u.Estimated {
		e.Assumptions = append(e.Assumptions, "Token counts are estimates, not provider measurements.")
	}
	low, high := inputMin+outputMin, inputMax+outputMax
	if math.IsNaN(low) || math.IsInf(high, 0) {
		e.Reason = "Estimate exceeds numeric limits."
		return e
	}
	e.Minimum = &low
	e.Maximum = &high
	e.Status = "range"
	if exact {
		e.Status = "estimated"
		e.Amount = &low
		e.Breakdown["uncachedInput"] = inputMin - knownInputCost
		if q.LongContextAbove > 0 && u.Input > q.LongContextAbove {
			e.Breakdown["uncachedInput"] = float64(remaining) * q.Input * q.LongInputMultiplier / 1e6
		}
		e.Breakdown["output"] = outputMin
	} else {
		e.Breakdown = nil
	}
	return e
}

func validQuote(q *Quote) bool {
	if q.Currency != "USD" || len(q.SourceURLs) == 0 {
		return false
	}
	for _, v := range []float64{q.Input, q.Output, q.CacheRead, q.CacheWriteShort, q.CacheWriteLong} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return q.LongContextAbove <= 0 || (q.LongInputMultiplier >= 1 && q.LongOutputMultiplier >= 1)
}
