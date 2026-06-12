package dailyspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// ---- fakes ---------------------------------------------------------

// fakeEngine implements the Execute slice of IntegrationEngineAccess by
// embedding the interface (other methods panic if ever called, which
// they are not on this path). It dispatches per named query/mutation
// and mutates its own state on writes so idempotency (re-run = no-op)
// is observable: ended participants leave the active set, a rolled-over
// space leaves the active-daily set.
type fakeEngine struct {
	memql.IntegrationEngineAccess

	users        []map[string]any
	dailySpaces  map[string][]map[string]any // userId -> active daily space rows
	participants map[string][]map[string]any // spaceId -> active participant rows
	presence     map[string][]map[string]any // spaceId -> presence rows

	queries        []string
	leaveCalls     []map[string]any
	presenceWrites []map[string]any
	rolloverWrites []map[string]any

	failOn func(name string, args map[string]any) error
}

func rowsResult(rows []map[string]any) *memql.ExecuteResult {
	nodes := make([]*memqlv1.MemoryNode, 0, len(rows))
	for _, row := range rows {
		id, _ := row["id"].(string)
		payload, _ := structpb.NewStruct(row)
		nodes = append(nodes, &memqlv1.MemoryNode{Id: id, Payload: payload})
	}
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}
}

// splitCall parses "name({...json...})" into name + decoded args.
func splitCall(t *testing.T, query string) (string, map[string]any) {
	t.Helper()
	open := strings.Index(query, "(")
	if open < 0 || !strings.HasSuffix(query, ")") {
		t.Fatalf("unparseable query: %s", query)
	}
	name := query[:open]
	raw := query[open+1 : len(query)-1]
	args := map[string]any{}
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			t.Fatalf("unparseable args in %s: %v", query, err)
		}
	}
	return name, args
}

func (f *fakeEngine) Execute(_ context.Context, query string) (*memql.ExecuteResult, error) {
	f.queries = append(f.queries, query)
	open := strings.Index(query, "(")
	if open < 0 {
		return nil, fmt.Errorf("fakeEngine: unparseable query %q", query)
	}
	name := query[:open]
	raw := strings.TrimSuffix(query[open+1:], ")")
	args := map[string]any{}
	if strings.TrimSpace(raw) != "" {
		// The ensure path uses fmt-built `key: "value"` literals; the
		// sweep path uses JSON. Only JSON needs decoding here.
		_ = json.Unmarshal([]byte(raw), &args)
	}
	if f.failOn != nil {
		if err := f.failOn(name, args); err != nil {
			return nil, err
		}
	}

	str := func(k string) string { s, _ := args[k].(string); return s }

	switch name {
	case "queryActiveUsers":
		return rowsResult(f.users), nil
	case "queryUserById":
		// fmt-built literal: extract the id between quotes.
		for _, u := range f.users {
			if id, _ := u["id"].(string); id != "" && strings.Contains(query, fmt.Sprintf("%q", id)) {
				return rowsResult([]map[string]any{u}), nil
			}
		}
		return rowsResult(nil), nil
	case "mutationCreateDailySpace":
		return rowsResult(nil), nil
	case "queryActiveDailySpacesForUser":
		return rowsResult(f.dailySpaces[str("userId")]), nil
	case "querySpaceParticipants":
		spaceId := str("spaceId")
		status := str("status")
		var out []map[string]any
		for _, p := range f.participants[spaceId] {
			if status == "" || p["status"] == status {
				out = append(out, p)
			}
		}
		return rowsResult(out), nil
	case "mutationLeaveSpace":
		f.leaveCalls = append(f.leaveCalls, args)
		// Apply the write: the participant's latest version is now
		// status=left, so a re-run's status:"active" query misses it.
		payload, _ := args["payload"].(map[string]any)
		pid, _ := args["participantId"].(string)
		spaceId, _ := payload["spaceId"].(string)
		for _, p := range f.participants[spaceId] {
			if p["id"] == pid {
				p["status"] = "left"
			}
		}
		return rowsResult(nil), nil
	case "querySpaceParticipantPresence":
		return rowsResult(f.presence[str("spaceId")]), nil
	case "mutationUpdateParticipantPresence":
		f.presenceWrites = append(f.presenceWrites, args)
		spaceId := str("spaceId")
		presenceId := str("presenceId")
		for _, row := range f.presence[spaceId] {
			if row["id"] == presenceId {
				row["state"] = str("state")
				row["lastUpdatedAt"] = time.Now().UTC().Format(time.RFC3339)
			}
		}
		return rowsResult(nil), nil
	case "mutationRolloverDailySpace":
		f.rolloverWrites = append(f.rolloverWrites, args)
		// Apply the write: the space leaves the active-daily set
		// (status != active drops it from queryActiveDailySpacesForUser).
		spaceId, _ := args["spaceId"].(string)
		for uid, rows := range f.dailySpaces {
			kept := rows[:0]
			for _, row := range rows {
				if row["id"] != spaceId {
					kept = append(kept, row)
				}
			}
			f.dailySpaces[uid] = kept
		}
		return rowsResult(nil), nil
	}
	return nil, fmt.Errorf("fakeEngine: unexpected query %q", query)
}

type fakeRoomDeleter struct {
	deleted []string
	err     error
}

func (f *fakeRoomDeleter) DestroyRoom(_ context.Context, spaceId string) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, spaceId)
	return nil
}

// ---- fixture -------------------------------------------------------

const (
	testUserId      = "v1:identity:user:u1"
	oldSpaceBareId  = "daily-old"
	oldSpaceRowId   = spaceConceptPrefix + oldSpaceBareId
	todaySpaceRowId = spaceConceptPrefix + "daily-today"
)

func dateKeys() (today, yesterday string) {
	now := time.Now().UTC()
	return now.Format("2006-01-02"), now.AddDate(0, 0, -1).Format("2006-01-02")
}

// newFixture builds an engine with one active user owning yesterday's
// (un-swept) daily and today's daily. Yesterday's daily carries one
// human + one GA participant (both active) and one non-idle presence
// row.
func newFixture(prefs map[string]any) *fakeEngine {
	today, yesterday := dateKeys()
	if prefs == nil {
		prefs = map[string]any{}
	}
	return &fakeEngine{
		users: []map[string]any{{
			"id":          testUserId,
			"active":      true,
			"preferences": prefs,
		}},
		dailySpaces: map[string][]map[string]any{
			testUserId: {
				{
					"id": oldSpaceRowId, "name": "Daily " + yesterday,
					"kind": "daily", "private": true, "status": "active",
					"active": true, "dailyDateKey": yesterday,
				},
				{
					"id": todaySpaceRowId, "name": "Daily " + today,
					"kind": "daily", "private": true, "status": "active",
					"active": true, "dailyDateKey": today,
				},
			},
		},
		participants: map[string][]map[string]any{
			oldSpaceRowId: {
				{
					"id": "human-1", "spaceId": oldSpaceRowId, "userId": testUserId,
					"participantType": "human", "displayName": "Jose", "status": "active",
				},
				{
					"id": "si-ga-1", "spaceId": oldSpaceRowId, "agentId": "sofia",
					"participantType": "si", "displayName": "Sofia", "status": "active",
				},
			},
		},
		presence: map[string][]map[string]any{
			oldSpaceRowId: {
				{
					"id": "presence-si-ga-1", "spaceId": oldSpaceRowId,
					"participantId": "si-ga-1", "state": "thinking",
					"label": "Thinking…", "lastUpdatedAt": "2026-06-11T00:00:00Z",
				},
			},
		},
	}
}

func runRollover(t *testing.T, eng *fakeEngine, rd RoomDeleter) rolloverSummary {
	t.Helper()
	d := NewIntegration(eng)
	d.SetRoomDeleter(rd)
	nodes, err := d.handleRolloverAllUsers(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("handleRolloverAllUsers: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 summary node, got %d", len(nodes))
	}
	var summary rolloverSummary
	if err := json.Unmarshal(nodes[0].Payload, &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	return summary
}

// ---- tests ---------------------------------------------------------

// TestSweep_ClosesOutRolledOverDaily is the happy path: yesterday's
// daily gets its participants ended, its presence superseded with a
// terminal idle snapshot, its LiveKit room deleted (both id forms),
// and the space archived (the default rollover action). Today's daily
// is untouched.
func TestSweep_ClosesOutRolledOverDaily(t *testing.T) {
	eng := newFixture(nil)
	rd := &fakeRoomDeleter{}
	summary := runRollover(t, eng, rd)

	// Participants: both rows ended with the terminal state.
	if len(eng.leaveCalls) != 2 {
		t.Fatalf("expected 2 mutationLeaveSpace calls, got %d", len(eng.leaveCalls))
	}
	for _, call := range eng.leaveCalls {
		payload, _ := call["payload"].(map[string]any)
		if payload["status"] != "left" {
			t.Errorf("participant payload status = %v, want left", payload["status"])
		}
		if s, _ := payload["leftAt"].(string); s == "" {
			t.Errorf("participant payload missing leftAt")
		}
		if payload["spaceId"] != oldSpaceRowId {
			t.Errorf("participant payload spaceId = %v, want %s", payload["spaceId"], oldSpaceRowId)
		}
	}

	// Presence: the dangling thinking snapshot is superseded by idle.
	if len(eng.presenceWrites) != 1 {
		t.Fatalf("expected 1 presence write, got %d", len(eng.presenceWrites))
	}
	pw := eng.presenceWrites[0]
	if pw["state"] != "idle" || pw["participantId"] != "si-ga-1" || pw["presenceId"] != "presence-si-ga-1" {
		t.Errorf("unexpected presence write: %v", pw)
	}

	// Room: deleted under both the canonical and the bare id form.
	wantDeleted := map[string]bool{oldSpaceRowId: true, oldSpaceBareId: true}
	if len(rd.deleted) != 2 {
		t.Fatalf("expected 2 DestroyRoom calls, got %d (%v)", len(rd.deleted), rd.deleted)
	}
	for _, sid := range rd.deleted {
		if !wantDeleted[sid] {
			t.Errorf("unexpected DestroyRoom(%q)", sid)
		}
	}

	// Space: archived with retention stamp, owner preserved.
	if len(eng.rolloverWrites) != 1 {
		t.Fatalf("expected 1 mutationRolloverDailySpace call, got %d", len(eng.rolloverWrites))
	}
	rw := eng.rolloverWrites[0]
	if rw["spaceId"] != oldSpaceRowId {
		t.Errorf("rollover spaceId = %v, want %s", rw["spaceId"], oldSpaceRowId)
	}
	payload, _ := rw["payload"].(map[string]any)
	if payload["status"] != "archived" || payload["active"] != false {
		t.Errorf("rollover payload status/active = %v/%v, want archived/false", payload["status"], payload["active"])
	}
	if payload["ownerUserId"] != testUserId {
		t.Errorf("rollover payload ownerUserId = %v, want %s (must NOT be the system actor)", payload["ownerUserId"], testUserId)
	}
	if s, _ := payload["archivedAt"].(string); s == "" {
		t.Errorf("rollover payload missing archivedAt")
	}
	if s, _ := payload["expiresAt"].(string); s == "" {
		t.Errorf("rollover payload missing expiresAt")
	}

	// Today's daily untouched.
	for _, q := range eng.queries {
		if strings.HasPrefix(q, "querySpaceParticipants") && strings.Contains(q, "daily-today") {
			t.Errorf("today's daily was swept: %s", q)
		}
	}

	// Summary counts.
	if summary.SpacesSwept != 1 || summary.ParticipantsEnded != 2 ||
		summary.PresenceCleared != 1 || summary.RoomsDeleted != 1 || summary.SweepErrors != 0 {
		t.Errorf("unexpected summary: %+v", summary)
	}
}

// TestSweep_IdempotentRerun pins the cluster-safety contract: after a
// clean sweep the space is archived (drops out of the active-daily
// query) so a re-run -- same replica next tick, or a concurrent
// replica observing the post-sweep state -- performs zero writes and
// zero room deletions.
func TestSweep_IdempotentRerun(t *testing.T) {
	eng := newFixture(nil)
	rd := &fakeRoomDeleter{}
	first := runRollover(t, eng, rd)
	if first.SpacesSwept != 1 {
		t.Fatalf("first run swept %d spaces, want 1", first.SpacesSwept)
	}

	leaves, presences, rollovers, rooms := len(eng.leaveCalls), len(eng.presenceWrites), len(eng.rolloverWrites), len(rd.deleted)
	second := runRollover(t, eng, rd)
	if second.SpacesSwept != 0 || second.ParticipantsEnded != 0 ||
		second.PresenceCleared != 0 || second.RoomsDeleted != 0 || second.SweepErrors != 0 {
		t.Errorf("re-run was not a no-op: %+v", second)
	}
	if len(eng.leaveCalls) != leaves || len(eng.presenceWrites) != presences ||
		len(eng.rolloverWrites) != rollovers || len(rd.deleted) != rooms {
		t.Errorf("re-run issued extra writes: leave %d->%d presence %d->%d rollover %d->%d rooms %d->%d",
			leaves, len(eng.leaveCalls), presences, len(eng.presenceWrites),
			rollovers, len(eng.rolloverWrites), rooms, len(rd.deleted))
	}
}

// TestSweep_SavePreference honors dailySpaceRolloverAction="save":
// the terminal version is saved (preserved indefinitely), not archived.
func TestSweep_SavePreference(t *testing.T) {
	eng := newFixture(map[string]any{"dailySpaceRolloverAction": "save"})
	runRollover(t, eng, &fakeRoomDeleter{})

	if len(eng.rolloverWrites) != 1 {
		t.Fatalf("expected 1 rollover write, got %d", len(eng.rolloverWrites))
	}
	payload, _ := eng.rolloverWrites[0]["payload"].(map[string]any)
	if payload["status"] != "saved" || payload["active"] != false {
		t.Errorf("save preference: status/active = %v/%v, want saved/false", payload["status"], payload["active"])
	}
	if s, _ := payload["savedAt"].(string); s == "" {
		t.Errorf("save preference: missing savedAt")
	}
	if _, hasExpiry := payload["expiresAt"]; hasExpiry {
		t.Errorf("save preference: saved spaces must not carry expiresAt")
	}
}

// TestSweep_RoomDeleteFailureBlocksMarker pins the retry contract: a
// failing LiveKit deletion leaves the space un-archived so the next
// tick retries, while the DB-side close-out (participants) still
// happened and is itself idempotent. Once the deleter heals, the
// retry completes the sweep.
func TestSweep_RoomDeleteFailureBlocksMarker(t *testing.T) {
	eng := newFixture(nil)
	rd := &fakeRoomDeleter{err: errors.New("livekit unreachable")}
	first := runRollover(t, eng, rd)

	if len(eng.rolloverWrites) != 0 {
		t.Fatalf("space was archived despite room-deletion failure")
	}
	if first.SpacesSwept != 0 || first.SweepErrors == 0 {
		t.Errorf("unexpected summary on failure: %+v", first)
	}
	if first.ParticipantsEnded != 2 {
		t.Errorf("participants should still be ended on room failure, got %d", first.ParticipantsEnded)
	}

	// Deleter heals -> next tick completes the close-out.
	rd.err = nil
	second := runRollover(t, eng, rd)
	if second.SpacesSwept != 1 || second.RoomsDeleted != 1 || second.SweepErrors != 0 {
		t.Errorf("retry did not complete the sweep: %+v", second)
	}
	// Participants were already ended -- the retry found none active.
	if second.ParticipantsEnded != 0 {
		t.Errorf("retry re-ended participants: %d", second.ParticipantsEnded)
	}
	if len(eng.rolloverWrites) != 1 {
		t.Errorf("expected exactly 1 rollover write after retry, got %d", len(eng.rolloverWrites))
	}
}

// TestSweep_NilRoomDeleterStillClosesOut: an environment without
// LiveKit has no room to delete; the DB-side close-out proceeds and
// the space is archived (RoomsDeleted stays 0).
func TestSweep_NilRoomDeleterStillClosesOut(t *testing.T) {
	eng := newFixture(nil)
	summary := runRollover(t, eng, nil)
	if summary.SpacesSwept != 1 || summary.RoomsDeleted != 0 || summary.SweepErrors != 0 {
		t.Errorf("unexpected summary with nil deleter: %+v", summary)
	}
	if len(eng.rolloverWrites) != 1 {
		t.Errorf("expected the space to be archived, got %d rollover writes", len(eng.rolloverWrites))
	}
}

// TestSweep_DateKeyGuards: dailies with an empty or today/future date
// key are never swept -- only strictly-prior keys roll over.
func TestSweep_DateKeyGuards(t *testing.T) {
	today, _ := dateKeys()
	future := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
	eng := newFixture(nil)
	eng.dailySpaces[testUserId] = []map[string]any{
		{"id": spaceConceptPrefix + "daily-nokey", "kind": "daily", "status": "active", "active": true, "dailyDateKey": ""},
		{"id": todaySpaceRowId, "kind": "daily", "status": "active", "active": true, "dailyDateKey": today},
		{"id": spaceConceptPrefix + "daily-future", "kind": "daily", "status": "active", "active": true, "dailyDateKey": future},
	}
	rd := &fakeRoomDeleter{}
	summary := runRollover(t, eng, rd)
	if summary.SpacesSwept != 0 || len(eng.rolloverWrites) != 0 || len(rd.deleted) != 0 {
		t.Errorf("guarded spaces were swept: %+v (rollovers=%d rooms=%v)", summary, len(eng.rolloverWrites), rd.deleted)
	}
}

// TestSweep_ParticipantEndFailureBlocksMarker: if ending a participant
// errors, the space stays un-archived so the next tick retries the
// whole close-out.
func TestSweep_ParticipantEndFailureBlocksMarker(t *testing.T) {
	eng := newFixture(nil)
	eng.failOn = func(name string, _ map[string]any) error {
		if name == "mutationLeaveSpace" {
			return errors.New("db down")
		}
		return nil
	}
	summary := runRollover(t, eng, &fakeRoomDeleter{})
	if len(eng.rolloverWrites) != 0 {
		t.Fatalf("space was archived despite participant-end failure")
	}
	if summary.SweepErrors == 0 || summary.SpacesSwept != 0 {
		t.Errorf("unexpected summary: %+v", summary)
	}
}

// TestSpaceIdForms pins the canonical/bare duality the sweep queries
// and deletes under.
func TestSpaceIdForms(t *testing.T) {
	got := spaceIdForms(oldSpaceRowId)
	if len(got) != 2 || got[0] != oldSpaceRowId || got[1] != oldSpaceBareId {
		t.Errorf("spaceIdForms(canonical) = %v", got)
	}
	got = spaceIdForms(oldSpaceBareId)
	if len(got) != 2 || got[0] != oldSpaceBareId || got[1] != oldSpaceRowId {
		t.Errorf("spaceIdForms(bare) = %v", got)
	}
}

// TestSplitCallHelper keeps the test-side parser honest about the
// query format the sweep emits.
func TestSplitCallHelper(t *testing.T) {
	name, args := splitCall(t, `mutationLeaveSpace({"participantId":"p1","payload":{"status":"left"}})`)
	if name != "mutationLeaveSpace" || args["participantId"] != "p1" {
		t.Errorf("splitCall parsed %q / %v", name, args)
	}
}
