package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

type recordedSignInStore struct {
	fakeRegistrationStore
	rows []AppSignInUpdateRow
}

func (r *recordedSignInStore) UpdateAppSignIn(_ context.Context, _ string, row AppSignInUpdateRow) error {
	r.rows = append(r.rows, row)
	return nil
}

func (r *recordedSignInStore) ReadOwnerFeaturePolicy(context.Context, string, string) (OwnerFeaturePolicy, error) {
	return OwnerFeaturePolicy{Features: OwnerFeatures{Apps: true}, Version: time.Unix(1, 0)}, nil
}

// signInSession is a stream session as far as a sign-in needs one: its sends go to a
// recording stream and its row writes to a recording store.
func signInSession(t *testing.T, descriptors ...AppDescriptor) (*streamSession, *recordedSignInStore, *fakeWorkerStream) {
	t.Helper()
	store := &recordedSignInStore{}
	stream := &fakeWorkerStream{}
	w := &Worker{RegistrationId: "machine", OwnerUserId: "alice"}
	w.SetAppDescriptors(descriptors)
	s := &streamSession{server: &server{store: store}, worker: w, stream: stream}
	return s, store, stream
}

func TestAppSignInStartAnswersToTheMachinesDescriptor(t *testing.T) {
	// A Cockpit that predates the field would ignore the message, and the person
	// would watch a row nothing ever moves.
	s, _, stream := signInSession(t, AppDescriptor{Id: AppIdCodex, Harness: HarnessCodexMCP})
	err := s.sendAppSignIn(context.Background(), AppSignInCommand{Action: AppSignInActionStart, SignInId: "s1", App: AppIdCodex})
	if err == nil || !strings.Contains(err.Error(), "update Cockpit") {
		t.Fatalf("got %v", err)
	}
	if len(stream.sent) != 0 {
		t.Fatalf("sent anyway: %v", stream.sent)
	}
}

func TestAppSignInGoesToTheMachineAndItsAnswerToTheRow(t *testing.T) {
	s, store, stream := signInSession(t, AppDescriptor{Id: AppIdCodex, Harness: HarnessCodexAppServer, SignIn: true})
	if err := s.sendAppSignIn(context.Background(), AppSignInCommand{Action: AppSignInActionStart, SignInId: "s1", App: AppIdCodex}); err != nil {
		t.Fatal(err)
	}
	if start := stream.sent[0].GetAppSignInStart(); start.GetSignInId() != "s1" || start.GetApp() != AppIdCodex {
		t.Fatalf("got %+v", stream.sent[0])
	}
	s.handleAppSignInUpdate(&memqlv1.AppSignInUpdate{SignInId: "s1", State: AppSignInStateWaiting, Url: "https://auth.openai.com/codex/device", UserCode: "ABCD-EFGH"})
	if len(store.rows) != 1 || store.rows[0].Status != AppSignInStateWaiting || store.rows[0].URL == "" || store.rows[0].UserCode != "ABCD-EFGH" {
		t.Fatalf("got %+v", store.rows)
	}
	s.handleAppSignInUpdate(&memqlv1.AppSignInUpdate{SignInId: "s1", State: AppSignInStateSucceeded})
	if len(store.rows) != 2 || store.rows[1].Status != AppSignInStateSucceeded {
		t.Fatalf("got %+v", store.rows)
	}
	if s.holdsAppSignIn("s1") {
		t.Fatal("a finished sign-in stayed open")
	}
}

// After a reconnect the process holding the verifier is gone. Saying so beats
// sending a code nobody will read.
func TestAppSignInCodeGoesOnlyToASignInThisStreamStarted(t *testing.T) {
	s, _, stream := signInSession(t, AppDescriptor{Id: AppIdClaudeCode, Harness: HarnessClaudeHeadless, SignIn: true})
	err := s.sendAppSignIn(context.Background(), AppSignInCommand{Action: AppSignInActionCode, SignInId: "elsewhere", Code: "pasted-secret"})
	if err == nil || strings.Contains(err.Error(), "pasted-secret") {
		t.Fatalf("got %v", err)
	}
	if len(stream.sent) != 0 {
		t.Fatalf("sent anyway: %v", stream.sent)
	}
	if err := s.sendAppSignIn(context.Background(), AppSignInCommand{Action: AppSignInActionStart, SignInId: "s1", App: AppIdClaudeCode}); err != nil {
		t.Fatal(err)
	}
	if err := s.sendAppSignIn(context.Background(), AppSignInCommand{Action: AppSignInActionCode, SignInId: "s1", Code: "pasted"}); err != nil {
		t.Fatal(err)
	}
	if input := stream.sent[1].GetAppSignInInput(); input.GetCode() != "pasted" || input.GetSignInId() != "s1" {
		t.Fatalf("got %+v", stream.sent[1])
	}
}

func TestAppSignInUpdateForAnUnknownSignInIsDropped(t *testing.T) {
	s, store, _ := signInSession(t)
	s.handleAppSignInUpdate(&memqlv1.AppSignInUpdate{SignInId: "never-started", State: AppSignInStateSucceeded})
	if len(store.rows) != 0 {
		t.Fatalf("a machine wrote a sign-in it was never asked to run: %+v", store.rows)
	}
}

// The person is watching the row, and the process on the machine goes with its
// connection, so the stream ending must close it.
func TestAppSignInClosesWhenTheStreamEnds(t *testing.T) {
	s, store, _ := signInSession(t, AppDescriptor{Id: AppIdCodex, Harness: HarnessCodexAppServer, SignIn: true})
	if err := s.sendAppSignIn(context.Background(), AppSignInCommand{Action: AppSignInActionStart, SignInId: "s1", App: AppIdCodex}); err != nil {
		t.Fatal(err)
	}
	s.failOpenAppSignIns()
	if len(store.rows) != 1 || store.rows[0].Status != AppSignInStateFailed || !strings.Contains(store.rows[0].ErrorMessage, "disconnected") {
		t.Fatalf("got %+v", store.rows)
	}
}

// failingStream is a stream whose machine went away after it started.
type failingStream struct {
	fakeWorkerStream
	fail bool
}

func (f *failingStream) Send(msg *memqlv1.WorkerServerMessage) error {
	if f.fail {
		return errors.New("stream closed")
	}
	return f.fakeWorkerStream.Send(msg)
}

// A code that did not go leaves the sign-in open, so the stream ending closes it rather
// than leaving a person reading `waiting` on a process that went with the stream.
func TestAppSignInCodeThatDidNotGoIsClosedByTheStreamEnding(t *testing.T) {
	s, store, _ := signInSession(t, AppDescriptor{Id: AppIdClaudeCode, Harness: HarnessClaudeHeadless, SignIn: true})
	stream := &failingStream{}
	s.stream = stream
	if err := s.sendAppSignIn(context.Background(), AppSignInCommand{Action: AppSignInActionStart, SignInId: "s1", App: AppIdClaudeCode}); err != nil {
		t.Fatal(err)
	}
	stream.fail = true
	err := s.sendAppSignIn(context.Background(), AppSignInCommand{Action: AppSignInActionCode, SignInId: "s1", Code: "pasted-secret"})
	if err == nil || strings.Contains(err.Error(), "pasted-secret") {
		t.Fatalf("got %v", err)
	}
	s.failOpenAppSignIns()
	if len(store.rows) != 1 || store.rows[0].Status != AppSignInStateFailed {
		t.Fatalf("got %+v", store.rows)
	}
}

// A person cancelled, and the engine wrote `cancelled`. Whatever comes after that from
// the machine or the stream ending must leave their answer on the row.
func TestAppSignInCancelStaysCancelled(t *testing.T) {
	s, store, stream := signInSession(t, AppDescriptor{Id: AppIdCodex, Harness: HarnessCodexAppServer, SignIn: true})
	if err := s.sendAppSignIn(context.Background(), AppSignInCommand{Action: AppSignInActionStart, SignInId: "s1", App: AppIdCodex}); err != nil {
		t.Fatal(err)
	}
	if err := s.sendAppSignIn(context.Background(), AppSignInCommand{Action: AppSignInActionCancel, SignInId: "s1", Reason: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if cancel := stream.sent[1].GetAppSignInCancel(); cancel.GetSignInId() != "s1" {
		t.Fatalf("got %+v", stream.sent[1])
	}
	s.handleAppSignInUpdate(&memqlv1.AppSignInUpdate{SignInId: "s1", State: AppSignInStateFailed, Error: "the process was stopped"})
	s.failOpenAppSignIns()
	if len(store.rows) != 0 {
		t.Fatalf("a cancelled sign-in was rewritten: %+v", store.rows)
	}
}

// A finished sign-in's row holds neither the address nor the code.
func TestEngineStoreClearsTheAddressWhenASignInEnds(t *testing.T) {
	recorder := &recordingExecutor{}
	store := &EngineStore{Engine: recorder}
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if err := store.UpdateAppSignIn(context.Background(), "alice", AppSignInUpdateRow{SignInId: "s1", Status: AppSignInStateWaiting, URL: "https://example.invalid/x", UserCode: "CODE", ExpiresAt: at, At: at}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorder.statements[0], `url: "https://example.invalid/x"`) || !strings.Contains(recorder.statements[0], `userCode: "CODE"`) {
		t.Fatalf("got %s", recorder.statements[0])
	}
	if err := store.UpdateAppSignIn(context.Background(), "alice", AppSignInUpdateRow{SignInId: "s1", Status: AppSignInStateSucceeded, URL: "https://example.invalid/x", UserCode: "CODE", At: at}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(recorder.statements[1], "example.invalid") || strings.Contains(recorder.statements[1], "CODE") {
		t.Fatalf("a finished sign-in kept its address: %s", recorder.statements[1])
	}
}
