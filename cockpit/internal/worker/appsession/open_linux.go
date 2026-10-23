//go:build linux

package appsession

import (
	"errors"
	"os"
	"os/exec"
	"strings"
)

// linuxTerminals are tried in order. $TERMINAL first, because an operator
// who set it has already answered this question.
var linuxTerminals = []struct {
	binary string
	flag   string
}{
	{"x-terminal-emulator", "-e"},
	{"gnome-terminal", "--"},
	{"konsole", "-e"},
	{"xfce4-terminal", "-e"},
	{"alacritty", "-e"},
	{"kitty", "-e"},
	{"wezterm", "start"},
	{"foot", "-e"},
	{"xterm", "-e"},
}

// platformOpenCommand resolves a terminal emulator and runs the launcher
// in it. The returned process exits when the window closes, which is the
// signal the session ends on.
//
// A machine with no display fails HERE, by name. That is the point of the
// kind: a headless server cannot hand anything to a human, and reporting
// that is far more useful than launching something nobody will ever see.
func platformOpenCommand(script string) ([]string, string, error) {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return nil, "", errors.New("no display: DISPLAY and WAYLAND_DISPLAY are both unset, " +
			"so this machine cannot put a window in front of anyone")
	}
	if custom := strings.TrimSpace(os.Getenv("TERMINAL")); custom != "" {
		if path, err := exec.LookPath(custom); err == nil {
			return []string{path, "-e", script}, "opened in " + custom + " ($TERMINAL)", nil
		}
	}
	for _, t := range linuxTerminals {
		path, err := exec.LookPath(t.binary)
		if err != nil {
			continue
		}
		return []string{path, t.flag, script}, "opened in " + t.binary, nil
	}
	return nil, "", errors.New("no terminal emulator found on PATH " +
		"(tried x-terminal-emulator, gnome-terminal, konsole, xfce4-terminal, alacritty, kitty, wezterm, foot, xterm)")
}
