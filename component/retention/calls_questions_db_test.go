package retention

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func questionRetentionFixture(t *testing.T) (*fixture, func(string, time.Duration, map[string]any)) {
	t.Helper()
	f := newFixture(t)
	f.sweep.Concepts = memorynodes.NewRegistry(map[string]*memorynodes.Concept{callsQuestionConcept: {Name: callsQuestionConcept}})
	f.sweep.GateDB = func() *sql.DB { return f.db.DB }
	put := func(id string, age time.Duration, fields map[string]any) {
		t.Helper()
		f.concept = &memorynodes.Concept{Name: callsQuestionConcept}
		raw, err := json.Marshal(fields)
		require.NoError(t, err)
		f.insert(t, "MemoryNodes", callsQuestionConcept+":"+id, age, string(raw))
	}
	return f, put
}

func questionRetentionFields(deadline time.Time, revision int, status string) map[string]any {
	body := "Benign fixture question"
	if status == "withdrawn" || status == "dismissed" || status == "resolved" {
		body = ""
	}
	state := callrules.QuestionStoredState{Author: callrules.QuestionAuthor{Name: "Fixture author", Kind: string(callrules.KindPerson), Identity: "admission-identity"}, SeatID: "author-seat", SessionID: "author-session", JoinedAt: deadline.Add(-3 * time.Hour), SkillID: "working-session", SkillVersion: 1}
	for i := 0; i < revision; i++ {
		opStatus := "pending"
		if i == 1 {
			opStatus = "selected"
		}
		if i == revision-1 {
			opStatus = status
		}
		state.Operations = append(state.Operations, callrules.QuestionStoredOperation{Key: fmt.Sprintf("request-%d", i), Digest: strings.Repeat("b", 64), Actor: strings.Repeat("c", 64), BudgetKey: strings.Repeat("d", 64), Status: opStatus, Revision: int64(i + 1), At: deadline.Add(-2 * time.Hour).Add(time.Duration(i) * time.Second)})
	}
	data, err := json.Marshal(state)
	if err != nil {
		panic(err)
	}
	return map[string]any{
		"siteId": "site", "ownerUserId": "owner", "roomId": "v1:calls:room:room", "runId": "run",
		"authorKeyId": "v1:calls:key:author", "authorAdmission": strings.Repeat("a", 64),
		"submittedAt":  deadline.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		"roomDeadline": deadline.Add(-time.Hour).Format(time.RFC3339Nano), "expiresAt": deadline.Format(time.RFC3339Nano),
		"revision": revision, "status": status, "body": body, "stateData": string(data),
	}
}

func TestCallQuestionsPurgeAllVersionsOnlyAtImmutableExpiry(t *testing.T) {
	f, put := questionRetentionFixture(t)
	for _, id := range []string{"expired", "active"} {
		deadline := f.now
		if id == "active" {
			deadline = deadline.Add(time.Second)
		}
		put(id, time.Hour, questionRetentionFields(deadline, 1, "pending"))
		put(id, time.Minute, questionRetentionFields(deadline, 2, "selected"))
		put(id, time.Second, questionRetentionFields(deadline, 3, "resolved"))
	}
	f.concept = &memorynodes.Concept{Name: "v1:calls:wallet"}
	f.insert(t, "MemoryNodes", "v1:calls:wallet:preserved", time.Hour, `{"balanceCents":1200}`)
	ctx := context.Background()
	report, err := f.sweep.Run(ctx, true)
	require.NoError(t, err)
	require.Equal(t, 1, report.Eligible)
	require.Zero(t, report.Deleted)
	require.Equal(t, 7, f.count(t, "MemoryNodes"))
	report, err = f.sweep.Run(ctx, false)
	require.NoError(t, err)
	require.Equal(t, 3, report.Deleted)
	require.Zero(t, report.Archived)
	require.Empty(t, f.archive.objects)
	require.Empty(t, f.versionsOf(t, callsQuestionConcept+":expired"))
	require.Len(t, f.versionsOf(t, callsQuestionConcept+":active"), 3)
	require.Len(t, f.invalidated, 1)
	// The terminal body is already empty, but its earlier private versions are
	// retained until expiry. This pass never revives an earlier private body.
	var bodies []string
	require.NoError(t, f.db.NewRaw(`SELECT payload->>'body' FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt"`, callsQuestionConcept+":active").Scan(ctx, &bodies))
	require.Equal(t, []string{"Benign fixture question", "Benign fixture question", ""}, bodies)
	report, err = f.sweep.Run(ctx, false)
	require.NoError(t, err)
	require.Zero(t, report.Deleted)
	f.now = f.now.Add(time.Second)
	report, err = f.sweep.Run(ctx, false)
	require.NoError(t, err)
	require.Equal(t, 3, report.Deleted)
	require.Len(t, f.versionsOf(t, "v1:calls:wallet:preserved"), 1)
}

func TestCallQuestionsPurgePreservesAllIfOlderRowAdmissionDenied(t *testing.T) {
	f, put := questionRetentionFixture(t)
	put("denied", time.Hour, questionRetentionFields(f.now, 1, "pending"))
	put("denied", time.Second, questionRetentionFields(f.now, 2, "withdrawn"))
	f.sweep.Admit = func(_ context.Context, row memorynodes.MemoryNode) bool {
		return row.CreatedAt.After(f.now.Add(-time.Minute))
	}
	report, err := f.sweep.Run(context.Background(), false)
	require.ErrorContains(t, err, "admission refused")
	require.Zero(t, report.Deleted)
	require.Equal(t, 2, f.count(t, "MemoryNodes"))
}

func TestCallQuestionsPurgePreservesMismatchedImmutableFields(t *testing.T) {
	for _, field := range []string{"siteId", "ownerUserId", "accountId", "roomId", "runId", "authorKeyId", "authorAdmission", "submittedAt", "roomDeadline", "expiresAt"} {
		t.Run(field, func(t *testing.T) {
			f, put := questionRetentionFixture(t)
			older := questionRetentionFields(f.now, 1, "pending")
			switch field {
			case "authorAdmission":
				older[field] = strings.Repeat("b", 64)
			case "submittedAt":
				older[field] = f.now.Add(-3 * time.Hour).Format(time.RFC3339Nano)
			case "roomDeadline":
				older[field] = f.now.Add(-30 * time.Minute).Format(time.RFC3339Nano)
			case "expiresAt":
				older[field] = f.now.Add(-time.Second).Format(time.RFC3339Nano)
			default:
				older[field] = "different"
			}
			put("mismatch", time.Hour, older)
			put("mismatch", time.Second, questionRetentionFields(f.now, 2, "dismissed"))
			report, err := f.sweep.Run(context.Background(), false)
			require.ErrorContains(t, err, "binding or revision mismatch")
			require.Zero(t, report.Deleted)
			require.Equal(t, 2, f.count(t, "MemoryNodes"))
		})
	}
}

func TestCallQuestionsPurgePreservesInvalidRowsAndScrubsErrors(t *testing.T) {
	cases := map[string]func(map[string]any){
		"missing owner":         func(m map[string]any) { delete(m, "ownerUserId") },
		"malformed admission":   func(m map[string]any) { m["authorAdmission"] = "not-an-admission" },
		"invalid date":          func(m map[string]any) { m["submittedAt"] = "private-fixture-do-not-echo" },
		"deadline exceeded":     func(m map[string]any) { m["roomDeadline"] = "2000-01-01T00:00:00Z" },
		"noncanonical date":     func(m map[string]any) { m["expiresAt"] = strings.TrimSuffix(m["expiresAt"].(string), "Z") + "+00:00" },
		"overdue too long":      func(m map[string]any) { m["submittedAt"] = "2000-01-01T00:00:00Z" },
		"unknown status":        func(m map[string]any) { m["status"] = "unknown" },
		"revision zero":         func(m map[string]any) { m["revision"] = 0 },
		"revision large":        func(m map[string]any) { m["revision"] = 4 },
		"revision not integer":  func(m map[string]any) { m["revision"] = 1.5 },
		"pending revision two":  func(m map[string]any) { m["revision"] = 2 },
		"selected revision one": func(m map[string]any) { m["status"] = "selected" },
		"terminal body present": func(m map[string]any) { m["status"], m["revision"] = "resolved", 3 },
		"empty pending body":    func(m map[string]any) { m["body"] = "" },
		"oversized body":        func(m map[string]any) { m["body"] = strings.Repeat("x", 2001) },
		"metadata not object":   func(m map[string]any) { m["stateData"] = `[]` },
		"metadata null":         func(m map[string]any) { m["stateData"] = `null` },
		"oversized metadata":    func(m map[string]any) { m["stateData"] = `{"x":"` + strings.Repeat("x", 8192) + `"}` },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, put := questionRetentionFixture(t)
			fields := questionRetentionFields(f.now, 1, "pending")
			mutate(fields)
			put("invalid", time.Second, fields)
			report, err := f.sweep.Run(context.Background(), false)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private-fixture-do-not-echo")
			require.NotContains(t, err.Error(), "Benign fixture question")
			require.Zero(t, report.Deleted)
			require.Equal(t, 1, f.count(t, "MemoryNodes"))
		})
	}
}

func TestCallQuestionsPurgePreservesUnknownStorageAndRevisionInventory(t *testing.T) {
	for _, mode := range []string{"extra version", "duplicate revision", "out of order revision", "missing initial", "missing intermediate", "transition after terminal", "secret copy", "wrong type"} {
		t.Run(mode, func(t *testing.T) {
			f, put := questionRetentionFixture(t)
			put("invalid", time.Hour, questionRetentionFields(f.now, 1, "pending"))
			switch mode {
			case "extra version":
				for _, age := range []time.Duration{time.Minute, 2 * time.Second, time.Second} {
					put("invalid", age, questionRetentionFields(f.now, 2, "selected"))
				}
			case "duplicate revision":
				put("invalid", time.Second, questionRetentionFields(f.now, 1, "pending"))
			case "out of order revision":
				put("invalid", 2*time.Hour, questionRetentionFields(f.now, 2, "selected"))
			case "missing initial":
				_, err := f.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes"`)
				require.NoError(t, err)
				put("invalid", time.Second, questionRetentionFields(f.now, 2, "selected"))
			case "missing intermediate":
				put("invalid", time.Second, questionRetentionFields(f.now, 3, "resolved"))
			case "transition after terminal":
				put("invalid", time.Minute, questionRetentionFields(f.now, 2, "dismissed"))
				put("invalid", time.Second, questionRetentionFields(f.now, 3, "resolved"))
			case "secret copy":
				raw, err := json.Marshal(questionRetentionFields(f.now, 1, "pending"))
				require.NoError(t, err)
				f.insert(t, "SecretMemoryNodes", callsQuestionConcept+":invalid", time.Hour, string(raw))
			case "wrong type":
				_, err := f.db.ExecContext(context.Background(), `UPDATE "MemoryNodes" SET type='deleted'`)
				require.NoError(t, err)
			}
			count := f.count(t, "MemoryNodes")
			report, err := f.sweep.Run(context.Background(), false)
			require.Error(t, err)
			require.Zero(t, report.Deleted)
			require.Equal(t, count, f.count(t, "MemoryNodes"))
			if mode == "secret copy" {
				require.Equal(t, 1, f.count(t, "SecretMemoryNodes"))
			}
		})
	}
}

func TestCallQuestionsPurgeUsesNativeMetadataValidationAndPreservesImmutableJournal(t *testing.T) {
	cases := map[string]func(*callrules.QuestionStoredState){
		"author kind":                   func(s *callrules.QuestionStoredState) { s.Author.Kind = "unknown" },
		"author identity":               func(s *callrules.QuestionStoredState) { s.Author.Identity = "" },
		"missing budget hash":           func(s *callrules.QuestionStoredState) { s.Operations[0].BudgetKey = "" },
		"malformed digest":              func(s *callrules.QuestionStoredState) { s.Operations[0].Digest = strings.Repeat("A", 64) },
		"invalid initial transition":    func(s *callrules.QuestionStoredState) { s.Operations[0].Status = "selected" },
		"nonmonotonic operation":        func(s *callrules.QuestionStoredState) { s.Operations[2].At = s.Operations[0].At },
		"operation expiry":              func(s *callrules.QuestionStoredState) { s.Operations[2].At = s.Operations[0].At.Add(3 * time.Hour) },
		"missing operation":             func(s *callrules.QuestionStoredState) { s.Operations = s.Operations[:2] },
		"author seat changed":           func(s *callrules.QuestionStoredState) { s.SeatID = "different-seat" },
		"author session changed":        func(s *callrules.QuestionStoredState) { s.SessionID = "different-session" },
		"author identity changed":       func(s *callrules.QuestionStoredState) { s.Author.Identity = "different-identity" },
		"author name changed":           func(s *callrules.QuestionStoredState) { s.Author.Name = "Changed fixture author" },
		"author admission time changed": func(s *callrules.QuestionStoredState) { s.JoinedAt = s.JoinedAt.Add(time.Second) },
		"originating skill changed":     func(s *callrules.QuestionStoredState) { s.SkillVersion++ },
		"receipt actor changed":         func(s *callrules.QuestionStoredState) { s.Operations[0].Actor = strings.Repeat("e", 64) },
		"receipt digest changed":        func(s *callrules.QuestionStoredState) { s.Operations[0].Digest = strings.Repeat("e", 64) },
		"receipt budget changed":        func(s *callrules.QuestionStoredState) { s.Operations[0].BudgetKey = strings.Repeat("e", 64) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, put := questionRetentionFixture(t)
			put("metadata", time.Hour, questionRetentionFields(f.now, 1, "pending"))
			put("metadata", time.Minute, questionRetentionFields(f.now, 2, "selected"))
			fields := questionRetentionFields(f.now, 3, "resolved")
			var state callrules.QuestionStoredState
			require.NoError(t, json.Unmarshal([]byte(fields["stateData"].(string)), &state))
			mutate(&state)
			data, err := json.Marshal(state)
			require.NoError(t, err)
			fields["stateData"] = string(data)
			put("metadata", time.Second, fields)
			report, err := f.sweep.Run(context.Background(), false)
			require.Error(t, err)
			require.Zero(t, report.Deleted)
			require.Equal(t, 3, f.count(t, "MemoryNodes"))
		})
	}
}

func TestCallQuestionsPurgeRereadsExactSnapshotUnderRoomGate(t *testing.T) {
	f, put := questionRetentionFixture(t)
	put("gated", time.Hour, questionRetentionFields(f.now, 1, "pending"))
	ctx := context.Background()
	release, err := memql.AcquireWriteGate(ctx, f.db.DB, "calls.room.room")
	require.NoError(t, err)
	selected := make(chan struct{}, 1)
	f.sweep.Admit = func(context.Context, memorynodes.MemoryNode) bool {
		select {
		case selected <- struct{}{}:
		default:
		}
		return true
	}
	done := make(chan error, 1)
	go func() { _, err := f.sweep.Run(ctx, false); done <- err }()
	select {
	case <-selected:
	case <-time.After(3 * time.Second):
		release()
		t.Fatal("purger did not select question")
	}
	select {
	case err := <-done:
		release()
		t.Fatalf("purger bypassed room gate: %v", err)
	default:
	}
	put("gated", time.Second, questionRetentionFields(f.now, 2, "withdrawn"))
	release()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "snapshot changed")
	case <-time.After(5 * time.Second):
		t.Fatal("purger did not finish")
	}
	require.Equal(t, 2, f.count(t, "MemoryNodes"))
	report, err := f.sweep.Run(ctx, false)
	require.NoError(t, err)
	require.Equal(t, 2, report.Deleted)
}

func TestCallQuestionsPurgeTransactionRefusesNewerOrChangedVersions(t *testing.T) {
	for _, mode := range []string{"newer version", "changed metadata after read", "changed owner after read"} {
		t.Run(mode, func(t *testing.T) {
			f, put := questionRetentionFixture(t)
			put("race", time.Hour, questionRetentionFields(f.now, 1, "pending"))
			put("race", time.Second, questionRetentionFields(f.now, 2, "selected"))
			olderReads := 0
			f.sweep.Admit = func(_ context.Context, row memorynodes.MemoryNode) bool {
				if !row.CreatedAt.Equal(f.now.Add(-time.Hour)) {
					return true
				}
				olderReads++
				if mode == "newer version" && olderReads == 1 {
					put("race", 0, questionRetentionFields(f.now, 3, "resolved"))
				} else if olderReads == 2 && mode != "newer version" {
					// Mutate after the transaction has read and compared this row,
					// proving the DELETE predicate itself fences the full snapshot.
					query := `UPDATE "MemoryNodes" SET metadata='{"changed":true}' WHERE id=? AND "createdAt"=?`
					if mode == "changed owner after read" {
						query = `UPDATE "MemoryNodes" SET payload=jsonb_set(payload,'{ownerUserId}','"changed"') WHERE id=? AND "createdAt"=?`
					}
					_, err := f.db.ExecContext(context.Background(), query, row.ID, row.CreatedAt)
					require.NoError(t, err)
				}
				return true
			}
			report, err := f.sweep.Run(context.Background(), false)
			require.ErrorContains(t, err, "snapshot changed")
			require.Zero(t, report.Deleted)
			expected := 2
			if mode == "newer version" {
				expected = 3
			}
			require.Equal(t, expected, f.count(t, "MemoryNodes"))
		})
	}
}

func TestCallQuestionsPurgeRollsBackEarlierDeletesWhenLaterDeleteFails(t *testing.T) {
	f, put := questionRetentionFixture(t)
	put("rollback", time.Hour, questionRetentionFields(f.now, 1, "pending"))
	put("rollback", time.Second, questionRetentionFields(f.now, 2, "selected"))
	ctx := context.Background()
	_, err := f.db.ExecContext(ctx, `CREATE FUNCTION refuse_second_question_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF OLD.payload->>'revision'='2' THEN RAISE EXCEPTION 'private-fixture-do-not-echo'; END IF; RETURN OLD; END $$`)
	require.NoError(t, err)
	_, err = f.db.ExecContext(ctx, `CREATE TRIGGER refuse_second_question_delete BEFORE DELETE ON "MemoryNodes" FOR EACH ROW EXECUTE FUNCTION refuse_second_question_delete()`)
	require.NoError(t, err)
	report, err := f.sweep.Run(ctx, false)
	require.ErrorContains(t, err, "delete unavailable")
	require.NotContains(t, err.Error(), "private-fixture-do-not-echo")
	require.Zero(t, report.Deleted)
	require.Equal(t, 2, f.count(t, "MemoryNodes"))
	require.Empty(t, f.invalidated)
	_, err = f.db.ExecContext(ctx, `DROP TRIGGER refuse_second_question_delete ON "MemoryNodes"`)
	require.NoError(t, err)
	report, err = f.sweep.Run(ctx, false)
	require.NoError(t, err)
	require.Equal(t, 2, report.Deleted)
}

func TestCallQuestionsPurgeContinuesAfterMalformedCandidate(t *testing.T) {
	f, put := questionRetentionFixture(t)
	fields := questionRetentionFields(f.now, 1, "pending")
	fields["authorAdmission"] = "invalid"
	put("a-malformed", time.Second, fields)
	put("b-valid", time.Second, questionRetentionFields(f.now, 1, "pending"))
	report, err := f.sweep.Run(context.Background(), false)
	require.Error(t, err)
	require.Equal(t, 1, report.Deleted)
	require.Len(t, f.versionsOf(t, callsQuestionConcept+":a-malformed"), 1)
	require.Empty(t, f.versionsOf(t, callsQuestionConcept+":b-valid"))
}

func TestCallQuestionsScheduledSweepUsesPrivateRowAdmission(t *testing.T) {
	f, put := questionRetentionFixture(t)
	_, err := memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	concept, err := memorynodes.Get(callsQuestionConcept)
	require.NoError(t, err)
	require.Nil(t, concept.Retention, "custom question expiry must retain its complete bounded version inventory")
	registry := memorynodes.NewRegistry(map[string]*memorynodes.Concept{concept.Name: concept})
	put("maintenance", time.Second, questionRetentionFields(f.now.Add(-time.Second), 1, "pending"))
	integration := &Integration{context: memql.PluginContext{Concepts: registry, BunDB: func() *bun.DB { return f.db }, GateDB: func() *sql.DB { return f.db.DB }, AdmitSourceRow: memql.AdmitSourceRow, InvalidateConcept: func(string) {}}}
	maintenance := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor(auth.AutomationOriginEngine, "retireDeclaredConceptVersions"))
	nodes, err := integration.sweep(maintenance, map[string]any{}, 0)
	require.NoError(t, err)
	require.Equal(t, 1, sweepReport(t, nodes).Deleted)
	require.Zero(t, f.count(t, "MemoryNodes"))
}
