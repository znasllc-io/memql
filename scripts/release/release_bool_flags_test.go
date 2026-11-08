// release_bool_flags_test.go -- release.sh's booleans take a value.
//
// `make release` forwarded every boolean as a BARE flag whenever its variable
// was non-empty, `$${ALLOW_OVERWRITE:+--allow-overwrite}`, and release.sh
// accepted only the bare form. So `make release ALLOW_OVERWRITE=0` ALLOWED an
// overwrite of an immutable release tag, and `PUSH=0` asked for a push. The
// recipe forwards the value now, and the script reads `--flag=<bool>` with
// scripts/lib/bool_flag.sh, the same vocabulary as capability.sh's cap_bool.
//
// Hermetic: a stub docker on a PATH prefix records every call and succeeds,
// so a run that is not a dry run builds and pushes nothing real. Its
// `manifest inspect` answers like a registry that already holds the tag.
//
// These cases pass the break-glass phrase, so they are held to two guards
// against ever pushing for real: every push names registry.invalid, a
// reserved name that cannot resolve (RFC 6761), never the shared ACR; and the
// run is refused unless `docker` resolves to the stub in the environment the
// script gets.
package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	stubDocker = "#!/bin/sh\nprintf 'docker %s\\n' \"$*\" >> \"$RELEASE_DOCKER_CALLS\"\nexit 0\n"
	// unreachableRegistry is where every push in this file points.
	unreachableRegistry = "--registry=registry.invalid"
)

// stubDockerEnv returns a PATH prefixed with a recording docker, and the log
// its calls land in. It fails the test unless that PATH resolves docker to
// the stub.
func stubDockerEnv(t *testing.T) (env []string, calls string) {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "docker")
	if err := os.WriteFile(stub, []byte(stubDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	calls = filepath.Join(dir, "calls.log")
	env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RELEASE_DOCKER_CALLS="+calls)
	probe := exec.Command("bash", "-c", "command -v docker")
	probe.Env = env
	got, err := probe.Output()
	if err != nil || strings.TrimSpace(string(got)) != stub {
		t.Fatalf("docker resolves to %q, not the stub %q: refusing to run a release that could push", strings.TrimSpace(string(got)), stub)
	}
	return env, calls
}

func runStubbed(t *testing.T, args ...string) (out string, code int, calls string) {
	t.Helper()
	env, log := stubDockerEnv(t)
	cmd := exec.Command("bash", append([]string{scriptPath(t)}, args...)...)
	cmd.Env = env
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running release.sh %v: %v\n%s", args, err, b)
	}
	logged, _ := os.ReadFile(log)
	return string(b), code, string(logged)
}

func TestReleaseBooleanFlagsTakeAValue(t *testing.T) {
	confirm := "--confirm=" + pushPhrase
	cases := []struct {
		name        string
		args        []string
		plan        []string // plan lines that must appear
		built       bool     // docker build actually ran
		pushed      bool     // docker push actually ran
		wantRefused int
	}{
		{name: "--dry-run=yes is a dry run", args: []string{gateVersion, "--dry-run=yes"},
			plan: []string{"Dry run:    true"}},
		{name: "--dry-run=false builds", args: []string{gateVersion, "--dry-run=false"},
			plan: []string{"Dry run:    false"}, built: true},
		{name: "--dry-run=0 builds", args: []string{gateVersion, "--dry-run=0"},
			plan: []string{"Dry run:    false"}, built: true},
		{name: "--push=false plans no push", args: []string{gateVersion, unreachableRegistry, "--push=false", "--dry-run"},
			plan: []string{"Push:       false"}},
		{name: "--push=true still needs the phrase", args: []string{gateVersion, unreachableRegistry, "--push=true", "--dry-run"},
			wantRefused: 3},
		{name: "--push=1 with the phrase pushes", args: []string{gateVersion, unreachableRegistry, "--push=1", confirm, "--allow-overwrite=1", "--dry-run=no"},
			plan: []string{"Push:       true", "Overwrite:  true"}, built: true, pushed: true},
		{name: "--allow-overwrite=0 reads false", args: []string{gateVersion, "--allow-overwrite=0", "--dry-run"},
			plan: []string{"Overwrite:  false"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			out, code, calls := runStubbed(t, tc.args...)
			if tc.wantRefused != 0 {
				if code != tc.wantRefused {
					t.Fatalf("exit %d, want %d\n%s", code, tc.wantRefused, out)
				}
				return
			}
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			for _, want := range tc.plan {
				if !strings.Contains(out, want) {
					t.Errorf("plan does not say %q\n%s", want, out)
				}
			}
			if got := strings.Contains(calls, "docker build"); got != tc.built {
				t.Errorf("docker build ran = %v, want %v\ncalls:\n%s", got, tc.built, calls)
			}
			if got := strings.Contains(calls, "docker push"); got != tc.pushed {
				t.Errorf("docker push ran = %v, want %v\ncalls:\n%s", got, tc.pushed, calls)
			}
		})
	}
}

// --allow-overwrite=0 is what `make release ALLOW_OVERWRITE=0` forwards, and
// it must leave the write-once tag protected: against a registry that already
// holds the tag, the push is refused before anything is pushed.
func TestReleaseAllowOverwriteFalseKeepsTheTagImmutable(t *testing.T) {
	for _, overwrite := range []string{"--allow-overwrite=0", "--allow-overwrite=false"} {
		overwrite := overwrite
		t.Run(overwrite, func(t *testing.T) {
			out, code, calls := runStubbed(t, gateVersion, unreachableRegistry, "--push", "--confirm="+pushPhrase, overwrite)
			if code == 0 || !strings.Contains(out, "tag already exists") {
				t.Fatalf("exit %d: %s did not protect an existing tag\n%s", code, overwrite, out)
			}
			if strings.Contains(calls, "docker push") {
				t.Errorf("refused, yet pushed:\n%s", calls)
			}
		})
	}
}

// A value that is neither true nor false stops the script before docker is
// touched, and says which flag it was.
func TestReleaseRefusesAMistypedBoolean(t *testing.T) {
	for _, arg := range []string{"--dry-run=ture", "--push=maybe", "--allow-overwrite=2"} {
		arg := arg
		t.Run(arg, func(t *testing.T) {
			out, code, calls := runStubbed(t, gateVersion, arg)
			flag := arg[:strings.Index(arg, "=")]
			if code == 0 {
				t.Fatalf("%s was accepted\n%s", arg, out)
			}
			if !strings.Contains(out, flag) || !strings.Contains(out, "takes a boolean") {
				t.Errorf("the refusal does not name %s as a boolean\n%s", flag, out)
			}
			if strings.TrimSpace(calls) != "" {
				t.Errorf("docker ran on a mistyped boolean:\n%s", calls)
			}
		})
	}
}

// TestHelpRunsNoCommand: release.sh's help is an UNQUOTED heredoc (it
// interpolates the default ACR), so a backtick in its text is a command
// substitution. One shipped in this change before this test: the help named
// `make release` in backticks, so every --help ran make release in the
// caller's directory. Under scripts/release that was make's built-in
// `%: %.sh` rule writing a copy of release.sh named release; at the repo root
// it would have been a real release build. tag-submodules.sh is held to the
// same rule.
func TestHelpRunsNoCommand(t *testing.T) {
	for _, script := range []string{"release.sh", "tag-submodules.sh"} {
		script := script
		t.Run(script, func(t *testing.T) {
			dir := t.TempDir()
			ran := filepath.Join(dir, "ran.log")
			for _, name := range []string{"make", "docker"} {
				stub := "#!/bin/sh\nprintf '" + name + " %s\\n' \"$*\" >> \"$HELP_STUB_LOG\"\nexit 0\n"
				if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "release", script), "--help")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "HELP_STUB_LOG="+ran)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("--help: %v\n%s", err, out)
			}
			if b, _ := os.ReadFile(ran); len(b) > 0 {
				t.Errorf("--help ran a command:\n%s", b)
			}
		})
	}
}

// TestMakeReleaseAllowOverwriteZeroKeepsTheTagImmutable drives the real recipe.
func TestMakeReleaseAllowOverwriteZeroKeepsTheTagImmutable(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not available")
	}
	env, log := stubDockerEnv(t)
	cmd := exec.Command("make", "--no-print-directory", "-C", repoRoot(t), "release",
		"VERSION=9.9.9", "REGISTRY=registry.invalid", "PUSH=1", "CONFIRM="+pushPhrase, "ALLOW_OVERWRITE=0")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	calls, _ := os.ReadFile(log)
	if err == nil || !strings.Contains(string(out), "tag already exists") {
		t.Fatalf("make release ALLOW_OVERWRITE=0 did not protect an existing tag (err %v)\n%s", err, out)
	}
	if strings.Contains(string(calls), "docker push") {
		t.Errorf("make release ALLOW_OVERWRITE=0 pushed over an existing tag:\n%s", calls)
	}
}
