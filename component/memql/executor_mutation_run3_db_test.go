package memql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// executor_mutation_run3_db_test.go is the real-engine reproduction + fix guard
// for the run-3 MCP QA write-path failures rolled up under memql#1672:
//
//   - #1678  read-merge not applied to touchSession / updateSessionDevices /
//            updateSessionStreams / deleteKnowledgeDomain (full-record
//            replacement wiped omitted fields / required a full payload).
//   - #1686  mutationUpdateClusterSettings wiped internalDomains when omitted.
//   - #1673  calendar create blocked: reserved field createdBy/createdAt in the
//            insert payload.
//   - #1676  mutationCreateDelegation wrote the literal string 'nil' into the
//            date-time revokedAt field.
//   - #1683  notes/todos create stamped the literal string 'null' into omitted
//            optional fields (now dispatched via typed function handler instead
//            of $args string interpolation -- see dsl/{notes,todos}/tools.memql).
//   - #1677  mutationAttachDocumentToDomain arg type (object) contradicted the
//            concept field type (array).
//
// Postgres-gated, identical harness to executor_mutation_readmerge_db_test.go:
// skips when no DB is reachable.

// runMutationRaw is runMutation without the success assertion -- used by the
// negative-path checks that want to inspect the error.
func runMutationRaw(t *testing.T, ctx context.Context, eng *MemQLEngine, name string, args map[string]any) (string, error) {
	t.Helper()
	body, err := json.Marshal(args)
	require.NoError(t, err)
	res, execErr := eng.Execute(ctx, name+"("+string(body)+")")
	if execErr != nil {
		return "", execErr
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return "", nil
	}
	return res.Bundle.Nodes[0].Id, nil
}

// TestRun3_CalendarCreate_NoReservedFields covers #1673: a calendar event must
// create without declaring the engine-reserved createdBy / createdAt fields,
// for both a timed event and an all-day event.
func TestRun3_CalendarCreate_NoReservedFields(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:calendar:calendarEvent"

	// Timed event, allDay omitted entirely (mutation coalesces to false).
	timedId := "cal-" + uniqueSuffix("timed")
	gotTimed := runMutation(t, ctx, eng, "mutationCreateCalendarEvent", map[string]any{
		"eventId":  timedId,
		"title":    "Dentist",
		"startsAt": "2026-07-01T09:00:00Z",
	})
	pTimed := latestPayload(t, ctx, db, conceptName, gotTimed)
	require.Equal(t, false, pTimed["allDay"], "omitted allDay must coalesce to bool false (#1673)")
	require.Equal(t, "native", pTimed["source"])
	// createdBy is an engine-stamped ROW column, never a payload field -- the
	// whole point of #1673 is that the mutation must NOT declare it in the
	// payload. Its absence from the payload here confirms the fix.
	_, inPayload := pTimed["createdBy"]
	require.False(t, inPayload, "createdBy must not be a payload field (#1673 reserved field)")

	// All-day event, allDay an explicit bool true.
	allDayId := "cal-" + uniqueSuffix("allday")
	gotAllDay := runMutation(t, ctx, eng, "mutationCreateCalendarEvent", map[string]any{
		"eventId":  allDayId,
		"title":    "Birthday",
		"startsAt": "2026-07-02T00:00:00Z",
		"allDay":   true,
	})
	pAllDay := latestPayload(t, ctx, db, conceptName, gotAllDay)
	require.Equal(t, true, pAllDay["allDay"], "explicit allDay=true must persist as bool true")
}

// TestRun3_CreateDelegation_NoLiteralNil covers #1676: creating a delegation
// without revokedAt must succeed (no literal 'nil' written into the date-time
// field) and leave revokedAt absent.
func TestRun3_CreateDelegation_NoLiteralNil(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:identity:delegation"

	delegationId := "deleg-" + uniqueSuffix("create")
	got := runMutation(t, ctx, eng, "mutationCreateDelegation", map[string]any{
		"delegationId":     delegationId,
		"identityId":       "ident-" + uniqueSuffix("d"),
		"identitySubject":  "user:deleg-test",
		"identityType":     "human",
		"agentId":          "agent-deleg-test",
		"roleCeiling":      "writer",
		"scopes":           []string{"read", "write"},
		"createdBySubject": "user:deleg-test",
	})
	p := latestPayload(t, ctx, db, conceptName, got)
	require.Equal(t, true, p["active"], "fresh delegation must be active")
	if v, ok := p["revokedAt"]; ok {
		require.NotEqual(t, "nil", v, "revokedAt must never be the literal string 'nil' (#1676)")
		require.Empty(t, v, "revokedAt should be absent/empty on a fresh delegation")
	}
}

// TestRun3_NotesCreate_OmittedTitle covers #1683: creating a note without a
// title must succeed and must not stamp the literal string "null" into title.
func TestRun3_NotesCreate_OmittedTitle(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:notes:note"

	noteId := "note-" + uniqueSuffix("notitle")
	got := runMutation(t, ctx, eng, "mutationCreateNote", map[string]any{
		"noteId": noteId,
		"body":   "remember the milk",
	})
	p := latestPayload(t, ctx, db, conceptName, got)
	if v, ok := p["title"]; ok {
		require.NotEqual(t, "null", v, "omitted title must not become the literal string 'null' (#1683)")
	}
	require.Equal(t, "remember the milk", p["body"])
}

// TestRun3_TodosCreate_OmittedDueAt covers #1683: creating a to-do without a
// dueAt must succeed and must not stamp the literal string "null" into the
// date-time dueAt field.
func TestRun3_TodosCreate_OmittedDueAt(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:todos:todo"

	todoId := "todo-" + uniqueSuffix("nodue")
	got := runMutation(t, ctx, eng, "mutationCreateTodo", map[string]any{
		"todoId": todoId,
		"title":  "ship the fix",
	})
	p := latestPayload(t, ctx, db, conceptName, got)
	if v, ok := p["dueAt"]; ok {
		require.NotEqual(t, "null", v, "omitted dueAt must not become the literal string 'null' (#1683)")
	}
	require.Equal(t, false, p["done"])
}

// TestRun3_TouchSession_ReadMerge covers #1678: a minimal touchSession (id +
// the one changed field) must succeed and preserve the omitted required
// discriminators (subject / tokenHash / source / expiresAt).
func TestRun3_TouchSession_ReadMerge(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:identity:authSession"

	sessionId := "sess-" + uniqueSuffix("touch")
	// Seed a full session row first. Reuse the canonical stored id the engine
	// returns for both the touch arg and the read-back (ids may be canonicalized).
	storedId := runMutation(t, ctx, eng, "mutationCreateAuthSession", map[string]any{
		"sessionId": sessionId,
		"subject":   "user:touch-test",
		"tokenHash": "hash-touch-1",
		"source":    "bff_exchange",
		"expiresAt": "2026-12-31T00:00:00Z",
	})
	before := latestPayload(t, ctx, db, conceptName, storedId)

	// MINIMAL touch: only the changed field in payload.
	runMutation(t, ctx, eng, "mutationTouchSession", map[string]any{
		"sessionId": storedId,
		"payload":   map[string]any{"lastActivityAt": "2026-06-18T12:00:00Z"},
	})
	after := latestPayload(t, ctx, db, conceptName, storedId)

	require.Equal(t, "2026-06-18T12:00:00Z", after["lastActivityAt"], "touch must apply the changed field")
	require.Equal(t, before["subject"], after["subject"], "subject preserved (#1678)")
	require.Equal(t, before["tokenHash"], after["tokenHash"], "tokenHash preserved (#1678)")
	require.Equal(t, before["source"], after["source"], "source preserved (#1678)")
	require.Equal(t, before["expiresAt"], after["expiresAt"], "expiresAt preserved (#1678)")
}

// TestRun3_DeleteKnowledgeDomain_ReadMerge covers #1678: a minimal
// deleteKnowledgeDomain (id only) must flip active=false while preserving every
// other field (name / description / category / source / scope / tier / ...).
func TestRun3_DeleteKnowledgeDomain_ReadMerge(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:common:knowledgeDomain"

	domainId := "kd-" + uniqueSuffix("del")
	// Seed via the create mutation (full row). Reuse the canonical stored id.
	storedId := runMutation(t, ctx, eng, "mutationCreateKnowledgeDomain", map[string]any{
		"domainId":    domainId,
		"name":        "Tax Law",
		"description": "US federal tax statutes",
		"category":    "business",
	})
	before := latestPayload(t, ctx, db, conceptName, storedId)

	// MINIMAL delete: id only.
	runMutation(t, ctx, eng, "mutationDeleteKnowledgeDomain", map[string]any{
		"domainId": storedId,
	})
	after := latestPayload(t, ctx, db, conceptName, storedId)

	require.Equal(t, false, after["active"], "delete must flip active=false")
	require.Equal(t, before["name"], after["name"], "name preserved (#1678)")
	require.Equal(t, before["description"], after["description"], "description preserved (#1678)")
	require.Equal(t, before["category"], after["category"], "category preserved (#1678)")
}

// TestRun3_UpdateClusterSettings_PreservesInternalDomains covers #1686: a
// partial update that omits internalDomains must NOT wipe it.
func TestRun3_UpdateClusterSettings_PreservesInternalDomains(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:identity:clusterSettings"
	const settingsId = "cluster"

	// Seed the singleton with a non-empty internalDomains.
	storedId := runMutation(t, ctx, eng, "mutationCreateClusterSettings", map[string]any{
		"id":                  settingsId,
		"registrationMode":    "open",
		"internalDefaultRole": "reader",
		"internalDomains":     "znas.io,example.com",
	})
	before := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, "znas.io,example.com", before["internalDomains"])

	// Partial update: change brandName ONLY, omit internalDomains.
	runMutation(t, ctx, eng, "mutationUpdateClusterSettings", map[string]any{
		"id":                  storedId,
		"registrationMode":    "open",
		"internalDefaultRole": "reader",
		"brandName":           "Acme",
	})
	after := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, "Acme", after["brandName"], "brandName must update")
	require.Equal(t, "znas.io,example.com", after["internalDomains"],
		"omitted internalDomains must be preserved, not wiped (#1686)")
}

// TestRun3_AttachDocumentToDomain_ArrayArg covers #1677: the attachedDomains
// arg now matches the concept's array type, so an array value validates.
func TestRun3_AttachDocumentToDomain_ArrayArg(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:knowledge:document"

	docId := "doc-" + uniqueSuffix("attach")
	// Seed a document. Reuse the canonical stored id for the attach + read-back.
	storedId := runMutation(t, ctx, eng, "mutationCreateDocument", map[string]any{
		"documentId":   docId,
		"attachmentId": "att-1",
		"planId":       "plan-1",
		"spaceId":      "space-1",
		"fileName":     "data.csv",
		"mimeType":     "text/csv",
		"format":       "spreadsheet",
		"uploadedBy":   "user:attach-test",
	})

	// Array form must now validate (previously: object arg vs array concept field).
	got, err := runMutationRaw(t, ctx, eng, "mutationAttachDocumentToDomain", map[string]any{
		"documentId":      storedId,
		"attachedDomains": []string{"kd-finance", "kd-legal"},
	})
	require.NoError(t, err, "array attachedDomains must validate against the array concept field (#1677)")
	require.NotEmpty(t, got)

	p := latestPayload(t, ctx, db, conceptName, storedId)
	domains, ok := p["attachedDomains"].([]any)
	require.True(t, ok, "attachedDomains must persist as an array")
	require.Len(t, domains, 2)
}
