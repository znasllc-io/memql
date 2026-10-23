package inference

import (
	"errors"
	"strings"
	"testing"
)

// FuzzSplitCommand. This function decides what actually gets EXECUTED on
// somebody's machine, so its properties are the ones worth fuzzing.
//
// Three of them, and each is a different way the install path could be
// turned into an arbitrary-execution primitive:
//
//   - what comes back is exactly what the person was SHOWN. The commands
//     are printed, consented to, and then run; a splitter that produced
//     argv the printed line does not describe would have the person
//     approving one thing and the machine running another. The round
//     trip is the whole guarantee, and it holds because there are no
//     quoting rules -- a line that needed quoting is refused instead.
//
//   - no argument carries a shell metacharacter. Nothing here goes
//     through a shell, but the allow-list is what keeps that true even
//     if a future caller reached for one.
//
//   - a privilege-escalating command is always refused. This cockpit
//     never runs sudo; it prints one for the person and says so.
func FuzzSplitCommand(f *testing.F) {
	for _, seed := range []string{
		"brew install ollama",
		"docker run -d --name ollama --restart unless-stopped -p 127.0.0.1:11434:11434 ollama/ollama",
		"", "   ", "\t\n",
		"sudo systemctl start docker",
		"doas pkg install x",
		"pkexec whatever",
		"rm -rf /; echo pwned",
		"echo $(whoami)",
		"echo `id`",
		"cmd 'quoted arg'",
		`cmd "double quoted"`,
		"cmd arg&&other",
		"cmd arg|other",
		"cmd >file",
		"cmd\nnewline",
		"café install",
		strings.Repeat("a ", 500),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, cmd string) {
		argv, err := splitCommand(cmd)
		if err != nil {
			if argv != nil {
				t.Fatalf("splitCommand(%q) returned argv %q alongside %v", cmd, argv, err)
			}
			return
		}
		if len(argv) == 0 {
			t.Fatalf("splitCommand(%q) returned no error and no argv", cmd)
		}

		// THE ROUND TRIP. What runs is what was printed.
		if got := strings.Join(argv, " "); got != strings.Join(strings.Fields(cmd), " ") {
			t.Fatalf("splitCommand(%q) -> %q, which does not describe the line", cmd, got)
		}

		// NO METACHARACTERS reach an argument.
		for _, arg := range argv {
			if arg == "" {
				t.Fatalf("splitCommand(%q) produced an empty argument: %q", cmd, argv)
			}
			for _, r := range arg {
				if !argvRune(r) {
					t.Fatalf("splitCommand(%q) admitted %q in %q", cmd, r, arg)
				}
			}
			for _, bad := range []string{";", "|", "&", "$", "`", ">", "<", "(", ")", "'", `"`, "\\", "\n", "\r", " "} {
				if strings.Contains(arg, bad) {
					t.Fatalf("splitCommand(%q) admitted %q in argument %q", cmd, bad, arg)
				}
			}
		}

		// NOTHING ESCALATES. Checked against the returned argv rather
		// than the input, because the argv is what would be executed.
		switch argv[0] {
		case "sudo", "doas", "pkexec":
			t.Fatalf("splitCommand(%q) admitted a privilege escalation: %q", cmd, argv)
		}
	})
}

// And the refusals stay TELLABLE APART, because the caller prints a
// different sentence for each. A single generic error would collapse
// "there is nothing to run" into "this command is unsafe".
func FuzzSplitCommandRefusalsAreDistinct(f *testing.F) {
	for _, seed := range []string{"", "sudo x", "cmd;rm", "brew install ollama"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, cmd string) {
		_, err := splitCommand(cmd)
		if err == nil {
			return
		}
		n := 0
		for _, sentinel := range []error{ErrNothingToRun, ErrSudo, ErrUnsafeCommand} {
			if errors.Is(err, sentinel) {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("splitCommand(%q) matched %d sentinels: %v", cmd, n, err)
		}
	})
}
