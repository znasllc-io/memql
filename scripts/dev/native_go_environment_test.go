package dev

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Service processes do not inherit login-shell Go paths. Exercise each real
// Linux native lane with Go available only in the operator's local install,
// replacing effects with recording executables. The helper must discover Go
// and the lane must then restore the monorepo workspace instead of GOWORK=off.
func TestNativeLinuxLanesDiscoverGoAndKeepWorkspace(t *testing.T) {
	helper, err := os.ReadFile(filepath.Join(repoRoot(t), "cockpit/scripts/ci/native-env.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"docker-integration", "provider-qualification"} {
		t.Run(name, func(t *testing.T) {
			_, step := nativeStep(t, name)
			root, home := t.TempDir(), t.TempDir()
			bin := filepath.Join(root, "bin")
			goBin := filepath.Join(home, ".local/go/bin")
			helperPath := filepath.Join(root, "cockpit/scripts/ci/native-env.sh")
			for _, dir := range []string{bin, goBin, filepath.Dir(helperPath)} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(helperPath, helper, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			write := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(goBin, "go"), `test "$GOWORK" = "$PWD/go.work"
test "$GOTOOLCHAIN" = auto
test "$GOPATH" = "$PWD/.memql-native/go"
test "$GOCACHE" = "$PWD/.memql-native/go-build"
printf 'go:%s\n' "$*"
`)
			write(filepath.Join(bin, "docker"), "test \"$*\" = info\n")
			write(filepath.Join(bin, "bash"), `test "$GOWORK" = "$PWD/go.work"
test "$GOTOOLCHAIN" = auto
command -v go >/dev/null
printf 'capability:%s\n' "$1"
`)
			mkdir, err := exec.LookPath("mkdir")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(mkdir, filepath.Join(bin, "mkdir")); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "-c", step.Run)
			cmd.Dir = root
			cmd.Env = []string{"HOME=" + home, "PATH=" + bin, "MEMQL_PACKAGES=./cockpit/..."}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("lane with service PATH failed: %v\n%s", err, out)
			}
			want := "go:test -race -count=1 -timeout=600s ./cockpit/..."
			if name == "provider-qualification" {
				want = "capability:scripts/ci/delivery-artifacts.sh"
			}
			if !strings.Contains(string(out), want) {
				t.Fatalf("lane did not reach required command: %s", out)
			}
		})
	}
}
