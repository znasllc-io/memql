package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// TestWriteRealtimeSpeakingPresence_QueryParses pins the presence-write query
// the #1421 speaking signal builds: it must parse cleanly through the REAL DSL
// engine for both states, carry the mapped state string ("responding" / "idle")
// and the deterministic per-participant id. This is the non-DB guard (the
// parse, mapping, and quoting), mirroring the realParseResolver coverage in
// voice_agent_real_engine_test.go. The round-trip-with-DB guard below proves the
// row actually lands.
func TestWriteRealtimeSpeakingPresence_QueryParses(t *testing.T) {
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memqlengine.New(nil)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(concept.DefaultRegistry()))

	pid := "v1:cognition:participant:ga-xyz"

	for _, tc := range []struct {
		state string
		label string
	}{
		{"responding", "Speaking…"},
		{"idle", "Idle"},
	} {
		// Rebuild the exact query writeRealtimeSpeakingPresence emits.
		id := realtimePresenceRecordId(pid)
		require.Equal(t, "ga-xyz", id, "deterministic id is the trailing participant segment")
		now := time.Now().UTC().Format(time.RFC3339)
		payload := map[string]any{
			"spaceId": "sp1", "participantId": pid, "state": tc.state,
			"label": tc.label, "reason": "", "sinceAt": now, "lastUpdatedAt": now,
		}
		payloadJSON, _ := json.Marshal(payload)
		idJSON, _ := json.Marshal(id)
		query := fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
			mustJSONString(concept.ConceptCognitionParticipantPresence),
			string(idJSON), string(payloadJSON))

		_, perr := eng.Parse(query)
		require.NoError(t, perr, "presence write query must parse for state=%s: %s", tc.state, query)
		assert.Contains(t, query, fmt.Sprintf(`"state":%q`, tc.state), "carries the mapped state string")
	}
}

// TestHandleVoiceAgentRealtimeSpeaking_RoundTrip is the end-to-end #1421 guard:
// it drives the actual handler (which resolves the GA participant + writes
// presence through the engine) against a real engine + Postgres and reads the
// presence row back. speaking=true -> state=responding; speaking=false ->
// state=idle. Skips when no Postgres is reachable (the in-memory engine has no
// store), mirroring the #1454 full-path DB test.
func TestHandleVoiceAgentRealtimeSpeaking_RoundTrip(t *testing.T) {
	db := openVoiceScopeTestDB(t)
	defer db.Close()
	ctx := contextWithVoiceAgentActor(context.Background())

	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memqlengine.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(concept.DefaultRegistry()))

	uniq := fmt.Sprint(time.Now().UnixNano())
	spaceId := "sp-1421-" + uniq
	agentId := "v1:agents:agent:assistant-1421-" + uniq

	// Seed an active SI participant via the production mutation so the row is
	// canonicalized + partitioned exactly as the autoJoinSI automation lands
	// it -- a raw DB insert is not seen by querySiParticipantForSpace's
	// partition-scoped read.
	joinQ := fmt.Sprintf(
		`mutationJoinSpaceAsSI({spaceId: %q, agentId: %q, displayName: "Sofia", forUserId: "v1:identity:user:owner-1421"})`,
		spaceId, agentId)
	_, err = eng.Execute(ctx, joinQ)
	require.NoError(t, err, "seed SI participant via mutationJoinSpaceAsSI")

	svc := &service{engine: eng}
	sess := &streamSession{service: svc}

	// Sanity: the handler resolves the seeded participant.
	resolved := sess.resolveSIParticipantId(ctx, spaceId)
	require.NotEmpty(t, resolved, "handler resolves the seeded SI participant")
	presenceId := realtimePresenceRecordId(resolved)
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", resolved).Exec(context.Background())
	})
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).
			Where("id LIKE ?", "v1:cognition:participant:presence:"+presenceId+"%").Exec(context.Background())
	})

	// Read the GA's presence state via the same space-scoped query the
	// frontend's useParticipantPresence uses (latest-wins by participantId).
	readPresenceState := func() string {
		q := fmt.Sprintf(`querySpaceParticipantPresence({spaceId: %q})`, spaceId)
		res, qerr := eng.Execute(ctx, q)
		if qerr != nil {
			return ""
		}
		for _, row := range normalizeResultRows(res.OutputPayload()) {
			inner := row
			if p, ok := row["payload"].(map[string]any); ok {
				inner = p
			}
			if pid, _ := inner["participantId"].(string); pid != resolved {
				continue
			}
			if s, ok := inner["state"].(string); ok {
				return s
			}
		}
		return ""
	}

	// speaking=true -> responding (the orb animates).
	require.NoError(t, sess.handleVoiceAgentRealtimeSpeaking(
		&memqlv1.MemqlClientMessage{MessageId: "m1"},
		&memqlv1.VoiceAgentRealtimeSpeaking{SpaceId: spaceId, GaAgentId: "v1:agents:agent:assistant-1421", Speaking: true},
	))
	require.Eventually(t, func() bool { return readPresenceState() == "responding" },
		3*time.Second, 25*time.Millisecond, "first output audio writes presence state=responding")

	// speaking=false -> idle (the orb stops).
	require.NoError(t, sess.handleVoiceAgentRealtimeSpeaking(
		&memqlv1.MemqlClientMessage{MessageId: "m2"},
		&memqlv1.VoiceAgentRealtimeSpeaking{SpaceId: spaceId, GaAgentId: "v1:agents:agent:assistant-1421", Speaking: false},
	))
	require.Eventually(t, func() bool { return readPresenceState() == "idle" },
		3*time.Second, 25*time.Millisecond, "response.done writes presence state=idle")
}

// TestHandleVoiceAgentRealtimeSpeaking_MissingFields verifies the handler is a
// safe no-op (no panic, no error) when required attribution is missing -- it is
// fire-and-forget, so a malformed signal must never break the stream.
func TestHandleVoiceAgentRealtimeSpeaking_MissingFields(t *testing.T) {
	svc := &service{engine: &memqlengine.MemQLEngine{}}
	sess := &streamSession{service: svc}

	require.NoError(t, sess.handleVoiceAgentRealtimeSpeaking(
		&memqlv1.MemqlClientMessage{MessageId: "m1"},
		&memqlv1.VoiceAgentRealtimeSpeaking{SpaceId: "", GaAgentId: "", Speaking: true},
	))
	require.NoError(t, sess.handleVoiceAgentRealtimeSpeaking(
		&memqlv1.MemqlClientMessage{MessageId: "m2"}, nil,
	))
}
