package memql

import (
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// callsConcepts is every concept of the calls namespace (dsl/calls/concepts.memql).
var callsConcepts = []string{
	"v1:calls:key", "v1:calls:session", "v1:calls:room", "v1:calls:seat",
	"v1:calls:queueEntry", "v1:calls:scheduledCall", "v1:calls:mediaServer", "v1:calls:policy",
}

// TestCallsConceptsAreServerWrittenAndUnreadable pins the two declarations the calls
// capability's secrecy rests on: every v1:calls concept is @serverWritten, so no raw
// write can mint a key, pass or seat around the integration, and sits on
// clientUnreadableConcepts, so no raw read, subscription or event hands a client a
// digest. A new concept added to the namespace without both fails here.
func TestCallsConceptsAreServerWrittenAndUnreadable(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	declared := map[string]bool{}
	for _, name := range callsConcepts {
		declared[name] = true
		c, err := memorynodes.Get(name)
		if err != nil || c == nil {
			t.Fatalf("%s is not in the loaded tree: %v", name, err)
		}
		if !c.ServerWritten {
			t.Errorf("%s is not @serverWritten", name)
		}
		if _, listed := clientUnreadableConcepts[name]; !listed {
			t.Errorf("%s is not on clientUnreadableConcepts", name)
		}
	}
	for name := range clientUnreadableConcepts {
		if strings.HasPrefix(name, "v1:calls:") && !declared[name] {
			t.Errorf("clientUnreadableConcepts names %s, which the calls namespace does not declare", name)
		}
	}
}
