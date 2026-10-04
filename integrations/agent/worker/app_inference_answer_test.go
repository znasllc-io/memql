//go:build agent

package worker

import (
	"testing"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

// A plain chat turn answered through the app door returns the app's own
// output. The transcript also carries stderr -- the cockpit's note of which
// model the level ran as -- and that note is a record, not part of the reply.
func TestAnswerFromAPlainTurnIsTheAppsOwnOutput(t *testing.T) {
	got, err := answerFrom(workerservice.RunResult{
		Answer:     "Hi! What would you like help with?",
		Transcript: "[memql] level strong runs claude-code with --model sonnet --effort high (the cockpit's built-in table)\nHi! What would you like help with?",
	}, false)
	if err != nil {
		t.Fatalf("answerFrom: %v", err)
	}
	if got != "Hi! What would you like help with?" {
		t.Errorf("answer = %q, want the app's stdout alone", got)
	}
}
