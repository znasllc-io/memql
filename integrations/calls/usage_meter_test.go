package calls

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func meterTestRoom(t *testing.T, rows []usageInterval) roomRow {
	t.Helper()
	data, err := json.Marshal(rows)
	require.NoError(t, err)
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	c := callrules.CallConfiguration{ParticipantLimit: 100, DurationMinutes: 60, RateVersion: callrules.PreviewRateVersion}
	return roomRow{OpenedAt: start, roomFunding: roomFunding{FundingMode: usageMeteredMode, FundingDeadline: start.Add(time.Hour), FundingMaxPeople: 100, UsageConfig: encodeUsageConfig(c), UsageReservedCents: callrules.PreviewReservation(c), UsageMeter: string(data)}}
}

func TestUsageCostBillsOnlyProvenIntervals(t *testing.T) {
	start := meterTestRoom(t, nil).OpenedAt
	interval := func(sid, identity string, a, b time.Duration) usageInterval {
		return usageInterval{SID: sid, Identity: identity, Start: start.Add(a), Through: start.Add(b)}
	}
	for name, tc := range map[string]struct {
		rows []usageInterval
		end  time.Duration
		want int64
	}{
		"no connection":                         {[]usageInterval{}, time.Hour, 0},
		"admission is not elapsed media":        {[]usageInterval{interval("a", "host", 0, 0)}, time.Hour, 0},
		"one minute rounds at settlement":       {[]usageInterval{interval("a", "host", 0, time.Minute)}, time.Hour, 0},
		"round once across gaps":                {[]usageInterval{interval("a", "host", 0, time.Minute), interval("b", "host", 10*time.Minute, 11*time.Minute)}, time.Hour, 1},
		"through does not extrapolate":          {[]usageInterval{interval("a", "host", 0, 10*time.Minute)}, time.Hour, 3},
		"overlapping reconnect unions identity": {[]usageInterval{interval("a", "host", 0, 10*time.Minute), interval("b", "host", 5*time.Minute, 15*time.Minute)}, time.Hour, 4},
		"distinct identities both count":        {[]usageInterval{interval("a", "host", 0, 10*time.Minute), interval("b", "guest", 0, 10*time.Minute)}, time.Hour, 5},
		"end truncates interval":                {[]usageInterval{interval("a", "host", 0, time.Hour)}, 10 * time.Minute, 3},
		"opened and deadline bound interval":    {[]usageInterval{interval("a", "host", -time.Hour, 2*time.Hour)}, 2 * time.Hour, 15},
		"end before opened costs zero":          {[]usageInterval{interval("a", "host", 0, time.Hour)}, -time.Minute, 0},
		"same timestamp boundaries":             {[]usageInterval{interval("a", "host", 0, 10*time.Minute), interval("b", "host", 10*time.Minute, 20*time.Minute), interval("c", "guest", 10*time.Minute, 20*time.Minute)}, time.Hour, 8},
	} {
		t.Run(name, func(t *testing.T) {
			room := meterTestRoom(t, tc.rows)
			cost, err := usageCost(room, start.Add(tc.end))
			require.NoError(t, err)
			require.Equal(t, tc.want, cost)
			// Provider delivery order must not alter a settled charge.
			for i, j := 0, len(tc.rows)-1; i < j; i, j = i+1, j-1 {
				tc.rows[i], tc.rows[j] = tc.rows[j], tc.rows[i]
			}
			cost, err = usageCost(meterTestRoom(t, tc.rows), start.Add(tc.end))
			require.NoError(t, err)
			require.Equal(t, tc.want, cost)
		})
	}
	row := interval("late", "host", 0, 30*time.Minute)
	row.End = start.Add(10 * time.Minute)
	cost, err := usageCost(meterTestRoom(t, []usageInterval{row}), start.Add(time.Hour))
	require.NoError(t, err)
	require.EqualValues(t, 3, cost, "a signed late disconnect shortens a previous observation")
}

func TestUsageCostRepricesConcurrentPeopleAndHonorsReservation(t *testing.T) {
	room := meterTestRoom(t, nil)
	rows := make([]usageInterval, 100)
	for i := range rows {
		through := room.OpenedAt.Add(30 * time.Minute)
		if i < 2 {
			through = room.OpenedAt.Add(time.Hour)
		}
		rows[i] = usageInterval{SID: fmt.Sprint(i), Identity: fmt.Sprint(i), Start: room.OpenedAt, Through: through}
	}
	room = meterTestRoom(t, rows)
	cost, err := usageCost(room, room.FundingDeadline)
	require.NoError(t, err)
	require.EqualValues(t, 302, cost, "100 people for 30 minutes plus two for 30 minutes: $3.015 rounds once")
	room.FundingMaxPeople = 8
	cost, err = usageCost(room, room.FundingDeadline)
	require.NoError(t, err)
	require.EqualValues(t, 75, cost, "stray provider identities cannot bill beyond admitted capacity")
	room.UsageReservedCents = 10
	cost, err = usageCost(room, room.FundingDeadline)
	require.NoError(t, err)
	require.EqualValues(t, 10, cost, "the ceiling is never exceeded")
}

func TestUsageMeterRejectsMalformedJournal(t *testing.T) {
	room := meterTestRoom(t, nil)
	for name, raw := range map[string]string{
		"missing": "", "invalid JSON": "[", "wrong type": "{}", "oversize": strings.Repeat(" ", 2*1024*1024+1),
		"missing SID":      `[{"i":"host","a":"2026-11-20T12:00:00Z","b":"2026-11-20T12:01:00Z"}]`,
		"missing identity": `[{"s":"a","a":"2026-11-20T12:00:00Z","b":"2026-11-20T12:01:00Z"}]`,
		"missing start":    `[{"s":"a","i":"host","b":"2026-11-20T12:01:00Z"}]`,
		"negative span":    `[{"s":"a","i":"host","a":"2026-11-20T12:01:00Z","b":"2026-11-20T12:00:00Z"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			room.UsageMeter = raw
			_, err := usageCost(room, room.FundingDeadline)
			require.Equal(t, "allowance_unavailable", callrules.ReasonOf(err))
		})
	}
	rows := make([]usageInterval, usageIntervalLimit+1)
	for i := range rows {
		rows[i] = usageInterval{SID: fmt.Sprint(i), Identity: "host", Start: room.OpenedAt, Through: room.OpenedAt}
	}
	_, err := readUsageMeter(meterTestRoom(t, rows))
	require.Equal(t, "allowance_unavailable", callrules.ReasonOf(err))
}

func TestConfiguredStoredFundingFailsClosed(t *testing.T) {
	valid := meterTestRoom(t, []usageInterval{})
	require.NoError(t, fundingRoomAllowed(valid, valid.OpenedAt))
	for name, change := range map[string]func(*roomRow){
		"invalid configuration": func(r *roomRow) { r.UsageConfig = "{}" },
		"capacity mismatch":     func(r *roomRow) { r.FundingMaxPeople-- },
		"reservation mismatch":  func(r *roomRow) { r.UsageReservedCents-- },
		"deadline extended":     func(r *roomRow) { r.FundingDeadline = r.FundingDeadline.Add(time.Second) },
		"deadline shortened":    func(r *roomRow) { r.FundingDeadline = r.FundingDeadline.Add(-time.Second) },
		"legacy grant mixed in": func(r *roomRow) { r.FundingPassID = "legacy-pass" },
	} {
		t.Run(name, func(t *testing.T) {
			r := valid
			change(&r)
			require.Equal(t, "allowance_unavailable", callrules.ReasonOf(fundingRoomAllowed(r, valid.OpenedAt)))
		})
	}
	require.Equal(t, endReasonAllowance, callrules.ReasonOf(fundingRoomAllowed(valid, valid.FundingDeadline)))
}
