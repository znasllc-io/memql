package logstore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

func ctxWithRole(role auth.Role) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "user-" + string(role), Role: role})
}

func newTestIntegration(sink *Sink) *Integration {
	return &Integration{
		logger:    discard(),
		limiter:   NewClientLimiter(),
		now:       func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) },
		sink:      func() *Sink { return sink },
		container: "",
	}
}

// The role floors, at every rung of the one ladder plus the actors that are
// not people. Developer OUTRANKS admin in this cluster, so an admin floor
// admits it -- a string comparison against "admin" would not.
func TestRoleFloors(t *testing.T) {
	admitReads := map[auth.Role]bool{
		auth.RoleOwner: true, auth.RoleDeveloper: true, auth.RoleAdmin: true,
		auth.RoleWriter: false, auth.RoleReader: false,
	}
	for role, want := range admitReads {
		err := requireAdmin(ctxWithRole(role))
		if (err == nil) != want {
			t.Errorf("requireAdmin(%s) = %v, want admitted=%v", role, err, want)
		}
		if err != nil && !strings.Contains(err.Error(), "admin, developer or owner required") {
			t.Errorf("the refusal must name the floor: %v", err)
		}
	}
	for role, want := range map[auth.Role]bool{auth.RoleOwner: true, auth.RoleDeveloper: false, auth.RoleAdmin: false, auth.RoleWriter: false} {
		if err := requireOwner(ctxWithRole(role)); (err == nil) != want {
			t.Errorf("requireOwner(%s) = %v, want admitted=%v", role, err, want)
		}
	}
	// Not people: refused by every floor, including the principal one.
	anon := auth.ContextWithAnonymousActor(context.Background())
	connector := auth.ContextWithAccess(context.Background(), auth.ConnectorActor("shopify"))
	none := context.Background()
	for name, ctx := range map[string]context.Context{"anonymous": anon, "connector": connector, "none": none} {
		if err := requireAdmin(ctx); err == nil {
			t.Errorf("requireAdmin admitted %s", name)
		}
		if err := requireOwner(ctx); err == nil {
			t.Errorf("requireOwner admitted %s", name)
		}
		if _, err := requirePrincipal(ctx); err == nil {
			t.Errorf("requirePrincipal admitted %s", name)
		}
	}
	// A reader is a person: recordClient admits them.
	if _, err := requirePrincipal(ctxWithRole(auth.RoleReader)); err != nil {
		t.Errorf("requirePrincipal refused a signed-in reader: %v", err)
	}
	// The maintenance principal the nightly automation runs under clears
	// the owner floor -- that is what listing logsRetentionSweep buys.
	ma := auth.MaintenanceActor("logsRetentionSweep")
	if ma == nil {
		t.Fatal("logsRetentionSweep is not on the maintenance list; the nightly sweep would run as a reader and be refused by its own floor")
	}
	if err := requireOwner(auth.ContextWithAccess(context.Background(), ma)); err != nil {
		t.Errorf("the maintenance principal must clear the owner floor: %v", err)
	}
}

func TestHandlersRepeatTheFloorBeforeTouchingAnything(t *testing.T) {
	i := newTestIntegration(nil)
	writer := ctxWithRole(auth.RoleWriter)
	if _, err := i.handleSearch(writer, map[string]any{"windowStart": "2026-09-01T00:00:00Z", "windowEnd": "2026-09-02T00:00:00Z"}, 0); err == nil || !strings.Contains(err.Error(), "may not read") {
		t.Errorf("search by a writer: %v", err)
	}
	if _, err := i.handleTail(writer, map[string]any{}, 0); err == nil {
		t.Errorf("tail by a writer was admitted")
	}
	if _, err := i.handleSources(writer, map[string]any{}, 0); err == nil {
		t.Errorf("sources by a writer was admitted")
	}
	if _, err := i.handleStatus(writer, nil, 0); err == nil {
		t.Errorf("status by a writer was admitted")
	}
	if _, err := i.handleArchiveList(writer, nil, 0); err == nil {
		t.Errorf("archiveList by a writer was admitted")
	}
	admin := ctxWithRole(auth.RoleAdmin)
	if _, err := i.handleSweep(admin, nil, 0); err == nil || !strings.Contains(err.Error(), "cluster owner") {
		t.Errorf("sweep by an admin: %v", err)
	}
	if _, err := i.handleArchiveRestore(admin, map[string]any{"day": "2026-08-01"}, 0); err == nil {
		t.Errorf("archiveRestore by an admin was admitted")
	}
	// Admitted callers hit the missing-database answer, which proves the
	// floor was the first check and the database the second.
	if _, err := i.handleSearch(admin, map[string]any{"windowStart": "2026-09-01T00:00:00Z", "windowEnd": "2026-09-02T00:00:00Z"}, 0); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Errorf("search by an admin with no database: %v", err)
	}
	if _, err := i.handleSearch(admin, map[string]any{"windowStart": "2026-09-01T00:00:00Z"}, 0); err == nil || !strings.Contains(err.Error(), "windowEnd is required") {
		t.Errorf("a missing window bound must be refused before the read: %v", err)
	}
	if _, err := i.handleSearch(admin, map[string]any{"windowStart": "yesterday", "windowEnd": "2026-09-02T00:00:00Z"}, 0); err == nil || !strings.Contains(err.Error(), "RFC 3339") {
		t.Errorf("an unreadable datetime must be refused: %v", err)
	}
}

func TestRecordClientStampsTheActorAndRefusesTheRest(t *testing.T) {
	capture := &captureInsert{}
	sink := NewSink(nil, SinkOptions{QueueSize: 500, FlushInterval: 5 * time.Millisecond, MaxLinesPerSecond: 100000,
		NodeType: "bff", Node: "bff-7", Insert: capture.insert, Logger: discard()})
	sink.Start()
	t.Cleanup(func() { stopSink(t, sink) })
	i := newTestIntegration(sink)
	user := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "user-42", Role: auth.RoleReader})
	args := func(n int) map[string]any {
		lines := make([]any, 0, n)
		for k := 0; k < n; k++ {
			lines = append(lines, map[string]any{"level": "error", "message": "boom", "app": "files", "at": "2026-09-03T11:59:30Z"})
		}
		return map[string]any{"session": "os-abcd1234", "lines": lines}
	}

	// Anonymous and connector actors are refused; nothing is written.
	if _, err := i.handleRecordClient(auth.ContextWithAnonymousActor(context.Background()), args(1), 0); err == nil {
		t.Errorf("an anonymous actor recorded a line")
	}
	if _, err := i.handleRecordClient(auth.ContextWithAccess(context.Background(), auth.ConnectorActor("shopify")), args(1), 0); err == nil {
		t.Errorf("a connector recorded a line")
	}
	// A bad session and an oversized call are errors naming the cap.
	if _, err := i.handleRecordClient(user, map[string]any{"session": "x", "lines": args(1)["lines"]}, 0); err == nil || !strings.Contains(err.Error(), "session") {
		t.Errorf("bad session: %v", err)
	}
	if _, err := i.handleRecordClient(user, args(51), 0); err == nil || !strings.Contains(err.Error(), "cap of 50") {
		t.Errorf("51 lines: %v", err)
	}

	// A good call: stamped userId, nodeType os, node blank, session kept,
	// the userId in the payload -- were one sent -- never read.
	nodes, err := i.handleRecordClient(user, args(3), 0)
	if err != nil {
		t.Fatalf("recordClient: %v", err)
	}
	var reply ClientReply
	if err := json.Unmarshal(nodes[0].Payload, &reply); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if reply.Accepted != 3 || reply.Dropped != 0 || reply.Reason != "" {
		t.Errorf("reply %+v", reply)
	}
	stopSink(t, sink)
	rows := capture.all()
	if len(rows) != 3 {
		t.Fatalf("stored %d rows, want 3", len(rows))
	}
	r := rows[0]
	if r.UserId != "user-42" || r.NodeType != NodeTypeOS || r.Node != "" || r.Session != "os-abcd1234" || r.App != "files" || r.Component != "os.files" || r.Level != "error" {
		t.Errorf("OS row stamp: %+v", r)
	}
	if !r.OccurredAt.Equal(time.Date(2026, 9, 3, 11, 59, 30, 0, time.UTC)) {
		t.Errorf("an `at` inside the skew must be kept: %v", r.OccurredAt)
	}

	// The (user, session) bucket: 120 lines, then a rate_limited REPLY, not
	// an error, so the shell drops and counts rather than retrying.
	i2 := newTestIntegration(sink)
	for k := 0; k < 4; k++ { // 4 x 30 = 120
		if _, err := i2.handleRecordClient(user, args(30), 0); err != nil {
			t.Fatalf("call %d: %v", k, err)
		}
	}
	nodes, err = i2.handleRecordClient(user, args(5), 0)
	if err != nil {
		t.Fatalf("over-rate call errored instead of replying: %v", err)
	}
	if err := json.Unmarshal(nodes[0].Payload, &reply); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if reply.Accepted != 0 || reply.Dropped != 5 || reply.Reason != ReasonRateLimited {
		t.Errorf("over-rate reply %+v, want {0 5 rate_limited}", reply)
	}
	// No sink on this node: a sentence, not a silent zero.
	if _, err := newTestIntegration(nil).handleRecordClient(user, args(1), 0); err == nil || !strings.Contains(err.Error(), "keeps no log lines") {
		t.Errorf("no store: %v", err)
	}
}

func TestArgDecoding(t *testing.T) {
	args := map[string]any{
		"limit":    float64(1e30),
		"big":      int64(1) << 62,
		"list":     []any{"a", " b ", 3, ""},
		"when":     "2026-09-03T12:00:00.5Z",
		"whenBad":  "noon",
		"whenTime": time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
	}
	if got := argInt(args, "limit", 200); got != 200 {
		t.Errorf("a float64 that does not fit must answer the default, got %d", got)
	}
	if got := argInt(args, "big", 200); got != 1<<62 {
		t.Errorf("an int64 that fits must be kept, got %d", got)
	}
	if got := argInt(args, "absent", 7); got != 7 {
		t.Errorf("absent -> %d", got)
	}
	if got := argStrings(args, "list"); strings.Join(got, ",") != "a,b" {
		t.Errorf("argStrings = %v", got)
	}
	if at, ok, err := argTime(args, "when"); err != nil || !ok || at.Nanosecond() != 500_000_000 {
		t.Errorf("argTime string: %v %v %v", at, ok, err)
	}
	if _, _, err := argTime(args, "whenBad"); err == nil {
		t.Errorf("an unreadable datetime must error rather than read as open")
	}
	if at, ok, _ := argTime(args, "whenTime"); !ok || at.Hour() != 12 {
		t.Errorf("argTime time.Time: %v %v", at, ok)
	}
	if _, ok, err := argTime(args, "absent"); ok || err != nil {
		t.Errorf("absent datetime: %v %v", ok, err)
	}
	q, err := queryFromArgs(map[string]any{"levels": []any{"fatal"}})
	if err == nil {
		t.Errorf("an unknown level must be refused: %+v", q)
	}
	if got := normalizeLimit(0); got != DefaultLimit {
		t.Errorf("normalizeLimit(0) = %d", got)
	}
	if got := normalizeLimit(10_000); got != MaxLimit {
		t.Errorf("normalizeLimit(10000) = %d", got)
	}
}

func TestRowNodesCarryTheConceptFields(t *testing.T) {
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	nodes, err := rowNodes([]Row{{OccurredAt: at, ID: "r1", NodeType: "bff", Node: "bff-1", Level: "warn", Component: "svc",
		Message: "m", Attributes: json.RawMessage(`{"k":1}`), Subject: "s", SubjectConcept: "v1:x:y"}})
	if err != nil {
		t.Fatalf("rowNodes: %v", err)
	}
	n := nodes[0]
	if n.ID != "r1" || n.Concept != ConceptID || !n.CreatedAt.Equal(at) || n.Type != "object" {
		t.Errorf("node: %+v", n)
	}
	var payload map[string]any
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	for _, key := range []string{"occurredAt", "id", "nodeType", "node", "level", "component", "app", "message", "attributes", "subject", "subjectConcept", "session", "userId"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("payload lacks %q: %v", key, payload)
		}
	}
	if attrs, _ := payload["attributes"].(map[string]any); attrs["k"] != float64(1) {
		t.Errorf("attributes must be inline: %v", payload["attributes"])
	}
}

func TestCapabilitiesMatchTheDeclaredExecutors(t *testing.T) {
	i := newTestIntegration(nil)
	if i.IntegrationName() != "logs" {
		t.Fatalf("name %q", i.IntegrationName())
	}
	want := map[string]bool{"search": true, "tail": true, "sources": true, "status": false, "recordClient": false, "sweep": false, "archiveList": true, "archiveRestore": false}
	got := map[string]bool{}
	for _, c := range i.Capabilities() {
		if c.Handler == nil {
			t.Errorf("%s has no handler", c.Name)
		}
		got[c.Name] = c.PreserveOrder
	}
	for name, preserve := range want {
		p, ok := got[name]
		if !ok {
			t.Errorf("capability %s missing (the DSL declares integration.logs.%s)", name, name)
			continue
		}
		if p != preserve {
			t.Errorf("%s PreserveOrder = %v, want %v", name, p, preserve)
		}
	}
	if len(got) != len(want) {
		t.Errorf("capabilities %v", got)
	}
}
