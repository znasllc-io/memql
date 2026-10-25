package dev

import (
	"fmt"
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

// Exercise the command actually declared in the manifest. Preparation can retry
// a toolchain download, but cannot turn a failed test into a second test run.
func TestNativeMacLanePreparesToolchainBeforeRunningTestsOnce(t *testing.T) {
	_, step := nativeStep(t, "computeruse")
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mode          string
		preparations, tests int
		ok                  bool
	}{
		{"transient download", "transient", 2, 1, true},
		{"permanent download", "permanent", 3, 0, false},
		{"wrong platform", "wrong", 1, 0, false},
		{"partial platform", "partial", 1, 0, false},
		{"test failure", "test-failure", 1, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			bin := filepath.Join(root, "bin")
			ci := filepath.Join(root, "cockpit/scripts/ci")
			for _, dir := range []string{bin, ci} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"native-env.sh", "native_go_platform.py"} {
				data, err := os.ReadFile(filepath.Join(repoRoot(t), "cockpit/scripts/ci", name))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(ci, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(python, filepath.Join(bin, "python3")); err != nil {
				t.Fatal(err)
			}
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nset -eu\n"+body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write("go", `test "$GOWORK" = "$PWD/go.work"
test "$GOTOOLCHAIN" = auto
test "$GOPATH" = "$PWD/.memql-native/go"
test "$GOCACHE" = "$PWD/.memql-native/go-build"
case "$*" in
  'env GOOS')
    printf '%s\n' 'fixture: sum.golang.org TLS handshake timeout' >&2
    exit 1 ;;
  'env GOARCH') printf 'arm64\n' ;;
  'env GOOS GOARCH')
    count=0
    if test -f preparations; then read -r count < preparations; fi
    count=$((count + 1))
    printf '%s\n' "$count" > preparations
    case "$MODE:$count" in
      permanent:*|transient:1)
        printf 'darwin\n'
        printf '%s\n' 'fixture: sum.golang.org TLS handshake timeout' >&2
        exit 1 ;;
      wrong:*) printf 'linux\narm64\n'; exit 0 ;;
      partial:*) printf 'darwin\n'; exit 0 ;;
    esac
    printf 'darwin\narm64\n' ;;
  'test -race -tags computeruse -count=1 -timeout=600s ./cockpit/...')
    test -f css-ran
    printf 'test\n' >> tests
    test "$MODE" != test-failure ;;
  *) exit 2 ;;
esac
`)
			write("bash", `test "$*" = scripts/identity/build-css.sh
printf 'css\n' > css-ran
`)
			cmd := exec.Command("/bin/sh", "-c", step.Run)
			cmd.Dir = root
			// The affected-package lane receives this selection from StepRequest.Environment.
			// Supply the same input when executing its real command outside the runner.
			cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin", "MODE=" + tc.mode, "MEMQL_PACKAGES=./cockpit/..."}
			out, runErr := cmd.CombinedOutput()
			if (runErr == nil) != tc.ok {
				t.Fatalf("lane result = %v, want success=%v\n%s", runErr, tc.ok, out)
			}
			preparations, _ := os.ReadFile(filepath.Join(root, "preparations"))
			if strings.TrimSpace(string(preparations)) != fmt.Sprint(tc.preparations) {
				t.Fatalf("preparations=%q want %d\n%s", preparations, tc.preparations, out)
			}
			tests, _ := os.ReadFile(filepath.Join(root, "tests"))
			if strings.Count(string(tests), "test\n") != tc.tests {
				t.Fatalf("test executions=%q want %d\n%s", tests, tc.tests, out)
			}
			if tc.tests == 0 {
				if _, err := os.Stat(filepath.Join(root, "css-ran")); !os.IsNotExist(err) {
					t.Fatal("build started before platform preparation succeeded")
				}
			}
		})
	}
}

func TestNativeGoPreparationContract(t *testing.T) {
	cmd := exec.Command("python3", "-m", "unittest", "discover", "-s", "cockpit/scripts/ci", "-p", "native*_test.py")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native preparation contract: %v\n%s", err, out)
	}
}

// The actual native command must generate before compilation, and generation
// failure must stop the lane. Recording executables avoid network/build effects.
func TestNativeGoChecksRequireCompleteArchitectureGeneration(t *testing.T) {
	_, step := nativeStep(t, "go-checks")
	for _, fails := range []bool{false, true} {
		t.Run(fmt.Sprint(fails), func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"bash", "go", "make"} {
				body := "#!/bin/sh\nprintf '%s:%s\\n' '" + name + "' \"$*\"\n"
				if name == "make" {
					body += "if [ \"$1\" = arch-model ] && [ \"$FAIL_ARCH\" = 1 ]; then exit 17; fi\n"
				}
				if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			fail := "0"
			if fails {
				fail = "1"
			}
			cmd := exec.Command("/bin/sh", "-c", step.Run)
			cmd.Dir = root
			cmd.Env = []string{"PATH=" + root, "MEMQL_BASE_SHA=fixture", "FAIL_ARCH=" + fail}
			output, err := cmd.CombinedOutput()
			text := string(output)
			if strings.Count(text, "make:arch-model ") != 1 {
				t.Fatalf("canonical generation did not run exactly once: %s", text)
			}
			generated, built := strings.Index(text, "make:arch-model "), strings.Index(text, "go:build ")
			if fails {
				if err == nil || built >= 0 {
					t.Fatalf("generation failure did not stop compilation: %v %s", err, text)
				}
			} else if err != nil || built < generated {
				t.Fatalf("build order: %v %s", err, text)
			}
		})
	}
}
