package appsession

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/znasllc-io/memql/cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql/cockpit/internal/worker/harness"
)

// signin.go runs Fleet's Sign in action on this machine.
//
// THE MACHINE'S OWN CONSENT STILL DECIDES. The engine admits only the machine's owner
// and only an app it reads as allowed, and this checks the live policy again, because
// a person who withdrew consent here a moment ago has the last word on their machine.
//
// IT ENDS WITH THE STREAM. The engine closes the row when the stream goes, so a process
// left waiting would be a login nobody could finish, and StopAll stops it.
//
// A pasted code passes through here on its way to the app's stdin. No line in this file
// logs it, and neither does anything it calls.

// SignInSender is the worker's side of the stream, as a sign-in needs it.
type SignInSender interface {
	SendAppSignInUpdate(update *memqlv1.AppSignInUpdate) error
}

// The answers a sign-in reports, as AppSignInUpdate.state carries them.
const (
	signInWaiting   = "waiting"
	signInSucceeded = "succeeded"
	signInFailed    = "failed"
)

// liveSignIn is one sign-in this machine is running.
type liveSignIn struct {
	id     string
	app    string
	cancel context.CancelFunc

	// session is nil until the app has said where to sign in.
	session harness.SignIn
}

// StartSignIn begins a sign-in and returns at once: this runs on the one goroutine
// reading the stream. A newer sign-in to the same app replaces an older one, because a
// person pressing the button again has given up on the first.
func (m *Manager) StartSignIn(ctx context.Context, sender SignInSender, start *memqlv1.AppSignInStart) {
	id, app := strings.TrimSpace(start.GetSignInId()), strings.TrimSpace(start.GetApp())
	if id == "" {
		m.logger.Warn("app sign-in start with no id; ignoring")
		return
	}
	refuse := func(reason string) {
		if err := sender.SendAppSignInUpdate(&memqlv1.AppSignInUpdate{SignInId: id, State: signInFailed, Error: reason}); err != nil {
			m.logger.Warn("app sign-in refusal could not be sent", "sign_in_id", id, "error", err)
		}
	}
	if !apps.IsKnownID(app) {
		refuse(fmt.Sprintf("%q is not an app this machine drives", app))
		return
	}
	if m.opts.Allowed == nil || !m.opts.Allowed(app) {
		refuse(app + " is not allowed for this cluster by this machine's policy.yaml. Allow it there first.")
		return
	}
	signInCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), harness.SignInLifetime)
	live := &liveSignIn{id: id, app: app, cancel: cancel}
	m.mu.Lock()
	if m.signIns == nil {
		m.signIns = make(map[string]*liveSignIn)
	}
	var replaced []*liveSignIn
	for _, other := range m.signIns {
		if other.app == app {
			replaced = append(replaced, other)
		}
	}
	m.signIns[id] = live
	m.mu.Unlock()
	for _, other := range replaced {
		other.cancel()
	}
	go m.runSignIn(signInCtx, sender, live)
}

// SubmitSignInCode passes on what the person pasted.
func (m *Manager) SubmitSignInCode(sender SignInSender, input *memqlv1.AppSignInInput) {
	id := strings.TrimSpace(input.GetSignInId())
	m.mu.Lock()
	live := m.signIns[id]
	var session harness.SignIn
	if live != nil {
		session = live.session
	}
	m.mu.Unlock()
	if session == nil {
		// The engine holds this stream's sign-ins and refuses a stale code itself, so
		// this is a sign-in that ended a moment ago.
		_ = sender.SendAppSignInUpdate(&memqlv1.AppSignInUpdate{SignInId: id, State: signInFailed, Error: "That sign-in has ended. Sign in again."})
		return
	}
	if err := session.SubmitCode(input.GetCode()); err != nil {
		m.logger.Info("app sign-in could not take a code", "sign_in_id", id, "error", err)
	}
}

// CancelSignIn stops a sign-in. The engine has already closed its row.
func (m *Manager) CancelSignIn(cancel *memqlv1.AppSignInCancel) {
	id := strings.TrimSpace(cancel.GetSignInId())
	m.mu.Lock()
	live := m.signIns[id]
	m.mu.Unlock()
	if live != nil {
		live.cancel()
	}
}

// StopSignIns stops every sign-in, when the stream goes.
func (m *Manager) StopSignIns() {
	m.mu.Lock()
	live := make([]*liveSignIn, 0, len(m.signIns))
	for _, s := range m.signIns {
		live = append(live, s)
	}
	m.mu.Unlock()
	for _, s := range live {
		s.cancel()
	}
}

func (m *Manager) forgetSignIn(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.signIns, id)
}

func (m *Manager) runSignIn(ctx context.Context, sender SignInSender, live *liveSignIn) {
	defer m.forgetSignIn(live.id)
	defer live.cancel()
	logger := m.logger.With("sign_in_id", live.id, "app", live.app)
	send := func(update *memqlv1.AppSignInUpdate) {
		update.SignInId = live.id
		if err := sender.SendAppSignInUpdate(update); err != nil {
			logger.Warn("app sign-in update could not be sent", "state", update.GetState(), "error", err)
		}
	}
	session, err := m.startSignIn(ctx, live.app)
	if err != nil {
		logger.Warn("app sign-in could not start", "error", err)
		send(&memqlv1.AppSignInUpdate{State: signInFailed, Error: err.Error()})
		return
	}
	defer session.Stop()
	go func() {
		<-ctx.Done()
		session.Stop()
	}()

	m.mu.Lock()
	live.session = session
	m.mu.Unlock()
	prompt := session.Prompt()
	send(&memqlv1.AppSignInUpdate{
		State:     signInWaiting,
		Url:       prompt.URL,
		UserCode:  prompt.UserCode,
		NeedsCode: prompt.NeedsCode,
		ExpiresAt: timestamppb.New(time.Now().Add(harness.SignInLifetime)),
	})
	logger.Info("app sign-in waiting on its owner")

	err = session.Wait()
	switch {
	case err == nil:
		logger.Info("app sign-in finished")
		send(&memqlv1.AppSignInUpdate{State: signInSucceeded})
		// The app has written a login, and the next check reads it. Fleet shows it
		// working once that check has run, not because this said so.
		if r := m.opts.Readiness; r != nil {
			r.Recheck(live.app)
		}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		send(&memqlv1.AppSignInUpdate{State: signInFailed, Error: "The sign-in timed out. Sign in again."})
	case ctx.Err() != nil:
		// Stopped: by the person, by a newer sign-in, or with the stream. The row has
		// already said so, or the engine is about to.
		logger.Info("app sign-in stopped")
	default:
		logger.Warn("app sign-in failed", "error", err)
		send(&memqlv1.AppSignInUpdate{State: signInFailed, Error: err.Error()})
	}
}

// startSignIn resolves the app and starts its login in an empty directory.
func (m *Manager) startSignIn(ctx context.Context, app string) (harness.SignIn, error) {
	detector := m.opts.Detector
	if detector == nil {
		detector = &apps.Detector{}
	}
	spec, ok := detector.ResolveSpec(ctx, app)
	if !ok {
		return nil, fmt.Errorf("%s is not on this machine's PATH", app)
	}
	if err := checkLoginWritable(app); err != nil {
		return nil, err
	}
	launch := m.opts.SignInLauncher
	if launch == nil {
		launch = CheckLauncher()
	}
	dir, err := os.MkdirTemp("", "memql-app-signin-")
	if err != nil {
		return nil, fmt.Errorf("no directory to sign in from: %w", err)
	}
	// Its work is a login in the app's own home, so an empty directory is all it needs,
	// and the process keeps it only as a working directory.
	go func() {
		<-ctx.Done()
		_ = os.RemoveAll(dir)
	}()
	checkSpec := harness.CheckSpec{Binary: spec.Binary, Dir: dir, Launch: launch}
	switch app {
	case apps.IDClaudeCode:
		return harness.StartClaudeSignIn(ctx, checkSpec)
	case apps.IDCodex:
		if spec.Harness != apps.HarnessCodexAppServer {
			return nil, errors.New("This Codex cannot sign in by device code. Update Codex on that machine, then sign in again.")
		}
		return harness.StartCodexSignIn(ctx, checkSpec)
	}
	return nil, fmt.Errorf("%q is not an app this machine drives", app)
}

// checkLoginWritable says, before anything starts, what an old Linux service unit
// otherwise makes a person discover at the end: the app can show its page and take the
// code, and only then fail to write the login. A home that cannot be read is left to
// the app to report.
func checkLoginWritable(app string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	dir := filepath.Join(home, ".claude")
	if app == apps.IDCodex {
		dir = filepath.Join(home, ".codex")
	}
	if err := os.MkdirAll(dir, 0o700); err == nil {
		var probe *os.File
		if probe, err = os.CreateTemp(dir, ".memql-write-check-*"); err == nil {
			_ = probe.Close()
			_ = os.Remove(probe.Name())
			return nil
		}
	} else if !errors.Is(err, errReadOnly) && !os.IsPermission(err) {
		return nil
	}
	return fmt.Errorf("This machine's Cockpit service cannot write to %s, so it could not save a login. Run the Cockpit installer on that machine again to update the service.", dir)
}
