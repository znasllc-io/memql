package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// signin.go runs each app's own login, so a person can finish it from wherever they
// are looking at Fleet.
//
// NO BROWSER OPENS ON THE MACHINE, and the machine's owner may not be sitting at it.
// Claude Code honours BROWSER, and Codex's device flow opens none. The page opens in
// the person's own browser, on any computer.
//
// CREDENTIALS STAY ON THE MACHINE. The app writes its own login there. What leaves is
// the page's address and a device code, and what comes back is a code the page showed,
// which is useless without the verifier this process holds. None of it is logged here,
// and an error carries the app's sentence and never the code.

// signInStartTimeout bounds how long an app may take to say where to sign in.
const signInStartTimeout = 30 * time.Second

// SignInLifetime is how long an app waits for a person. Codex prints 15 minutes;
// Claude Code does not say, and this is the longest either is given.
const SignInLifetime = 15 * time.Minute

// SignInPrompt is what a person needs to finish a sign-in.
type SignInPrompt struct {
	URL string
	// UserCode is what they enter on that page, for a device login.
	UserCode string
	// NeedsCode means the page shows a code to paste back.
	NeedsCode bool
}

// SignIn is one login in progress.
type SignIn interface {
	Prompt() SignInPrompt
	// SubmitCode passes on what the person pasted.
	SubmitCode(code string) error
	// Wait blocks until the app finishes, nil meaning it reports a login.
	Wait() error
	// Stop ends it. Safe to call at any time and more than once.
	Stop()
}

// claudeLoginPage finds the address in "If the browser didn't open, visit: <url>", the
// manual one whose page shows a code. The browser Claude Code would have opened
// redirects to localhost, which reaches nothing from another computer.
var claudeLoginPage = regexp.MustCompile(`https://\S+`)

// claudeLoginPrompt is what Claude Code prints when it starts reading a code.
const claudeLoginPrompt = "Paste code here if prompted >"

type claudeSignIn struct {
	proc   Process
	prompt SignInPrompt

	mu   sync.Mutex
	tail string
	done chan struct{}
}

// StartClaudeSignIn runs `claude auth login` and returns once it has printed the page.
//
// --claudeai IS NOT A DEFAULT TO LEAVE IMPLICIT: this is the subscription, and the
// other login bills an API account. A plain pipe is enough for the pasted code
// (measured on 2.1.304), so no terminal is involved.
func StartClaudeSignIn(ctx context.Context, spec CheckSpec) (SignIn, error) {
	env := append(claudeEnv(nil), "BROWSER=true")
	proc, err := spec.Launch(ctx, spec.Dir, []string{spec.Binary, "auth", "login", "--claudeai"}, env, true)
	if err != nil {
		return nil, fmt.Errorf("could not start Claude Code's sign-in: %w", err)
	}
	s := &claudeSignIn{proc: proc, prompt: SignInPrompt{NeedsCode: true}, done: make(chan struct{})}
	found := make(chan string, 1)
	go s.pump(found)
	select {
	case url := <-found:
		if url == "" {
			err := s.Wait()
			if err == nil {
				err = errors.New("Claude Code ended its sign-in without saying where to go")
			}
			return nil, err
		}
		s.prompt.URL = url
		return s, nil
	case <-ctx.Done():
		proc.Terminate()
		return nil, fmt.Errorf("Claude Code's sign-in did not start: %w", ctx.Err())
	case <-time.After(signInStartTimeout):
		proc.Terminate()
		return nil, errors.New("Claude Code did not say where to sign in within 30 seconds")
	}
}

// pump reads both streams to their end. The last line said is the one an error needs.
func (s *claudeSignIn) pump(found chan<- string) {
	var wg sync.WaitGroup
	once := sync.Once{}
	report := func(url string) { once.Do(func() { found <- url }) }
	read := func(src io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(src)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			// The prompt has no newline, so it arrives glued to whatever follows it,
			// or alone when stdout closes after the failure already went to stderr.
			line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(scanner.Text()), claudeLoginPrompt))
			if line == "" {
				continue
			}
			if strings.Contains(line, "visit:") {
				if url := claudeLoginPage.FindString(line); url != "" {
					report(url)
					continue
				}
			}
			s.mu.Lock()
			s.tail = line
			s.mu.Unlock()
		}
	}
	wg.Add(2)
	go read(s.proc.Stdout())
	go read(s.proc.Stderr())
	wg.Wait()
	report("")
	close(s.done)
}

func (s *claudeSignIn) Prompt() SignInPrompt { return s.prompt }

func (s *claudeSignIn) SubmitCode(code string) error {
	stdin := s.proc.Stdin()
	if stdin == nil {
		return errors.New("Claude Code's sign-in is not reading a code")
	}
	if _, err := io.WriteString(stdin, code+"\n"); err != nil {
		return errors.New("Claude Code's sign-in has already ended")
	}
	return nil
}

func (s *claudeSignIn) Wait() error {
	<-s.done
	if err := s.proc.Wait(); err != nil {
		s.mu.Lock()
		tail := s.tail
		s.mu.Unlock()
		if tail != "" {
			return errors.New(tail)
		}
		return fmt.Errorf("Claude Code's sign-in ended without a login: %w", err)
	}
	return nil
}

func (s *claudeSignIn) Stop() { s.proc.Terminate() }

// codexMethodLoginStart and its neighbours are the login calls. Source: `codex
// app-server generate-json-schema`, codex-cli 0.162.1.
const (
	codexMethodLoginStart    = "account/login/start"
	codexMethodLoginCancel   = "account/login/cancel"
	codexNotifyLoginComplete = "account/login/completed"
)

type codexSignIn struct {
	conn    *jsonrpcConn
	prompt  SignInPrompt
	loginID string

	mu       sync.Mutex
	finished bool
	result   error
	done     chan struct{}
}

// StartCodexSignIn asks Codex's app-server for a device login. It is the app's own
// protocol, so the address and the code arrive as fields and nothing is read off a
// terminal, and the login is written where Codex keeps it, by Codex.
func StartCodexSignIn(ctx context.Context, spec CheckSpec) (SignIn, error) {
	proc, err := spec.Launch(ctx, spec.Dir, []string{spec.Binary, "app-server"}, nil, true)
	if err != nil {
		return nil, fmt.Errorf("could not start Codex's sign-in: %w", err)
	}
	s := &codexSignIn{done: make(chan struct{})}
	// A completion can land before the start's answer is read, and the login id it
	// names is only known after, so the notification is matched in finish.
	var early *codexLoginCompleted
	var earlyMu sync.Mutex
	s.conn = newJSONRPCConn(proc, func(method string, params json.RawMessage, _ []byte) {
		if method != codexNotifyLoginComplete {
			return
		}
		var n codexLoginCompleted
		if json.Unmarshal(params, &n) != nil {
			return
		}
		earlyMu.Lock()
		known := s.loginID != ""
		if !known {
			early = &n
		}
		earlyMu.Unlock()
		if known {
			s.finish(n)
		}
	})
	fail := func(err error) (SignIn, error) {
		s.conn.close()
		return nil, err
	}
	startCtx, cancel := context.WithTimeout(ctx, signInStartTimeout)
	defer cancel()
	if _, err := s.conn.call(startCtx, "codex sign-in", codexMethodInitialize, codexInitializeParams()); err != nil {
		return fail(fmt.Errorf("Codex's app-server did not answer: %w", err))
	}
	if err := s.conn.notifyServer(codexMethodInitialized, nil); err != nil {
		return fail(fmt.Errorf("Codex's app-server did not answer: %w", err))
	}
	raw, err := s.conn.call(startCtx, "codex sign-in", codexMethodLoginStart, map[string]any{"type": "chatgptDeviceCode"})
	if err != nil {
		var refusal *rpcError
		if errors.As(err, &refusal) && strings.Contains(strings.ToLower(refusal.Message), "variant") {
			return fail(errors.New("This Codex cannot sign in by device code. Update Codex on that machine, then sign in again."))
		}
		return fail(fmt.Errorf("Codex refused the sign-in: %w", err))
	}
	var started struct {
		LoginID         string `json:"loginId"`
		VerificationURL string `json:"verificationUrl"`
		UserCode        string `json:"userCode"`
	}
	if err := json.Unmarshal(raw, &started); err != nil || started.VerificationURL == "" || started.UserCode == "" {
		return fail(errors.New("Codex answered the sign-in with something this build cannot read"))
	}
	s.prompt = SignInPrompt{URL: started.VerificationURL, UserCode: started.UserCode}
	earlyMu.Lock()
	s.loginID = started.LoginID
	pending := early
	earlyMu.Unlock()
	if pending != nil {
		s.finish(*pending)
	}
	go func() {
		// The process ending without a completion is the end of this login too.
		<-s.conn.dead
		s.finishWith(errors.New("Codex's sign-in ended without a login"))
	}()
	return s, nil
}

// codexLoginCompleted is AccountLoginCompletedNotification.
type codexLoginCompleted struct {
	LoginID *string `json:"loginId"`
	Success bool    `json:"success"`
	Error   *string `json:"error"`
}

// finish files a completion, if it is this login's.
func (s *codexSignIn) finish(n codexLoginCompleted) {
	if n.LoginID != nil && *n.LoginID != s.loginID {
		return
	}
	if n.Success {
		s.finishWith(nil)
		return
	}
	reason := "Codex's sign-in failed"
	if n.Error != nil && strings.TrimSpace(*n.Error) != "" {
		reason = strings.TrimSpace(*n.Error)
	}
	s.finishWith(errors.New(reason))
}

func (s *codexSignIn) finishWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finished = true
	s.result = err
	close(s.done)
}

func (s *codexSignIn) Prompt() SignInPrompt { return s.prompt }

func (s *codexSignIn) SubmitCode(string) error {
	return errors.New("Codex's sign-in finishes on its own page and takes no code here")
}

func (s *codexSignIn) Wait() error {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

// Stop cancels the login and ends the process, which is what stops Codex polling. The
// cancel is best effort for a server that may be gone already.
func (s *codexSignIn) Stop() {
	s.mu.Lock()
	finished := s.finished
	s.mu.Unlock()
	if !finished {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = s.conn.call(ctx, "codex sign-in", codexMethodLoginCancel, map[string]any{"loginId": s.loginID})
		cancel()
	}
	s.conn.close()
	s.finishWith(errors.New("the sign-in was stopped"))
}
