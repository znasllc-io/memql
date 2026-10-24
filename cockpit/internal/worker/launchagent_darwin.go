//go:build darwin

package worker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const launchAgentLabel = "com.znasllc.memql-worker"

// legacyLaunchAgentLabel is the pre-rename label. Install and
// Uninstall both retire it so a machine upgraded in place never runs
// two workers (znasllc-io/memql#4553).
const legacyLaunchAgentLabel = "com.znasllc.memql-cockpit-worker"

// InstallLaunchAgent drops a per-user LaunchAgent plist and loads
// it. Subsequent reboots auto-start the worker without the user
// having to run a command.
//
// Per-user LaunchAgent (Library/LaunchAgents) is the right shape:
// macOS only delivers TCC grants to processes running in a logged-
// in user's session, and LaunchAgents run after login while
// LaunchDaemons run before. The cockpit needs accessibility +
// screen-recording, both gated by TCC, so per-user is mandatory.
func InstallLaunchAgent(binaryPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("launch agent: home dir: %w", err)
	}
	if binaryPath == "" {
		binaryPath, err = os.Executable()
		if err != nil {
			return fmt.Errorf("launch agent: executable path: %w", err)
		}
	}
	if abs, err := filepath.Abs(binaryPath); err == nil {
		binaryPath = abs
	}
	association := ""
	if real, err := filepath.EvalSymlinks(binaryPath); err == nil && strings.HasSuffix(real, "/MemQL.app/Contents/MacOS/MemQL") {
		binaryPath = real
		association = "<key>AssociatedBundleIdentifiers</key><array><string>com.znasllc.memql-worker</string></array>"
	}

	stateDir := filepath.Join(home, ".memql", "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("launch agent: state dir: %w", err)
	}

	plistDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(plistDir, 0o755); err != nil {
		return fmt.Errorf("launch agent: plist dir: %w", err)
	}
	plistPath := filepath.Join(plistDir, launchAgentLabel+".plist")

	// Retire the legacy agent before loading the new one.
	if legacy := filepath.Join(plistDir, legacyLaunchAgentLabel+".plist"); fileExists(legacy) {
		_ = exec.Command("launchctl", "unload", legacy).Run()
		_ = os.Remove(legacy)
	}

	plist := launchAgentPlist(launchAgentLabel, binaryPath, association, stateDir, home)

	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("launch agent: write plist: %w", err)
	}

	// Unload-then-load idempotently. Ignore unload errors -- the
	// agent might not have been loaded before.
	_ = exec.Command("launchctl", "unload", plistPath).Run()
	if err := exec.Command("launchctl", "load", plistPath).Run(); err != nil {
		return fmt.Errorf("launch agent: launchctl load: %w", err)
	}
	return nil
}

// UninstallLaunchAgent stops and removes the LaunchAgent. Used by
// the disconnect flow.
func UninstallLaunchAgent() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("launch agent: home dir: %w", err)
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
	if legacy := filepath.Join(home, "Library", "LaunchAgents", legacyLaunchAgentLabel+".plist"); fileExists(legacy) {
		_ = exec.Command("launchctl", "unload", legacy).Run()
		_ = os.Remove(legacy)
	}
	if _, err := os.Stat(plistPath); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_ = exec.Command("launchctl", "unload", plistPath).Run()
	return os.Remove(plistPath)
}

// fileExists reports whether path exists (any type).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
