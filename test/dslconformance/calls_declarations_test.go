package dslconformance

import (
	"regexp"
	"strings"
	"testing"
)

// The calls namespace's declaration gates, read from the embedded tree the way the
// sibling gates read theirs.

// constructBlock returns the text of the named query or mutation in a calls file.
func callsConstruct(t *testing.T, file, name string) string {
	t.Helper()
	src := readTreeFile(t, "calls/"+file)
	re := regexp.MustCompile(`(?m)^(?:query|mutation) \w+ ` + regexp.QuoteMeta(name) + ` \{`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("calls/%s declares no construct %s", file, name)
	}
	rest := src[loc[0]:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// EVERY DIGEST LOOKUP IS SCOPED TO ONE SITE, as siteAccessCodeByHash is: a key, pass or
// link minted for one of an owner's deployables must never resolve at another.
func TestCallsDigestLookupsAreSiteScoped(t *testing.T) {
	for _, name := range []string{
		"callsKeyByDigest", "callsSessionByTokenDigest",
		"callsRoomByGuestDigest", "callsRoomByHostDigest",
		"callsScheduledCallByGuestDigest", "callsScheduledCallByHostDigest",
		"callsInvitationLinkById",
	} {
		body := callsConstruct(t, "queries.memql", name)
		if !regexp.MustCompile(`siteId\s+string!`).MatchString(body) {
			t.Errorf("%s takes no required siteId argument", name)
		}
		if !strings.Contains(body, "row.siteId == args.siteId") {
			t.Errorf("%s does not filter on row.siteId == args.siteId", name)
		}
	}
}

// A FIELD DESCRIBED AS "ABSENT READS AS X" MUST BE GIVEN X BY THE CREATE. An unset field
// never equals a literal, so a room created with no status would escape isOpen and a
// server created with no state would never be offered for placement.
func TestCallsCreatesWriteTheDefaultsTheirConceptsDescribe(t *testing.T) {
	for _, c := range []struct{ mutation, stamp string }{
		{"createCallsKey", `kind: args.kind ?? "test"`},
		{"createCallsKey", `status: args.status ?? "active"`},
		{"createCallsRoom", `status: args.status ?? "open"`},
		{"createCallsQueueEntry", `status: args.status ?? "waiting"`},
		{"createCallsScheduledCall", `status: args.status ?? "scheduled"`},
		{"createCallsMediaServer", `state: args.state ?? "active"`},
	} {
		body := callsConstruct(t, "mutations.memql", c.mutation)
		if !strings.Contains(body, c.stamp) {
			t.Errorf("%s does not stamp %q", c.mutation, c.stamp)
		}
	}
}

// NOTHING SHIPPED IN THE CALLS NAMESPACE OR THE SWITCH'S DESCRIPTION CALLS A ROOM
// "anonymous": a room is joined without a MemQL account, which is what is true.
func TestCallsWordingIsAccurate(t *testing.T) {
	for _, p := range []string{"calls/concepts.memql", "calls/queries.memql", "calls/mutations.memql", "calls/shapes.memql", "platform/concepts.memql"} {
		src := readTreeFile(t, p)
		for i, line := range strings.Split(src, "\n") {
			if strings.Contains(line, "calls") && strings.Contains(strings.ToLower(line), "anonymous") {
				t.Errorf("%s:%d calls the capability anonymous", p, i+1)
			}
		}
	}
}
