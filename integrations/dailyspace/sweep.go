// Daily-rollover sweep (memql#1384): the deferred half of
// rolloverAllUsers. When a user's daily space rolls over to a new date
// key, yesterday's space used to be left dangling -- participant rows
// stuck status="active", presence snapshots frozen mid-state, and a
// LiveKit room that nothing ever deleted (exactly the stale-room bait
// the pre-0.9.39 voice dispatcher wedged on). The sweep closes a
// rolled-over daily out completely:
//
//  1. end every still-active participant row (status="left" + leftAt --
//     the participant concept's terminal state; rows are never deleted,
//     a new version supersedes per the append-only model),
//  2. write a terminal idle presence snapshot for every participant
//     whose latest presence is non-idle (presence is append-only too;
//     "cleanup" = superseding the dangling state, not row deletion),
//  3. delete the polyphon-<partitionId> LiveKit room via the room-provider's
//     RoomService client (idempotent; already-gone rooms are success),
//  4. archive or save the space per the owner's
//     preferences.dailySpaceRolloverAction (default "archive"), which is
//     also the sweep's idempotency marker -- activeDailySpacesForUser
//     filters on statusIsActive + isActiveRecord, so a swept
//     space never comes back.
//
// Cluster safety mirrors how rollover itself is guarded: pure
// idempotency, no lease. createDailySpace is safe to race
// because the id is content-addressed and the insert collapses; the
// sweep is safe to race because every write is a deterministic-id
// versioned insert (racing replicas converge on the same terminal
// version) and room deletion tolerates already-gone rooms. The marker
// (step 4) is written LAST and only when steps 1-3 succeeded, so a
// partially-failed sweep stays visible to the next hourly tick and is
// retried.
//
// Deliberately NOT touched: mesh_outbox / mesh_cursor rows for the
// space's routing keys -- the #1328 retention sweep (shipped in 0.9.39)
// owns aging those out.
//
// Env-agnostic by construction: the sweep runs the same DSL mutations
// and the same RoomService deletion locally and on staging; the only
// per-environment variance is configuration (POLYPHON_LIVEKIT_* values),
// never behavior.
package dailyspace

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// spaceConceptPrefix is the canonical id prefix for v1:cognition:space
// rows. Different writers historically stored the space id in both the
// canonical and the bare-short form (participants inserted by the SPA
// carry whatever form the client held), so the sweep queries and
// deletes under both forms.
const spaceConceptPrefix = "v1:cognition:space:"

// defaultArchiveRetentionDays mirrors the v1:identity:user
// preferences.archiveRetentionDays default used by the archive flow.
const defaultArchiveRetentionDays = 30

// RoomDeleter is the narrow slice of the polyphon room provider the
// sweep needs: delete the LiveKit room backing a space. Implemented by
// polyphon.LocalRoomProvider (wired in plugin.go when LiveKit is
// configured); nil when the environment has no LiveKit, in which case
// there is no room to delete and the sweep skips step 3.
type RoomDeleter interface {
	DestroyRoom(ctx context.Context, partitionId string) error
}

// SetRoomDeleter wires the LiveKit room-deletion client. Called from
// the plug-in factory; exposed for tests.
func (d *Integration) SetRoomDeleter(rd RoomDeleter) {
	d.roomDeleter = rd
}

// sweepStats is the rollover summary's sweep section. Embedded in
// rolloverSummary so the cron automation's one-row audit entry carries
// the close-out counts alongside the ensure counts.
type sweepStats struct {
	SpacesSwept       int      `json:"spacesSwept"`
	ParticipantsEnded int      `json:"participantsEnded"`
	PresenceCleared   int      `json:"presenceCleared"`
	RoomsDeleted      int      `json:"roomsDeleted"`
	SweepErrors       int      `json:"sweepErrors"`
	SweepErrorBatch   []string `json:"sweepErrorBatch,omitempty"` // first 5 for triage
}

func (s *sweepStats) recordError(err error) {
	s.SweepErrors++
	if len(s.SweepErrorBatch) < 5 {
		s.SweepErrorBatch = append(s.SweepErrorBatch, err.Error())
	}
}

// sweepRolledOverDailies finds the user's active daily spaces whose
// dailyDateKey predates todayKey (the user's local today, computed by
// ensureForUser) and closes each one out. Spaces with an empty or
// future date key are never touched -- `<` (not `!=`) so clock skew or
// a timezone-preference change can't sweep today's or a future space.
func (d *Integration) sweepRolledOverDailies(ctx context.Context, userId, todayKey string, prefs map[string]any, stats *sweepStats) {
	if strings.TrimSpace(todayKey) == "" {
		return
	}
	rows, err := d.queryRows(ctx, "activeDailySpacesForUser", map[string]any{"userId": userId})
	if err != nil {
		stats.recordError(fmt.Errorf("sweep(%s): list daily spaces: %w", userId, err))
		return
	}
	for _, space := range rows {
		dateKey := strings.TrimSpace(stringField(space, "dailyDateKey"))
		if dateKey == "" || dateKey >= todayKey {
			continue
		}
		d.sweepSpace(ctx, userId, space, prefs, stats)
	}
}

// sweepSpace closes out one rolled-over daily space. Steps 1-3 are
// individually idempotent; step 4 (the archive/save marker) only runs
// when 1-3 succeeded so a failed sweep is retried on the next tick.
func (d *Integration) sweepSpace(ctx context.Context, userId string, space map[string]any, prefs map[string]any, stats *sweepStats) {
	spaceRowId := strings.TrimSpace(stringField(space, "id"))
	if spaceRowId == "" {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	clean := true

	// 1. End still-active participant rows (human + GA alike; the
	// system actor is elevated, so the GA-pin guard admits the
	// status="left" version).
	for _, sid := range partitionIdForms(spaceRowId) {
		parts, err := d.queryRows(ctx, "spaceParticipants", map[string]any{
			"partitionId": sid,
			"status":  "active",
		})
		if err != nil {
			stats.recordError(fmt.Errorf("sweep(%s): list participants: %w", sid, err))
			clean = false
			continue
		}
		for _, p := range parts {
			pid := strings.TrimSpace(stringField(p, "id"))
			if pid == "" {
				continue
			}
			payload := payloadFromRow(p)
			payload["status"] = "left"
			payload["leftAt"] = now
			if err := d.execMutation(ctx, "leaveSpace", map[string]any{
				"participantId": pid,
				"payload":       payload,
			}); err != nil {
				stats.recordError(fmt.Errorf("sweep(%s): end participant %s: %w", sid, pid, err))
				clean = false
				continue
			}
			stats.ParticipantsEnded++
		}
	}

	// 2. Supersede dangling presence with a terminal idle snapshot
	// (latest-wins per participantId, matching the SPA's read model).
	for _, sid := range partitionIdForms(spaceRowId) {
		rows, err := d.queryRows(ctx, "spaceParticipantPresence", map[string]any{"partitionId": sid})
		if err != nil {
			stats.recordError(fmt.Errorf("sweep(%s): list presence: %w", sid, err))
			clean = false
			continue
		}
		for _, row := range latestPresenceByParticipant(rows) {
			if strings.EqualFold(strings.TrimSpace(stringField(row, "state")), "idle") {
				continue
			}
			participantId := strings.TrimSpace(stringField(row, "participantId"))
			if participantId == "" {
				continue
			}
			args := map[string]any{
				"participantId": participantId,
				"partitionId":       sid,
				"state":         "idle",
				"label":         "Inactive",
				"reason":        "daily space rolled over",
			}
			if presenceId := strings.TrimSpace(stringField(row, "id")); presenceId != "" {
				args["presenceId"] = presenceId
			}
			if err := d.execMutation(ctx, "updateParticipantPresence", args); err != nil {
				stats.recordError(fmt.Errorf("sweep(%s): clear presence for %s: %w", sid, participantId, err))
				clean = false
				continue
			}
			stats.PresenceCleared++
		}
	}

	// 3. Delete the LiveKit room. Both id forms are attempted because
	// the room name embeds whatever space-id form the joining client
	// held; DestroyRoom tolerates rooms that never existed, so the
	// extra call is harmless. A deletion FAILURE (LiveKit reachable
	// but erroring, network fault, ...) blocks the marker so the next
	// tick retries; a nil deleter (no LiveKit in this environment)
	// means there is nothing to delete and is not a failure.
	if d.roomDeleter != nil {
		deleted := true
		for _, sid := range partitionIdForms(spaceRowId) {
			if err := d.roomDeleter.DestroyRoom(ctx, sid); err != nil {
				stats.recordError(fmt.Errorf("sweep(%s): delete LiveKit room: %w", sid, err))
				clean = false
				deleted = false
			}
		}
		if deleted {
			stats.RoomsDeleted++
		}
	}

	// 4. Archive/save marker -- LAST, and only on a clean sweep.
	if !clean {
		return
	}
	payload := payloadFromRow(space)
	// spaceFull does not project ownerUserId; re-stamp the real owner
	// (the sweep iterates per owning user, so userId IS the owner).
	payload["ownerUserId"] = userId
	payload["active"] = false
	if rolloverAction(prefs) == "save" {
		payload["status"] = "saved"
		payload["savedAt"] = now
	} else {
		payload["status"] = "archived"
		payload["archivedAt"] = now
		retention := defaultArchiveRetentionDays
		if n := intFromPrefs(prefs, "archiveRetentionDays"); n > 0 {
			retention = n
		}
		payload["expiresAt"] = time.Now().UTC().AddDate(0, 0, retention).Format(time.RFC3339)
	}
	if err := d.execMutation(ctx, "rolloverDailySpace", map[string]any{
		"partitionId": spaceRowId,
		"payload": payload,
	}); err != nil {
		stats.recordError(fmt.Errorf("sweep(%s): mark rolled over: %w", spaceRowId, err))
		return
	}
	stats.SpacesSwept++
}

// queryRows executes a named query under the system actor and returns
// the flattened rows.
func (d *Integration) queryRows(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	raw, err := d.engine.Execute(systemActorContext(ctx), fmt.Sprintf("%s(%s)", name, encodeArgs(args)))
	if err != nil {
		return nil, err
	}
	return extractRows(raw), nil
}

// execMutation executes a named mutation under the system actor.
func (d *Integration) execMutation(ctx context.Context, name string, args map[string]any) error {
	_, err := d.engine.Execute(systemActorContext(ctx), fmt.Sprintf("%s(%s)", name, encodeArgs(args)))
	return err
}

// encodeArgs renders a MemQL named-argument object literal from a Go
// map. The DSL accepts JSON-shaped object literals as the single
// positional argument to a mutation/query; json.Marshal produces
// exactly that (same approach as integrations/planner's encodeArgs),
// and it handles the nested payload objects without manual quoting.
func encodeArgs(args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil || string(b) == "null" {
		return "{}"
	}
	return string(b)
}

// payloadFromRow rebuilds a writable payload map from a flattened query
// row: every projected payload field survives, row intrinsics (id,
// concept, createdAt, ...) are stripped, and nils are dropped so the
// new version doesn't write explicit nulls for absent optionals.
func payloadFromRow(row map[string]any) map[string]any {
	intrinsics := map[string]struct{}{
		"id": {}, "concept": {}, "type": {}, "schema": {},
		"createdAt": {}, "createdBy": {}, "partition": {},
	}
	out := make(map[string]any, len(row))
	for k, v := range row {
		if _, isIntrinsic := intrinsics[k]; isIntrinsic {
			continue
		}
		if v == nil {
			continue
		}
		out[k] = v
	}
	return out
}

// partitionIdForms returns the distinct id forms a space is referenced
// under across the graph: the row id as stored plus the counterpart
// form (canonical v1:cognition:space:<short> vs bare <short>).
func partitionIdForms(spaceRowId string) []string {
	if strings.HasPrefix(spaceRowId, spaceConceptPrefix) {
		bare := strings.TrimPrefix(spaceRowId, spaceConceptPrefix)
		if bare == "" || bare == spaceRowId {
			return []string{spaceRowId}
		}
		return []string{spaceRowId, bare}
	}
	return []string{spaceRowId, spaceConceptPrefix + spaceRowId}
}

// latestPresenceByParticipant folds presence rows latest-wins per
// participantId, ordered by lastUpdatedAt (falling back to sinceAt /
// createdAt). Mirrors the SPA's read model so the sweep only
// supersedes the snapshot a viewer would actually see.
func latestPresenceByParticipant(rows []map[string]any) []map[string]any {
	latest := map[string]map[string]any{}
	for _, row := range rows {
		pid := strings.TrimSpace(stringField(row, "participantId"))
		if pid == "" {
			continue
		}
		cur, ok := latest[pid]
		if !ok || presenceOrderKey(row) >= presenceOrderKey(cur) {
			latest[pid] = row
		}
	}
	out := make([]map[string]any, 0, len(latest))
	for _, row := range latest {
		out = append(out, row)
	}
	return out
}

func presenceOrderKey(row map[string]any) string {
	for _, k := range []string{"lastUpdatedAt", "sinceAt", "createdAt"} {
		if v := strings.TrimSpace(stringField(row, k)); v != "" {
			return v
		}
	}
	return ""
}

// rolloverAction normalizes preferences.dailySpaceRolloverAction:
// "save" is honored, everything else (missing, "archive", typos)
// archives -- matching the documented default.
func rolloverAction(prefs map[string]any) string {
	if strings.EqualFold(strings.TrimSpace(asString(prefs["dailySpaceRolloverAction"])), "save") {
		return "save"
	}
	return "archive"
}

// intFromPrefs reads an int-ish preference value (JSON numbers decode
// as float64).
func intFromPrefs(prefs map[string]any, key string) int {
	switch v := prefs[key].(type) {
	case int:
		return v
	case int64:
		return clampInt64(v)
	case float64:
		if v > math.MaxInt || v < math.MinInt {
			return 0
		}
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return clampInt64(n)
		}
	}
	return 0
}

// clampInt64 narrows an int64 to int, returning 0 when the value does not
// fit the platform int. On a 64-bit build (int is int64) the guard never
// fires and this is int(v); it protects the 32-bit narrowing flagged by
// go/incorrect-integer-conversion. Preference values are small counts, so
// an out-of-range value is nonsense and the zero default -- which callers
// already treat as "unset" via their `> 0` guards -- is the safe reading.
func clampInt64(v int64) int {
	if v > math.MaxInt || v < math.MinInt {
		return 0
	}
	return int(v)
}
