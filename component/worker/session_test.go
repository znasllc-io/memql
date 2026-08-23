package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// fakeCockpitStream is a WorkerService_StreamServer double: it
// records everything the engine sends so a test can assert on the
// wire, and lets the test push worker->server messages back in.
type fakeCockpitStream struct {
	grpc.ServerStream

	ctx context.Context

	mu   sync.Mutex
	sent []*memqlv1.WorkerServerMessage
	err  error
}

func newFakeCockpitStream() *fakeCockpitStream {
	return &fakeCockpitStream{ctx: context.Background()}
}

func (f *fakeCockpitStream) Send(msg *memqlv1.WorkerServerMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeCockpitStream) Recv() (*memqlv1.WorkerClientMessage, error) { return nil, io.EOF }
func (f *fakeCockpitStream) Context() context.Context                    { return f.ctx }
func (f *fakeCockpitStream) SetHeader(metadata.MD) error                 { return nil }
func (f *fakeCockpitStream) SendHeader(metadata.MD) error                { return nil }
func (f *fakeCockpitStream) SetTrailer(metadata.MD)                      {}
func (f *fakeCockpitStream) SendMsg(m any) error                         { return nil }
func (f *fakeCockpitStream) RecvMsg(m any) error                         { return io.EOF }

func (f *fakeCockpitStream) messages() []*memqlv1.WorkerServerMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*memqlv1.WorkerServerMessage, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *fakeCockpitStream) starts() []*memqlv1.AppSessionStart {
	var out []*memqlv1.AppSessionStart
	for _, m := range f.messages() {
		if s := m.GetAppSessionStart(); s != nil {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeCockpitStream) controls() []*memqlv1.AppSessionControl {
	var out []*memqlv1.AppSessionControl
	for _, m := range f.messages() {
		if c := m.GetAppSessionControl(); c != nil {
			out = append(out, c)
		}
	}
	return out
}

// newAppSessionTestSession builds a streamSession over the fake
// stream with one worker that runs both apps.
func newAppSessionTestSession(t *testing.T) (*streamSession, *fakeCockpitStream) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	clock := func() time.Time { return time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC) }
	srv := newServer(logger, &fakeRegistrationStore{}, NewRegistry(nil, clock), nil, clock, "test-node")
	stream := newFakeCockpitStream()
	w := &Worker{
		RegistrationId: "reg-1",
		OwnerUserId:    "user-1",
		Capabilities:   []string{CapabilityHeadless},
	}
	w.SetApps([]AppInfo{
		{Id: AppIdClaudeCode, Version: "2.1.4", Allowed: true, SignedIn: true, Subscription: SubscriptionPresent},
		{Id: AppIdCodex, Version: "1.0", Allowed: true, SignedIn: true, Subscription: SubscriptionPresent},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	session := newStreamSession(srv, stream, w, ctx, cancel)
	w.SetAppSessionFunc(session.openAppSession)
	return session, stream
}

// TestAppSessionRoundTrip is design §9.1: a fake cockpit round-trips
// start / chunk / end, and the caller sees the chunks in order and
// the reported usage verbatim.
func TestAppSessionRoundTrip(t *testing.T) {
	session, stream := newAppSessionTestSession(t)

	handle, err := session.worker.StartAppSession(context.Background(), AppSessionRequest{
		SessionId:   "sess-1",
		App:         AppIdClaudeCode,
		Kind:        AppSessionKindRun,
		Prompt:      "refactor the parser",
		Workspace:   "/home/u/work",
		Credential:  "bearer-1",
		MCPEndpoint: "https://mcp.example.com/mcp",
		PlanId:      "plan-1",
		TaskId:      "task-1",
		Limits:      AppSessionLimits{CredentialLifetime: 4 * time.Hour},
	})
	if err != nil {
		t.Fatalf("StartAppSession: %v", err)
	}

	starts := stream.starts()
	if len(starts) != 1 {
		t.Fatalf("expected 1 AppSessionStart, got %d", len(starts))
	}
	if starts[0].GetCredential() != "bearer-1" || starts[0].GetMcpEndpoint() != "https://mcp.example.com/mcp" {
		t.Fatalf("the back-channel was not carried on the start: %+v", starts[0])
	}
	if got := starts[0].GetLimits().GetCredentialLifetimeSeconds(); got != int64(4*3600) {
		t.Fatalf("credential lifetime = %d, want %d", got, int64(4*3600))
	}

	var collected []string
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for c := range handle.Chunks() {
			collected = append(collected, string(c.Data))
		}
	}()

	session.handleAppSessionChunk(&memqlv1.AppSessionChunk{SessionId: "sess-1", Stream: AppSessionStreamStdout, Data: []byte("one"), Seq: 1})
	session.handleAppSessionChunk(&memqlv1.AppSessionChunk{SessionId: "sess-1", Stream: AppSessionStreamStdout, Data: []byte("two"), Seq: 2})
	session.handleAppSessionEnd(&memqlv1.AppSessionEnd{
		SessionId: "sess-1",
		ExitCode:  0,
		Usage: &memqlv1.AppSessionUsage{
			InputTokens: 1200, OutputTokens: 800, CostUsd: 0.42, Known: true,
		},
		AppSessionRef:       "cc-abc",
		ProducedArtifactIds: []string{"artifact-1"},
	})
	<-drained

	outcome, err := handle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(collected) != 2 || collected[0] != "one" || collected[1] != "two" {
		t.Fatalf("chunks = %v, want [one two]", collected)
	}
	if outcome.Usage.InputTokens != 1200 || outcome.Usage.CostUSD != 0.42 || !outcome.Usage.Known {
		t.Fatalf("usage not carried verbatim: %+v", outcome.Usage)
	}
	if outcome.AppSessionRef != "cc-abc" || len(outcome.ProducedArtifactIds) != 1 {
		t.Fatalf("end fields lost: %+v", outcome)
	}
}

// TestAppSessionDropsReplayedChunks pins the transcript-integrity
// rule: seq is monotonic, and an out-of-order or duplicated chunk is
// DROPPED. Appending it would corrupt the record in a way no later
// reader could detect.
func TestAppSessionDropsReplayedChunks(t *testing.T) {
	session, _ := newAppSessionTestSession(t)
	handle, err := session.worker.StartAppSession(context.Background(), AppSessionRequest{
		SessionId: "sess-2", App: AppIdCodex, Kind: AppSessionKindRun,
	})
	if err != nil {
		t.Fatalf("StartAppSession: %v", err)
	}

	var got []string
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for c := range handle.Chunks() {
			got = append(got, string(c.Data))
		}
	}()

	session.handleAppSessionChunk(&memqlv1.AppSessionChunk{SessionId: "sess-2", Data: []byte("a"), Seq: 1})
	session.handleAppSessionChunk(&memqlv1.AppSessionChunk{SessionId: "sess-2", Data: []byte("b"), Seq: 3})
	session.handleAppSessionChunk(&memqlv1.AppSessionChunk{SessionId: "sess-2", Data: []byte("stale"), Seq: 2})
	session.handleAppSessionChunk(&memqlv1.AppSessionChunk{SessionId: "sess-2", Data: []byte("dup"), Seq: 3})
	session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-2"})
	<-drained

	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("chunks = %v, want [a b] (stale + duplicate dropped)", got)
	}
}

// TestAppSessionCancelAndRenew is design §9.1's control half: cancel
// reaches the worker, and renewal carries the replacement bearer.
func TestAppSessionCancelAndRenew(t *testing.T) {
	session, stream := newAppSessionTestSession(t)
	handle, err := session.worker.StartAppSession(context.Background(), AppSessionRequest{
		SessionId: "sess-3", App: AppIdClaudeCode, Kind: AppSessionKindRun, Credential: "bearer-1",
	})
	if err != nil {
		t.Fatalf("StartAppSession: %v", err)
	}

	if err := handle.RenewCredential("bearer-2"); err != nil {
		t.Fatalf("RenewCredential: %v", err)
	}
	if err := handle.Cancel("user_cancelled"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	controls := stream.controls()
	if len(controls) != 2 {
		t.Fatalf("expected 2 control messages, got %d", len(controls))
	}
	if controls[0].GetAction() != AppSessionActionRenewCredential || controls[0].GetCredential() != "bearer-2" {
		t.Fatalf("renew control wrong: %+v", controls[0])
	}
	if controls[1].GetAction() != AppSessionActionCancel || controls[1].GetReason() != "user_cancelled" {
		t.Fatalf("cancel control wrong: %+v", controls[1])
	}

	// The worker acknowledges the cancel by ending the session.
	session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-3", ExitCode: 130, Error: "cancelled"})
	outcome, err := handle.Wait(context.Background())
	if err == nil {
		t.Fatal("a session that ended with an error must surface it")
	}
	if outcome.ExitCode != 130 {
		t.Fatalf("exit code = %d, want 130", outcome.ExitCode)
	}

	// Cancel after end is a no-op, not an error: the caller racing the
	// worker's own exit is normal, and turning that race into a failure
	// would make every clean cancel look broken half the time.
	if err := handle.Cancel("again"); err != nil {
		t.Fatalf("cancel after end must be a no-op, got %v", err)
	}
}

// TestAppSessionRefusesUnrunnableApp: the gate that keeps a plan from
// committing to a machine that would refuse the run.
func TestAppSessionRefusesUnrunnableApp(t *testing.T) {
	session, _ := newAppSessionTestSession(t)
	session.worker.SetApps([]AppInfo{{Id: AppIdClaudeCode, Allowed: false, SignedIn: true}})

	if _, err := session.worker.StartAppSession(context.Background(), AppSessionRequest{
		SessionId: "sess-4", App: AppIdClaudeCode, Kind: AppSessionKindRun,
	}); err == nil {
		t.Fatal("started a session on a machine whose policy.yaml refuses the app")
	}

	// An app id the engine does not drive is refused before the wire.
	if _, err := session.worker.StartAppSession(context.Background(), AppSessionRequest{
		SessionId: "sess-5", App: "some-future-app", Kind: AppSessionKindRun,
	}); err == nil {
		t.Fatal("started a session for an app id outside the engine's closed set")
	}

	// So is a kind the protocol does not define.
	if _, err := session.worker.StartAppSession(context.Background(), AppSessionRequest{
		SessionId: "sess-6", App: AppIdCodex, Kind: "teleport",
	}); err == nil {
		t.Fatal("started a session with an undefined kind")
	}
}

// TestAppSessionKindsOpenAndAttach: all three kinds reach the wire,
// and attach carries the app's own session id.
func TestAppSessionKindsOpenAndAttach(t *testing.T) {
	session, stream := newAppSessionTestSession(t)
	for _, kind := range []string{AppSessionKindRun, AppSessionKindOpen, AppSessionKindAttach} {
		if _, err := session.worker.StartAppSession(context.Background(), AppSessionRequest{
			SessionId:     "sess-" + kind,
			App:           AppIdClaudeCode,
			Kind:          kind,
			AppSessionRef: "cc-existing",
		}); err != nil {
			t.Fatalf("kind %s: %v", kind, err)
		}
	}
	starts := stream.starts()
	if len(starts) != 3 {
		t.Fatalf("expected 3 starts, got %d", len(starts))
	}
	for i, want := range []string{AppSessionKindRun, AppSessionKindOpen, AppSessionKindAttach} {
		if starts[i].GetKind() != want {
			t.Fatalf("start %d kind = %q, want %q", i, starts[i].GetKind(), want)
		}
	}
	if starts[2].GetAppSessionRef() != "cc-existing" {
		t.Fatal("attach did not carry the app's own session id")
	}
}

// TestAppSessionEndsOnDisconnect: a worker going away must release
// every parked caller with a NAMED error. Without it a caller in
// Wait sits until its own context expires with nothing in the log
// saying the machine left.
func TestAppSessionEndsOnDisconnect(t *testing.T) {
	session, _ := newAppSessionTestSession(t)
	handle, err := session.worker.StartAppSession(context.Background(), AppSessionRequest{
		SessionId: "sess-7", App: AppIdCodex, Kind: AppSessionKindRun,
	})
	if err != nil {
		t.Fatalf("StartAppSession: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, waitErr := handle.Wait(context.Background())
		done <- waitErr
	}()

	session.close()

	select {
	case waitErr := <-done:
		if !errors.Is(waitErr, ErrWorkerDisconnected) {
			t.Fatalf("Wait error = %v, want ErrWorkerDisconnected", waitErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after the worker disconnected")
	}
}

// TestAppSessionCallerCancelStopsTheRun: a caller whose context dies
// must not leave a headless agent running on somebody's laptop.
func TestAppSessionCallerCancelStopsTheRun(t *testing.T) {
	session, stream := newAppSessionTestSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := session.worker.StartAppSession(ctx, AppSessionRequest{
		SessionId: "sess-8", App: AppIdClaudeCode, Kind: AppSessionKindRun,
	}); err != nil {
		t.Fatalf("StartAppSession: %v", err)
	}
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range stream.controls() {
			if c.GetAction() == AppSessionActionCancel {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("caller context cancellation did not cancel the run on the machine")
}
