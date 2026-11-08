//go:build darwin

package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Exercise real codesign output: --display writes identity to stderr, so a
// stdout-only command wrapper would silently misclassify every signed app.
// Fixtures use an ad-hoc signature and never access or modify the Keychain.
func TestNativeAdhocBundleVerificationAndTamper(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	old := filepath.Join(root, "old", "MemQL.app")
	incoming := filepath.Join(root, "new", "MemQL.app")
	binary, err := os.ReadFile("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{old, incoming} {
		helper := filepath.Join(app, "Contents", "Library", "LoginItems", "MemQL Menu.app")
		for _, bundle := range []struct{ path, id, executable string }{{helper, "com.znasllc.memql-cockpit-menubar", "MemQLCockpit"}, {app, "com.znasllc.memql-worker", "MemQL"}} {
			if err := os.MkdirAll(filepath.Join(bundle.path, "Contents", "MacOS"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bundle.path, "Contents", "MacOS", bundle.executable), binary, 0755); err != nil {
				t.Fatal(err)
			}
			plist := `<?xml version="1.0"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>` + bundle.id + `</string><key>CFBundleExecutable</key><string>` + bundle.executable + `</string><key>CFBundlePackageType</key><string>APPL</string></dict></plist>`
			if err := os.WriteFile(filepath.Join(bundle.path, "Contents", "Info.plist"), []byte(plist), 0644); err != nil {
				t.Fatal(err)
			}
			if output, err := runCommand(ctx, "/usr/bin/codesign", "--force", "--sign", "-", bundle.path); err != nil {
				t.Fatalf("codesign fixture: %v %s", err, output)
			}
		}
	}
	c := &Client{home: root, bundle: old, run: runCommand}
	if err := c.verifyBundle(ctx, incoming); err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(incoming, "Contents", "MacOS", "MemQL")
	file, err := os.OpenFile(worker, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("modified"))
	_ = file.Close()
	if err := c.verifyBundle(ctx, incoming); err == nil {
		t.Fatal("accepted tampered code signature")
	}
}

func TestNativeBundleExchangeAndRollback(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "MemQL.app")
	incoming := filepath.Join(root, "new.app")
	previous := filepath.Join(root, "previous.app")
	for path, content := range map[string]string{target: "old", incoming: "new"} {
		_ = os.Mkdir(path, 0755)
		_ = os.WriteFile(filepath.Join(path, "version"), []byte(content), 0644)
	}
	if err := swap(target, incoming, previous); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(filepath.Join(target, "version")); string(body) != "new" {
		t.Fatal(string(body))
	}
	if err := restore(target, previous); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(filepath.Join(target, "version")); string(body) != "old" {
		t.Fatal(string(body))
	}
}
