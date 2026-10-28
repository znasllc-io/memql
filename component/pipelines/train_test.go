package pipelines

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

const trainOwnerID = "123456789012345678"

func TestParseTrainSettings(t *testing.T) {
	got, err := ParseTrainSettings(`{"repositories":{"ZnasLLC-io/MemQL":{"enabled":true,"departures":["09:00","15:00"],"timeZone":"America/Chicago","channel":"znas-instance","ownerDiscordUserId":"` + trainOwnerID + `"}}}`)
	if err != nil {
		t.Fatalf("ParseTrainSettings: %v", err)
	}
	s, ok := got["znasllc-io/memql"]
	if !ok {
		t.Fatalf("settings are keyed by the lower-cased repository; got keys %v", keysOf(got))
	}
	want := TrainSettings{
		Repository:         "znasllc-io/memql",
		Enabled:            true,
		Departures:         []string{"09:00", "15:00"},
		TimeZone:           "America/Chicago",
		Channel:            "znas-instance",
		OwnerDiscordUserID: trainOwnerID,
		MaxRebuilds:        2,
	}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("settings = %+v, want %+v", s, want)
	}
}

func TestParseTrainSettingsOfNothingIsNoTrains(t *testing.T) {
	for _, raw := range []string{"", "   ", "\n"} {
		got, err := ParseTrainSettings(raw)
		if err != nil || len(got) != 0 {
			t.Errorf("ParseTrainSettings(%q) = %v, %v; want no trains and no error", raw, got, err)
		}
	}
}

func TestParseTrainSettingsRefuses(t *testing.T) {
	entry := func(field string) string {
		fields := map[string]string{
			"enabled":            `"enabled":true`,
			"departures":         `"departures":["09:00"]`,
			"timeZone":           `"timeZone":"America/Chicago"`,
			"channel":            `"channel":"znas-instance"`,
			"ownerDiscordUserId": `"ownerDiscordUserId":"` + trainOwnerID + `"`,
		}
		var parts []string
		for _, k := range []string{"enabled", "departures", "timeZone", "channel", "ownerDiscordUserId"} {
			parts = append(parts, fields[k])
		}
		if field != "" {
			parts = append(parts, field)
		}
		return strings.Join(parts, ",")
	}
	cases := []struct {
		name, raw, names string
	}{
		{"an unknown field", `{"repositories":{"a/b":{` + entry(`"speed":3`) + `}}}`, "speed"},
		{"a departure that is not HH:MM", `{"repositories":{"a/b":{` + strings.Replace(entry(""), `["09:00"]`, `["9am"]`, 1) + `}}}`, "departures"},
		{"a departure past midnight", `{"repositories":{"a/b":{` + strings.Replace(entry(""), `["09:00"]`, `["24:00"]`, 1) + `}}}`, "departures"},
		{"an unknown time zone", `{"repositories":{"a/b":{` + strings.Replace(entry(""), "America/Chicago", "Mars/Base", 1) + `}}}`, "timeZone"},
		{"no channel", `{"repositories":{"a/b":{` + strings.Replace(entry(""), `"znas-instance"`, `""`, 1) + `}}}`, "channel"},
		{"a Discord id that is not digits", `{"repositories":{"a/b":{` + strings.Replace(entry(""), trainOwnerID, "abc", 1) + `}}}`, "ownerDiscordUserId"},
		{"too many rebuilds", `{"repositories":{"a/b":{` + entry(`"maxRebuilds":6`) + `}}}`, "maxRebuilds"},
		{"a repository with no owner", `{"repositories":{"memql":{` + entry("") + `}}}`, "memql"},
		{"no departures", `{"repositories":{"a/b":{` + strings.Replace(entry(""), `["09:00"]`, `[]`, 1) + `}}}`, "departures"},
		{"something that is not JSON", `repositories: a/b`, "MEMQL_PIPELINES_TRAINS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseTrainSettings(c.raw)
			if err == nil {
				t.Fatalf("ParseTrainSettings accepted %s", c.raw)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("error %q does not name %q", err, c.names)
			}
		})
	}
}

func TestDepartureDue(t *testing.T) {
	s := TrainSettings{Departures: []string{"09:00", "15:00"}, TimeZone: "America/Chicago"}
	at := func(v string) time.Time {
		tm, err := time.Parse(time.RFC3339, v)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	cases := []struct {
		name      string
		last, now string
		want      string // empty: none due
	}{
		// 2026-10-28 is daylight time in Chicago (UTC-5): 09:00 is 14:00Z.
		{"the minute the morning departure is reached", "2026-10-28T13:59:00Z", "2026-10-28T14:00:00Z", "2026-10-28T14:00:00Z"},
		{"a tick after that departure was already taken", "2026-10-28T14:00:00Z", "2026-10-28T14:01:00Z", ""},
		{"two departures missed: only the latest", "2026-10-28T13:00:00Z", "2026-10-28T21:00:00Z", "2026-10-28T20:00:00Z"},
		{"yesterday's afternoon reached across midnight UTC", "2026-10-27T19:00:00Z", "2026-10-28T01:00:00Z", "2026-10-27T20:00:00Z"},
		{"before any departure today", "2026-10-28T05:00:00Z", "2026-10-28T13:00:00Z", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, due := DepartureDue(s, at(c.last), at(c.now))
			if c.want == "" {
				if due {
					t.Fatalf("DepartureDue = %s, true; want none due", got)
				}
				return
			}
			if !due || !got.Equal(at(c.want)) {
				t.Fatalf("DepartureDue = %s, %v; want %s, true", got, due, c.want)
			}
		})
	}
}

func TestBoardingOrder(t *testing.T) {
	ready := func(n int) TrainCandidate {
		return TrainCandidate{Number: n, Title: "change", HeadSHA: "abcdef1234567890", HeadRef: "claude/x", BaseRef: "main", CheckStatus: "completed", CheckConclusion: "success"}
	}
	draft := ready(9)
	draft.Draft = true
	held := ready(12)
	held.Labels = []string{"needs-review", TrainHoldLabel}
	dropped := ready(14)
	dropped.Labels = []string{TrainDroppedLabel}
	pending := ready(15)
	pending.CheckStatus, pending.CheckConclusion = "in_progress", ""
	failed := ready(16)
	failed.CheckConclusion = "failure"
	otherBase := ready(17)
	otherBase.BaseRef = "release/1.2"
	aTrain := ready(18)
	aTrain.HeadRef = TrainBranchPrefix + "2026-10-28-1"

	board, skipped := BoardingOrder("main", "MemQL / shop", []TrainCandidate{held, ready(7), draft, dropped, pending, failed, otherBase, aTrain, ready(3)}, nil)
	if got := numbersOf(board); !reflect.DeepEqual(got, []int{3, 7}) {
		t.Errorf("boarded %v, want [3 7] in pull request number order", got)
	}
	reasons := map[int]string{}
	for _, s := range skipped {
		reasons[s.Number] = s.Reason
	}
	for n, want := range map[int]string{
		9:  "draft",
		12: TrainHoldLabel,
		14: TrainDroppedLabel,
		15: "MemQL / shop has not passed on abcdef1",
		16: "MemQL / shop has not passed on abcdef1",
		17: "release/1.2",
	} {
		if !strings.Contains(reasons[n], want) {
			t.Errorf("#%d skipped for %q, want a reason naming %q", n, reasons[n], want)
		}
	}
	if _, ok := reasons[18]; ok {
		t.Errorf("a train's own pull request was skipped with a reason; it is ignored silently")
	}

	board, skipped = BoardingOrder("main", "MemQL / shop", []TrainCandidate{held, ready(7), ready(3)}, []int{12, 7})
	if got := numbersOf(board); !reflect.DeepEqual(got, []int{7}) {
		t.Errorf("an express train boarded %v, want [7]: only the named pull requests, and #12 is held", got)
	}
	if len(skipped) != 1 || skipped[0].Number != 12 {
		t.Errorf("an express train skipped %+v, want only #12", skipped)
	}
}

func TestNextPatchVersion(t *testing.T) {
	for in, want := range map[string]string{"0.31.4\n": "0.31.5", "1.2.9": "1.2.10", " 0.0.0 ": "0.0.1"} {
		got, err := NextPatchVersion(in)
		if err != nil || got != want {
			t.Errorf("NextPatchVersion(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"v0.31.4", "0.31", "0.31.x", "0.31.4-rc.1", ""} {
		if got, err := NextPatchVersion(in); err == nil {
			t.Errorf("NextPatchVersion(%q) = %q; want an error", in, got)
		}
	}
}

func TestTrainBranch(t *testing.T) {
	day := time.Date(2026, 10, 28, 23, 30, 0, 0, time.UTC)
	if got := TrainBranch(day, 2); got != "train/2026-10-28-2" {
		t.Errorf("TrainBranch = %q, want train/2026-10-28-2", got)
	}
}

func keysOf(m map[string]TrainSettings) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func numbersOf(cs []TrainCandidate) []int {
	var out []int
	for _, c := range cs {
		out = append(out, c.Number)
	}
	return out
}
