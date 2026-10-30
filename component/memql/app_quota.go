package memql

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
)

var appResetDate = regexp.MustCompile(`(?i)resets? ([a-z]{3} [0-9]{1,2}) at ([0-9]{1,2}(?::[0-9]{2})?\s*[ap]m) \(([A-Za-z_]+/[A-Za-z_/]+)\)`)
var appResetISO = regexp.MustCompile(`(?i)(?:resets?(?: at)?|try again at) (\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:Z|[+-]\d{2}:\d{2}))`)
var appResetUnzoned = regexp.MustCompile(`(?i)try again at ([a-z]{3}) ([0-9]{1,2})(?:st|nd|rd|th)?, ([0-9]{4}) ([0-9]{1,2}:[0-9]{2}) ?([ap]m)`)

// appResetEarliestZone is the easternmost UTC offset any zone uses: a local
// date read at UTC minus this is the earliest instant it can mean.
const appResetEarliestZone = 14 * time.Hour

// appRefusalRecheck bounds how long a refusal with no known end blocks an app:
// a recheck, never an invented credit balance or a claim about a login.
const appRefusalRecheck = 15 * time.Minute

// AppQuotaReset reads when a provider says a usage limit resets, from its own
// sentence. Zero means it did not say anything this can read.
//
// A ZONED RESET IS TAKEN AS WRITTEN. Codex's "try again at Nov 3rd, 2026
// 11:52 PM" names no zone: it is the machine's, which a cluster node does not
// know. Unknown was the first answer here, and it blocked an app out of credits
// for days for only appRefusalRecheck, so every call met the refusal again. So
// the date is read as the EARLIEST instant it can mean. The block can then end
// up to a day before the real reset and never after it, and what the early end
// costs is one more zero-token refusal, which appRefusalUntil rechecks. A
// reset measured by the machine's own probe has no such gap.
func AppQuotaReset(message string, reportedAt time.Time) time.Time {
	if m := appResetISO.FindStringSubmatch(message); len(m) > 0 {
		reset, _ := time.Parse(time.RFC3339, m[1])
		return reset
	}
	if m := appResetUnzoned.FindStringSubmatch(message); len(m) > 0 {
		local, err := time.ParseInLocation("2006 Jan 2 3:04pm", m[3]+" "+m[1]+" "+m[2]+" "+m[4]+strings.ToLower(m[5]), time.UTC)
		if err != nil {
			return time.Time{}
		}
		return local.Add(-appResetEarliestZone)
	}
	m := appResetDate.FindStringSubmatch(message)
	if len(m) == 0 {
		return time.Time{}
	}
	zone, err := time.LoadLocation(m[3])
	if err != nil {
		return time.Time{}
	}
	local := reportedAt.In(zone)
	clock := strings.ToLower(strings.ReplaceAll(m[2], " ", ""))
	layout := "2006 Jan 2 3pm"
	if strings.Contains(clock, ":") {
		layout = "2006 Jan 2 3:04pm"
	}
	reset, err := time.ParseInLocation(layout, strconv.Itoa(local.Year())+" "+m[1]+" "+clock, zone)
	if err != nil {
		return time.Time{}
	}
	if local.Month() == time.December && reset.Month() == time.January {
		reset = reset.AddDate(1, 0, 0)
	}
	return reset
}

// appRefusalUntil is when a failed session's refusal stops blocking its app on
// its machine, or zero for a row that is not a refusal.
//
// This is the clock a machine that measures nothing is left with (appBlockedUntil).
//
// A LOGIN HAS NO RESET, so it takes the bounded recheck: a sign-in in a terminal
// writes no row here, and the next call after the window is what finds it. A
// reset the sentence names that has ALREADY PASSED takes the recheck too, because
// the app just refused anyway: appResetEarliestZone can read a reset up to a day
// early, and no block at all would have every call meet the refusal again.
func appRefusalUntil(row map[string]any) time.Time {
	message := mapString(row, "errorMessage")
	quota, signedOut := IsAppQuotaRefusal(message), IsAppAuthRefusal(message)
	if mapString(row, "status") != "failed" || (!quota && !signedOut) {
		return time.Time{}
	}
	end, err := time.Parse(time.RFC3339Nano, mapString(row, "endedAt"))
	if err != nil {
		return time.Time{}
	}
	if quota {
		if reset := AppQuotaReset(message, end); reset.After(end) {
			return reset
		}
	}
	return end.Add(appRefusalRecheck)
}

// appBlockedUntilMeasured is the end of a refusal on a machine that measures: no
// time at all, because its next check says when it is over (appBlockedUntil).
var appBlockedUntilMeasured = time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC)

// Latest quota/success evidence is read independently of the rolling usage
// window. Hundreds of other calls, restarts and replica changes cannot erase it.
//
// ON A MACHINE THAT MEASURES, THE CHECK ENDS A REFUSAL AND THE CLOCK DOES NOT. The
// owner: "we should keep a state and an interval checking for it until it becomes
// available, then it can be used again." A reset passing, or the fifteen-minute
// recheck, would otherwise send a person's real call to find out, for an answer the
// machine reports anyway. So the block holds until a "ready" measured after the
// refusal. If those checks stop, the app stays out, and a machine that went quiet is
// offline anyway. Tests: TestAppRefusalDBOutlivesItsResetUntilTheMachineSaysSo,
// TestAppRefusalDBClearsOnANewerMeasurement.
//
// A Cockpit that predates the checks has only the clock (appRefusalUntil), so on it a
// real call still finds out, and a refusal then moves on to the next app.
func (e *MemQLEngine) appBlockedUntil(ctx context.Context, registration, app string, settings map[string]any, recent []map[string]any, measures bool, measuredReadyAt time.Time) (time.Time, error) {
	call, _ := parser.RenderCall("appAvailabilityEvidence", map[string]any{"workerId": registration, "app": app})
	res, err := e.Execute(ContextWithFreshRead(ctx), "query "+call)
	if err != nil {
		return time.Time{}, err
	}
	rows := append([]map[string]any{}, MaterializeRows(res)...)
	// Compatibility with sessions recorded before quotaRefused and authRefused
	// were stamped.
	for _, row := range recent {
		if mapString(row, "app") == app && strings.TrimPrefix(mapString(row, "workerId"), WorkerRegistrationConcept+":") == strings.TrimPrefix(registration, WorkerRegistrationConcept+":") {
			if mapString(row, "status") == "ended" || !appRefusalUntil(row).IsZero() {
				rows = append(rows, row)
			}
		}
	}
	var latest map[string]any
	var latestAt time.Time
	for _, row := range rows {
		at, _ := time.Parse(time.RFC3339Nano, mapString(row, "endedAt"))
		if at.After(latestAt) {
			latest, latestAt = row, at
		}
	}
	retries, _ := settings["availabilityRetriedAt"].(map[string]any)
	retry, _ := time.Parse(time.RFC3339Nano, mapString(retries, app))
	if !retry.IsZero() && !retry.Before(latestAt) {
		return time.Time{}, nil
	}
	if !measuredReadyAt.IsZero() && !measuredReadyAt.Before(latestAt) {
		return time.Time{}, nil
	}
	until := appRefusalUntil(latest)
	if measures && !until.IsZero() {
		return appBlockedUntilMeasured, nil
	}
	return until, nil
}

func (e *MemQLEngine) evaluateFleetRetryAppAvailability(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	registration, app := stringArg(args, "registrationId"), stringArg(args, "app")
	if app != "claude-code" && app != "codex" {
		return nil, fmt.Errorf("choose a supported app")
	}
	if _, err := e.modelSetupRow(ctx, registration); err != nil {
		return nil, err
	}
	ctx, err := e.fenceWriteTarget(ctx, WorkerRegistrationConcept, registration)
	if err != nil {
		return nil, err
	}
	row, err := e.modelSetupRow(ContextWithFreshRead(ctx), registration)
	if err != nil {
		return nil, err
	}
	if mapString(row, "revokedAt") != "" {
		return nil, fmt.Errorf("pair this machine again before retrying its apps")
	}
	settings, _ := row["appRouting"].(map[string]any)
	next := map[string]any{}
	for key, value := range settings {
		next[key] = value
	}
	retries := map[string]any{}
	previous, _ := settings["availabilityRetriedAt"].(map[string]any)
	for key, value := range previous {
		retries[key] = value
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	retries[app] = at
	next["availabilityRetriedAt"] = retries
	call, _ := parser.RenderCall("setWorkerAppRouting", map[string]any{"registrationId": registration, "settings": next})
	if _, err := e.Execute(auth.ContextWithInternalOrigin(ctx), call); err != nil {
		return nil, err
	}
	return singleVirtualRow("v1:worker:appRoutingResult", registration, map[string]any{"app": app, "retriedAt": at})
}
