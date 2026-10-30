package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// appsignin.go is the stream half of Fleet's Sign in action: the replica holding a
// machine's stream tells it what the person did, and files what it answers on the
// v1:worker:appSignIn row, which is how the person watches.
//
// CREDENTIALS STAY ON THE MACHINE. Nothing here sees a token: the app writes its own
// login there. What crosses is a sign-in page's address, a device code to enter on
// it, and a code pasted back, which is useless without the verifier the process on
// the machine holds (PKCE). The pasted code is sent and forgotten. It is in no row,
// no log line and no error here.

// The actions of an AppSignInCommand.
const (
	AppSignInActionStart  = "start"
	AppSignInActionCode   = "code"
	AppSignInActionCancel = "cancel"
)

// The statuses a machine reports. The row adds "requested" before it has spoken and
// "cancelled" when a person stopped it.
const (
	AppSignInStateWaiting   = "waiting"
	AppSignInStateSucceeded = "succeeded"
	AppSignInStateFailed    = "failed"
)

// AppSignInCommand is one thing a person did to a sign-in.
type AppSignInCommand struct {
	Action   string
	SignInId string
	App      string
	Code     string
	Reason   string
}

// AppSignInFunc is the per-stream hook behind Worker.SendAppSignIn.
type AppSignInFunc func(context.Context, AppSignInCommand) error

// AppSignInUpdateRow is what a machine said, as the row stores it.
type AppSignInUpdateRow struct {
	SignInId     string
	Status       string
	URL          string
	UserCode     string
	NeedsCode    bool
	ErrorMessage string
	ExpiresAt    time.Time
	At           time.Time
}

// AppSignInStore writes a sign-in's row. Optional beside Store, as OwnerFeatureStore
// is: a stream whose store cannot write one refuses a start rather than sending a
// sign-in nobody could watch.
type AppSignInStore interface {
	UpdateAppSignIn(ctx context.Context, ownerUserId string, row AppSignInUpdateRow) error
}

// ErrAppSignInUnavailable means this machine cannot take this command right now.
var ErrAppSignInUnavailable = errors.New("worker: this machine cannot take that sign-in")

// SetAppSignInFunc wires the per-stream hook. Called once per stream, as its
// siblings are.
func (w *Worker) SetAppSignInFunc(fn AppSignInFunc) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.appSignInFn = fn
}

// SendAppSignIn tells the machine one thing a person did.
func (w *Worker) SendAppSignIn(ctx context.Context, cmd AppSignInCommand) error {
	if w == nil {
		return ErrAppSignInUnavailable
	}
	w.mu.Lock()
	fn := w.appSignInFn
	w.mu.Unlock()
	if fn == nil {
		return ErrAppSignInUnavailable
	}
	return fn(ctx, cmd)
}

// sendAppSignIn is the per-stream hook.
//
// A START ANSWERS TO THE MACHINE'S OWN DESCRIPTOR AND TO THE OWNER'S SWITCH. The
// descriptor because a Cockpit that predates the field ignores the message, which
// would leave a person waiting on a row nothing will ever move. The switch because
// turning apps off in Settings means nothing new starts.
//
// A code goes only to a sign-in this stream started. After a reconnect the process
// holding the verifier is gone, and saying so beats sending a code nobody reads.
func (s *streamSession) sendAppSignIn(ctx context.Context, cmd AppSignInCommand) error {
	var payload *memqlv1.WorkerServerMessage
	switch cmd.Action {
	case AppSignInActionStart:
		if _, ok := s.server.store.(AppSignInStore); !ok {
			return fmt.Errorf("%w: this node has nowhere to record it", ErrAppSignInUnavailable)
		}
		if d, ok := s.worker.AppDescriptor(cmd.App); !ok || !d.SignIn {
			return fmt.Errorf("%w: its Cockpit cannot run a sign-in for %s; update Cockpit on that machine", ErrAppSignInUnavailable, cmd.App)
		}
		_, release, err := s.admitOwnerFeature(ctx, FeatureApps)
		if err != nil {
			return err
		}
		release()
		s.mu.Lock()
		if s.appSignIns == nil {
			s.appSignIns = make(map[string]struct{})
		}
		s.appSignIns[cmd.SignInId] = struct{}{}
		s.mu.Unlock()
		payload = &memqlv1.WorkerServerMessage{Payload: &memqlv1.WorkerServerMessage_AppSignInStart{
			AppSignInStart: &memqlv1.AppSignInStart{SignInId: cmd.SignInId, App: cmd.App},
		}}
	case AppSignInActionCode:
		if !s.holdsAppSignIn(cmd.SignInId) {
			return fmt.Errorf("%w: that sign-in is no longer running on the machine; start it again", ErrAppSignInUnavailable)
		}
		payload = &memqlv1.WorkerServerMessage{Payload: &memqlv1.WorkerServerMessage_AppSignInInput{
			AppSignInInput: &memqlv1.AppSignInInput{SignInId: cmd.SignInId, Code: cmd.Code},
		}}
	case AppSignInActionCancel:
		payload = &memqlv1.WorkerServerMessage{Payload: &memqlv1.WorkerServerMessage_AppSignInCancel{
			AppSignInCancel: &memqlv1.AppSignInCancel{SignInId: cmd.SignInId, Reason: cmd.Reason},
		}}
	default:
		return fmt.Errorf("%w: unknown action %q", ErrAppSignInUnavailable, cmd.Action)
	}
	err := s.send(payload)
	// The engine closes the row of a cancel, whether or not the machine heard it, and of
	// a start that did not go. After that nothing on this stream may rewrite it: not a
	// last word from the machine, and not the stream ending. A code that did not go leaves
	// the sign-in open, for the stream ending to close.
	if cmd.Action == AppSignInActionCancel || (err != nil && cmd.Action == AppSignInActionStart) {
		s.forgetAppSignIn(cmd.SignInId)
	}
	if err != nil {
		return fmt.Errorf("%w: the machine could not be reached: %v", ErrAppSignInUnavailable, err)
	}
	return nil
}

func (s *streamSession) holdsAppSignIn(signInId string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, held := s.appSignIns[signInId]
	return held
}

func (s *streamSession) forgetAppSignIn(signInId string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.appSignIns, signInId)
}

// handleAppSignInUpdate files what the machine said.
//
// ONLY FOR A SIGN-IN THIS STREAM STARTED. The write is under the machine's owner, so
// a stray id could only touch their own rows, but a machine has no business writing
// a sign-in it was never asked to run.
func (s *streamSession) handleAppSignInUpdate(update *memqlv1.AppSignInUpdate) {
	signInId := strings.TrimSpace(update.GetSignInId())
	if signInId == "" || !s.holdsAppSignIn(signInId) {
		return
	}
	row := AppSignInUpdateRow{
		SignInId:     signInId,
		URL:          truncate(strings.TrimSpace(update.GetUrl()), maxAppSignInURLLen),
		UserCode:     truncate(strings.TrimSpace(update.GetUserCode()), maxAppFieldLen),
		NeedsCode:    update.GetNeedsCode(),
		ErrorMessage: truncate(strings.TrimSpace(update.GetError()), maxAppSignInErrorLen),
		At:           time.Now().UTC(),
	}
	if ts := update.GetExpiresAt(); ts != nil && ts.IsValid() {
		row.ExpiresAt = ts.AsTime().UTC()
	}
	switch update.GetState() {
	case AppSignInStateWaiting:
		row.Status = AppSignInStateWaiting
	case AppSignInStateSucceeded:
		row.Status = AppSignInStateSucceeded
		s.forgetAppSignIn(signInId)
	case AppSignInStateFailed:
		row.Status = AppSignInStateFailed
		s.forgetAppSignIn(signInId)
	default:
		return
	}
	s.writeAppSignIn(row)
}

// failOpenAppSignIns closes every sign-in this stream held, when it ends. The process
// on the machine goes with its connection, so nothing is coming, and a page waiting
// on it must say so.
func (s *streamSession) failOpenAppSignIns() {
	s.mu.Lock()
	open := make([]string, 0, len(s.appSignIns))
	for signInId := range s.appSignIns {
		open = append(open, signInId)
	}
	s.appSignIns = nil
	s.mu.Unlock()
	for _, signInId := range open {
		s.writeAppSignIn(AppSignInUpdateRow{
			SignInId:     signInId,
			Status:       AppSignInStateFailed,
			ErrorMessage: "The machine disconnected before the sign-in finished. Sign in again.",
			At:           time.Now().UTC(),
		})
	}
}

func (s *streamSession) writeAppSignIn(row AppSignInUpdateRow) {
	store, ok := s.server.store.(AppSignInStore)
	if !ok {
		return
	}
	// Not the stream's context: this also runs as the stream ends, and a person is
	// waiting on the answer.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.UpdateAppSignIn(ctx, s.worker.OwnerUserId, row); err != nil && s.server.logger != nil {
		s.server.logger.Warn("worker: could not record an app sign-in update",
			"registration_id", s.worker.RegistrationId, "sign_in_id", row.SignInId, "status", row.Status, "error", err)
	}
}

// maxAppSignInURLLen and maxAppSignInErrorLen bound what a machine can put on a row.
// The URL's own query is about 400 bytes; this leaves room for it to grow.
const (
	maxAppSignInURLLen   = 2048
	maxAppSignInErrorLen = 1000
)

// UpdateAppSignIn implements AppSignInStore. A terminal status clears the address and
// the code, which by then answer nothing, so the row a person later reads holds neither.
func (s *EngineStore) UpdateAppSignIn(ctx context.Context, ownerUserId string, row AppSignInUpdateRow) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	at := row.At.UTC().Format(time.RFC3339Nano)
	args := map[string]any{
		"signInId":  row.SignInId,
		"status":    row.Status,
		"updatedAt": at,
	}
	switch row.Status {
	case AppSignInStateWaiting:
		args["url"] = row.URL
		args["userCode"] = row.UserCode
		args["needsCode"] = row.NeedsCode
		if !row.ExpiresAt.IsZero() {
			args["expiresAt"] = row.ExpiresAt.Format(time.RFC3339Nano)
		}
	default:
		args["errorMessage"] = row.ErrorMessage
		args["endedAt"] = at
	}
	return s.executeMutation(appSessionWriteContext(ctx, ownerUserId), "updateAppSignIn", args)
}

var _ AppSignInStore = (*EngineStore)(nil)
