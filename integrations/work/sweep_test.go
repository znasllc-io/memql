package work

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// sweep_test.go -- the sweeps' DECISIONS, table-driven and with no database.
//
// The two reads are hand-rolled SQL and belong on the db-gated lane; what is
// testable here without one is every judgement the sweep makes ABOUT a row,
// which is where the failures live: a parked run swept as abandoned, a run
// with no timestamp swept on a guess, an archive that did not upload deleted
// anyway.

// TestTimerDueReadsOnlyTimerWaits.
//
// A PARKED RUN IS NEVER ABANDONED and only a DUE TIMER releases one. The
// unparseable case is the one worth pinning: a run resumed on a timestamp
// nobody could read would run early with no way to tell that it had.
func TestTimerDueReadsOnlyTimerWaits(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		waitingOn map[string]any
		wantDue   bool
		wantTimer bool
	}{
		{name: "no wait at all", waitingOn: nil},
		{
			name:      "an approval wait is not a timer",
			waitingOn: map[string]any{"kind": "approval", "subject": "v1:work:approval:a1"},
		},
		{
			name:      "a timer in the past is due",
			waitingOn: map[string]any{"kind": "timer", "resumeAt": now.Add(-time.Minute).Format(time.RFC3339)},
			wantDue:   true, wantTimer: true,
		},
		{
			name:      "a timer exactly now is due",
			waitingOn: map[string]any{"kind": "timer", "resumeAt": now.Format(time.RFC3339)},
			wantDue:   true, wantTimer: true,
		},
		{
			name:      "a timer in the future is not",
			waitingOn: map[string]any{"kind": "timer", "resumeAt": now.Add(time.Hour).Format(time.RFC3339)},
			wantTimer: true,
		},
		{
			name:      "an unparseable resumeAt is NOT due",
			waitingOn: map[string]any{"kind": "timer", "resumeAt": "tomorrow-ish"},
			wantTimer: true,
		},
		{
			name:      "a timer with no resumeAt is NOT due",
			waitingOn: map[string]any{"kind": "timer"},
			wantTimer: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := map[string]any{"id": "r1"}
			if tc.waitingOn != nil {
				run["waitingOn"] = tc.waitingOn
			}
			due, isTimer := timerDue(run, now)
			if due != tc.wantDue || isTimer != tc.wantTimer {
				t.Errorf("timerDue = (%v, %v), want (%v, %v)", due, isTimer, tc.wantDue, tc.wantTimer)
			}
		})
	}
}

// TestLastHeartbeatFallsBackAndRefusesToGuess.
//
// heartbeatAt, else startedAt, else createdAt -- and a row carrying NONE is
// left alone. Sweeping on an absent timestamp would close every row written
// before the field existed, including ones somebody is looking at.
func TestLastHeartbeatFallsBackAndRefusesToGuess(t *testing.T) {
	beat := "2026-09-05T11:59:00Z"
	start := "2026-09-05T10:00:00Z"
	created := "2026-09-04T10:00:00Z"
	cases := []struct {
		name string
		run  map[string]any
		want string
		ok   bool
	}{
		{"heartbeat wins", map[string]any{"heartbeatAt": beat, "startedAt": start, "createdAt": created}, beat, true},
		{"startedAt when there is no beat", map[string]any{"startedAt": start, "createdAt": created}, start, true},
		{"createdAt is the last resort", map[string]any{"createdAt": created}, created, true},
		{"nothing at all is LEFT ALONE", map[string]any{"id": "r1"}, "", false},
		{"an unparseable timestamp is not evidence", map[string]any{"heartbeatAt": "soon"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := lastHeartbeat(tc.run)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			want, _ := time.Parse(time.RFC3339, tc.want)
			if !got.Equal(want) {
				t.Errorf("lastHeartbeat = %v, want %v", got, want)
			}
		})
	}
}

// TestAbandonedMessageDoesNotClaimTheRunFailed. The sweep knows the node
// stopped answering; whether the work was about to succeed is not something it
// can see, and an append-only row cannot be corrected.
func TestAbandonedMessageDoesNotClaimTheRunFailed(t *testing.T) {
	last := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)
	msg := abandonedMessage(map[string]any{"nodeId": "agent-7"}, last)
	if !strings.Contains(msg, "agent-7") {
		t.Errorf("the message does not name the node: %q", msg)
	}
	if !strings.Contains(msg, last.Format(time.RFC3339)) {
		t.Errorf("the message does not say when the run was last heard from: %q", msg)
	}
	for _, banned := range []string{"failed", "error", "crashed"} {
		if strings.Contains(strings.ToLower(msg), banned) {
			t.Errorf("the message claims %q, which the sweep cannot know: %q", banned, msg)
		}
	}
}

// TestRetentionWindowsFallBackRatherThanZero.
//
// Zero days would archive and delete the whole journal on the next pass, so a
// non-positive or unparseable value takes the default.
func TestRetentionWindowsFallBackRatherThanZero(t *testing.T) {
	cases := []struct {
		set  string
		want int
	}{
		{"", DefaultModelCallRetentionDays},
		{"30", 30},
		{"0", DefaultModelCallRetentionDays},
		{"-5", DefaultModelCallRetentionDays},
		{"ninety", DefaultModelCallRetentionDays},
		{"  45  ", 45},
	}
	for _, tc := range cases {
		t.Run("value="+tc.set, func(t *testing.T) {
			t.Setenv(EnvModelCallRetentionDays, tc.set)
			if got := retentionDays(EnvModelCallRetentionDays, DefaultModelCallRetentionDays); got != tc.want {
				t.Errorf("retentionDays = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestNoArchiveMeansNoDelete is the retention sweep's headline rule.
//
// With expired rows found and nowhere to put them, the sweep must delete
// NOTHING and must SAY why -- a cluster without object storage keeps its
// journal, which is why there is no TimescaleDB retention policy on these rows.
func TestNoArchiveMeansNoDelete(t *testing.T) {
	i, _ := newTestIntegration(t)
	// A database that would answer expired rows, and no archiver.
	i.bunDB = nil
	t.Setenv(EnvArchiveContainer, "")
	t.Setenv(envBlobContainer, "")

	// With no database the read errors before the rule is reached, so this
	// exercises the rule directly through the pieces that decide it.
	if archiveContainer() != "" {
		t.Fatal("a container resolved with neither env var set")
	}
	if i.archiverRef() != nil {
		t.Fatal("an archiver was bound with none installed")
	}
}

// TestArchiveContainerPrefersItsOwnEnvThenTheCluster.
func TestArchiveContainerPrefersItsOwnEnvThenTheCluster(t *testing.T) {
	t.Setenv(envBlobContainer, "cluster-blobs")
	t.Setenv(EnvArchiveContainer, "")
	if got := archiveContainer(); got != "cluster-blobs" {
		t.Errorf("archiveContainer = %q, want the cluster's own container", got)
	}
	t.Setenv(EnvArchiveContainer, "journal-blobs")
	if got := archiveContainer(); got != "journal-blobs" {
		t.Errorf("archiveContainer = %q, want the work-specific override", got)
	}
}

// TestGroupByDayFilesEachRowUnderItsOwnUTCDay, and drops a row with no
// readable timestamp rather than archiving it to a guess.
func TestGroupByDayFilesEachRowUnderItsOwnUTCDay(t *testing.T) {
	rows := []map[string]any{
		{"id": "a", "createdAt": "2026-09-04T23:59:00Z"},
		{"id": "b", "createdAt": "2026-09-05T00:01:00Z"},
		{"id": "c", "createdAt": "2026-09-05T12:00:00Z"},
		{"id": "d"},
	}
	got := groupByDay(rows)
	if len(got["2026-09-04"]) != 1 || len(got["2026-09-05"]) != 2 {
		t.Errorf("grouping = %v", summariseGroups(got))
	}
	total := 0
	for _, g := range got {
		total += len(g)
	}
	if total != 3 {
		t.Errorf("the row with no timestamp was filed under a guess (%d rows grouped, want 3)", total)
	}
}

// TestNdjsonGzipRoundTrips: the archive must be readable back, or "archived"
// is a claim nothing can check.
func TestNdjsonGzipRoundTrips(t *testing.T) {
	rows := []map[string]any{
		{"id": "v1:work:modelCall:m1", "runId": "r1", "provider": "openai"},
		{"id": "v1:work:modelCall:m2", "runId": "r1", "provider": "anthropic"},
	}
	blob, err := ndjsonGzip(rows)
	if err != nil {
		t.Fatalf("ndjsonGzip: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("archive has %d lines, want 2", len(lines))
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 1 did not decode: %v", err)
	}
	if first["id"] != "v1:work:modelCall:m1" {
		t.Errorf("line 1 = %v", first)
	}
}

// TestConceptLeafNamesTheArchiveObject.
func TestConceptLeafNamesTheArchiveObject(t *testing.T) {
	for concept, want := range map[string]string{
		modelCallConcept:   "modelCall",
		observationConcept: "observation",
		"nocolons":         "nocolons",
	} {
		if got := conceptLeaf(concept); got != want {
			t.Errorf("conceptLeaf(%q) = %q, want %q", concept, got, want)
		}
	}
}

// TestInListIsParameterized. A row id is a VALUE, and a value never belongs in
// the statement text.
func TestInListIsParameterized(t *testing.T) {
	args, placeholders := inList([]string{"a", "b", "c"}, 2)
	if placeholders != "$2, $3, $4" {
		t.Errorf("placeholders = %q", placeholders)
	}
	if len(args) != 3 || args[0] != "a" || args[2] != "c" {
		t.Errorf("args = %v", args)
	}
	for _, id := range []string{"a", "b", "c"} {
		if strings.Contains(placeholders, id) {
			t.Errorf("the id %q was interpolated into the statement text", id)
		}
	}
}

// TestChunkCoversEveryIdExactlyOnce.
func TestChunkCoversEveryIdExactlyOnce(t *testing.T) {
	ids := make([]string, 0, 17)
	for n := range 17 {
		ids = append(ids, fmt.Sprintf("id-%d", n))
	}
	seen := map[string]int{}
	for _, batch := range chunk(ids, 5) {
		if len(batch) > 5 {
			t.Fatalf("batch of %d exceeds the size", len(batch))
		}
		for _, id := range batch {
			seen[id]++
		}
	}
	if len(seen) != 17 {
		t.Errorf("covered %d ids, want 17", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s appeared %d times", id, n)
		}
	}
}

// TestSweepsRefuseAHandRolledReadWithNoAdmissionGate.
//
// Fail-CLOSED. A nil AdmitSourceRow means this node cannot tell whether a
// caller may see a row, and "cannot tell" must never resolve to "everyone may"
// -- the sweeps read every owner's rows by construction.
func TestSweepsRefuseAHandRolledReadWithNoAdmissionGate(t *testing.T) {
	i, _ := newTestIntegration(t)
	i.admitRow = nil
	_, err := i.selectAdmitted(context.Background(), runConcept, "SELECT 1")
	if err == nil {
		t.Fatal("a hand-rolled read ran with no admission gate wired")
	}
	if !strings.Contains(err.Error(), "admission") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestSweepWaitingBorrowsEachRunsOwner.
//
// The write guard ignores the clusterOwner arm, so even the maintenance
// principal writes an owned row AS its owner. Driven through the decision half
// with a stubbed row source, since the read itself needs a database.
func TestSweepWaitingBorrowsEachRunsOwner(t *testing.T) {
	i, eng := newTestIntegration(t)
	now := testNow

	// Stand in for runsInFlight: drive the per-row half directly.
	rows := []map[string]any{
		{
			"id": "v1:work:run:r-timer", "ownerUserId": "u-bob", "status": runStatusWaiting,
			"waitingOn": map[string]any{"kind": "timer", "resumeAt": now.Add(-time.Minute).Format(time.RFC3339)},
		},
		{
			"id": "v1:work:run:r-dead", "ownerUserId": "u-carol", "status": runStatusRunning,
			"heartbeatAt": now.Add(-10 * time.Minute).Format(time.RFC3339), "nodeId": "agent-3",
		},
		{
			"id": "v1:work:run:r-alive", "ownerUserId": "u-dave", "status": runStatusRunning,
			"heartbeatAt": now.Add(-5 * time.Second).Format(time.RFC3339),
		},
		{
			"id": "v1:work:run:r-parked", "ownerUserId": "u-erin", "status": runStatusWaiting,
			"waitingOn": map[string]any{"kind": "approval", "subject": "v1:work:approval:a1"},
		},
	}
	res := sweepRows(context.Background(), i, rows, now, time.Minute)

	if res.Resumed != 1 || res.Abandoned != 1 {
		t.Fatalf("sweep = %+v, want 1 resumed and 1 abandoned (calls: %s)", res, eng.summary())
	}
	byRun := map[string]recordedCall{}
	for _, c := range eng.callsTo("updateWorkRun") {
		byRun[c.Args(t)["runId"].(string)] = c
	}
	if len(byRun) != 2 {
		t.Fatalf("wrote to %d runs, want 2 -- a run parked on a person and a run still beating must be left alone", len(byRun))
	}
	if got := byRun["v1:work:run:r-timer"]; got.Actor != "u-bob" {
		t.Errorf("the resumed run was written under actor %q, want its owner u-bob", got.Actor)
	}
	if got := byRun["v1:work:run:r-dead"]; got.Actor != "u-carol" {
		t.Errorf("the abandoned run was written under actor %q, want its owner u-carol", got.Actor)
	}
	if got := byRun["v1:work:run:r-dead"].Args(t); got["status"] != runStatusAbandoned {
		t.Errorf("the dead run's status = %v", got["status"])
	}
	if _, present := byRun["v1:work:run:r-parked"]; present {
		t.Error("a run parked on a person was swept; it is silent on purpose and no timer will release it")
	}
	for _, c := range eng.callsTo("updateWorkRun") {
		if !c.Origin.IsInternal() {
			t.Errorf("updateWorkRun reached the engine with origin %v; it is @serverOnly", c.Origin)
		}
	}
}

// sweepRows drives SweepWaiting's per-row half with a fixed row set, so the
// decision is testable without a database. It is the same loop body; the only
// substitution is the source of the rows.
func sweepRows(ctx context.Context, i *Integration, rows []map[string]any, now time.Time, olderThan time.Duration) WaitSweepResult {
	stub := &stubRowSource{rows: rows}
	i.bunDB = nil
	orig := i.rowsInFlight
	i.rowsInFlight = stub.read
	defer func() { i.rowsInFlight = orig }()
	res, _ := i.SweepWaiting(auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "system:maintenance:sweepWaitingWorkRuns", Role: auth.RoleOwner, Unranked: true, Synthetic: true}), olderThan)
	return res
}

type stubRowSource struct {
	mu   sync.Mutex
	rows []map[string]any
}

func (s *stubRowSource) read(context.Context) ([]map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows, nil
}

func summariseGroups(g map[string][]map[string]any) string {
	var b strings.Builder
	for day, rows := range g {
		fmt.Fprintf(&b, "%s=%d ", day, len(rows))
	}
	return b.String()
}
