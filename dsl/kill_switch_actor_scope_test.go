package dsl

import (
	"io"
	"regexp"
	"strings"
	"testing"
)

// memql#2840: the computer-use kill switch must only ever be flipped for the
// CALLER.
//
// `v1:identity:user.preferences.computerUseEnabled` is one of the three
// permission layers checked before any computer-use dispatch (agent capability
// flag, standing scope grant, per-user kill switch --
// component/memql/authoring_capability_gate.go). The mutation that sets it
// selected its row by `id: args.userId`, so any caller could disable -- or
// re-enable -- another user's kill switch.
//
// There is no other line of defence to fall back on. Mutations carry no
// executable guard (dsl/forge/mutations.memql says so outright: "the DSL cannot
// gate a transition on actor.role"), and the request path consults no
// capability before executing a named mutation (#2802). What the author writes
// in the DSL is the whole of the authorization, so the row selection has to be
// the actor.
//
// Scoped to this one mutation on purpose. The other ten `mutate user`
// constructs also select by `args.userId`, and several must: createUser and
// createUserOnFirstLogin run during provisioning before an actor exists, and
// the account-lifecycle writes are administrative by nature. A blanket rule
// would be wrong; deciding which of those may act on another user is the
// design work tracked in #2802 / #2803.
const killSwitchMutation = "worker/mutations.memql"

func TestComputerUseKillSwitchIsActorScoped(t *testing.T) {
	f, err := Tree().Open(killSwitchMutation)
	if err != nil {
		t.Fatalf("open %s: %v", killSwitchMutation, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", killSwitchMutation, err)
	}
	src := string(raw)

	start := strings.Index(src, "mutate user toggleComputerUseEnabled")
	if start < 0 {
		t.Fatalf("toggleComputerUseEnabled is no longer declared in %s; if it moved or was renamed this guard must follow it, not be deleted", killSwitchMutation)
	}
	open := strings.IndexByte(src[start:], '{')
	if open < 0 {
		t.Fatal("malformed mutation header")
	}
	depth, end := 1, -1
	for i := start + open + 1; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		t.Fatal("unterminated mutation body")
	}
	body := src[start+open+1 : end]

	idSel := regexp.MustCompile(`(?m)^\s*id:\s*(\S+)`)
	m := idSel.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("toggleComputerUseEnabled has no `id:` row selector")
	}
	if got := strings.TrimSuffix(m[1], ","); got != "actor.userId" {
		t.Errorf("toggleComputerUseEnabled selects its row by %q; it must be actor.userId, or any caller can flip another user's computer-use kill switch (memql#2840)", got)
	}

	// And the arg must be gone, not merely unused: leaving `userId` declared
	// invites a reader to believe it still targets that user, and the engine
	// silently drops undeclared args on the non-MCP path, so a stale client
	// would look like it was working.
	if regexp.MustCompile(`(?m)^\s*userId\s`).MatchString(body) {
		t.Error("toggleComputerUseEnabled still declares a userId arg; drop it so the signature cannot imply it targets an arbitrary user")
	}
}
