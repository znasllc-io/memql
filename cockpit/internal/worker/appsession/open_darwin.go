//go:build darwin

package appsession

import (
	"errors"
	"os/exec"
)

// macOSTerminals are tried in order. Terminal.app is present on every
// macOS install, so the loop below effectively always resolves -- the
// earlier entries exist so a machine whose owner uses one of them gets
// their own terminal rather than a stray Apple Terminal window.
var macOSTerminals = []string{"iTerm", "Ghostty", "Alacritty", "kitty", "WezTerm", "Terminal"}

// platformOpenCommand hands the launcher to a terminal application.
//
// `-W` waits for the launched application, which is what makes the
// returned process watchable at all. It is a coarse signal -- for a
// Terminal that was already running it returns when Terminal itself
// quits, not when the run's window closes -- which is precisely why the
// caller prefers the exit file the launcher writes and treats this
// process as the fallback.
func platformOpenCommand(script string) ([]string, string, error) {
	for _, app := range macOSTerminals {
		if _, err := appBundlePath(app); err != nil {
			continue
		}
		return []string{"/usr/bin/open", "-W", "-a", app, script}, "opened in " + app, nil
	}
	return nil, "", errors.New("no terminal application could be resolved on this machine")
}

// appBundlePath asks Launch Services whether an app bundle exists,
// without opening it.
func appBundlePath(app string) (string, error) {
	// `open -Ra <app>` resolves the bundle and returns non-zero when it
	// is absent, without launching anything.
	if err := exec.Command("/usr/bin/open", "-Ra", app).Run(); err != nil {
		return "", err
	}
	return app, nil
}
