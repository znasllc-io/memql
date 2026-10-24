package worker

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// servicepath.go gives the worker a PATH that can find the apps it reports.
//
// A SERVICE DOES NOT GET THE OWNER'S SHELL PATH. launchd starts a
// LaunchAgent with its four system directories and nothing else, and a user
// systemd unit is not much better. Claude Code's own installer puts `claude`
// in ~/.local/bin and Homebrew puts everything under /opt/homebrew/bin, so
// the detector (exec.LookPath) found neither, reported apps=[] on every
// beat, and the machine never appeared as an app door. The session exec
// would have failed the same way: it starts the name, not a path. The fix
// was a PATH typed into the plist by hand, which the next pair or reinstall
// wrote away again.
//
// So the worker extends its OWN PATH once, at start, before anything looks
// a binary up. That one call covers every lookup this process makes -- the
// app inventory, the session's harness, the child's environment, the
// runtime probes -- so none of them can disagree with another about where
// `claude` is. The plist writers write the same PATH (launchAgentPlist and
// the three shell scripts TestShellPlistWritersCarryTheServicePath names),
// which makes this a no-op on a machine they set up.
//
// APPENDED, NEVER PREPENDED. The worker runs launchctl, plutil and friends
// by bare name, and every directory added here is one the user can write
// to; put first, a stray `launchctl` in ~/.local/bin would run instead of
// the system's. Nothing already on PATH moves either: a worker started from
// a terminal keeps the owner's own order, and only what it lacks is added.
//
// The owner's login shell is NOT consulted. Running `$SHELL -l` would
// execute their profile on every worker start, from a service; the list
// below is where the supported installers put the apps, and a machine that
// keeps them elsewhere can say so in the plist's own PATH.

// launchdDefaultPath is the PATH launchd starts every LaunchAgent with, and
// the floor this file builds on when a process was given no PATH at all.
const launchdDefaultPath = "/usr/bin:/bin:/usr/sbin:/sbin"

// serviceAppDirs are the directories the supported apps' installers use, in
// the order they are searched. Home-relative ones are left out when there
// is no home, because ".local/bin" relative to wherever the worker happens
// to run is not a directory anybody installed anything into.
func serviceAppDirs(home string) []string {
	var dirs []string
	if strings.TrimSpace(home) != "" {
		dirs = append(dirs,
			// Claude Code's native installer.
			filepath.Join(home, ".local", "bin"),
			// Claude Code's older per-user install.
			filepath.Join(home, ".claude", "local"),
		)
	}
	return append(dirs,
		// Homebrew on Apple silicon.
		"/opt/homebrew/bin",
		// Homebrew on Intel, and npm's default global prefix.
		"/usr/local/bin",
	)
}

// servicePath returns current with every app directory it does not already
// name appended, in serviceAppDirs' order. An empty current starts from
// launchd's system directories, so the app directories are never the only
// -- and therefore the first -- thing searched.
func servicePath(current, home string) string {
	var entries []string
	if strings.TrimSpace(current) == "" {
		entries = filepath.SplitList(launchdDefaultPath)
	} else {
		entries = filepath.SplitList(current)
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		seen[filepath.Clean(e)] = true
	}
	for _, dir := range serviceAppDirs(home) {
		if seen[filepath.Clean(dir)] {
			continue
		}
		seen[filepath.Clean(dir)] = true
		entries = append(entries, dir)
	}
	return strings.Join(entries, string(os.PathListSeparator))
}

// ensureServicePath extends this process's PATH with servicePath. Call it
// once, at worker start, before the app inventory or anything else resolves
// a binary: exec.LookPath and exec.Command both read the process PATH at
// the moment they run, and a child inherits it through os.Environ.
func ensureServicePath(logger *slog.Logger) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	before := os.Getenv("PATH")
	after := servicePath(before, home)
	if after == before {
		return
	}
	if err := os.Setenv("PATH", after); err != nil {
		logger.Warn("could not extend the worker's PATH; apps installed outside the system directories will not be found",
			"error", err)
		return
	}
	// Once, and at Info: it is the line that answers "which PATH did the
	// worker look for claude on" without anybody attaching to the process.
	logger.Info("worker PATH extended so the apps it reports can be found", "path", after)
}
