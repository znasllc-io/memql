package costestimate

import (
	"fmt"
	"math"
	"net/url"
	"time"
)

// Tariff retains the utility's rate evidence; it contains no street address.
// Bands span applicable TOU periods/tiers when household consumption or a
// per-period meter reading is unavailable. Fixed/demand charges are not silently
// assigned to an individual inference call.
type Tariff struct {
	Provider       string     `json:"provider"`
	Name           string     `json:"name"`
	Currency       string     `json:"currency"`
	Timezone       string     `json:"timezone"`
	SourceURLs     []string   `json:"sourceUrls"`
	CheckedAt      string     `json:"checkedAt"`
	RefreshAfter   string     `json:"refreshAfter"`
	EffectiveFrom  string     `json:"effectiveFrom"`
	EffectiveUntil string     `json:"effectiveUntil,omitempty"`
	RateBands      []RateBand `json:"rateBands"`
	Assumptions    []string   `json:"assumptions"`
}
type RateBand struct {
	Name        string  `json:"name"`
	PerKWh      float64 `json:"perKWh"`
	AppliesWhen string  `json:"appliesWhen"`
}
type Energy struct {
	Method     string   `json:"method"` // measured-kwh or estimated-watts
	KWh        *float64 `json:"kWh"`
	Watts      *float64 `json:"watts"`
	Seconds    float64  `json:"seconds"`
	Source     string   `json:"source"`
	Allocation string   `json:"allocation"`
}
type ElectricityEstimate struct {
	Kind         string   `json:"kind"`
	Status       string   `json:"status"`
	Reason       string   `json:"reason,omitempty"`
	Currency     string   `json:"currency"`
	Amount       *float64 `json:"amount"`
	Minimum      *float64 `json:"minimum"`
	Maximum      *float64 `json:"maximum"`
	KWh          *float64 `json:"kWh"`
	Energy       Energy   `json:"energy"`
	Tariff       *Tariff  `json:"tariff"`
	Freshness    string   `json:"freshness"`
	CalculatedAt string   `json:"calculatedAt"`
	Assumptions  []string `json:"assumptions"`
}

func ValidateTariff(t Tariff) error {
	if t.Provider == "" || t.Name == "" || len(t.Currency) != 3 || len(t.RateBands) == 0 || len(t.RateBands) > 32 {
		return fmt.Errorf("provider, tariff, currency and bounded rate bands are required")
	}
	if _, err := time.LoadLocation(t.Timezone); err != nil {
		return fmt.Errorf("tariff timezone is invalid")
	}
	checked, err := time.Parse(time.RFC3339, t.CheckedAt)
	if err != nil {
		return fmt.Errorf("tariff checked-at date is invalid")
	}
	refresh, err := time.Parse(time.RFC3339, t.RefreshAfter)
	if err != nil || !refresh.After(checked) {
		return fmt.Errorf("tariff refresh date must follow its evidence date")
	}
	effective, err := time.Parse("2006-01-02", t.EffectiveFrom)
	if err != nil {
		return fmt.Errorf("tariff effective date is required")
	}
	if t.EffectiveUntil != "" {
		end, err := time.Parse("2006-01-02", t.EffectiveUntil)
		if err != nil || !end.After(effective) {
			return fmt.Errorf("tariff end date is invalid")
		}
	}
	if len(t.SourceURLs) == 0 || len(t.SourceURLs) > 8 {
		return fmt.Errorf("tariff needs cited utility evidence")
	}
	for _, s := range t.SourceURLs {
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			return fmt.Errorf("tariff citations must be public HTTPS URLs")
		}
	}
	for _, b := range t.RateBands {
		if b.Name == "" || b.AppliesWhen == "" || !finiteNonnegative(b.PerKWh) {
			return fmt.Errorf("each rate needs a name, applicability and nonnegative price per kWh")
		}
	}
	return nil
}
func finiteNonnegative(n float64) bool { return n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0) }

func Electricity(t *Tariff, energy Energy, at time.Time) ElectricityEstimate {
	e := ElectricityEstimate{Kind: "electricity", Status: "unavailable", Currency: "", Energy: energy, Tariff: t, Freshness: "unavailable", CalculatedAt: at.UTC().Format(time.RFC3339), Assumptions: []string{"Marginal electricity estimate; not a utility bill. Fixed/demand charges, taxes and other fees are excluded unless explicitly included in the rate evidence."}}
	if t == nil {
		e.Reason = "Confirm the machine's utility and applicable tariff."
		return e
	}
	if err := ValidateTariff(*t); err != nil {
		e.Reason = err.Error()
		return e
	}
	e.Currency = t.Currency
	checked, _ := time.Parse(time.RFC3339, t.CheckedAt)
	refresh, _ := time.Parse(time.RFC3339, t.RefreshAfter)
	zone, _ := time.LoadLocation(t.Timezone)
	day := at.In(zone).Format("2006-01-02")
	if checked.After(at) || day < t.EffectiveFrom || (t.EffectiveUntil != "" && day >= t.EffectiveUntil) {
		e.Reason = "Tariff evidence does not cover this date."
		return e
	}
	e.Freshness = "current"
	if !at.Before(refresh) {
		e.Freshness = "stale"
	}
	if energy.Source == "" || energy.Allocation == "" {
		e.Reason = "Energy needs a measurement or estimate source and allocation basis."
		return e
	}
	var kwh float64
	switch energy.Method {
	case "measured-kwh":
		if energy.KWh == nil || !finiteNonnegative(*energy.KWh) {
			e.Reason = "Measured kWh is unavailable or invalid."
			return e
		}
		kwh = *energy.KWh
	case "estimated-watts":
		if energy.Watts == nil || !finiteNonnegative(*energy.Watts) || !finiteNonnegative(energy.Seconds) || energy.Seconds <= 0 {
			e.Reason = "Estimated energy needs watts and a positive measured duration."
			return e
		}
		kwh = *energy.Watts * energy.Seconds / 3600000
		e.Assumptions = append(e.Assumptions, "Energy is estimated as watts × seconds ÷ 3,600,000, not derived from tokens.")
	default:
		e.Reason = "No energy measurement or explicit wattage estimate."
		return e
	}
	if !finiteNonnegative(kwh) {
		e.Reason = "Energy exceeds numeric limits."
		return e
	}
	low, high := math.Inf(1), 0.0
	for _, b := range t.RateBands {
		low = math.Min(low, b.PerKWh)
		high = math.Max(high, b.PerKWh)
	}
	low *= kwh
	high *= kwh
	if !finiteNonnegative(high) {
		e.Reason = "Cost exceeds numeric limits."
		return e
	}
	e.KWh = &kwh
	e.Minimum = &low
	e.Maximum = &high
	e.Status = "range"
	e.Assumptions = append(e.Assumptions, t.Assumptions...)
	if low == high {
		e.Amount = &low
		e.Status = "estimated"
	} else {
		e.Assumptions = append(e.Assumptions, "Range spans the retained time-of-use/tier bands; per-band energy and household tier consumption are unknown.")
	}
	return e
}
