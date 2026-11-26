package memql

import (
	"sort"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// EVERY CALLS CONCEPT IS THE CALLS INTEGRATION'S ALONE. A browser and an agent
// reach a call only through the calls integration, which reads as the site's
// owner with internal origin and decides each row by native admission, host and
// grant checks the row tier cannot state. A calls concept left off
// clientUnreadableConcepts is a second door nobody chose: the site's owner or a
// cluster owner reads its rows raw, and every write publishes a graph event
// carrying the whole row to the bus, where an authored automation can journal it.
//
// The moderated Questions queue was that door when it was found: its rows carry a
// participant's private question text, readable natively only by the author and
// an authorized host, and nothing listed the concept. This pins the rule for
// every calls concept, the next one included, rather than for that one name.
func TestEveryCallsConceptIsClientUnreadable(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	var calls []string
	for name := range memorynodes.All() {
		if strings.HasPrefix(name, "v1:calls:") {
			calls = append(calls, name)
		}
	}
	sort.Strings(calls)
	if len(calls) < 30 {
		t.Fatalf("found %d v1:calls concepts; the calls domain did not load, so this test would pass empty", len(calls))
	}
	for _, name := range calls {
		reason, listed := clientUnreadableConcepts[name]
		if !listed {
			t.Errorf("%s is not on clientUnreadableConcepts: a raw read by the site's owner or a cluster owner would reach its rows, and its writes would publish graph events carrying them", name)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is listed without its reason", name)
		}
	}
}

// The question concept's listing names its own reason: the rows hold a
// participant's words, not bookkeeping digests, so the calls-wide reason would
// describe them wrongly to the next reader.
func TestCallsQuestionsAreUnreadableForTheirTextsReason(t *testing.T) {
	const questions = "v1:calls:workflowQuestion"
	reason, listed := clientUnreadableConcepts[questions]
	if !listed {
		t.Fatalf("%s is not on clientUnreadableConcepts", questions)
	}
	if reason == callsClientUnreadableReason || !strings.Contains(reason, "question text") {
		t.Errorf("%s is listed with %q; it should say that its rows hold private question text", questions, reason)
	}
}
