package calls

import (
	"fmt"
	"testing"
)

func TestCallConfigurationBounds(t *testing.T) {
	for _, people := range []int{1, 8, 25, 50, 100} {
		for _, minutes := range []int{10, 15, 30, 180, 1425, 1440} {
			if !ValidCallConfiguration(CallConfiguration{ParticipantLimit: people, DurationMinutes: minutes, RateVersion: PreviewRateVersion}) {
				t.Fatalf("valid configuration rejected: %d people, %d minutes", people, minutes)
			}
		}
	}
	valid := CallConfiguration{ParticipantLimit: 100, DurationMinutes: 1440, RateVersion: PreviewRateVersion}
	for name, change := range map[string]func(*CallConfiguration){
		"zero people":   func(c *CallConfiguration) { c.ParticipantLimit = 0 },
		"extra people":  func(c *CallConfiguration) { c.ParticipantLimit = 101 },
		"negative time": func(c *CallConfiguration) { c.DurationMinutes = -15 },
		"zero time":     func(c *CallConfiguration) { c.DurationMinutes = 0 },
		"short time":    func(c *CallConfiguration) { c.DurationMinutes = 5 },
		"time gap":      func(c *CallConfiguration) { c.DurationMinutes = 11 },
		"time step":     func(c *CallConfiguration) { c.DurationMinutes = 16 },
		"over day":      func(c *CallConfiguration) { c.DurationMinutes = 1455 },
		"insights":      func(c *CallConfiguration) { c.Insights = true },
		"missing rate":  func(c *CallConfiguration) { c.RateVersion = "" },
		"unknown rate":  func(c *CallConfiguration) { c.RateVersion = "preview-future" },
	} {
		t.Run(name, func(t *testing.T) {
			c := valid
			change(&c)
			if ValidCallConfiguration(c) {
				t.Fatalf("invalid configuration accepted: %+v", c)
			}
		})
	}
}

func TestPreviewPricingUsesMarginalBandsAndCeilingReservation(t *testing.T) {
	for _, tc := range []struct {
		people int
		rate   int64
	}{
		{-1, 0}, {0, 0}, {1, 2500}, {8, 20000}, {9, 21500},
		{25, 45500}, {26, 46500}, {50, 70500}, {51, 71000}, {100, 95500}, {101, 0},
	} {
		t.Run(fmt.Sprint(tc.people), func(t *testing.T) {
			if got := PreviewRate(tc.people); got != tc.rate {
				t.Fatalf("rate=%d, want %d", got, tc.rate)
			}
		})
	}
	for _, tc := range []struct {
		people, minutes int
		cents           int64
	}{{1, 10, 3}, {1, 15, 4}, {2, 60, 30}, {8, 60, 120}, {100, 10, 96}, {100, 1440, 13752}} {
		c := CallConfiguration{ParticipantLimit: tc.people, DurationMinutes: tc.minutes, RateVersion: PreviewRateVersion}
		if got := PreviewReservation(c); got != tc.cents {
			t.Fatalf("%+v reservation=%d, want %d", c, got, tc.cents)
		}
	}
	// Every supported quote covers its unrounded maximum; no duration or band
	// boundary can reserve less than the eventual full-room settlement.
	for people := 1; people <= 100; people++ {
		for minutes := 10; minutes <= 1440; minutes++ {
			c := CallConfiguration{ParticipantLimit: people, DurationMinutes: minutes, RateVersion: PreviewRateVersion}
			if !ValidCallConfiguration(c) {
				continue
			}
			raw := PreviewRate(people) * int64(minutes)
			reserved := PreviewReservation(c)
			if reserved*10000 < raw || (reserved-1)*10000 >= raw || reserved > 50000 {
				t.Fatalf("invalid reservation for %+v: %d cents, raw %d", c, reserved, raw)
			}
		}
	}
}

// New capabilities need explicit paid configuration; old signed configurations
// cannot silently gain them. Every reservation covers its maximum settlement.
func TestGuidanceAndResourcesUseExplicitVersionAndCoveredRates(t *testing.T) {
	for _, insights := range []bool{false, true} {
		for _, resources := range []bool{false, true} {
			if !insights && !resources {
				continue
			}
			for people := 1; people <= 100; people++ {
				for _, minutes := range []int{10, 15, 60, 1440} {
					c := CallConfiguration{ParticipantLimit: people, DurationMinutes: minutes, Insights: insights, Resources: resources, RateVersion: GuidanceRateVersion}
					if !ValidCallConfiguration(c) {
						t.Fatalf("valid capability refused: %+v", c)
					}
					want := PreviewRate(people)
					if insights {
						want = want * 175 / 100
					}
					if resources {
						want += 2500
					}
					if got := ConfigurationRate(c, people); got != want {
						t.Fatalf("rate %d want %d", got, want)
					}
					reserved := PreviewReservation(c)
					raw := want * int64(minutes)
					if reserved*10000 < raw || (reserved-1)*10000 >= raw {
						t.Fatalf("underfunded capability: %+v", c)
					}
					c.RateVersion = PreviewRateVersion
					if ValidCallConfiguration(c) || ConfigurationRate(c, people) != 0 {
						t.Fatal("legacy configuration gained new powers")
					}
				}
			}
		}
	}
}
