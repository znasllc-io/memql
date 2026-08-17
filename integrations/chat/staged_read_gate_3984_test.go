package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// staged_read_gate_3984_test.go -- epic memql#3974, task memql#3984.
//
// The five reads in recent_chat.go are hand-rolled bun statements that feed LLM
// context, so nothing memql#3983 injects at the engine's seams reaches them and
// the check in each function IS the enforcement.
//
// Built with a NIL bun callback on purpose: if the gate runs, each function
// returns before it dereferences the handle; if the gate were removed, the same
// call would panic on `c.bun().NewSelect()`. So this test cannot pass against an
// ungated function.

func nilBun() *bun.DB { return nil }

// admitAll is the row gate wired open, so these tests observe the STAGED
// check alone. The per-row authorization gate beside it (memql#4029) has its
// own suite in rowauthz_repack_gate_4029_test.go.
func admitAll(context.Context, memorynodes.MemoryNode) bool { return true }

// neverStaged is the staged check wired open, the mirror of admitAll, so the
// memql#4029 suite next door observes the AUTHORIZATION gate alone.
func neverStaged(string) bool { return false }

// TestRecentChatWithholdsStagedRows: every read answers empty when its concept
// is staged, and does so without touching the database.
func TestRecentChatWithholdsStagedRows(t *testing.T) {
	ctx := context.Background()
	c := NewIntegration(nilBun, func(string) bool { return true }, admitAll)

	if nodes, err := c.readRecent(ctx, "space-1", 10); err != nil || len(nodes) != 0 {
		t.Errorf("readRecent = (%d, %v), want (0, nil)", len(nodes), err)
	}
	if nodes, err := c.readByKeyword(ctx, "space-1", "secret"); err != nil || len(nodes) != 0 {
		t.Errorf("readByKeyword = (%d, %v), want (0, nil) -- this one is an unbounded ILIKE over "+
			"conversation text with no owner predicate of its own", len(nodes), err)
	}
	if nodes, err := c.readByTime(ctx, "space-1", "", ""); err != nil || len(nodes) != 0 {
		t.Errorf("readByTime = (%d, %v), want (0, nil)", len(nodes), err)
	}
	if nodes, err := c.getSpaceContext(ctx, "space-1"); err != nil || len(nodes) != 0 {
		t.Errorf("getSpaceContext = (%d, %v), want (0, nil)", len(nodes), err)
	}
	if nodes, err := c.listParticipants(ctx, "space-1"); err != nil || len(nodes) != 0 {
		t.Errorf("listParticipants = (%d, %v), want (0, nil)", len(nodes), err)
	}
}

// TestRecentChatRefusesWhenTheStagedPredicateIsMissing: an unwired predicate is
// a boot misconfiguration and must be refused rather than read as "nothing is
// staged" -- the default that looks like it works and publishes.
func TestRecentChatRefusesWhenTheStagedPredicateIsMissing(t *testing.T) {
	c := NewIntegration(nilBun, nil, admitAll)
	if _, err := c.readRecent(context.Background(), "space-1", 10); err == nil ||
		!strings.Contains(err.Error(), "not wired") {
		t.Errorf("readRecent with no predicate = %v, want a refusal naming the unwired predicate", err)
	}
}

// TestRecentChatProceedsWhenNothingIsStaged is the control: a gate that
// withheld unconditionally would pass the test above, so this one asserts the
// read gets PAST the gate when nothing is staged.
//
// With a nil handle "past the gate" means a panic on c.bun(), which is exactly
// the observable that distinguishes the two -- so it is recovered here rather
// than avoided.
func TestRecentChatProceedsWhenNothingIsStaged(t *testing.T) {
	c := NewIntegration(nilBun, neverStaged, admitAll)
	reached := false
	func() {
		defer func() {
			if recover() != nil {
				reached = true
			}
		}()
		_, _ = c.readRecent(context.Background(), "space-1", 10)
	}()
	if !reached {
		t.Error("readRecent returned without reaching the database when nothing is staged -- a gate " +
			"that withholds unconditionally is not a gate, it is an outage")
	}
}
