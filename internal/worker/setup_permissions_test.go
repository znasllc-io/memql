package worker

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// The permissions stage (memql-cockpit#548): it waits for the person, and
// only the person's grant or their explicit choice ends the wait.

// statusScript answers the worker's status from a list, repeating the last.
type statusScript struct {
	mu    sync.Mutex
	steps []func() (LocalStatus, error)
	reads int
}

func (s *statusScript) status() (LocalStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := min(s.reads, len(s.steps)-1)
	s.reads++
	return s.steps[i]()
}

func reportsPermissions(accessibility, screen string) func() (LocalStatus, error) {
	return func() (LocalStatus, error) {
		return LocalStatus{Accessibility: accessibility, ScreenRecording: screen, PermissionRequests: true}, nil
	}
}

func answeredSkip(accessibility, screen string) func() (LocalStatus, error) {
	return func() (LocalStatus, error) {
		return LocalStatus{Accessibility: accessibility, ScreenRecording: screen, PermissionRequests: true,
			Setup: &LocalSetup{State: "running", Answers: map[string]SetupAnswer{"permissions": {Stage: "permissions", Choice: "skip"}}}}, nil
	}
}

func workerRestarting() (LocalStatus, error) {
	return LocalStatus{}, errors.New("background worker is not available")
}

type permissionsRun struct {
	code    int
	reports *recordedReports
	opened  int
	out     string
}

func runPermissions(t *testing.T, script *statusScript, interactive bool, typed <-chan struct{}, openErr error) permissionsRun {
	t.Helper()
	rec := &recordedReports{}
	var out bytes.Buffer
	run := permissionsRun{reports: rec}
	w := permissionsWait{
		status:      script.status,
		open:        func() error { run.opened++; return openErr },
		typed:       typed,
		interactive: interactive,
		every:       time.Millisecond,
		progress:    startSetupReporter(rec.send, time.Hour),
		out:         &out,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run.code = w.run(ctx)
	w.progress.Close()
	run.out = out.String()
	return run
}

func TestPermissionsAlreadyAllowedEndsTheStageAtOnce(t *testing.T) {
	run := runPermissions(t, &statusScript{steps: []func() (LocalStatus, error){reportsPermissions("Granted", "Granted")}}, true, nil, nil)
	if run.code != SetupExitOK || run.reports.states() != "permissions=done" || run.opened != 0 {
		t.Fatalf("both allowed: exit %d, reports %q, window opened %d times", run.code, run.reports.states(), run.opened)
	}
}

func TestPermissionsWaitThroughARestartUntilBothAreAllowed(t *testing.T) {
	script := &statusScript{steps: []func() (LocalStatus, error){
		reportsPermissions("Not granted", "Not granted"),
		reportsPermissions("Granted", "Not granted"),
		// Screen Recording needs the worker restarted to be seen.
		workerRestarting, workerRestarting,
		reportsPermissions("Granted", "Granted"),
	}}
	run := runPermissions(t, script, true, nil, nil)
	if run.code != SetupExitOK || run.reports.states() != "permissions=running permissions=done" {
		t.Fatalf("exit %d, reports %q", run.code, run.reports.states())
	}
	if run.opened != 1 || !strings.Contains(run.out, "turn on Accessibility and Screen Recording") {
		t.Fatalf("the window opens once and the terminal says what to turn on: opened %d\n%s", run.opened, run.out)
	}
}

func TestPermissionsSkippedInTheWindowEndsTheStageAsTheyChose(t *testing.T) {
	script := &statusScript{steps: []func() (LocalStatus, error){
		reportsPermissions("Not granted", "Not granted"),
		reportsPermissions("Granted", "Not granted"),
		answeredSkip("Granted", "Not granted"),
	}}
	run := runPermissions(t, script, true, nil, nil)
	if run.code != SetupExitPrereq || run.reports.states() != "permissions=running permissions=skipped" {
		t.Fatalf("exit %d, reports %q", run.code, run.reports.states())
	}
	if got := run.reports.last("permissions").Detail; got != "Continued without Screen Recording." {
		t.Fatalf("the stage ends in this command's own words for what was left off: %q", got)
	}
}

func TestPermissionsSkippedAtTheTerminal(t *testing.T) {
	typed := make(chan struct{})
	close(typed)
	run := runPermissions(t, &statusScript{steps: []func() (LocalStatus, error){reportsPermissions("Not granted", "Not granted")}}, true, typed, nil)
	if run.code != SetupExitPrereq || run.reports.last("permissions").Detail != "Skipped for now." {
		t.Fatalf("exit %d, last %+v", run.code, run.reports.last("permissions"))
	}
	if !strings.Contains(run.out, "press Return here") {
		t.Fatalf("a terminal that can answer is told how:\n%s", run.out)
	}
}

func TestPermissionsWithNobodyToAskAreNotAsked(t *testing.T) {
	run := runPermissions(t, &statusScript{steps: []func() (LocalStatus, error){reportsPermissions("Not granted", "Not granted")}}, false, nil, nil)
	if run.code != SetupExitRefused || run.opened != 0 || run.reports.states() != "permissions=failed" {
		t.Fatalf("no terminal: exit %d, opened %d, reports %q", run.code, run.opened, run.reports.states())
	}
	if !strings.HasPrefix(run.reports.last("permissions").Detail, "Not asked") {
		t.Fatalf("a question nobody was there to answer is not a choice they made: %q", run.reports.last("permissions").Detail)
	}
}

func TestPermissionsTheWorkerCannotAnswer(t *testing.T) {
	if run := runPermissions(t, &statusScript{steps: []func() (LocalStatus, error){workerRestarting}}, true, nil, nil); run.code != SetupExitOpFailed || run.reports.states() != "" {
		t.Fatalf("no worker: exit %d, reports %q", run.code, run.reports.states())
	}
	cannot := func() (LocalStatus, error) {
		return LocalStatus{Accessibility: "Unknown", ScreenRecording: "Unknown"}, nil
	}
	if run := runPermissions(t, &statusScript{steps: []func() (LocalStatus, error){cannot}}, true, nil, nil); run.code != SetupExitOpFailed || run.reports.states() != "permissions=failed" {
		t.Fatalf("a build that cannot ask: exit %d, reports %q", run.code, run.reports.states())
	}
}

func TestPermissionsWindowThatDoesNotOpenStillWaits(t *testing.T) {
	script := &statusScript{steps: []func() (LocalStatus, error){
		reportsPermissions("Not granted", "Not granted"),
		reportsPermissions("Granted", "Granted"),
	}}
	run := runPermissions(t, script, true, nil, errors.New("no handler"))
	if run.code != SetupExitOK || !strings.Contains(run.out, "Set Up Permissions") {
		t.Fatalf("exit %d; the person is told where else to find it:\n%s", run.code, run.out)
	}
}

func TestPermissionsSkippedSentence(t *testing.T) {
	for _, tc := range []struct{ accessibility, screen, want string }{
		{"Not granted", "Not granted", "Skipped for now."},
		{"Granted", "Not granted", "Continued without Screen Recording."},
		{"Not granted", "Granted", "Continued without Accessibility."},
		{"Unknown", "Granted", "Continued without Accessibility."},
	} {
		if got := permissionsSkippedSentence(LocalStatus{Accessibility: tc.accessibility, ScreenRecording: tc.screen}); got != tc.want {
			t.Errorf("%s/%s: %q, want %q", tc.accessibility, tc.screen, got, tc.want)
		}
	}
}
