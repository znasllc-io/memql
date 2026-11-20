package calls

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCallConfigurationBounds(t *testing.T) {
	for _, people := range []int{1, 8, 25, 50, 100} {
		for _, minutes := range []int{10, 15, 30, 180, 1425, 1440} {
			require.True(t, ValidCallConfiguration(CallConfiguration{ParticipantLimit: people, DurationMinutes: minutes, RateVersion: PreviewRateVersion}))
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
			require.False(t, ValidCallConfiguration(c))
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
		t.Run(fmt.Sprint(tc.people), func(t *testing.T) { require.Equal(t, tc.rate, PreviewRate(tc.people)) })
	}
	for _, tc := range []struct {
		people, minutes int
		cents           int64
	}{{1, 10, 3}, {1, 15, 4}, {2, 60, 30}, {8, 60, 120}, {100, 10, 96}, {100, 1440, 13752}} {
		c := CallConfiguration{ParticipantLimit: tc.people, DurationMinutes: tc.minutes, RateVersion: PreviewRateVersion}
		require.Equal(t, tc.cents, PreviewReservation(c))
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
			require.GreaterOrEqual(t, reserved*10000, raw)
			require.Less(t, (reserved-1)*10000, raw)
			require.LessOrEqual(t, reserved, int64(50000))
		}
	}
}
