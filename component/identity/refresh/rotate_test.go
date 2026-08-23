package refresh

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// rotate_test.go covers what the rotator WRITES, which until memql#4328 was
// covered only indirectly through the HTTP handler and not at all for the
// grace-window path.
//
// Three things are under test and they are easy to conflate:
//   - which LOG a row lands on (audit vs activity),
//   - whether the row names its actor (memql#4327 -- the Trail's actor column
//     was blank for every rotation),
//   - and reuse detection (memql#4329), which is a lookup on the retired hash
//     that the activity row is what records.

const (
	testSessionId = "v1:identity:authSession:sess-1"
	testUserId    = "v1:identity:user:owner-1"
	testUserEmail = "owner@example.com"
	testUserRole  = "owner"
)

// rotateFakeEngine answers the reads and writes the rotation path drives.
// Query strings are matched by construct name; nothing here parses MemQL, so
// the parse-level coverage lives in component/identity/activity_sink_test.go
// and component/grpc/render_query_args_parse_test.go instead.
type rotateFakeEngine struct {
	mu sync.Mutex

	curHash  string
	prevHash string
	prevAt   time.Time
	revoked  bool

	// retiredHashes maps a retired refresh-token hash to the session it
	// belonged to -- what an authActivity row records, seen from the lookup's
	// side.
	retiredHashes map[string]string

	rotations      int
	revokeCalls    []string // revokedReason per call
	activityLookup []string // hashes looked up
}

func (f *rotateFakeEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	empty := &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}

	switch {
	case strings.Contains(q, "authSessionByRefreshTokenHash("):
		if extractArg(q, "refreshTokenHash") != f.curHash || f.curHash == "" {
			return empty, nil
		}
		return f.sessionBundle(), nil

	case strings.Contains(q, "authSessionByPreviousRefreshTokenHash("):
		if extractArg(q, "previousRefreshTokenHash") != f.prevHash || f.prevHash == "" {
			return empty, nil
		}
		return f.sessionBundle(), nil

	case strings.Contains(q, "authActivityByRetiredHash("):
		hash := extractArg(q, "retiredTokenHash")
		f.activityLookup = append(f.activityLookup, hash)
		sess, ok := f.retiredHashes[hash]
		if !ok {
			return empty, nil
		}
		node := &memqlv1.MemoryNode{
			Id: "v1:identity:authActivity:act-1",
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"id":               structpb.NewStringValue("v1:identity:authActivity:act-1"),
				"sessionId":        structpb.NewStringValue(sess),
				"actorUserId":      structpb.NewStringValue(testUserId),
				"retiredTokenHash": structpb.NewStringValue(hash),
				"occurredAt":       structpb.NewStringValue("2026-08-23T09:00:00Z"),
			}},
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{node}}}, nil

	case strings.Contains(q, "rotateAuthSession("):
		f.rotations++
		if f.retiredHashes == nil {
			f.retiredHashes = map[string]string{}
		}
		if prev := extractArg(q, "previousRefreshTokenHash"); prev != "" {
			f.retiredHashes[prev] = testSessionId
		}
		f.prevHash = extractArg(q, "previousRefreshTokenHash")
		f.prevAt = time.Now().UTC()
		f.curHash = extractArg(q, "newRefreshTokenHash")
		return empty, nil

	case strings.Contains(q, "revokeAuthSession("):
		f.revoked = true
		f.revokeCalls = append(f.revokeCalls, extractArg(q, "revokedReason"))
		return empty, nil

	case strings.Contains(q, "userByIdSystem(") || strings.Contains(q, "userById("):
		node := &memqlv1.MemoryNode{
			Id: testUserId,
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"id":           structpb.NewStringValue(testUserId),
				"primaryEmail": structpb.NewStringValue(testUserEmail),
				"role":         structpb.NewStringValue(testUserRole),
				"active":       structpb.NewBoolValue(true),
			}},
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{node}}}, nil
	}
	return empty, nil
}

func (f *rotateFakeEngine) sessionBundle() *memqlengine.ExecuteResult {
	fields := map[string]*structpb.Value{
		"id":                       structpb.NewStringValue(testSessionId),
		"userId":                   structpb.NewStringValue(testUserId),
		"refreshTokenHash":         structpb.NewStringValue(f.curHash),
		"previousRefreshTokenHash": structpb.NewStringValue(f.prevHash),
		"clientLabel":              structpb.NewStringValue("Firefox on Linux"),
	}
	if !f.prevAt.IsZero() {
		fields["previousRotatedAt"] = structpb.NewStringValue(f.prevAt.Format(time.RFC3339Nano))
	}
	if f.revoked {
		fields["revokedAt"] = structpb.NewStringValue(time.Now().UTC().Format(time.RFC3339Nano))
	}
	node := &memqlv1.MemoryNode{Id: testSessionId, Payload: &structpb.Struct{Fields: fields}}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{node}}}
}

// extractArg reads `name: "value"` out of a rendered call.
func extractArg(q, name string) string {
	i := strings.Index(q, name+`: "`)
	if i < 0 {
		return ""
	}
	rest := q[i+len(name)+3:]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

type recordingAudit struct {
	mu     sync.Mutex
	events []identity.AuditEvent
}

func (r *recordingAudit) Log(_ context.Context, ev identity.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingAudit) onStream(s identity.AuditStream) []identity.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []identity.AuditEvent
	for _, ev := range r.events {
		if ev.Stream == s {
			out = append(out, ev)
		}
	}
	return out
}

func newRotator(t *testing.T, eng *rotateFakeEngine) (*Rotator, *recordingAudit) {
	t.Helper()
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	if err != nil {
		t.Fatalf("key manager: %v", err)
	}
	if err := km.Load(); err != nil {
		t.Fatalf("load keys: %v", err)
	}
	cfg := identity.Config{Enabled: true, BaseURL: "https://identity.test", JWTAudience: "memql", KeyDir: dir}
	iss, err := identity.NewJWTIssuer(km, cfg)
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}
	audit := &recordingAudit{}
	return &Rotator{
		Cfg:    cfg,
		Store:  &identity.Store{Engine: eng},
		Issuer: iss,
		Audit:  audit,
	}, audit
}

func seedSession(t *testing.T) (*rotateFakeEngine, string) {
	t.Helper()
	plain, hash, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return &rotateFakeEngine{curHash: hash, retiredHashes: map[string]string{}}, plain
}

// A rotation is a MECHANIC: one activity row, no audit row. That split is the
// whole point of memql#4328 -- the operator's Audit Trail is a generic concept
// walk over auditEvent with no filter of any kind, so a mechanic on that log is
// a mechanic in the operator's face.
func TestRotationWritesActivityNotAudit(t *testing.T) {
	eng, plain := seedSession(t)
	r, audit := newRotator(t, eng)

	if _, err := r.Rotate(context.Background(), RotateInput{
		PresentedRefreshToken: plain,
		SourceIP:              "203.0.113.7",
		UserAgent:             "Mozilla/5.0",
	}); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	if got := audit.onStream(identity.StreamAudit); len(got) != 0 {
		t.Errorf("a rotation wrote %d audit row(s): %+v", len(got), got)
	}
	activity := audit.onStream(identity.StreamActivity)
	if len(activity) != 1 {
		t.Fatalf("a rotation wrote %d activity row(s), want 1", len(activity))
	}
	ev := activity[0]
	if ev.Action != "session_refreshed" {
		t.Errorf("action = %q, want session_refreshed", ev.Action)
	}
	if ev.Outcome != identity.AuditOutcomeSuccess {
		t.Errorf("outcome = %q, want success", ev.Outcome)
	}
	// memql#4327: the Trail's actor column was blank on every rotation.
	if ev.ActorEmail != testUserEmail || ev.ActorRole != testUserRole {
		t.Errorf("actor = (%q, %q), want (%q, %q) -- the row must name who refreshed",
			ev.ActorEmail, ev.ActorRole, testUserEmail, testUserRole)
	}
	if ev.ActorUserId != testUserId {
		t.Errorf("actorUserId = %q, want %q", ev.ActorUserId, testUserId)
	}
	if ev.ClientLabel != "Firefox on Linux" {
		t.Errorf("clientLabel = %q, want the session's label", ev.ClientLabel)
	}
	// memql#4328/#4329: the retired hash is what reuse detection keys on. A
	// rotation that does not record it makes the detection blind.
	if ev.RetiredHash == "" {
		t.Error("the rotation recorded no retiredTokenHash, so a replay of the token it just " +
			"retired would be indistinguishable from a stale cookie")
	}
	if ev.SourceIP != "203.0.113.7" || ev.UserAgent != "Mozilla/5.0" {
		t.Errorf("source = (%q, %q), want the presenting IP and UA", ev.SourceIP, ev.UserAgent)
	}
}

// A refusal is a mechanic too.
func TestBlockedRotationWritesActivity(t *testing.T) {
	eng := &rotateFakeEngine{curHash: "not-the-presented-hash", retiredHashes: map[string]string{}}
	r, audit := newRotator(t, eng)

	plain, _, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := r.Rotate(context.Background(), RotateInput{PresentedRefreshToken: plain}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Rotate = %v, want ErrSessionNotFound", err)
	}
	if got := audit.onStream(identity.StreamAudit); len(got) != 0 {
		t.Errorf("a blocked rotation wrote %d audit row(s)", len(got))
	}
	activity := audit.onStream(identity.StreamActivity)
	if len(activity) != 1 || activity[0].Action != "session_refresh_blocked" {
		t.Fatalf("activity = %+v, want one session_refresh_blocked", activity)
	}
	if activity[0].FailureReason != "session_not_found" {
		t.Errorf("failureReason = %q, want session_not_found", activity[0].FailureReason)
	}
	// It must NOT be a reuse: nothing ever retired this hash.
	if eng.revoked {
		t.Error("a token that was never issued revoked a session -- reuse detection must only " +
			"fire on a hash some rotation actually retired")
	}
}

// The grace window is the SPA-hard-refresh race, and it was slog-only. It is a
// legitimate mid-rotation retry and must never read as reuse.
func TestGraceWindowAcceptWritesItsOwnActivityRow(t *testing.T) {
	eng, plain := seedSession(t)
	r, audit := newRotator(t, eng)

	// First rotation moves the hash forward; `plain` is now the PREVIOUS one.
	if _, err := r.Rotate(context.Background(), RotateInput{PresentedRefreshToken: plain}); err != nil {
		t.Fatalf("first Rotate: %v", err)
	}
	// The client aborted before receiving the new cookie and retries with the
	// old token, inside the 30s window.
	if _, err := r.Rotate(context.Background(), RotateInput{PresentedRefreshToken: plain}); err != nil {
		t.Fatalf("grace-window Rotate: %v", err)
	}

	var grace int
	for _, ev := range audit.onStream(identity.StreamActivity) {
		if ev.Action == "grace_window_accept" {
			grace++
			if ev.Outcome != identity.AuditOutcomeSuccess {
				t.Errorf("grace_window_accept outcome = %q, want success", ev.Outcome)
			}
		}
	}
	if grace != 1 {
		t.Fatalf("wrote %d grace_window_accept row(s), want 1", grace)
	}
	if eng.revoked {
		t.Fatal("a grace-window retry revoked the session -- it is a legitimate retry, never reuse")
	}
}

// THE EVENT THAT WAS MISSING (memql#4329). A replay of a token some rotation
// retired is not a stale cookie: it means the token reached someone else.
func TestReplayOfARetiredTokenRevokesTheSession(t *testing.T) {
	eng, plain := seedSession(t)
	r, audit := newRotator(t, eng)

	var notices []SecurityNoticeInput
	r.SecurityNotice = func(_ context.Context, in SecurityNoticeInput) {
		notices = append(notices, in)
	}

	if _, err := r.Rotate(context.Background(), RotateInput{PresentedRefreshToken: plain}); err != nil {
		t.Fatalf("first Rotate: %v", err)
	}
	// Age the grace window out, so the retry path is closed and the replay is
	// judged on the retired-hash lookup alone.
	eng.mu.Lock()
	eng.prevAt = time.Now().Add(-time.Hour)
	eng.mu.Unlock()

	_, err := r.Rotate(context.Background(), RotateInput{
		PresentedRefreshToken: plain,
		SourceIP:              "198.51.100.9",
		UserAgent:             "curl/8.0",
	})
	if !errors.Is(err, ErrTokenMismatch) {
		t.Fatalf("replay returned %v, want ErrTokenMismatch", err)
	}
	if !eng.revoked {
		t.Fatal("the session was not revoked on a detected replay")
	}
	if len(eng.revokeCalls) != 1 || eng.revokeCalls[0] != "reuse_detected" {
		t.Errorf("revokedReason = %v, want [reuse_detected]", eng.revokeCalls)
	}

	// The SIGNAL goes to the audit log, not the activity log: it is a security
	// decision, and it is exactly the kind of row the Trail exists for.
	signals := audit.onStream(identity.StreamAudit)
	var found *identity.AuditEvent
	for i := range signals {
		if signals[i].Action == "refresh_token_reuse_detected" {
			found = &signals[i]
		}
	}
	if found == nil {
		t.Fatalf("no refresh_token_reuse_detected on the audit stream; got %+v", signals)
	}
	if found.Outcome != identity.AuditOutcomeBlocked {
		t.Errorf("outcome = %q, want blocked", found.Outcome)
	}
	if found.SourceIP != "198.51.100.9" || found.UserAgent != "curl/8.0" {
		t.Errorf("the signal must carry the PRESENTING IP and UA, got (%q, %q)",
			found.SourceIP, found.UserAgent)
	}
	if found.TargetId != testSessionId {
		t.Errorf("targetId = %q, want the revoked session", found.TargetId)
	}
	if _, ok := found.Detail["retiredAt"]; !ok {
		t.Errorf("the signal carries no retiredAt; detail = %v", found.Detail)
	}

	if len(notices) != 1 {
		t.Fatalf("sent %d security notice(s), want 1", len(notices))
	}
	if notices[0].UserId != testUserId || notices[0].SessionId != testSessionId {
		t.Errorf("notice = %+v, want it to name the affected user and session", notices[0])
	}
}

// A token nothing ever issued stays what it was: a stale cookie. Reuse
// detection must not manufacture a security signal out of one.
func TestReplayOfANeverIssuedTokenIsNotReuse(t *testing.T) {
	eng, _ := seedSession(t)
	r, audit := newRotator(t, eng)

	stranger, _, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := r.Rotate(context.Background(), RotateInput{PresentedRefreshToken: stranger}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Rotate = %v, want ErrSessionNotFound", err)
	}
	if eng.revoked {
		t.Fatal("an unknown token revoked a session")
	}
	for _, ev := range audit.onStream(identity.StreamAudit) {
		if ev.Action == "refresh_token_reuse_detected" {
			t.Fatal("an unknown token produced a reuse signal")
		}
	}
	// The lookup DID run -- otherwise this test proves nothing about the
	// detection path, only that an unrelated code path stayed quiet.
	if len(eng.activityLookup) == 0 {
		t.Fatal("the retired-hash lookup never ran, so this test could not have detected a reuse")
	}
}

// Detection reaches back exactly as far as the activity retention window. Past
// it the row is gone and the replay is indistinguishable from a stale cookie --
// a documented limit, asserted so it stays a known one.
func TestReplayOlderThanTheActivityWindowFallsBackToSessionNotFound(t *testing.T) {
	eng, plain := seedSession(t)
	r, audit := newRotator(t, eng)

	// TWO rotations, which is what "old" actually looks like: the session row
	// keeps exactly ONE previous hash, so after the second rotation the
	// original token matches neither the current nor the previous hash. A
	// token old enough to have aged out of a 30-day activity window is many
	// more rotations behind than that.
	res, err := r.Rotate(context.Background(), RotateInput{PresentedRefreshToken: plain})
	if err != nil {
		t.Fatalf("first Rotate: %v", err)
	}
	if _, err := r.Rotate(context.Background(), RotateInput{PresentedRefreshToken: res.RefreshToken}); err != nil {
		t.Fatalf("second Rotate: %v", err)
	}
	eng.mu.Lock()
	// The retention job has pruned the activity rows that recorded the hashes.
	eng.retiredHashes = map[string]string{}
	eng.mu.Unlock()

	if _, err := r.Rotate(context.Background(), RotateInput{PresentedRefreshToken: plain}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Rotate = %v, want ErrSessionNotFound once the activity row has aged out", err)
	}
	if eng.revoked {
		t.Fatal("the session was revoked with no evidence the hash was ever retired")
	}
	var blocked int
	for _, ev := range audit.onStream(identity.StreamActivity) {
		if ev.Action == "session_refresh_blocked" && ev.FailureReason == "session_not_found" {
			blocked++
		}
	}
	if blocked == 0 {
		t.Error("no session_not_found activity row for the aged-out replay")
	}
}
