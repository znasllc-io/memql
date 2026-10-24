package desktopmenu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const service = "memql-menu.service"
const marker = "# Managed by MemQL Cockpit desktop menu"

type Paths struct{ Unit, Desktop, Launcher, Icon string }

func paths(config, data string) Paths {
	return Paths{filepath.Join(config, "systemd/user", service), filepath.Join(config, "autostart/memql-cockpit.desktop"), filepath.Join(data, "applications/memql-cockpit.desktop"), filepath.Join(data, "icons/hicolor/64x64/apps/memql-cockpit.png")}
}
func userPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		config = filepath.Join(home, ".config")
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local/share")
	}
	if !filepath.IsAbs(config) || !filepath.IsAbs(data) {
		return Paths{}, errors.New("XDG configuration and data directories must be absolute paths")
	}
	return paths(config, data), nil
}

// Desktop entries decode string escapes before command quoting. systemd uses
// its own quoting, with percent specifiers and environment expansion disabled.
func quoteExec(value string, desktop bool) (string, error) {
	if !filepath.IsAbs(value) || strings.ContainsAny(value, "\r\n\t\x00=") {
		return "", errors.New("menu executable must be an absolute single-line path without '='")
	}
	value = strings.ReplaceAll(value, "%", "%%")
	if desktop {
		value = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`").Replace(value)
		value = strings.ReplaceAll(value, `\`, `\\`)
	} else {
		value = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
	}
	return `"` + value + `"`, nil
}
func writeFile(path string, content []byte, managed bool) error {
	if prior, err := os.ReadFile(path); err == nil && managed && !strings.Contains(string(prior), marker) {
		return fmt.Errorf("refusing to replace unrecognized file %s", path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".memql-menu-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err = file.Chmod(0644); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
func installFiles(p Paths, executable string) error {
	quoted, err := quoteExec(executable, true)
	if err != nil {
		return err
	}
	unitQuoted, err := quoteExec(executable, false)
	if err != nil {
		return err
	}
	unit := marker + "\n[Unit]\nDescription=MemQL Cockpit menu\nPartOf=graphical-session.target\nAfter=graphical-session.target\n\n[Service]\nType=simple\nExecStart=:" + unitQuoted + " menu\nRestart=on-failure\nRestartSec=5\n"
	desktop := marker + "\n[Desktop Entry]\nType=Application\nName=MemQL Cockpit\nComment=MemQL worker connections\nExec=" + quoted + " menu --start\nIcon=memql-cockpit\nTerminal=false\nCategories=Utility;\n"
	for path, body := range map[string]string{p.Unit: unit, p.Desktop: desktop, p.Launcher: desktop} {
		if err := writeFile(path, []byte(body), true); err != nil {
			return err
		}
	}
	return writeFile(p.Icon, icon, false)
}
func systemctl(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("menu service: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
func Start() error {
	// XDG autostart runs in the desktop session. Carry that session's display
	// environment to xdg-open, including COSMIC/Wayland, before starting the unit.
	keys := []string{}
	for _, key := range []string{"DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY", "XDG_CURRENT_DESKTOP", "XDG_SESSION_TYPE"} {
		if os.Getenv(key) != "" {
			keys = append(keys, key)
		}
	}
	if len(keys) > 0 {
		if err := systemctl(append([]string{"import-environment"}, keys...)...); err != nil {
			return err
		}
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	return systemctl("restart", service)
}
func Install(executable string) error {
	p, err := userPaths()
	if err != nil {
		return err
	}
	if err = installFiles(p, executable); err != nil {
		return err
	}
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		fmt.Println("Cockpit menu will start at your next desktop sign-in.")
		return nil
	}
	if err = Start(); err != nil {
		return err
	}
	fmt.Println("Cockpit menu started. Your desktop's notifications tray displays the icon.")
	return nil
}
func Uninstall() error {
	p, err := userPaths()
	if err != nil {
		return err
	}
	unitExists := false
	for _, path := range []string{p.Unit, p.Desktop, p.Launcher} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !strings.Contains(string(data), marker) {
			return fmt.Errorf("refusing to remove unrecognized file %s", path)
		}
		unitExists = unitExists || path == p.Unit
	}
	if unitExists {
		if err = systemctl("stop", service); err != nil {
			return err
		}
	}
	for _, path := range []string{p.Unit, p.Desktop, p.Launcher} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !strings.Contains(string(data), marker) {
			return fmt.Errorf("refusing to remove unrecognized file %s", path)
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	}
	if err = os.Remove(p.Icon); err != nil && !os.IsNotExist(err) {
		return err
	}
	if unitExists {
		return systemctl("daemon-reload")
	}
	return nil
}
