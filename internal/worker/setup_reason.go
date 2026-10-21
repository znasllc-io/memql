package worker

import (
	"os"
	"strings"
)

// setupReasonEnv names a file `worker setup` writes its outcome to, as one
// line, when it does not finish (memql-cockpit#524). The installers set it
// and print the line in their closing summary -- "This machine can't run
// local models: <reason>" -- instead of a bare exit code. Unset writes
// nothing, so an older installer or a person at a terminal sees no
// difference.
const setupReasonEnv = "MEMQL_SETUP_REASON_FILE"

// maxSetupReason bounds the line: it is printed in a terminal summary.
const maxSetupReason = 400

func recordSetupReason(text string) {
	path := strings.TrimSpace(os.Getenv(setupReasonEnv))
	one := strings.Join(strings.Fields(text), " ")
	if path == "" || one == "" {
		return
	}
	if len(one) > maxSetupReason {
		one = strings.ToValidUTF8(one[:maxSetupReason], "") + "..."
	}
	_ = os.WriteFile(path, []byte(one+"\n"), 0o600)
}
