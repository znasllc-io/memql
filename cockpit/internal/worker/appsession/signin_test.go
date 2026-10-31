package appsession

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/cockpit/internal/worker/apps"
)

// signInSender records what the machine told the engine.
type signInSender struct {
	mu      sync.Mutex
	updates []*memqlv1.AppSignInUpdate
}

func (s *signInSender) SendAppSignInUpdate(update *memqlv1.AppSignInUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, update)
	return nil
}

func (s *signInSender) waitFor(t *testing.T, state string) *memqlv1.AppSignInUpdate {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		for _, update := range s.updates {
			if update.GetState() == state {
				s.mu.Unlock()
				return update
			}
		}
		s.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("never reported %q: %+v", state, s.snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *signInSender) snapshot() []*memqlv1.AppSignInUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*memqlv1.AppSignInUpdate(nil), s.updates...)
}

// fakeClaudeLogin is a `claude` on PATH that signs in on the code "good".
func fakeClaudeLogin(t *testing.T) {
	t.Helper()
	fakeApp(t, "claude", `
echo "If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true"
printf 'Paste code here if prompted > '
IFS= read -r code
if [ "$code" = good ]; then echo 'Login successful.'; exit 0; fi
echo 'Login failed: Request failed with status code 400'
exit 1
`)
}

func signInManager(t *testing.T, allowed bool, readiness SessionReadiness) *Manager {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return NewManager(Options{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		StateDir:  t.TempDir(),
		Allowed:   func(string) bool { return allowed },
		Detector:  &apps.Detector{RunVersion: func(context.Context, string, []string) (string, error) { return "1", nil }},
		Readiness: readiness,
	})
}

func TestSignInFinishesAndAsksForACheck(t *testing.T) {
	fakeClaudeLogin(t)
	readiness := &recordingReadiness{}
	m := signInManager(t, true, readiness)
	sender := &signInSender{}
	m.StartSignIn(context.Background(), sender, &memqlv1.AppSignInStart{SignInId: "s1", App: apps.IDClaudeCode})
	waiting := sender.waitFor(t, signInWaiting)
	if waiting.GetUrl() != "https://claude.com/cai/oauth/authorize?code=true" || !waiting.GetNeedsCode() || waiting.GetExpiresAt() == nil {
		t.Fatalf("got %+v", waiting)
	}
	m.SubmitSignInCode(sender, &memqlv1.AppSignInInput{SignInId: "s1", Code: "good"})
	sender.waitFor(t, signInSucceeded)
	deadline := time.Now().Add(5 * time.Second)
	for {
		readiness.mu.Lock()
		rechecked := append([]string(nil), readiness.rechecked...)
		readiness.mu.Unlock()
		if len(rechecked) == 1 && rechecked[0] == apps.IDClaudeCode {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a finished sign-in did not ask for a check: %v", rechecked)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The machine's consent has the last word on the machine, whatever the engine read.
func TestSignInRefusesWhatThisMachinesPolicyDoesNotAllow(t *testing.T) {
	fakeClaudeLogin(t)
	m := signInManager(t, false, nil)
	sender := &signInSender{}
	m.StartSignIn(context.Background(), sender, &memqlv1.AppSignInStart{SignInId: "s1", App: apps.IDClaudeCode})
	failed := sender.waitFor(t, signInFailed)
	if !strings.Contains(failed.GetError(), "policy.yaml") {
		t.Fatalf("got %+v", failed)
	}
}

func TestSignInReportsTheAppsOwnFailureAndNeverTheCode(t *testing.T) {
	fakeClaudeLogin(t)
	m := signInManager(t, true, nil)
	sender := &signInSender{}
	m.StartSignIn(context.Background(), sender, &memqlv1.AppSignInStart{SignInId: "s1", App: apps.IDClaudeCode})
	sender.waitFor(t, signInWaiting)
	m.SubmitSignInCode(sender, &memqlv1.AppSignInInput{SignInId: "s1", Code: "wrong-secret-code"})
	failed := sender.waitFor(t, signInFailed)
	if !strings.Contains(failed.GetError(), "status code 400") || strings.Contains(failed.GetError(), "wrong-secret-code") {
		t.Fatalf("got %+v", failed)
	}
}

func TestSignInStopsWithTheStream(t *testing.T) {
	fakeClaudeLogin(t)
	m := signInManager(t, true, nil)
	sender := &signInSender{}
	m.StartSignIn(context.Background(), sender, &memqlv1.AppSignInStart{SignInId: "s1", App: apps.IDClaudeCode})
	sender.waitFor(t, signInWaiting)
	m.StopSignIns()
	deadline := time.Now().Add(10 * time.Second)
	for {
		m.mu.Lock()
		running := len(m.signIns)
		m.mu.Unlock()
		if running == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sign-in outlived its stream")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, update := range sender.snapshot() {
		if update.GetState() == signInSucceeded {
			t.Fatal("a stopped sign-in read as finished")
		}
	}
}

// A person pressing Sign in again has given up on the first one.
func TestANewerSignInReplacesAnOlderOne(t *testing.T) {
	fakeClaudeLogin(t)
	m := signInManager(t, true, nil)
	sender := &signInSender{}
	m.StartSignIn(context.Background(), sender, &memqlv1.AppSignInStart{SignInId: "s1", App: apps.IDClaudeCode})
	sender.waitFor(t, signInWaiting)
	m.StartSignIn(context.Background(), sender, &memqlv1.AppSignInStart{SignInId: "s2", App: apps.IDClaudeCode})
	deadline := time.Now().Add(10 * time.Second)
	for {
		m.mu.Lock()
		_, first := m.signIns["s1"]
		_, second := m.signIns["s2"]
		m.mu.Unlock()
		if !first && second {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("first alive: %v, second alive: %v", first, second)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// What an old Linux service unit otherwise makes a person discover after they have
// done everything: the page opens and the code is taken, and only then does the login
// fail to save. Measured under that unit on 2026-10-29.
func TestCheckLoginWritableSaysToUpdateTheService(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only mode")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".codex"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(home, ".codex"), 0o700) })
	err := checkLoginWritable(apps.IDCodex)
	if err == nil || !strings.Contains(err.Error(), "installer") {
		t.Fatalf("got %v", err)
	}
	if err := checkLoginWritable(apps.IDClaudeCode); err != nil {
		t.Fatalf("a writable home was refused: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(home, ".claude"))
	if len(entries) != 0 {
		t.Fatalf("the check left something behind: %v", entries)
	}
}

// A code that reaches a sign-in nobody is running is answered, not dropped: the person
// is waiting on the row.
func TestACodeForAnEndedSignInSaysSo(t *testing.T) {
	m := signInManager(t, true, nil)
	sender := &signInSender{}
	m.SubmitSignInCode(sender, &memqlv1.AppSignInInput{SignInId: "gone", Code: "x"})
	failed := sender.waitFor(t, signInFailed)
	if failed.GetSignInId() != "gone" {
		t.Fatalf("got %+v", failed)
	}
}
