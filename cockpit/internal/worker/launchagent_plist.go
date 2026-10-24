package worker

import (
	"fmt"
	"html"
)

// launchAgentPlist renders the worker's LaunchAgent (InstallLaunchAgent,
// the pair flow's writer).
//
// It is outside the darwin build tag so the rendering is testable on every
// platform CI runs; only writing and loading it is macOS-specific.
//
// PATH is written with HOME. Without it launchd starts the worker on its
// four system directories and the app inventory finds no `claude` or
// `codex` an installer put anywhere else (servicepath.go). The value is
// the one ensureServicePath arrives at, so a worker started from this plist
// has nothing left to add -- and the shell writers spell the same string,
// pinned by TestShellPlistWritersCarryTheServicePath.
func launchAgentPlist(label, binaryPath, association, stateDir, home string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    %s
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>worker</string>
        <string>run</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s/worker.log</string>
    <key>StandardErrorPath</key>
    <string>%s/worker.log</string>
    <key>ThrottleInterval</key>
    <integer>5</integer>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>%s</string>
        <key>PATH</key>
        <string>%s</string>
    </dict>
</dict>
</plist>
`, html.EscapeString(label), association, html.EscapeString(binaryPath),
		html.EscapeString(stateDir), html.EscapeString(stateDir), html.EscapeString(home),
		html.EscapeString(servicePath(launchdDefaultPath, home)))
}
