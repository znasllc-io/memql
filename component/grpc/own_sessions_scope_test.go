package memql

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The session read behind both revoke handlers is CALLER-SCOPED (memql#4768).
//
// # What this stops coming back
//
// It was `authSessionsForSubject(subject: ...)`: filtered on that argument and
// nothing else -- no role gate, no @serverOnly -- so any signed-in caller
// could pass any user id over the wire and read that person's sessions. Its
// two Go callers here only ever passed the caller's own verified JWT `sub`, so
// the argument bought nothing and cost everything.
//
// The fix removes the enumeration surface instead of gating it: the query
// takes no argument and filters `subject==actor.userId`. That is only true
// while THREE things hold together, and each is asserted below because each
// fails silently on its own.
//
// # Why this is a source test rather than an engine test
//
// The property is "no id reaches this read from Go, and the read runs as the
// caller". An engine test proves the query filters correctly; it cannot see a
// handler that reintroduces an argument or hands the query the elevated
// context.
//
// On the second: `contextWithSystemActor` replaces claims + TokenInfo and
// leaves the AccessContext alone, and `actor.*` binds from the AccessContext
// -- so the read resolves to the caller under either context TODAY. This
// pins the caller's context anyway, because the alternative works by which
// fields that helper happens not to touch. Attaching an AccessContext to the
// system actor is the natural way to make it real, and the day someone does,
// a caller-scoped read under it matches nothing and revoke-all reports
// revoking zero on a healthy account -- no error, no log line.

func authSessionHandlerSource(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("auth_session_handlers.go"))
	if err != nil {
		t.Fatalf("read auth_session_handlers.go: %v", err)
	}
	return string(body)
}

//  1. The query is invoked with NO arguments. A parenthesised argument list
//     here would mean an id had been reintroduced on a read whose whole
//     defence is that it does not take one.
func TestOwnSessionsReadTakesNoArgument(t *testing.T) {
	src := authSessionHandlerSource(t)

	if !strings.Contains(src, "query authSessionsForSelfIncludingRevoked()") {
		t.Fatalf("the caller-scoped session read is gone from auth_session_handlers.go.\n" +
			"If it was renamed, update this test. If it grew an argument, do not: " +
			"the read is caller-scoped precisely so there is no id to pass (memql#4768).")
	}

	// The old spelling must not reappear under any argument.
	if strings.Contains(src, "authSessionsForSubject") &&
		!strings.Contains(src, "// `authSessionsForSubject(subject: ...)`") {
		t.Errorf("authSessionsForSubject is referenced outside its historical note -- " +
			"that query was replaced because a caller-supplied subject let any signed-in " +
			"caller read anyone's sessions (memql#4768)")
	}
}

// 2. The read runs on the STREAM context, never on the elevated one.
//
//	contextWithSystemActor stamps sub=grpc-system-actor, so a read handed
//	that context compares `subject` against the bridge agent and matches
//	nothing. The writes still need it; the read must not have it.
func TestOwnSessionsReadRunsAsTheCaller(t *testing.T) {
	src := authSessionHandlerSource(t)

	// Line-based rather than one regex over the file: the argument list
	// contains a nested `)`, so a naive `[^)]*` group truncates at
	// `s.stream.Context(` and then fails on its own truncation -- a test that
	// reports the code is wrong when the test is.
	calls := 0
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "listOwnSessions(") {
			continue
		}
		if strings.HasPrefix(trimmed, "func ") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		calls++
		if !strings.Contains(trimmed, "listOwnSessions(s.stream.Context()") {
			t.Errorf("call site does not read from the stream context: %s\n"+
				"A caller-scoped read must take the context that defines its scope. The "+
				"elevated context here happens to leave the AccessContext alone, so it "+
				"resolves the same today -- but that is a coupling, not a guarantee: give "+
				"the system actor an AccessContext and this read silently matches nothing, "+
				"reporting 'revoked 0 sessions' on a healthy account (memql#4768).", trimmed)
		}
	}
	if calls == 0 {
		t.Fatal("no listOwnSessions call site found -- if the helper was renamed, update this test")
	}
	// Both revoke handlers read it. One would mean a call site was dropped or
	// went back to reading sessions some other way.
	if calls != 2 {
		t.Errorf("found %d listOwnSessions call sites, want 2 (revoke-all and revoke-one)", calls)
	}
}

// 3. The helper itself does not accept a subject.
//
//	Signature-level, because a helper that takes one invites a caller to
//	supply one, and the next caller may not be reading its own claims.
func TestListOwnSessionsHasNoSubjectParameter(t *testing.T) {
	src := authSessionHandlerSource(t)

	sig := regexp.MustCompile(`func listOwnSessions\(([^)]*)\)`).FindStringSubmatch(src)
	if sig == nil {
		t.Fatal("listOwnSessions signature not found -- if renamed, update this test")
	}
	if strings.Contains(sig[1], "subject") {
		t.Errorf("listOwnSessions takes a subject parameter (%s).\n"+
			"It must not: the caller is the scope, and a parameter is what let this "+
			"read be pointed at other people (memql#4768).", sig[1])
	}
}
