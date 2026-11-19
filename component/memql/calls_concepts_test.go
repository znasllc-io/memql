package memql

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// callsConcepts is the eight concepts of the calls namespace (dsl/calls/concepts.memql).
// It is the EXPECTED set, not the iterated one: the test below takes the loaded set from
// the registry and asserts it equals this, so a ninth concept fails here until it is
// listed and carries both properties.
var callsConcepts = []string{
	"v1:calls:key", "v1:calls:mediaServer", "v1:calls:policy", "v1:calls:queueEntry",
	"v1:calls:room", "v1:calls:scheduledCall", "v1:calls:seat", "v1:calls:session",
}

// TestCallsConceptsAreServerWrittenAndUnreadable pins the two declarations the calls
// capability's secrecy rests on: every v1:calls concept is @serverWritten, so no raw
// write can mint a key, pass or seat around the integration, and sits on
// clientUnreadableConcepts, so no raw read, subscription or event hands a client a
// digest. The set is read from the loaded registry by prefix, so a concept added to the
// namespace without both fails here (and without being listed above).
func TestCallsConceptsAreServerWrittenAndUnreadable(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	var loaded []string
	for name, c := range memorynodes.All() {
		if !strings.HasPrefix(name, "v1:calls:") {
			continue
		}
		loaded = append(loaded, name)
		if !c.ServerWritten {
			t.Errorf("%s is not @serverWritten", name)
		}
		if _, listed := clientUnreadableConcepts[name]; !listed {
			t.Errorf("%s is not on clientUnreadableConcepts", name)
		}
	}
	sort.Strings(loaded)
	if strings.Join(loaded, ",") != strings.Join(callsConcepts, ",") {
		t.Errorf("the loaded calls concepts are %v, want exactly %v", loaded, callsConcepts)
	}
	for name := range clientUnreadableConcepts {
		if strings.HasPrefix(name, "v1:calls:") && memorynodes.All()[name] == nil {
			t.Errorf("clientUnreadableConcepts names %s, which the calls namespace does not declare", name)
		}
	}
}

// TestCallsPolicyIsASiteRow pins the policy's key: its SITE. It was keyed by a package
// deployable's name, which is unique only together with its package, so one package's
// limits applied to every other package's deployable of that name, across
// organizations. The row now names its site (required), belongs to the site's owner
// with the cluster owner's bypass, as every other site-scoped calls row does, and has
// no `deployable` field left to be keyed by.
func TestCallsPolicyIsASiteRow(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	c := memorynodes.All()["v1:calls:policy"]
	if c == nil {
		t.Fatal("v1:calls:policy is not loaded")
	}
	if c.RowAuthz == nil || c.RowAuthz.Tier != langparser.RowAuthzOwned || c.RowAuthz.Owner != "ownerUserId" || !c.RowAuthz.ClusterOwnerBypass {
		t.Fatalf("v1:calls:policy declares %+v, want @rowAuthz(owner=\"ownerUserId\", clusterOwner)", c.RowAuthz)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(c.Schemas["definition"], &schema); err != nil || len(schema.Properties) == 0 {
		t.Fatalf("v1:calls:policy declares no fields in the loaded tree: %v", err)
	}
	if _, keyedByName := schema.Properties["deployable"]; keyedByName {
		t.Error("v1:calls:policy still has a `deployable` field to be keyed by")
	}
	for _, field := range []string{"siteId", "ownerUserId"} {
		if _, ok := schema.Properties[field]; !ok {
			t.Errorf("v1:calls:policy has no %s field", field)
		}
		if !slices.Contains(c.RequiredFields(), field) {
			t.Errorf("v1:calls:policy does not require %s", field)
		}
	}
}

// TestCallsHotReadsHaveLookupIndexes holds payloadLookups to the reads the join and
// queue paths make on every request: seats and queue entries of one room, rooms and
// scheduled calls of one key, and the open rooms of one media server, which the
// capacity count reads under the server's gate. Without an index each enumerates every
// id of its concept.
func TestCallsHotReadsHaveLookupIndexes(t *testing.T) {
	for _, c := range []struct{ concept, field string }{
		{"v1:calls:seat", "roomId"}, {"v1:calls:queueEntry", "roomId"},
		{"v1:calls:room", "keyId"}, {"v1:calls:scheduledCall", "keyId"},
		{"v1:calls:room", "mediaServerId"},
		{"v1:calls:key", "digest"}, {"v1:calls:session", "tokenDigest"},
		{"v1:calls:room", "guestDigest"}, {"v1:calls:room", "hostDigest"},
		{"v1:calls:scheduledCall", "guestDigest"}, {"v1:calls:scheduledCall", "hostDigest"},
	} {
		term := payloadLookupTerm{field: c.field}
		if _, ok := payloadLookupFor(c.concept, term, []payloadLookupTerm{term}); !ok {
			t.Errorf("no payloadLookups entry serves %s.%s", c.concept, c.field)
		}
	}
}
