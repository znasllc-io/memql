//go:build computeruse

package worker

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/go-vgo/robotgo"

	"github.com/znasllc-io/memql/cockpit/internal/worker/tools"
)

// runSetupWizard is the macOS / Linux X11 permissions pre-flight.
//
// Dispatches between two implementations:
//
//   - Single-panel TUI (cli/ui + cli/canvas, see wizard_computeruse.go).
//     The default path on every interactive terminal session.
//   - Plain printf, with the same probe logic. Used when stdin or
//     stdout is not a TTY (CI, install scripts, piped output) so
//     scripted callers don't choke on tcell escape codes.
//
// The printf flow IS the setup UX since the TUI left (memql#4552):
// plain sequential output, safe for terminals, CI and pipes alike.
//
// macOS notes:
//
//   - TCC gates Accessibility + Screen Recording per signed-binary
//     identity. A command-line binary launched from Terminal
//     INHERITS Terminal's grants -- the wizard might say "ok" even
//     when the binary itself would be denied if launched as a
//     LaunchAgent at login. Both the TUI + printf paths report the
//     per-binary `tccutil check` result so the user can tell the
//     cases apart.
//   - Probes the actual gated operations (not just reads):
//     Accessibility    -> robotgo.MoveRelative(2,0) + back
//     (CGEventPost; denied -> silent no-op)
//     Screen Recording -> robotgo.SaveCapture; denied capture
//     comes back tiny / empty
func runSetupWizard() error {
	return runSetupPrintf()
}

// runSetupPrintf is the non-TTY fallback. Same probe logic, just
// rendered as scrolling stdout text instead of a tcell single
// panel.
func runSetupPrintf() error {
	fmt.Println("memql worker setup")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	switch runtime.GOOS {
	case "darwin":
		return runSetupMacOS()
	case "linux":
		return runSetupLinux()
	default:
		fmt.Printf("INFO: setup wizard not implemented for %s; the worker will probe permissions at first run.\n", runtime.GOOS)
		return nil
	}
}

func runSetupMacOS() error {
	binPath, _ := os.Executable()
	if binPath != "" {
		if abs, err := filepath.Abs(binPath); err == nil {
			binPath = abs
		}
	}

	fmt.Println("Binary:", binPath)
	fmt.Println()
	fmt.Println("Note: macOS grants Accessibility + Screen Recording PER")
	fmt.Println("BINARY. Running this wizard from Terminal probes whether")
	fmt.Println("the call SUCCEEDS, which can be true because Terminal")
	fmt.Println("already has the permission and child processes inherit.")
	fmt.Println("When the LaunchAgent runs memql detached at login,")
	fmt.Println("the binary needs ITS OWN entry under System Settings ->")
	fmt.Println("Privacy & Security. The wizard now also reports the per-")
	fmt.Println("binary status from `tccutil` so you can tell the two")
	fmt.Println("cases apart.")
	fmt.Println()

	fmt.Println("Step 1/3: Accessibility")
	fmt.Println(strings.Repeat("-", 60))
	accessOK, accessDetail := probeAccessibility()
	fmt.Printf("  active probe: %s\n", accessDetail)
	bundleHasAccess := tccCheck("Accessibility", binPath)
	fmt.Printf("  per-binary grant: %s\n", bundleHasAccess)
	if !accessOK {
		fmt.Println("  Accessibility preflight: FAIL")
		fmt.Println()
		fmt.Println("  Accessibility is DENIED in this process tree.")
		fmt.Println("  Open System Settings -> Privacy & Security -> Accessibility")
		fmt.Println("  and approve EITHER memql OR your terminal app.")
		fmt.Println("  If you plan to run as a LaunchAgent later, you must")
		fmt.Println("  approve the memql binary directly.")
		fmt.Println()
		if !waitForKey("Press Enter after approving (or Ctrl-C to abort)...") {
			return setupPrereq("setup cancelled before the grant was confirmed")
		}
		// Re-probe after approval.
		accessOK, accessDetail = probeAccessibility()
		fmt.Printf("  re-probe: %s\n", accessDetail)
		if !accessOK {
			return setupPrereq("Accessibility still denied after approval -- restart the binary or check System Settings")
		}
	}
	fmt.Println("  Accessibility preflight: PASS")
	fmt.Println()

	fmt.Println("Step 2/3: Screen Recording")
	fmt.Println(strings.Repeat("-", 60))
	srOK, srDetail := probeScreenRecording()
	fmt.Printf("  active probe: %s\n", srDetail)
	bundleHasSR := tccCheck("ScreenCapture", binPath)
	fmt.Printf("  per-binary grant: %s\n", bundleHasSR)
	if !srOK {
		fmt.Println("  Screen Recording preflight: FAIL")
		fmt.Println()
		fmt.Println("  Screen Recording is DENIED in this process tree.")
		fmt.Println("  Open System Settings -> Privacy & Security -> Screen Recording")
		fmt.Println("  and approve EITHER memql OR your terminal app.")
		fmt.Println("  If you plan to run as a LaunchAgent later, you must")
		fmt.Println("  approve the memql binary directly.")
		fmt.Println()
		if !waitForKey("Press Enter after approving (or Ctrl-C to abort)...") {
			return setupPrereq("setup cancelled before the grant was confirmed")
		}
		srOK, srDetail = probeScreenRecording()
		fmt.Printf("  re-probe: %s\n", srDetail)
		if !srOK {
			return setupPrereq("Screen Recording still denied after approval -- macOS sometimes requires a binary restart; re-run setup")
		}
	}
	fmt.Println("  Screen Recording preflight: PASS")
	fmt.Println()

	fmt.Println("Step 3/3: Validate")
	fmt.Println(strings.Repeat("-", 60))
	width, height := robotgo.GetScreenSize()
	x, y := robotgo.Location()
	fmt.Printf("  display:   %dx%d\n", width, height)
	fmt.Printf("  cursor:    (%d,%d)\n", x, y)
	fmt.Println()
	fmt.Println("SUCCESS: permission checks passed for this setup process.")
	fmt.Println("This does not verify the background worker's permissions.")
	fmt.Println("Fleet reports the running worker's own passive checks every 15 seconds.")
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Enroll a home in ~/.memql/workers.yaml (cluster_url + token).")
	fmt.Println("  2. Run `memql worker run` to start serving.")
	fmt.Println()
	fmt.Println("If you'll run this as a LaunchAgent: install via")
	fmt.Println("`scripts/install/install-mac.sh` -- it loads the agent and")
	fmt.Println("approve the installed memql binary in System Settings ->")
	fmt.Println("Privacy & Security -> Accessibility and Screen Recording.")
	fmt.Println("The background worker checks silently; it does not request grants.")
	fmt.Println("Restart the worker if macOS requires it after a permission change.")
	return nil
}

// probeAccessibility issues a real CGEventPost (mouse-relative
// move) and verifies the cursor actually moved. Reading
// robotgo.Location() doesn't require Accessibility on macOS so a
// pure read isn't a meaningful probe.
//
// Wrapped in a 5-second timeout for the same reason
// probeScreenRecording is: the underlying CGO calls can block
// indefinitely on a TCC-state mismatch and we'd rather time out
// with a clear message than freeze the wizard.
//
// Returns (granted, human-readable detail).
//
// On darwin the active probe is combined with the AXIsProcessTrusted
// preflight (accessibility_preflight_darwin.go) -- the exact check
// the dispatcher enforces before every input action -- so the
// wizard's verdict can't disagree with runtime gating. On linux the
// display-server detection runs first: on a Wayland or display-less
// session the XTEST probe below would fail confusingly, so the
// wizard answers from the same detection the dispatcher uses.
func probeAccessibility() (bool, string) {
	if runtime.GOOS == "linux" {
		if server := tools.DetectDisplayServer(); server != "x11" {
			return false, fmt.Sprintf("display server %q -- RobotGo requires an X11 session; input actions will return display_server_unsupported", server)
		}
	}
	type result struct {
		ok     bool
		detail string
	}
	resCh := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				resCh <- result{false, fmt.Sprintf("panic during probe: %v", r)}
			}
		}()
		startX, startY := robotgo.Location()
		robotgo.MoveRelative(2, 0)
		robotgo.MilliSleep(40)
		midX, midY := robotgo.Location()
		robotgo.Move(startX, startY)
		robotgo.MilliSleep(40)
		if midX != startX || midY != startY {
			resCh <- result{true, fmt.Sprintf("cursor moved (%d,%d)->(%d,%d)->back", startX, startY, midX, midY)}
			return
		}
		resCh <- result{false, fmt.Sprintf("cursor stayed at (%d,%d) after MoveRelative -- Accessibility denied", startX, startY)}
	}()
	select {
	case r := <-resCh:
		ok, detail := r.ok, r.detail
		if runtime.GOOS == "darwin" {
			// Per-call dispatch gating uses AXIsProcessTrusted, so
			// the wizard verdict must factor it in: an active probe
			// that succeeds via an inherited grant does not help if
			// the AX preflight will deny every input dispatch.
			trusted := preflightAccessibilityAccess()
			detail = fmt.Sprintf("AXIsProcessTrusted=%t; %s", trusted, detail)
			ok = ok && trusted
		}
		return ok, detail
	case <-time.After(5 * time.Second):
		return false, "probe timed out after 5s -- press R to re-probe after confirming Accessibility entry"
	}
}

// probeScreenRecording asks macOS via CGPreflightScreenCaptureAccess
// whether this process has a Screen Recording grant -- including the
// inherited grant from the parent shell when the binary is launched
// from Terminal. The CG call is synchronous and never blocks; the
// previous robotgo.SaveCapture-based probe was observed to hang
// indefinitely under certain TCC-state mismatches (user just toggled
// a grant on, the binary's hash changed since the last approval,
// inheritance not propagating to the child process). The 5-second
// timeout below is defensive; the cgo call itself returns in
// microseconds.
//
// On Linux / Windows builds (computeruse-tagged but non-darwin) the wrapper
// always reports true -- there's no system-level TCC ledger to
// preflight against; downstream capture failures surface as runtime
// errors rather than pre-flight denials.
func probeScreenRecording() (bool, string) {
	if runtime.GOOS == "linux" {
		if server := tools.DetectDisplayServer(); server != "x11" {
			return false, fmt.Sprintf("display server %q -- RobotGo requires an X11 session; screenshot will return display_server_unsupported", server)
		}
	}
	type result struct {
		ok     bool
		detail string
	}
	resCh := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				resCh <- result{false, fmt.Sprintf("panic during preflight: %v", r)}
			}
		}()
		if preflightScreenCaptureAccess() {
			resCh <- result{true, "CGPreflightScreenCaptureAccess returned granted"}
			return
		}
		resCh <- result{false, "CGPreflightScreenCaptureAccess returned denied -- approve memql (or its parent Terminal) under System Settings -> Privacy & Security -> Screen Recording"}
	}()
	select {
	case r := <-resCh:
		return r.ok, r.detail
	case <-time.After(5 * time.Second):
		return false, "preflight timed out after 5s -- press R to re-probe"
	}
}

// tccCheck shells out to /usr/bin/tccutil to query the per-binary
// permission status. Best-effort: tccutil's `check` subcommand was
// added in macOS Sonoma 14.4; older OSes report "unsupported".
func tccCheck(service, binaryPath string) string {
	if binaryPath == "" {
		return "unknown (cannot resolve binary path)"
	}
	tcc, err := exec.LookPath("tccutil")
	if err != nil {
		return "unknown (tccutil not found)"
	}
	cmd := exec.Command(tcc, "check", service, binaryPath)
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		// Older macOS without `check` reports usage on stderr.
		if strings.Contains(trimmed, "Usage") || strings.Contains(trimmed, "unrecognized") {
			return "unknown (this macOS version's tccutil does not support `check`)"
		}
		// `tccutil check X` exits non-zero when the answer is "denied".
		if trimmed != "" {
			return "denied (" + firstLine(trimmed) + ")"
		}
		return "denied"
	}
	if trimmed == "" {
		return "granted"
	}
	return "granted (" + firstLine(trimmed) + ")"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func runSetupLinux() error {
	// Step 1: the display-server preflight -- the exact detection the
	// dispatcher enforces per call (tools.DetectDisplayServer, wired
	// via display_preflight_linux.go), so the wizard's verdict can't
	// disagree with runtime gating.
	fmt.Println("Step 1/2: Display server")
	fmt.Println(strings.Repeat("-", 60))
	switch server := tools.DetectDisplayServer(); server {
	case "x11":
		fmt.Printf("  Display server preflight: PASS (x11, DISPLAY=%s)\n", os.Getenv("DISPLAY"))
	case "wayland":
		fmt.Println("  Display server preflight: FAIL (wayland session)")
		fmt.Println()
		fmt.Println("  RobotGo drives X11 only. On this session every")
		fmt.Println("  workerComputer input + screenshot action will return")
		fmt.Println("  display_server_unsupported.")
		fmt.Println()
		fmt.Println("  Action: log into an X11 (Xorg) session to enable computer-use")
		fmt.Println("  actions, or register the worker HEADLESS-only (the")
		fmt.Println("  install-linux.sh installer detects Wayland and does")
		fmt.Println("  this for you).")
		// A machine fact the operator has to change, reported as a
		// prerequisite rather than swallowed as success: an installer
		// reading exit 0 here registers a computer-use worker that
		// cannot move a mouse.
		return setupPrereq("wayland session: RobotGo drives X11 only")
	default:
		fmt.Printf("  Display server preflight: FAIL (%s)\n", server)
		return setupPrereq("no display server detected (DISPLAY unset); X11 unreachable")
	}
	fmt.Println()

	fmt.Println("Step 2/2: Input + screenshot probes")
	fmt.Println(strings.Repeat("-", 60))
	startX, startY := robotgo.Location()
	robotgo.MoveRelative(2, 0)
	robotgo.MilliSleep(40)
	midX, midY := robotgo.Location()
	robotgo.Move(startX, startY)
	if midX == startX && midY == startY {
		fmt.Println("  Input probe: FAIL")
		return setupPrereq("X11: cursor MoveRelative did not move; check XTEST + DISPLAY auth (Xauth/cookie)")
	}
	fmt.Println("  Input probe: PASS (cursor moved + restored)")
	tmp, err := os.CreateTemp("", "worker-setup-probe-*.png")
	if err != nil {
		return setupFailed("temp file: %v", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)
	if err := robotgo.SaveCapture(tmpPath, 0, 0, 16, 16); err != nil {
		fmt.Println("  Screenshot probe: FAIL")
		return setupPrereq("X11 screenshot probe: %v", err)
	}
	stat, err := os.Stat(tmpPath)
	if err != nil || stat.Size() < 200 {
		fmt.Println("  Screenshot probe: FAIL")
		return setupPrereq("X11 screenshot suspiciously small (%d bytes)", stat.Size())
	}
	fmt.Printf("  Screenshot probe: PASS (%d bytes)\n", stat.Size())
	fmt.Println()
	fmt.Println("SUCCESS: X11 permissions look good.")
	return nil
}

func waitForKey(prompt string) bool {
	if setupNonInteractive {
		fmt.Println("  (non-interactive: not waiting for approval)")
		return false
	}
	fmt.Print(prompt + " ")
	r := bufio.NewReader(os.Stdin)
	_, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return false
	}
	return true
}
