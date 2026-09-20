package worker

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

type recordingAppSessionStore struct {
	mu        sync.Mutex
	created   []AppSessionRow
	appends   []AppSessionRow
	finished  []AppSessionRow
	allocated map[string]int
	allocErr  error
}

func (s *recordingAppSessionStore) CreateAppSession(_ context.Context, row AppSessionRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, row)
	return nil
}

func (s *recordingAppSessionStore) RecordAppSessionProgress(_ context.Context, sessionId string, recordedSteps, droppedActions int, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A NEGATIVE COUNT MEANS "the caller did not name this field", which is
	// how the allocator advances one counter without resetting the other.
	// Recording it as 0 would make this fake claim a write the real store
	// never makes.
	row := AppSessionRow{ID: sessionId, Status: status}
	if recordedSteps >= 0 {
		row.RecordedSteps = recordedSteps
	}
	if droppedActions >= 0 {
		row.DroppedActions = droppedActions
	}
	s.appends = append(s.appends, row)
	return nil
}

func (s *recordingAppSessionStore) EndAppSession(_ context.Context, row AppSessionRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished = append(s.finished, row)
	return nil
}

// AllocateRecordingSeq is the SHARED allocator, modelled here as the real one
// behaves: read the count off the row, hand it out, advance. Keeping it in
// this fake rather than returning a fresh counter per caller is what lets a
// test observe the property that matters -- two writers into one session
// never take the same position.
func (s *recordingAppSessionStore) AllocateRecordingSeq(_ context.Context, sessionId, _ string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.allocErr != nil {
		return 0, s.allocErr
	}
	if s.allocated == nil {
		s.allocated = map[string]int{}
	}
	seq := s.allocated[sessionId]
	s.allocated[sessionId] = seq + 1
	return seq, nil
}

func (s *recordingAppSessionStore) terminal() []AppSessionRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AppSessionRow, len(s.finished))
	copy(out, s.finished)
	return out
}

type stubMinter struct {
	mu    sync.Mutex
	calls int
	err   error
	ttl   time.Duration
}

func (m *stubMinter) Mint(_ context.Context, req CredentialRequest) (Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return Credential{}, m.err
	}
	m.calls++
	m.ttl = req.TTL
	return Credential{
		Token:      "bearer-" + req.SessionId,
		ExpiresAt:  time.Now().UTC().Add(4 * time.Hour),
		IdentityId: "cred-" + req.SessionId,
	}, nil
}

type recordingAuditor struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (a *recordingAuditor) Emit(_ context.Context, ev AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
}

func (a *recordingAuditor) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.events))
	for _, e := range a.events {
		out = append(out, e.Action)
	}
	return out
}

// newRunnerFixture builds a runner over a fake stream, and returns
// the stream session so a test can play the worker's half.
func newRunnerFixture(t *testing.T, subscription string) (*SessionRunner, *streamSession, *recordingAppSessionStore, *recordingAuditor) {
	t.Helper()
	session, _ := newAppSessionTestSession(t)
	session.worker.SetApps([]AppInfo{
		{Id: AppIdClaudeCode, Version: "2.1.4", Allowed: true, SignedIn: true, Subscription: subscription},
	})
	session.server.registry.Add(session.worker)

	store := &recordingAppSessionStore{}
	auditor := &recordingAuditor{}
	runner := &SessionRunner{
		Store:       store,
		Minter:      &stubMinter{},
		Auditor:     auditor,
		MCPEndpoint: "https://mcp.example.com/mcp",
	}
	return runner, session, store, auditor
}

func runSpec() RunSpec {
	return RunSpec{
		SessionId:   "sess-run",
		OwnerUserId: "user-1",
		App:         AppIdClaudeCode,
		Kind:        AppSessionKindRun,
		Prompt:      "do the thing",
		Workspace:   "/w",
		RunId:       "plan-1",
		StepId:      "task-1",
	}
}

// TestRunnerRecordsSubscriptionBilling is design §9.5: an app that
// reports its usage on a machine whose subscription is present bills
// as `subscription`, and the row carries the reported numbers
// verbatim.
func TestRunnerRecordsSubscriptionBilling(t *testing.T) {
	runner, session, store, auditor := newRunnerFixture(t, SubscriptionPresent)

	go func() {
		waitForSession(t, session, "sess-run")
		session.handleAppSessionChunk(&memqlv1.AppSessionChunk{SessionId: "sess-run", Data: []byte("working"), Seq: 1})
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{
			SessionId:     "sess-run",
			Usage:         &memqlv1.AppSessionUsage{InputTokens: 900, OutputTokens: 400, CostUsd: 0.31, Known: true},
			AppSessionRef: "cc-1",
		})
	}()

	result, err := runner.Run(context.Background(), session.worker, runSpec(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Billing != BillingSubscription {
		t.Fatalf("billing = %q, want %q", result.Billing, BillingSubscription)
	}
	if result.Usage.InputTokens != 900 || result.Usage.CostUSD != 0.31 {
		t.Fatalf("usage not carried verbatim: %+v", result.Usage)
	}
	if result.Status != AppSessionStatusEnded {
		t.Fatalf("status = %q, want ended", result.Status)
	}

	if len(store.created) != 1 {
		t.Fatalf("expected the row to be created before the run, got %d", len(store.created))
	}
	if store.created[0].CredentialRef == "" || store.created[0].MCPEndpoint == "" {
		t.Fatalf("the back-channel was not recorded on the row: %+v", store.created[0])
	}
	terminal := store.terminal()
	if len(terminal) != 1 || terminal[0].Billing != BillingSubscription {
		t.Fatalf("terminal row wrong: %+v", terminal)
	}

	got := auditor.actions()
	if len(got) != 2 || got[0] != "app_session_started" || got[1] != "app_session_ended" {
		t.Fatalf("audit trail = %v, want [app_session_started app_session_ended]", got)
	}
}

// TestRunnerNeverInfersBilling is D5's honesty rule at the runner
// level: an app that reports NO usage bills as `unknown`, never as
// metered and never as free -- and a machine with no subscription
// signal bills as unknown too, not as metered by omission.
func TestRunnerNeverInfersBilling(t *testing.T) {
	for _, tc := range []struct {
		name         string
		subscription string
		usage        *memqlv1.AppSessionUsage
		want         string
	}{
		{"app reported nothing", SubscriptionPresent, nil, BillingUnknown},
		{"app reported known=false", SubscriptionPresent, &memqlv1.AppSessionUsage{Known: false}, BillingUnknown},
		{"machine subscription unknown", SubscriptionUnknown, &memqlv1.AppSessionUsage{InputTokens: 5, Known: true}, BillingUnknown},
		{"machine has no subscription", SubscriptionNone, &memqlv1.AppSessionUsage{InputTokens: 5, Known: true}, BillingMetered},
		{"both present", SubscriptionPresent, &memqlv1.AppSessionUsage{InputTokens: 5, Known: true}, BillingSubscription},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, session, _, _ := newRunnerFixture(t, tc.subscription)
			go func() {
				waitForSession(t, session, "sess-run")
				session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run", Usage: tc.usage})
			}()
			result, err := runner.Run(context.Background(), session.worker, runSpec(), nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.Billing != tc.want {
				t.Fatalf("billing = %q, want %q", result.Billing, tc.want)
			}
		})
	}
}

// TestRunnerRefusesWithoutAMinter: an app with no back-channel bearer
// can reach nothing over MCP and would report that as "MemQL's tools
// are broken", sending the reader to entirely the wrong place. So the
// run is REFUSED with a named reason instead of started blank.
func TestRunnerRefusesWithoutAMinter(t *testing.T) {
	runner, session, store, _ := newRunnerFixture(t, SubscriptionPresent)
	runner.Minter = nil

	_, err := runner.Run(context.Background(), session.worker, runSpec(), nil)
	if err == nil {
		t.Fatal("started a session with no credential minter")
	}
	if !strings.Contains(err.Error(), "back-channel") {
		t.Fatalf("error must name the missing back-channel, got %v", err)
	}
	if len(store.created) != 0 {
		t.Fatal("a refused run must not leave a session row")
	}
}

// TestRunnerNonZeroExitIsFailed: a delegated step that crashed must
// not read as a step that succeeded and produced nothing.
func TestRunnerNonZeroExitIsFailed(t *testing.T) {
	runner, session, store, _ := newRunnerFixture(t, SubscriptionPresent)
	go func() {
		waitForSession(t, session, "sess-run")
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run", ExitCode: 2})
	}()
	result, err := runner.Run(context.Background(), session.worker, runSpec(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != AppSessionStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	terminal := store.terminal()
	if len(terminal) != 1 || terminal[0].Status != AppSessionStatusFailed {
		t.Fatalf("terminal row status wrong: %+v", terminal)
	}
	if !strings.Contains(terminal[0].ErrorMessage, "exited 2") {
		t.Fatalf("terminal row must say why: %q", terminal[0].ErrorMessage)
	}
}

// TestRunnerBoundsTheTranscript: the row keeps a bounded, EXPLICITLY
// marked transcript. A transcript that just stops reads as a run that
// stopped, which is the wrong conclusion.
func TestRunnerBoundsTheTranscript(t *testing.T) {
	runner, session, store, _ := newRunnerFixture(t, SubscriptionPresent)
	spec := runSpec()
	spec.MaxTranscriptBytes = 32

	go func() {
		waitForSession(t, session, "sess-run")
		for i := 0; i < 10; i++ {
			session.handleAppSessionChunk(&memqlv1.AppSessionChunk{
				SessionId: "sess-run", Data: []byte("0123456789"), Seq: uint64(i + 1),
			})
		}
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run"})
	}()

	result, err := runner.Run(context.Background(), session.worker, spec, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.TranscriptTruncated {
		t.Fatal("a transcript past its bound must be marked truncated")
	}
	if !strings.Contains(result.Transcript, "truncated") {
		t.Fatalf("truncation must be visible in the transcript itself: %q", result.Transcript)
	}
	if terminal := store.terminal(); !terminal[0].TranscriptTruncated {
		t.Fatal("the terminal row must say the transcript file does not hold the whole output")
	}
}

// TestRunnerRechecksTheAppBeforeStarting. The router matched on the
// `app:` label at selection time; the machine may have signed out
// between that read and now. Starting anyway would hand the app a
// credential it cannot use, and the failure would surface as "MemQL's
// tools are broken" rather than as a signed-out app.
func TestRunnerRechecksTheAppBeforeStarting(t *testing.T) {
	runner, session, store, _ := newRunnerFixture(t, SubscriptionPresent)
	session.worker.SetApps([]AppInfo{{Id: AppIdClaudeCode, Allowed: true, SignedIn: false}})

	_, err := runner.Run(context.Background(), session.worker, runSpec(), nil)
	if err == nil {
		t.Fatal("ran on a machine that is not signed in to the app")
	}
	if !strings.Contains(err.Error(), AppIdClaudeCode) {
		t.Fatalf("the refusal must name the app, got %v", err)
	}
	if len(store.created) != 0 {
		t.Fatal("a refused run must not leave a session row")
	}
}

// TestRunnerNeedsASelectedMachine: selection belongs to the Fleet
// router, so a nil worker is a programming error the runner names
// rather than a nil dereference.
func TestRunnerNeedsASelectedMachine(t *testing.T) {
	runner, _, _, _ := newRunnerFixture(t, SubscriptionPresent)
	if _, err := runner.Run(context.Background(), nil, runSpec(), nil); err == nil {
		t.Fatal("a nil worker must be refused")
	}
}

// TestRunnerStreamsProgressLive: chunks reach the caller while the
// run is still going, which is what a live transcript panel needs.
func TestRunnerStreamsProgressLive(t *testing.T) {
	runner, session, _, _ := newRunnerFixture(t, SubscriptionPresent)

	var mu sync.Mutex
	var seen []string
	go func() {
		waitForSession(t, session, "sess-run")
		session.handleAppSessionChunk(&memqlv1.AppSessionChunk{SessionId: "sess-run", Data: []byte("a"), Seq: 1})
		session.handleAppSessionChunk(&memqlv1.AppSessionChunk{SessionId: "sess-run", Data: []byte("b"), Seq: 2})
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run"})
	}()

	if _, err := runner.Run(context.Background(), session.worker, runSpec(), func(c AppSessionChunk) {
		mu.Lock()
		seen = append(seen, string(c.Data))
		mu.Unlock()
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "a" || seen[1] != "b" {
		t.Fatalf("progress = %v, want [a b]", seen)
	}
}

// waitForSession blocks until the runner has registered its session
// on the stream, so a test can play the worker's half without racing
// the start.
func waitForSession(t *testing.T, s *streamSession, sessionId string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.lookupAppSession(sessionId) != nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Errorf("session %s never opened", sessionId)
}

// TestRunnerClassifiesCancellation: a caller giving up is a CANCELLED
// session, not a failed one. Nothing on the machine misbehaved, and
// recording it as failed puts a retry-shaped signal on a run that did
// exactly what it was told.
func TestRunnerClassifiesCancellation(t *testing.T) {
	runner, session, store, _ := newRunnerFixture(t, SubscriptionPresent)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		waitForSession(t, session, "sess-run")
		cancel()
	}()

	result, err := runner.Run(ctx, session.worker, runSpec(), nil)
	if err == nil {
		t.Fatal("a cancelled run must surface the cancellation to its caller")
	}
	if result.Status != AppSessionStatusCancelled {
		t.Fatalf("status = %q, want cancelled", result.Status)
	}
	terminal := store.terminal()
	if len(terminal) != 1 || terminal[0].Status != AppSessionStatusCancelled {
		t.Fatalf("the row must record cancelled, got %+v", terminal)
	}
}

// TestIsCancellationReasonMatchesLoosely: the reason comes from another
// process on somebody else's machine, and the two mistakes cost different
// amounts -- so neither spelling of the word decides the classification.
func TestIsCancellationReasonMatchesLoosely(t *testing.T) {
	for _, reason := range []string{"cancelled", "canceled", "Cancelled by user", "  CANCEL  "} {
		if !isCancellationReason(reason) {
			t.Errorf("isCancellationReason(%q) = false, want true", reason)
		}
	}
	for _, reason := range []string{"", "exec_failed", "timeout", "worker_disconnected"} {
		if isCancellationReason(reason) {
			t.Errorf("isCancellationReason(%q) = true, want false", reason)
		}
	}
}

// stampingStore is a recording store that ALSO serves the provenance stamper,
// which is how the runner finds one: the interface is separate from
// AppSessionStore so a binary that runs no sessions is not obliged to
// implement it, and a store that does not simply is not asked.
type stampingStore struct {
	recordingAppSessionStore
	mu       sync.Mutex
	stamps   []ArtifactProvenance
	ids      [][]string
	owners   []string
	stampErr error
}

func (s *stampingStore) StampArtifactProvenance(_ context.Context, ownerUserId string, artifactIds []string, p ArtifactProvenance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stamps = append(s.stamps, p)
	s.ids = append(s.ids, artifactIds)
	s.owners = append(s.owners, ownerUserId)
	return s.stampErr
}

func (s *stampingStore) recorded() ([]ArtifactProvenance, [][]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stamps, s.ids, s.owners
}

// newStampingFixture is newRunnerFixture with a store that can be asked for a
// stamp. It is a separate builder rather than a flag because the point of the
// interface split is that most stores cannot answer.
func newStampingFixture(t *testing.T) (*SessionRunner, *streamSession, *stampingStore) {
	t.Helper()
	session, _ := newAppSessionTestSession(t)
	session.worker.SetApps([]AppInfo{
		{Id: AppIdClaudeCode, Version: "2.1.4", Allowed: true, SignedIn: true, Subscription: SubscriptionPresent},
	})
	session.server.registry.Add(session.worker)
	store := &stampingStore{}
	return &SessionRunner{
		Store:       store,
		Minter:      &stubMinter{},
		Auditor:     &recordingAuditor{},
		MCPEndpoint: "https://mcp.example.com/mcp",
	}, session, store
}

// producedBy is stamped on EVERY artifact a session produced, the transcript
// included (epic memql#5391, design D9). A lifted construct's source reads
// this stamp, and the transcript is the artifact a recording pass reads first.
func TestEveryProducedArtifactIsStamped(t *testing.T) {
	runner, session, store := newStampingFixture(t)

	go func() {
		waitForSession(t, session, "sess-run")
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{
			SessionId:           "sess-run",
			Model:               "claude-opus-5",
			Effort:              "high",
			ProducedArtifactIds: []string{"artifact-1", "artifact-2", "artifact-transcript"},
		})
	}()

	if _, err := runner.Run(context.Background(), session.worker, runSpec(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stamps, ids, owners := store.recorded()
	if len(stamps) != 1 {
		t.Fatalf("expected one stamp call, got %d", len(stamps))
	}
	if len(ids[0]) != 3 {
		t.Fatalf("every produced artifact must be stamped, got %v", ids[0])
	}
	if owners[0] != "user-1" {
		t.Fatalf("the stamp must run as the session's owner, got %q", owners[0])
	}
	got := stamps[0]
	if got.App != AppIdClaudeCode || got.SessionId != "sess-run" {
		t.Fatalf("the stamp must name the app and the session: %+v", got)
	}
	if got.Model != "claude-opus-5" || got.Effort != "high" {
		t.Fatalf("the stamp carries the APP'S REPORT: %+v", got)
	}
}

// An app that reported NO model still gets a stamp: the app and the session id
// are known, and "an app produced this and reported nothing" is a different
// fact from "no session produced this". Only the second is an absent stamp.
func TestAStampWithNoReportedModelStillNamesTheApp(t *testing.T) {
	runner, session, store := newStampingFixture(t)
	spec := runSpec()
	spec.Level = "reasoning"

	go func() {
		waitForSession(t, session, "sess-run")
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{
			SessionId:           "sess-run",
			ProducedArtifactIds: []string{"artifact-1"},
		})
	}()

	if _, err := runner.Run(context.Background(), session.worker, spec, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	stamps, _, _ := store.recorded()
	if len(stamps) != 1 {
		t.Fatalf("expected one stamp call, got %d", len(stamps))
	}
	if stamps[0].App != AppIdClaudeCode || stamps[0].SessionId != "sess-run" {
		t.Fatalf("the stamp must still name the app and session: %+v", stamps[0])
	}
	// THE LEVEL IS NOT SUBSTITUTED. A level is what was asked for; this object
	// is what happened, and an effort of "reasoning" would be a measurement
	// nobody made -- readable months later as the app having confirmed it.
	if stamps[0].Model != "" || stamps[0].Effort != "" {
		t.Fatalf("an app that reported nothing leaves both empty, got %+v", stamps[0])
	}
}

// A session that produced NOTHING stamps nothing, and that is not an error.
func TestASessionWithNoArtifactsStampsNothing(t *testing.T) {
	runner, session, store := newStampingFixture(t)

	go func() {
		waitForSession(t, session, "sess-run")
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run"})
	}()

	if _, err := runner.Run(context.Background(), session.worker, runSpec(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stamps, _, _ := store.recorded(); len(stamps) != 0 {
		t.Fatalf("a session with no artifacts stamps nothing, got %+v", stamps)
	}
}

// A FAILED STAMP DOES NOT FAIL THE RUN. The session already happened on
// somebody's machine and already spent their subscription; losing the
// back-pointer costs a reader a journey, and refusing the work would cost them
// the work.
func TestAFailedStampDoesNotFailTheRun(t *testing.T) {
	runner, session, store := newStampingFixture(t)
	store.stampErr = errContext

	go func() {
		waitForSession(t, session, "sess-run")
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{
			SessionId:           "sess-run",
			ProducedArtifactIds: []string{"artifact-1"},
		})
	}()

	result, err := runner.Run(context.Background(), session.worker, runSpec(), nil)
	if err != nil {
		t.Fatalf("a failed stamp must not fail the run: %v", err)
	}
	if result.Status != AppSessionStatusEnded {
		t.Fatalf("status = %q, want the run's own outcome", result.Status)
	}
}

// errContext is a stand-in failure for the stamp path.
var errContext = context.Canceled
