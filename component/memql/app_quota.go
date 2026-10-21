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

// AppQuotaReset parses only an explicitly zoned provider reset. A machine-local
// date without its zone is ambiguous on a cluster node and remains unknown.
func AppQuotaReset(message string, reportedAt time.Time) time.Time {
	if m := appResetISO.FindStringSubmatch(message); len(m) > 0 {
		reset, _ := time.Parse(time.RFC3339, m[1])
		return reset
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

func appQuotaUntil(row map[string]any) time.Time {
	if mapString(row, "status") != "failed" || !IsAppQuotaRefusal(mapString(row, "errorMessage")) {
		return time.Time{}
	}
	end, err := time.Parse(time.RFC3339Nano, mapString(row, "endedAt"))
	if err != nil {
		return time.Time{}
	}
	if reset := AppQuotaReset(mapString(row, "errorMessage"), end); !reset.IsZero() {
		return reset
	}
	// Unknown reset: a bounded recheck, never an invented credit balance.
	return end.Add(15 * time.Minute)
}

// Latest quota/success evidence is read independently of the rolling usage
// window. Hundreds of other calls, restarts and replica changes cannot erase it.
func (e *MemQLEngine) appBlockedUntil(ctx context.Context, registration, app string, settings map[string]any, recent []map[string]any) (time.Time, error) {
	call, _ := parser.RenderCall("appAvailabilityEvidence", map[string]any{"workerId": registration, "app": app})
	res, err := e.Execute(ContextWithFreshRead(ctx), "query "+call)
	if err != nil {
		return time.Time{}, err
	}
	rows := append([]map[string]any{}, MaterializeRows(res)...)
	// Compatibility with sessions recorded before quotaRefused was stamped.
	for _, row := range recent {
		if mapString(row, "app") == app && strings.TrimPrefix(mapString(row, "workerId"), WorkerRegistrationConcept+":") == strings.TrimPrefix(registration, WorkerRegistrationConcept+":") {
			if mapString(row, "status") == "ended" || !appQuotaUntil(row).IsZero() {
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
	return appQuotaUntil(latest), nil
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
