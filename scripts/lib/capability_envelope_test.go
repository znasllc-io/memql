package lib

// capability_envelope_test.go -- cap_envelope_error_message, the one reader of another
// capability's envelope (scripts/k3d/bringup.sh reads make media-up's, scripts/k3d/down.sh
// reads make media-down's).
//
// Every envelope here is written by capability.sh itself (cap_fail, cap_ok), so what is
// read back is what the library writes. Each is read twice: with jq, where it is
// installed, and with a jq that fails standing first on PATH, which is the reader's own
// decoding -- the path a machine without jq takes, and the one where the readers this
// helper replaced cut a message at its first escaped quote.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// envelopeFrom runs body after sourcing capability.sh and cap_init, and returns the one
// envelope the run printed on stdout.
func envelopeFrom(t *testing.T, bash, body string) string {
	t.Helper()
	lib := filepath.Join(repoRoot(t), "scripts", "lib", "capability.sh")
	script := "set -euo pipefail\nsource \"" + lib + "\"\ncap_init \"test.producer\" \"writes an envelope\"\n" + body + "\n"
	out, _ := exec.Command(bash, "-c", script).Output() // cap_fail exits non-zero by design
	return strings.TrimSuffix(string(out), "\n")
}

// readMessage runs cap_envelope_error_message on envelope with fallback, the envelope and
// the answer passing through files so neither is quoted on the way.
func readMessage(t *testing.T, bash, envelope, fallback string, withJQ bool) string {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "envelope")
	out := filepath.Join(dir, "message")
	if err := os.WriteFile(in, []byte(envelope), 0o600); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	if !withJQ {
		bin := filepath.Join(dir, "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "jq"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		path = bin + string(os.PathListSeparator) + path
	}
	lib := filepath.Join(repoRoot(t), "scripts", "lib", "capability.sh")
	script := "set -euo pipefail\nsource \"" + lib + "\"\n" +
		"message=\"$(cap_envelope_error_message \"$(cat \"$1\")\" \"$2\")\"\n" +
		"printf '%s' \"$message\" > \"$3\"\n"
	cmd := exec.Command(bash, "-c", script, "_", in, fallback, out)
	cmd.Env = append(os.Environ(), "PATH="+path)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cap_envelope_error_message failed: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// envelopeReaders are the interpreters to read under: this PATH's bash, and bash 3.2
// where there is one (stock macOS), as every capability script runs on both.
func envelopeReaders(t *testing.T) []string {
	t.Helper()
	readers := []string{"bash"}
	for _, c := range []string{"/bin/bash"} {
		out, err := exec.Command(c, "-c", `printf '%s' "$BASH_VERSION"`).Output()
		if err == nil && strings.HasPrefix(string(out), "3.2.") {
			readers = append(readers, c)
		}
	}
	return readers
}

func TestCapEnvelopeErrorMessageReadsTheWholeMessage(t *testing.T) {
	const fallback = "make media-down gave no reason"
	cases := []struct {
		name, body, want string
	}{
		{
			name: "escaped quotes and a backslash",
			body: `cap_fail 5 "compose said \"no such project\" at C:\\media"`,
			want: `compose said "no such project" at C:\media`,
		},
		{
			name: "a newline and a tab",
			body: `cap_fail 5 "first line"$'\n'"second"$'\t'"column"`,
			want: "first line\nsecond\tcolumn",
		},
		{
			// The result may carry a field named message, or an object named error; the
			// envelope's own error object is its last member, and only it is read.
			name: "decoys in the result",
			body: `cap_result_set message "a result field"
cap_result_set_raw error '{"code":1,"message":"a nested object"}'
cap_fail 4 "the real reason"`,
			want: "the real reason",
		},
		{name: "a success envelope", body: `cap_ok`, want: fallback},
	}
	jqInstalled := true
	if _, err := exec.LookPath("jq"); err != nil {
		jqInstalled = false
	}
	for _, bash := range envelopeReaders(t) {
		for _, tc := range cases {
			envelope := envelopeFrom(t, bash, tc.body)
			if envelope == "" {
				t.Fatalf("%s: the producer printed no envelope", tc.name)
			}
			for _, withJQ := range []bool{true, false} {
				if withJQ && !jqInstalled {
					continue
				}
				got := readMessage(t, bash, envelope, fallback, withJQ)
				if got != tc.want {
					t.Errorf("%s (bash %s, jq %v): read %q from\n  %s\nwant %q", tc.name, bash, withJQ, got, envelope, tc.want)
				}
			}
		}
		// What is not an envelope, or not a whole one, is the fallback, never a fragment.
		for name, envelope := range map[string]string{
			"no output":              "",
			"a line that is not one": "docker: command not found",
			"a message never closed": `{"ok":false,"capability":"x","changed":false,"result":{},"error":{"code":5,"message":"cut off`,
		} {
			if got := readMessage(t, bash, envelope, fallback, false); got != fallback {
				t.Errorf("%s (bash %s): read %q, want the fallback %q", name, bash, got, fallback)
			}
		}
	}
}
