package costestimate

import (
	"math"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }
func now() time.Time    { return time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC) }
func TestTokenCacheAndLongContext(t *testing.T) {
	u := Usage{Input: 300000, Output: 1000, CacheRead: ptr(int64(100000)), CacheWriteShort: ptr(int64(20000)), CacheWriteLong: ptr(int64(10000)), Known: true}
	e := EstimateTokens("openai", "gpt-6-astra", u, now())
	want := (170000*10+100000*1+20000*12.5+10000*25)*2/1e6 + 1000*50*1.5/1e6
	if e.Amount == nil || math.Abs(*e.Amount-want) > 1e-9 {
		t.Fatalf("%+v want %v", e, want)
	}
	u.Aggregate = true
	e = EstimateTokens("openai", "gpt-6-astra", u, now())
	if e.Amount != nil || e.Status != "range" || *e.Minimum >= *e.Maximum {
		t.Fatal(e)
	}
}
func TestMissingModelUsageCacheAndStaleness(t *testing.T) {
	for _, model := range []string{"", "codex", "gpt-6-astra-unverified"} {
		if EstimateTokens("openai", model, Usage{Known: true}, now()).Amount != nil {
			t.Fatal(model)
		}
	}
	if EstimateTokens("openai", "gpt-6-astra", Usage{}, now()).Status != "unavailable" {
		t.Fatal("missing usage priced")
	}
	e := EstimateTokens("openai", "gpt-6-astra", Usage{Input: 100, Output: 10, Known: true}, now())
	if e.Amount != nil || e.Minimum == nil {
		t.Fatal(e)
	}
	e = EstimateTokens("openai", "gpt-6-astra", Usage{Known: true}, now().Add(30*24*time.Hour))
	if e.Freshness != "stale" {
		t.Fatal(e)
	}
	u := Usage{Known: true, Input: 10, CacheRead: ptr(int64(11))}
	if EstimateTokens("openai", "gpt-6-astra", u, now()).Status != "unavailable" {
		t.Fatal("invalid categories accepted")
	}
}
func tariff() Tariff {
	return Tariff{Provider: "Fixture utility", Name: "Residential", Currency: "USD", Timezone: "America/Phoenix", SourceURLs: []string{"https://utility.example/tariff"}, CheckedAt: "2026-10-22T00:00:00Z", RefreshAfter: "2026-11-22T00:00:00Z", EffectiveFrom: "2026-01-01", RateBands: []RateBand{{"off-peak", .1, "off-peak hours"}, {"peak", .3, "peak hours"}}}
}
func TestElectricityNeedsEnergyAndBoundsTariff(t *testing.T) {
	tr := tariff()
	e := Electricity(&tr, Energy{}, now())
	if e.Status != "unavailable" {
		t.Fatal(e)
	}
	e = Electricity(&tr, Energy{Method: "estimated-watts", Watts: ptr(200.0), Seconds: 3600, Source: "Owner estimate", Allocation: "Entire machine during this hour"}, now())
	if e.Amount != nil || *e.KWh != .2 || math.Abs(*e.Minimum-.02) > 1e-9 || math.Abs(*e.Maximum-.06) > 1e-9 {
		t.Fatal(e)
	}
	tr.EffectiveUntil = "2026-10-22"
	if Electricity(&tr, e.Energy, now()).Status != "unavailable" {
		t.Fatal("expired tariff")
	}
}
func TestCatalogEvidence(t *testing.T) {
	seen := map[string]bool{}
	for _, q := range Catalog() {
		if seen[q.ID] || !validQuote(&q) {
			t.Fatal(q)
		}
		seen[q.ID] = true
		if q.EffectiveFrom == nil && q.EffectiveDateReason == "" {
			t.Fatal("missing effective date explanation")
		}
	}
}
