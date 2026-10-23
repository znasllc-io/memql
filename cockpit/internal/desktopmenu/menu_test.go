package desktopmenu

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMenuFilesUseSelectedExecutableAndPreserveWorker(t *testing.T) {
	root := t.TempDir()
	p := paths(filepath.Join(root, "config space"), filepath.Join(root, "data"))
	executable := filepath.Join(root, `bin with spaces/$quote"%/memql`)
	for i := 0; i < 2; i++ {
		if err := installFiles(p, executable); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{p.Unit, p.Desktop, p.Launcher, p.Icon} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	unit, _ := os.ReadFile(p.Unit)
	desktop, _ := os.ReadFile(p.Desktop)
	if strings.Contains(string(unit), "worker run") || strings.Contains(string(desktop), "worker run") {
		t.Fatal("menu started a second worker")
	}
	if !strings.Contains(string(unit), `ExecStart=:"`) || !strings.Contains(string(unit), ` menu`) {
		t.Fatalf("bad selected executable: %s", unit)
	}
	if !strings.Contains(string(desktop), `\\$quote\\"%%/memql" menu --start`) {
		t.Fatalf("desktop shell/specifier characters were not escaped: %s", desktop)
	}
	if !strings.Contains(string(unit), "PartOf=graphical-session.target") {
		t.Fatal("menu not tied to graphical session")
	}
	if err := os.WriteFile(p.Desktop, []byte("unrelated content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := installFiles(p, executable); err == nil {
		t.Fatal("overwrote unmanaged autostart")
	}
	for _, bad := range []string{"relative/memql", "/tmp/a\nb", "/tmp/a\r"} {
		if _, err := quoteExec(bad, true); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
func TestMenuURLAndStateAreHonest(t *testing.T) {
	for _, bad := range []string{"javascript:alert(1)", "https://user:secret@example.test/", "http://example.test", "https://example.test/?token=secret", "https://example.test/#token"} {
		if _, err := safeOSURL(bad); err == nil {
			t.Fatalf("accepted unsafe destination %s", bad)
		}
	}
	if got, err := safeOSURL("https://os.memql.localhost/"); err != nil || got == "" {
		t.Fatal(got, err)
	}
	if summary(nil) != "Background worker unavailable" {
		t.Fatal("unknown worker reported ready")
	}
	if summary(&Status{Homes: []Home{{State: "Connected"}, {State: "Paused"}}}) != "1 of 2 servers connected" {
		t.Fatal("incorrect status")
	}
	image, err := png.Decode(bytes.NewReader(icon))
	if err != nil || image.Bounds().Dx() != 64 {
		t.Fatal("missing tray icon", err)
	}
}

func TestDesktopLifecycleUsesSessionAndRemovesOnlyMenu(t *testing.T) {
	root := t.TempDir()
	config, data := filepath.Join(root, "config"), filepath.Join(root, "data")
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "wayland-1")
	t.Setenv("XDG_SESSION_TYPE", "wayland")
	t.Setenv("XDG_CURRENT_DESKTOP", "COSMIC")
	calls := filepath.Join(root, "calls")
	t.Setenv("MEMQL_MENU_TEST_CALLS", calls)
	shim := filepath.Join(root, "systemctl")
	requireFile := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	requireFile(shim, "#!/bin/sh\nmain() { printf '%s\\n' \"$*\" >> \"$MEMQL_MENU_TEST_CALLS\"; if [ \"${MEMQL_MENU_TEST_REFUSE:-}\" = stop ] && [ \"$2\" = stop ]; then exit 1; fi; }\nmain \"$@\"\n")
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	worker := filepath.Join(root, "worker.yaml")
	requireFile(worker, "keep worker enrollment")
	p := paths(config, data)
	for i := 0; i < 2; i++ {
		if err := Install(filepath.Join(root, "bin with spaces/memql")); err != nil {
			t.Fatal(err)
		}
	}
	called, _ := os.ReadFile(calls)
	if !strings.Contains(string(called), "import-environment WAYLAND_DISPLAY") || !strings.Contains(string(called), "restart memql-menu.service") {
		t.Fatalf("session not imported/started: %s", called)
	}
	t.Setenv("MEMQL_MENU_TEST_REFUSE", "stop")
	if err := Uninstall(); err == nil {
		t.Fatal("removed a menu that could not be stopped")
	}
	if _, err := os.Stat(p.Unit); err != nil {
		t.Fatal("removed launch files while process still runs", err)
	}
	t.Setenv("MEMQL_MENU_TEST_REFUSE", "")
	if err := Uninstall(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(calls)
	if err := Uninstall(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(calls)
	if !bytes.Equal(before, after) {
		t.Fatal("already removed tray still called systemctl")
	}
	for _, path := range []string{p.Unit, p.Desktop, p.Launcher, p.Icon} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("not removed: %s", path)
		}
	}
	if contents, _ := os.ReadFile(worker); string(contents) != "keep worker enrollment" {
		t.Fatal("worker enrollment changed")
	}
}
