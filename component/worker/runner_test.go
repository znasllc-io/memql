package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

type recordingAppSessionStore struct {
	mu       sync.Mutex
	created  []AppSessionRow
	appends  []AppSessionRow
	finished []AppSessionRow
}

func (s *recordingAppSessionStore) CreateAppSession(_ context.Context, row AppSessionRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, row)
	return nil
}

func (s *recordingAppSessionStore) AppendAppSessionTranscript(_ context.Context, sessionId, transcript string, bytes int, truncated bool, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appends = append(s.appends, AppSessionRow{
		ID: sessionId, Transcript: transcript, TranscriptBytes: bytes,
		TranscriptTruncated: truncated, Status: status,
	})
	return nil
}

func (s *recordingAppSessionStore) EndAppSession(_ context.Context, row AppSessionRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished = append(s.finished, row)
	return nil
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
		Registry:    session.server.registry,
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
		PlanId:      "plan-1",
		TaskId:      "task-1",
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

	result, err := runner.Run(context.Background(), runSpec(), nil)
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
	if store.created[0].CredentialIdentityId == "" || store.created[0].MCPEndpoint == "" {
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
			result, err := runner.Run(context.Background(), runSpec(), nil)
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
	runner, _, store, _ := newRunnerFixture(t, SubscriptionPresent)
	runner.Minter = nil

	_, err := runner.Run(context.Background(), runSpec(), nil)
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
	result, err := runner.Run(context.Background(), runSpec(), nil)
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

	result, err := runner.Run(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.TranscriptTruncated {
		t.Fatal("a transcript past its bound must be marked truncated")
	}
	if !strings.Contains(result.Transcript, "truncated") {
		t.Fatalf("truncation must be visible in the transcript itself: %q", result.Transcript)
	}
	terminal := store.terminal()
	if terminal[0].TranscriptBytes != 100 {
		t.Fatalf("transcriptBytes = %d, want the full 100 seen", terminal[0].TranscriptBytes)
	}
}

// TestRunnerRefusesAnOfflineMachine: with nothing online that can run
// the app, the run is refused with a reason NAMING the app rather
// than a bare "no worker available".
func TestRunnerRefusesAnOfflineMachine(t *testing.T) {
	runner, session, _, _ := newRunnerFixture(t, SubscriptionPresent)
	session.worker.SetApps([]AppInfo{{Id: AppIdClaudeCode, Allowed: true, SignedIn: false}})

	_, err := runner.Run(context.Background(), runSpec(), nil)
	if err == nil {
		t.Fatal("ran on a machine that is not signed in to the app")
	}
	if !strings.Contains(err.Error(), AppIdClaudeCode) {
		t.Fatalf("the refusal must name the app, got %v", err)
	}
	if !errors.Is(err, ErrNoWorkerAvailable) {
		t.Fatalf("error must wrap ErrNoWorkerAvailable, got %v", err)
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

	if _, err := runner.Run(context.Background(), runSpec(), func(c AppSessionChunk) {
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
