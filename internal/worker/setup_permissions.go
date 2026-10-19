package worker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// `worker setup --permissions`: the installer's first stage on a computer-use
// Mac (memql-cockpit#548).
//
// ===========================================================================
// NOTHING DOWNLOADS BEFORE THE PERSON HAS DECIDED
// ===========================================================================
// The installer used to go from starting the worker straight to installing
// the model runtime and pulling eighteen gigabytes, and the Permissions
// window opened only once per version, by itself, if at all. A person who
// had asked for computer use found out an hour later that nothing could use
// the screen. This command is the stage that comes first: it opens the
// Permissions window and waits until Accessibility and Screen Recording are
// both allowed, or until the person chooses to go on without them -- in the
// window (Skip for now, or Continue without Screen Recording) or by pressing
// Return in the terminal. Neither silence nor a timeout decides for them.
//
// THE COMMAND REPORTS THE STAGE; THE PERSON ONLY ANSWERS. The window sends
// its choice as an answer on the control socket (setupprogress.go), this
// command reads it back from the worker's status and reports how the stage
// ended, so there is one author of the stage and its keepalive can never
// overwrite the choice.
//
// THE WORKER IS THE ONE THAT HOLDS THE GRANTS. macOS grants Accessibility
// and Screen Recording to MemQL.app's own process, not to this one, so the
// grant is read from the running worker's status and never probed here. A
// worker that stops answering for a while is the restart Screen Recording
// asks for, and the wait goes on through it.

// Exit codes, in setup_exit.go's vocabulary: 0 both allowed; 3 nobody could
// be asked (no terminal); 4 the person went on without one or both; 5 the
// worker could not be read, or this build cannot ask.

// permissionsWait is the stage's decision, with every seam the machine has
// -- the worker's status, the window, the terminal, the clock -- passed in,
// so the whole of it is testable without a Mac.
type permissionsWait struct {
	status func() (LocalStatus, error)
	// open shows the Permissions window; nil when the install has no menu
	// companion, and the person is sent to System Settings instead.
	open func() error
	// typed fires when the person presses Return at the terminal; nil when
	// nobody can type.
	typed <-chan struct{}
	// interactive is false for an install with nobody at a terminal: the
	// question is not asked at all rather than left waiting forever.
	interactive bool
	every       time.Duration
	progress    *setupReporter
	out         io.Writer
}

const (
	// permissionsPollEvery is how often the worker's status is read while
	// the person decides. The window reads it every second while System
	// Settings is open; this need be no faster.
	permissionsPollEvery = time.Second
	// permissionsStage is the stage's name on the report.
	permissionsStage = "permissions"
	// permissionsLater is where a permission left off is turned on later.
	permissionsLater = "MemQL in the menu bar, then Set Up Permissions…"
)

func (w permissionsWait) run(ctx context.Context) int {
	st, err := w.status()
	if err != nil {
		w.say("The MemQL worker is not answering, so its permissions cannot be checked now. Set them up later from " + permissionsLater + ".")
		recordSetupReason("Permissions not checked: the MemQL worker is not answering.")
		return SetupExitOpFailed
	}
	if permissionsAllowed(st) {
		return w.allowed()
	}
	if !st.PermissionRequests {
		detail := "This Cockpit build cannot ask macOS for permissions."
		w.progress.Stage(permissionsStage, "failed", detail)
		w.say(detail + " Update Cockpit, then set them up from " + permissionsLater + ".")
		recordSetupReason(detail)
		return SetupExitOpFailed
	}
	if !w.interactive {
		detail := "Not asked: this install ran without a terminal. Set them up from " + permissionsLater + "."
		w.progress.Stage(permissionsStage, "failed", detail)
		w.say(detail)
		recordSetupReason(detail)
		return SetupExitRefused
	}

	w.progress.Stage(permissionsStage, "running", "")
	w.say("Allow MemQL to use this Mac.")
	if w.open == nil {
		w.say("Turn MemQL on under System Settings, Privacy & Security, in Accessibility and in Screen Recording, then restart MemQL: launchctl kickstart -k gui/$(id -u)/com.znasllc.memql-worker")
	} else if err := w.open(); err != nil {
		w.say(fmt.Sprintf("The Permissions window did not open (%v). Open it from %s.", err, permissionsLater))
	} else {
		w.say("In the Permissions window, turn on " + missingPermissions(st) + ".")
	}
	if w.typed != nil {
		w.say("Setup continues once both are on. To go on without them for now, choose Skip there, or press Return here.")
	} else {
		w.say("Setup continues once both are on. To go on without them for now, choose Skip there.")
	}

	tick := time.NewTicker(w.every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return SetupExitOpFailed
		case <-w.typed:
			return w.skipped(st)
		case <-tick.C:
			next, err := w.status()
			if err != nil {
				// The worker restarting to see Screen Recording: the next
				// one answers within seconds.
				continue
			}
			st = next
			if permissionsAllowed(st) {
				return w.allowed()
			}
			if st.Setup != nil && st.Setup.Answers[permissionsStage].Choice == "skip" {
				return w.skipped(st)
			}
		}
	}
}

func (w permissionsWait) allowed() int {
	w.progress.Stage(permissionsStage, "done", "")
	w.say("Accessibility and Screen Recording are allowed.")
	return SetupExitOK
}

// skipped ends the stage as the person chose: on without what is not allowed
// yet. The sentence is this command's own, from what the worker reports at
// the moment of the choice, so the window and the terminal end it alike.
func (w permissionsWait) skipped(st LocalStatus) int {
	detail := permissionsSkippedSentence(st)
	w.progress.Stage(permissionsStage, "skipped", detail)
	w.say(detail + " Turn it on later from " + permissionsLater + ".")
	recordSetupReason(detail)
	return SetupExitPrereq
}

func (w permissionsWait) say(text string) {
	if w.out == nil {
		return
	}
	for _, line := range wrapText(text, inferenceWrapWidth) {
		fmt.Fprintln(w.out, line)
	}
}

func permissionsAllowed(st LocalStatus) bool {
	return st.Accessibility == "Granted" && st.ScreenRecording == "Granted"
}

// missingPermissions names what is not allowed yet, "Accessibility and
// Screen Recording" when it is both.
func missingPermissions(st LocalStatus) string {
	var missing []string
	if st.Accessibility != "Granted" {
		missing = append(missing, "Accessibility")
	}
	if st.ScreenRecording != "Granted" {
		missing = append(missing, "Screen Recording")
	}
	return strings.Join(missing, " and ")
}

// permissionsSkippedSentence is how a skipped stage ends: "Skipped for now."
// when neither is allowed, otherwise what setup went on without.
func permissionsSkippedSentence(st LocalStatus) string {
	missing := missingPermissions(st)
	if missing == "" || strings.Contains(missing, " and ") {
		return "Skipped for now."
	}
	return "Continued without " + missing + "."
}

// runPermissionsSetup is `worker setup --permissions` on this machine.
func runPermissionsSetup(noWindow, nonInteractive bool) int {
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(os.Stderr, "--permissions sets up macOS permissions for computer use; this machine has none to set up.")
		return SetupExitUsage
	}
	w := permissionsWait{
		status: func() (LocalStatus, error) {
			response, err := callControl(defaultControlPath(), controlRequest{Action: "status"})
			if err != nil {
				return LocalStatus{}, err
			}
			if !response.OK || response.Status == nil {
				return LocalStatus{}, fmt.Errorf("the worker did not report its status: %s", response.Error)
			}
			return *response.Status, nil
		},
		interactive: !nonInteractive,
		every:       permissionsPollEvery,
		progress:    newSetupReporter(sendSetupControl),
		out:         os.Stdout,
	}
	if !noWindow {
		w.open = func() error { return exec.Command("/usr/bin/open", "memql-cockpit://permissions").Run() }
	}
	if !nonInteractive {
		typed := make(chan struct{})
		go func() {
			// A line at the terminal is the person's answer. End of input
			// is not: stdin closed is nobody typing.
			if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err == nil {
				close(typed)
			}
		}()
		w.typed = typed
	}
	code := w.run(context.Background())
	w.progress.Close()
	return code
}
