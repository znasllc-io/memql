package worker

import (
	"os"

	"golang.org/x/term"
)

// isInteractiveTTY checks whether both stdin and stdout are real
// terminals. The TUI needs both: tcell reads keystrokes from stdin
// and writes escape sequences to stdout. CI / piped invocations
// have one or both as pipes, which is the signal to use the printf
// fallback.
//
// Lives in a tag-free file because both the computeruse-tagged TCC wizard
// and the (always-built) pair wizard use it.
func isInteractiveTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}
