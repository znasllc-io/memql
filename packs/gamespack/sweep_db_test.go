package gamespack

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// liveSkillsOf counts the skills of one owner whose newest version is not retired,
// reading around the owned tier: the test is asking what is in the table.
func (lg *liveGames) liveSkillsOf(t *testing.T, owner string) int {
	t.Helper()
	var n int
	if err := lg.db.DB.QueryRow(`SELECT count(*) FROM (
SELECT DISTINCT ON (id) payload->>'rung' AS rung FROM "MemoryNodes"
WHERE concept = $1 AND payload->>'ownerUserId' = $2 ORDER BY id, "createdAt" DESC) latest WHERE rung <> 'retired'`,
		conceptSkill, ownerKey(owner)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// gamesSkillsToSweep answers one page of 200, and a sweep that read one page would
// leave the owner's 201st live skill unexamined for as long as the first 200 stood:
// never retired, and never demoted under a policy that was tightened since. So the
// sweep reads the pages the engine's keyset cursor offers until there are none. The
// owner here has more than a page of skills, all long idle; a sweep at +31 days must
// retire every one of them, in one call.
func TestLiveASweepReachesEverySkillPastTheFirstPage(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-sweep-pages-" + n
	ctx := asOwner(owner)
	const skills = 205
	// Two hundred skills a run, in a database that keeps what tests write, is a lot of rows
	// for every other test that reads the skills of everybody (a cluster owner's); these are
	// taken away when the test is done.
	t.Cleanup(func() {
		if _, err := lg.db.DB.Exec(`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'ownerUserId' = $2`, conceptSkill, ownerKey(owner)); err != nil {
			t.Logf("could not take away the test's skills: %v", err)
		}
	})
	for i := 0; i < skills; i++ {
		lg.seedTorchSkill(t, owner, fmt.Sprintf("gs-page-%03d-%s", i, n))
	}
	if got := lg.liveSkillsOf(t, owner); got != skills {
		t.Fatalf("the setup: %d live skills, want %d", got, skills)
	}

	lg.provider.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	defer func() { lg.provider.now = time.Now }()
	swept := lg.must(t, ctx, "builtin", "gamesSweepSkills", nil)[0]
	if asInt(swept["examined"], -1) != skills || asInt(swept["retired"], -1) != skills || asInt(swept["demoted"], -1) != 0 {
		t.Fatalf("a sweep of %d skills idle for 31 days answered %v, want all of them examined and retired", skills, swept)
	}
	if got := lg.liveSkillsOf(t, owner); got != 0 {
		t.Fatalf("%d of the owner's skills are still live after a sweep that retired them all", got)
	}
}

// A sweep moves the skills it examines in the order the query hands them over: the one
// written to longest ago first. That is only roughly the least recently used (a use writes
// a version, and so does a decision, a demotion or learning), and it decides nothing about
// which skills are examined, since every page is read; it decides which a sweep that is cut
// short has done. Here the first skill of the three was written to last (a move of its
// ladder), so the order is the other two, and then it.
func TestLiveASweepMovesTheLeastRecentlyWrittenSkillFirst(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-sweep-order-" + n
	ctx := asOwner(owner)
	first := lg.plantSkill(t, owner, skillSpec{id: "gs-order-1-" + n, bindings: []map[string]any{{"0.who": "x"}}}) // the newest write
	time.Sleep(10 * time.Millisecond)
	second := lg.plantSkill(t, owner, skillSpec{id: "gs-order-2-" + n})
	time.Sleep(10 * time.Millisecond)
	third := lg.plantSkill(t, owner, skillSpec{id: "gs-order-3-" + n})

	var (
		mu    sync.Mutex
		order []string
	)
	lg.provider.lock = func(ctx context.Context, key string) (func(), error) {
		mu.Lock()
		order = append(order, strings.TrimPrefix(key, "games-skill:"))
		mu.Unlock()
		return memql.AcquireWriteGate(ctx, lg.provider.gates(), key)
	}
	lg.provider.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	defer func() { lg.provider.lock, lg.provider.now = nil, time.Now }()
	swept := lg.must(t, ctx, "builtin", "gamesSweepSkills", nil)[0]
	if asInt(swept["examined"], -1) != 3 || asInt(swept["retired"], -1) != 3 {
		t.Fatalf("the sweep answered %v, want all three examined and retired", swept)
	}
	if want := []string{second, third, first}; !sameStrings(order, want) {
		t.Fatalf("the sweep moved %v, want %v: the least recently written first", order, want)
	}
}
