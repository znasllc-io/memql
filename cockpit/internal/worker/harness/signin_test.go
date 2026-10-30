package harness

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// signin_test.go drives each app's login against a fake that prints what the real one
// printed (Claude Code 2.1.304 and codex-cli 0.162.1, measured 2026-10-29 in scratch
// config directories, with the browser shimmed and a fabricated pasted code).

const recordedClaudeLoginPage = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=user%3Ainference&code_challenge=abc&code_challenge_method=S256&state=def"

// fakeClaudeLogin prints the recorded prompt, then answers the code it is given.
func fakeClaudeLogin(t *testing.T) (binary, log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "login.log")
	script := "#!/bin/sh\n" +
		"printf '%s|BROWSER=%s\\n' \"$*\" \"$BROWSER\" >> '" + log + "'\n" +
		"echo 'Opening browser to sign in…'\n" +
		"echo \"If the browser didn't open, visit: " + recordedClaudeLoginPage + "\"\n" +
		"printf 'Paste code here if prompted > '\n" +
		"IFS= read -r code\n" +
		"printf 'read:%s\\n' \"$code\" >> '" + log + "'\n" +
		"if [ \"$code\" = 'good#code' ]; then echo 'Login successful.'; exit 0; fi\n" +
		"echo 'Login failed: Request failed with status code 400'\n" +
		"exit 1\n"
	return fakeBinary(t, "claude", script), log
}

func signInSpec(t *testing.T, binary string) CheckSpec {
	t.Helper()
	return CheckSpec{Binary: binary, Dir: t.TempDir(), Launch: testLaunch}
}

func TestClaudeSignInFinishesWithThePastedCode(t *testing.T) {
	binary, log := fakeClaudeLogin(t)
	session, err := StartClaudeSignIn(context.Background(), signInSpec(t, binary))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Stop()
	prompt := session.Prompt()
	if prompt.URL != recordedClaudeLoginPage || !prompt.NeedsCode || prompt.UserCode != "" {
		t.Fatalf("got %+v", prompt)
	}
	if err := session.SubmitCode("good#code"); err != nil {
		t.Fatal(err)
	}
	if err := session.Wait(); err != nil {
		t.Fatalf("got %v", err)
	}
	lines := commandLines(t, log)
	// No browser opens on a machine its owner may not be sitting at, and the login is
	// the subscription's, never the console's API billing.
	if !strings.Contains(lines[0], "auth login --claudeai") || !strings.Contains(lines[0], "BROWSER=true") {
		t.Fatalf("got %q", lines[0])
	}
	if lines[1] != "read:good#code" {
		t.Fatalf("got %q", lines[1])
	}
}

func TestClaudeSignInReportsTheAppsOwnFailureWithoutTheCode(t *testing.T) {
	binary, _ := fakeClaudeLogin(t)
	session, err := StartClaudeSignIn(context.Background(), signInSpec(t, binary))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Stop()
	if err := session.SubmitCode("expired-code-value"); err != nil {
		t.Fatal(err)
	}
	err = session.Wait()
	if err == nil || !strings.Contains(err.Error(), "status code 400") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "expired-code-value") {
		t.Fatalf("a pasted code reached an error: %v", err)
	}
}

// Measured on 2.1.304: the prompt has no newline, so it arrives when stdout closes,
// after the failure already went to stderr. The sentence must survive that.
func TestClaudeSignInFailureSurvivesThePromptArrivingLast(t *testing.T) {
	script := "#!/bin/sh\n" +
		"echo \"If the browser didn't open, visit: " + recordedClaudeLoginPage + "\"\n" +
		"printf 'Paste code here if prompted > '\n" +
		"IFS= read -r code\n" +
		"echo 'Login failed: Request failed with status code 400' >&2\n" +
		"sleep 0.2\n" +
		"exit 1\n"
	session, err := StartClaudeSignIn(context.Background(), signInSpec(t, fakeBinary(t, "claude", script)))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Stop()
	if err := session.SubmitCode("x"); err != nil {
		t.Fatal(err)
	}
	if err := session.Wait(); err == nil || !strings.Contains(err.Error(), "status code 400") {
		t.Fatalf("got %v", err)
	}
}

func TestClaudeSignInThatNeverPrintsAPageIsAnError(t *testing.T) {
	binary := fakeBinary(t, "claude", "#!/bin/sh\necho 'unknown option --claudeai' >&2\nexit 1\n")
	_, err := StartClaudeSignIn(context.Background(), signInSpec(t, binary))
	if err == nil || !strings.Contains(err.Error(), "unknown option") {
		t.Fatalf("got %v", err)
	}
}

func TestClaudeSignInStopsWhenStopped(t *testing.T) {
	binary, _ := fakeClaudeLogin(t)
	session, err := StartClaudeSignIn(context.Background(), signInSpec(t, binary))
	if err != nil {
		t.Fatal(err)
	}
	session.Stop()
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stopped sign-in read as finished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the process outlived Stop")
	}
}

// fakeCodexLogin answers account/login/start as the app-server does, and reports the
// completion it is told to.
func fakeCodexLogin(t *testing.T, start, completed string) (binary, log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "wire.log")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"app-server\" ]; then exit 64; fi\n" +
		"while IFS= read -r line; do\n" +
		"  printf '%s\\n' \"$line\" >> '" + log + "'\n" +
		"  id=$(printf '%s' \"$line\" | sed -n 's/^{\"jsonrpc\":\"2.0\",\"id\":\\([0-9]*\\),.*/\\1/p')\n" +
		"  case \"$line\" in\n" +
		"    *'\"method\":\"initialize\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{}}\\n' \"$id\" ;;\n" +
		"    *'\"method\":\"account/login/start\"'*)\n" +
		"      cat <<START_JSON\n{\"jsonrpc\":\"2.0\",\"id\":$id," + start + "}\nSTART_JSON\n" +
		"      sleep 0.1\n" +
		"      cat <<'DONE_JSON'\n" + completed + "\nDONE_JSON\n" +
		"      ;;\n" +
		"    *'\"method\":\"account/login/cancel\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{}}\\n' \"$id\" ;;\n" +
		"  esac\n" +
		"done\n"
	return fakeBinary(t, "codex", script), log
}

const recordedCodexDeviceStart = `"result":{"type":"chatgptDeviceCode","loginId":"login-1","verificationUrl":"https://auth.openai.com/codex/device","userCode":"ABCD-EFGH"}`

func TestCodexSignInReportsTheDeviceCodeAndItsEnd(t *testing.T) {
	binary, log := fakeCodexLogin(t, recordedCodexDeviceStart, `{"jsonrpc":"2.0","method":"account/login/completed","params":{"loginId":"login-1","success":true,"error":null}}`)
	session, err := StartCodexSignIn(context.Background(), signInSpec(t, binary))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Stop()
	prompt := session.Prompt()
	if prompt.URL != "https://auth.openai.com/codex/device" || prompt.UserCode != "ABCD-EFGH" || prompt.NeedsCode {
		t.Fatalf("got %+v", prompt)
	}
	if err := session.Wait(); err != nil {
		t.Fatalf("got %v", err)
	}
	if wire := strings.Join(commandLines(t, log), "\n"); !strings.Contains(wire, `"type":"chatgptDeviceCode"`) {
		t.Fatalf("got %s", wire)
	}
	if err := session.SubmitCode("anything"); err == nil {
		t.Fatal("a device login takes no pasted code")
	}
}

func TestCodexSignInReportsAFailedCompletion(t *testing.T) {
	binary, _ := fakeCodexLogin(t, recordedCodexDeviceStart, `{"jsonrpc":"2.0","method":"account/login/completed","params":{"loginId":"login-1","success":false,"error":"device code expired"}}`)
	session, err := StartCodexSignIn(context.Background(), signInSpec(t, binary))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Stop()
	if err := session.Wait(); err == nil || !strings.Contains(err.Error(), "device code expired") {
		t.Fatalf("got %v", err)
	}
}

func TestCodexSignInIgnoresAnotherLoginsCompletion(t *testing.T) {
	binary, _ := fakeCodexLogin(t, recordedCodexDeviceStart, `{"jsonrpc":"2.0","method":"account/login/completed","params":{"loginId":"someone-else","success":true,"error":null}}`)
	session, err := StartCodexSignIn(context.Background(), signInSpec(t, binary))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("finished on another login's completion: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	session.Stop()
	if err := <-done; err == nil {
		t.Fatal("a stopped sign-in read as finished")
	}
}

// An older Codex has no device-code login on its app-server. It says so as a refusal,
// and the person must read what to do.
func TestCodexSignInOnAnOlderCodexSaysToUpdateIt(t *testing.T) {
	binary, _ := fakeCodexLogin(t, `"error":{"code":-32600,"message":"Invalid request: unknown variant chatgptDeviceCode"}`, `{"jsonrpc":"2.0","method":"noop"}`)
	_, err := StartCodexSignIn(context.Background(), signInSpec(t, binary))
	if err == nil || !strings.Contains(err.Error(), "Update Codex") {
		t.Fatalf("got %v", err)
	}
}
