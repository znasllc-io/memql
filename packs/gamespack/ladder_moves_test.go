package gamespack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// ladder_moves_test.go -- the ladder's moves over an in-memory store: what moveLadder
// reads, in what order, and what it writes, without a database. The database tests
// (ladder_db_test.go, ladder_isolation_db_test.go, sweep_db_test.go) hold the same moves
// to the real engine.

// ladderNow is the clock of the tests below.
var ladderNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// ladderStore is a rowStore that keeps skills in memory and answers the reads a ladder
// move makes from them. recordLadder does what the mutation does: it writes the fields
// it was given and leaves the others alone, stamps createdAt with versionAt, and stamps
// ownerUserId from the ACTOR -- which is exactly what a move by the wrong caller would
// damage. It is also the pack's gate (Provider.lock). Everything that happens is listed
// in events, in order, because the order is the point: the gate, the fresh read, the
// policy, the write, the release.
type ladderStore struct {
	skills map[string]map[string]any // bare id -> the row as gamesSkillById reads it
	listed []map[string]any          // what gamesSkillsForSignature answers
	pages  [][]map[string]any        // what gamesSkillsToSweep answers, page by page
	policy []map[string]any          // what ladderPolicyCurrent answers

	events  []string
	writes  []map[string]any // the arguments of each recordLadder, in order
	fresh   map[string]bool  // whether the last read of each query was marked fresh
	locks   map[string]int   // how many times each gate was taken
	lockErr error            // the gate cannot be taken
	byIDErr error            // gamesSkillById fails
	policyE error            // ladderPolicyCurrent fails
	listErr error            // gamesSkillsForSignature fails
	walkErr error            // gamesSkillsToSweep fails
	writeE  error            // recordLadder fails
	// gone makes a skill disappear after the n-th time it has been listed or read: the
	// forget that deletes it between a caller's list and its move.
	vanish map[string]bool
}

func newLadderStore(skills ...map[string]any) *ladderStore {
	s := &ladderStore{skills: map[string]map[string]any{}, fresh: map[string]bool{}, locks: map[string]int{}, vanish: map[string]bool{}}
	for _, row := range skills {
		s.skills[bare(trimmed(row["id"]))] = row
	}
	return s
}

func (s *ladderStore) provider() *Provider {
	return &Provider{store: s, lock: s.lock, now: func() time.Time { return ladderNow }}
}

func (s *ladderStore) lock(_ context.Context, key string) (func(), error) {
	if s.lockErr != nil {
		return nil, s.lockErr
	}
	s.locks[key]++
	s.events = append(s.events, "lock "+key)
	released := false
	return func() { // as the real one: safe to call more than once
		if !released {
			released = true
			s.events = append(s.events, "release "+key)
		}
	}, nil
}

func (s *ladderStore) query(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	s.fresh[name] = memql.FreshReadFromContext(ctx)
	switch name {
	case "gamesSkillById":
		id := trimmed(args["skillId"])
		s.events = append(s.events, fmt.Sprintf("read %s %s fresh=%v", name, id, s.fresh[name]))
		if s.byIDErr != nil {
			return nil, s.byIDErr
		}
		row, ok := s.skills[id]
		if !ok || s.vanish[id] {
			return nil, nil
		}
		return []map[string]any{cloneRow(row)}, nil
	case "ladderPolicyCurrent":
		s.events = append(s.events, "read "+name)
		return s.policy, s.policyE
	case "gamesSkillsForSignature":
		s.events = append(s.events, "read "+name)
		if s.listErr != nil {
			return nil, s.listErr
		}
		out := make([]map[string]any, len(s.listed))
		for i, r := range s.listed {
			out[i] = cloneRow(r)
		}
		return out, nil
	}
	return nil, fmt.Errorf("ladderStore: no answer for the query %s", name)
}

func (s *ladderStore) walk(ctx context.Context, name string, _ map[string]any, visit func([]map[string]any) error) error {
	if name != "gamesSkillsToSweep" {
		return fmt.Errorf("ladderStore: no pages for the query %s", name)
	}
	if s.walkErr != nil {
		return s.walkErr
	}
	for i, page := range s.pages {
		s.events = append(s.events, fmt.Sprintf("page %d", i+1))
		rows := make([]map[string]any, len(page))
		for j, r := range page {
			rows[j] = cloneRow(r)
		}
		if err := visit(rows); err != nil {
			return err
		}
	}
	return nil
}

// ladderAccepted are the fields recordGamesSkillLadder's update accepts.
var ladderAccepted = []string{"rung", "state", "shadowMatches", "canaryMatches", "distinctBindings", "failures", "promotionApprovalId", "promotionProposedAt", "lastReplayAt", "lastBinding", "lastReason", "rungChangedAt"}

func (s *ladderStore) recordLadder(ctx context.Context, args map[string]any) error {
	id := trimmed(args["skillId"])
	s.events = append(s.events, "write "+id)
	s.writes = append(s.writes, cloneRow(args))
	if s.writeE != nil {
		return s.writeE
	}
	row, ok := s.skills[id]
	if !ok {
		return errors.New("update: there is no such row")
	}
	for _, k := range ladderAccepted {
		if v, present := args[k]; present {
			row[k] = cloneValue2(v)
		}
	}
	row["createdAt"] = args["versionAt"]
	row["ownerUserId"] = ownerKey(actorUserID(ctx)) // the stamp
	return nil
}

func (s *ladderStore) write(context.Context, string, map[string]any) error { return nil }
func (s *ladderStore) embedStore(context.Context, string, string, string) error {
	return nil
}
func (s *ladderStore) createTrace(context.Context, map[string]any) error { return nil }
func (s *ladderStore) createSkill(context.Context, map[string]any) error { return nil }

// cloneValue2 is what the engine does to a value it stores and reads back: it passes
// through JSON, so a number comes back a float64 and a list a []any.
func cloneValue2(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}

func cloneRow(row map[string]any) map[string]any {
	out, _ := cloneValue2(row).(map[string]any)
	return out
}

// untouched is true when nothing at all happened: no gate, no read, no write.
func (s *ladderStore) untouched() bool { return len(s.events) == 0 && len(s.writes) == 0 }

// skillFull is a skill as gamesSkillById reads it back: the torch skill (the holes
// 1.count and 2.count), in shadow with nothing counted, learned an hour before ladderNow.
func skillFull(owner, id string, mutate func(row map[string]any)) map[string]any {
	tmpl, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(6)})
	if err != nil {
		panic(err)
	}
	tm, err := tmpl.AsMap()
	if err != nil {
		panic(err)
	}
	row := map[string]any{
		"id": canonical(conceptSkill, id), "ownerUserId": ownerKey(owner),
		"goalText": "Light up the area!", "goalSignature": GoalSignature("Light up the area!", []string{"spot"}),
		"actions": stringsAsAny(torchActions), "template": tm, "templateDigest": tmpl.Digest(),
		"rung": "shadow", "state": "shadow", "shadowMatches": float64(0), "canaryMatches": float64(0),
		"distinctBindings": map[string]any{}, "failures": float64(0), "promotionApprovalId": "",
		"learnedAt": rfc3339(ladderNow.Add(-time.Hour)), "createdAt": rfc3339(ladderNow.Add(-time.Hour)),
	}
	if mutate != nil {
		mutate(row)
	}
	return cloneRow(row)
}

// inRung is a mutate for skillFull: the skill is on a rung, with that state.
func inRung(rung string, extra map[string]any) func(map[string]any) {
	return func(row map[string]any) {
		row["rung"], row["state"] = rung, rung
		for k, v := range extra {
			row[k] = v
		}
	}
}

// digestOf is the template digest of skillFull's skill.
func digestOf(row map[string]any) string { return trimmed(row["templateDigest"]) }

const ladderOwner = "user-games-ladder-unit"

// ---- the critical section ----

// A move is one critical section, in this order: the gate on the BARE id (a bare and a
// canonical spelling are one skill), the row read FRESH by that bare id, the policy, the
// write, and only then the gate let go. A write made from a row read before somebody
// else's write puts their move back; the gate and the fresh read are what keep it from
// happening, and the order is what makes them mean anything.
func TestMoveLadderIsOneCriticalSection(t *testing.T) {
	for name, sent := range map[string]string{"bare": "gs-1", "canonical": canonical(conceptSkill, "gs-1")} {
		t.Run(name, func(t *testing.T) {
			st := newLadderStore(skillFull(ladderOwner, "gs-1", inRung("canary", nil)))
			p := st.provider()
			if _, _, err := p.moveLadder(asOwner(ladderOwner), sent, func(map[string]any, work.LadderState) (work.LadderEvent, map[string]any, bool, error) {
				st.events = append(st.events, "move built")
				return work.LadderEvent{Kind: work.EventReplayed, At: ladderNow, Match: true}, nil, false, nil
			}); err != nil {
				t.Fatal(err)
			}
			want := []string{
				"lock games-skill:gs-1", "read gamesSkillById gs-1 fresh=true", "move built",
				"read ladderPolicyCurrent", "write gs-1", "release games-skill:gs-1",
			}
			if !sameStrings(st.events, want) {
				t.Fatalf("events = %v\nwant     %v", st.events, want)
			}
		})
	}
}

// The version of a move is stamped strictly after the version it read (memql#5431): a
// replica whose clock lags would otherwise file its write BEHIND the row it read, and
// the newest version, which is the one that counts, would be the older state.
func TestMoveLadderStampsItsVersionAfterTheOneItRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		read    time.Time // the createdAt of the row it reads; zero: unreadable
		unread  string
		wantAt  string
		explain string
	}{
		{"a row older than the clock", ladderNow.Add(-time.Hour), "", rfc3339(ladderNow), "the clock"},
		{"a row from the future, a clock that lags", ladderNow.Add(time.Hour), "", rfc3339(ladderNow.Add(time.Hour).Add(time.Microsecond)), "a microsecond after the row"},
		{"a row written this very instant", ladderNow, "", rfc3339(ladderNow.Add(time.Microsecond)), "a microsecond after the row"},
		{"a row at nanosecond precision, ahead", ladderNow.Add(time.Second + 700*time.Nanosecond), "", rfc3339(ladderNow.Add(time.Second + time.Microsecond)), "a microsecond after the row, truncated"},
		{"a row with no readable time", time.Time{}, "not a time", rfc3339(ladderNow), "the clock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := skillFull(ladderOwner, "gs-1", inRung("canary", nil))
			if tc.read.IsZero() {
				row["createdAt"] = tc.unread
			} else {
				row["createdAt"] = rfc3339(tc.read)
			}
			st := newLadderStore(row)
			if _, err := st.provider().reportSkillOutcome(asOwner(ladderOwner), map[string]any{"skillId": "gs-1", "ok": true}, 0); err != nil {
				t.Fatal(err)
			}
			if len(st.writes) != 1 || st.writes[0]["versionAt"] != tc.wantAt {
				t.Fatalf("versionAt = %v, want %s (%s)", st.writes, tc.wantAt, tc.explain)
			}
		})
	}
}

// ---- what a move refuses and what it leaves alone ----

// A move that cannot happen writes nothing, and lets its gate go. The skill that is
// not there, the skill that is another owner's, a read or a write that fails and a
// gate that is busy are each told apart by what they say.
func TestMoveLadderWritesNothingItCannotStandBehind(t *testing.T) {
	built := func(*[]string) ladderMove {
		return func(map[string]any, work.LadderState) (work.LadderEvent, map[string]any, bool, error) {
			return work.LadderEvent{Kind: work.EventReplayed, At: ladderNow, Match: true}, nil, false, nil
		}
	}
	mine := skillFull(ladderOwner, "gs-1", inRung("canary", nil))

	t.Run("no actor", func(t *testing.T) {
		st := newLadderStore(mine)
		_, _, err := st.provider().moveLadder(context.Background(), "gs-1", built(nil))
		if err == nil || !strings.HasPrefix(err.Error(), "games_unauthenticated: ") || !st.untouched() {
			t.Fatalf("a move for nobody = %v (events %v), want games_unauthenticated and nothing touched", err, st.events)
		}
	})
	t.Run("no such skill", func(t *testing.T) {
		st := newLadderStore()
		_, row, err := st.provider().moveLadder(asOwner(ladderOwner), "gs-nope", built(nil))
		var nf skillNotFound
		if !errors.As(err, &nf) || !strings.HasPrefix(err.Error(), "games_skill_not_found: ") || row != nil || len(st.writes) != 0 {
			t.Fatalf("a skill that is not there = %v (row %v, wrote %d), want games_skill_not_found", err, row, len(st.writes))
		}
		if last := st.events[len(st.events)-1]; last != "release games-skill:gs-nope" {
			t.Fatalf("events = %v, want the gate let go on the way out", st.events)
		}
	})

	// Another owner's skill is read by the cluster owner (the owned tier's read arm), and
	// by nobody else. The store answers it, as the engine does for that caller, and the
	// move must not: the mutation would stamp the caller as its owner.
	for name, holder := range map[string]string{
		"another owner's skill, canonical": ownerKey("user-games-other"),
		"another owner's skill, bare":      "user-games-other",
		"a skill that names no owner":      "",
	} {
		t.Run(name, func(t *testing.T) {
			foreign := skillFull(holder, "gs-9", inRung("canary", nil))
			foreign["ownerUserId"] = holder
			st := newLadderStore(foreign)
			moved := false
			_, row, err := st.provider().moveLadder(clusterOwnerOf(ladderOwner), "gs-9", func(map[string]any, work.LadderState) (work.LadderEvent, map[string]any, bool, error) {
				moved = true
				return work.LadderEvent{Kind: work.EventReplayed, At: ladderNow, Match: true}, nil, false, nil
			})
			var nf skillNotFound
			if !errors.As(err, &nf) || row != nil || moved || len(st.writes) != 0 {
				t.Fatalf("a skill that is not the caller's = %v (row %v, move built %v, wrote %d), want games_skill_not_found and nothing done", err, row, moved, len(st.writes))
			}
			if strings.Contains(err.Error(), "other") {
				t.Fatalf("the refusal says who holds the skill: %v", err)
			}
			if got := st.skills["gs-9"]["ownerUserId"]; got != holder {
				t.Fatalf("the skill's owner became %v", got)
			}
			if last := st.events[len(st.events)-1]; last != "release games-skill:gs-9" {
				t.Fatalf("events = %v, want the gate let go", st.events)
			}
		})
	}
	t.Run("a read that fails", func(t *testing.T) {
		st := newLadderStore(mine)
		st.byIDErr = errors.New("the database is away")
		if _, _, err := st.provider().moveLadder(asOwner(ladderOwner), "gs-1", built(nil)); err == nil || !strings.Contains(err.Error(), "the database is away") || len(st.writes) != 0 {
			t.Fatalf("a read that failed = %v (wrote %d)", err, len(st.writes))
		}
	})
	t.Run("a write that fails", func(t *testing.T) {
		st := newLadderStore(mine)
		st.writeE = errors.New("the mutation was refused")
		_, _, err := st.provider().moveLadder(asOwner(ladderOwner), "gs-1", built(nil))
		if err == nil || !strings.Contains(err.Error(), "the mutation was refused") {
			t.Fatalf("a write that failed = %v", err)
		}
		if last := st.events[len(st.events)-1]; last != "release games-skill:gs-1" {
			t.Fatalf("events = %v, want the gate let go", st.events)
		}
	})
	t.Run("a gate that is busy keeps its type", func(t *testing.T) {
		st := newLadderStore(mine)
		st.lockErr = fmt.Errorf("%w: another replica holds it", memql.ErrWriteGateBusy)
		_, _, err := st.provider().moveLadder(asOwner(ladderOwner), "gs-1", built(nil))
		if !errors.Is(err, memql.ErrWriteGateBusy) || !memql.IsWriteGateBusy(err) || !strings.Contains(err.Error(), "nothing moved") || !st.untouched() {
			t.Fatalf("a busy gate = %v (events %v), want the busy answer saying nothing moved, and nothing done", err, st.events)
		}
	})
	t.Run("a move that refuses", func(t *testing.T) {
		st := newLadderStore(mine)
		refusal := errors.New("games_skill_not_serving: no")
		_, row, err := st.provider().moveLadder(asOwner(ladderOwner), "gs-1", func(map[string]any, work.LadderState) (work.LadderEvent, map[string]any, bool, error) {
			return work.LadderEvent{}, nil, false, refusal
		})
		if err != refusal || row == nil || len(st.writes) != 0 || st.fresh["ladderPolicyCurrent"] {
			t.Fatalf("a refusal = %v (row %v, wrote %d), want it returned as it is, with the row, and nothing written", err, row, len(st.writes))
		}
	})
	t.Run("a move that skips", func(t *testing.T) {
		st := newLadderStore(mine)
		tr, row, err := st.provider().moveLadder(asOwner(ladderOwner), "gs-1", func(map[string]any, work.LadderState) (work.LadderEvent, map[string]any, bool, error) {
			return work.LadderEvent{}, nil, true, nil
		})
		if err != nil || row == nil || !reflect.DeepEqual(tr, work.Transition{}) || len(st.writes) != 0 {
			t.Fatalf("a skip = %+v, %v, %v (wrote %d), want a zero transition and nothing written", tr, row, err, len(st.writes))
		}
		for _, e := range st.events {
			if e == "read ladderPolicyCurrent" {
				t.Fatalf("a skip read the policy: %v", st.events)
			}
		}
	})
}

// ---- the policy ----

// The values the ladder decides under are a row (D15): an operator's edit reaches the
// next move. An absent or unreadable row is the design record's numbers, and a zero in
// the row means "not configured" and never "promote on nothing".
func TestPolicyReadsTheLadderPolicyRow(t *testing.T) {
	d := work.DefaultLadderPolicy()
	for _, tc := range []struct {
		name string
		rows []map[string]any
		err  error
		want work.LadderPolicy
	}{
		{"no row", nil, nil, d},
		{"an unreadable row", []map[string]any{{"shadowMatches": float64(2)}}, errors.New("not allowed"), d},
		{"every value", []map[string]any{{
			"shadowMatches": float64(2), "distinctBindings": float64(3), "canaryMatches": float64(4),
			"failuresToDemote": float64(5), "insufficientToDemote": float64(6), "retireAfterDays": float64(7),
		}}, nil, work.LadderPolicy{ShadowMatches: 2, DistinctBindings: 3, CanaryMatches: 4, FailuresToDemote: 5, InsufficientToDemote: 6, RetireAfterDays: 7}},
		{"zeros are not configured", []map[string]any{{"shadowMatches": float64(0), "canaryMatches": float64(3), "failuresToDemote": float64(-1)}}, nil,
			work.LadderPolicy{ShadowMatches: d.ShadowMatches, DistinctBindings: d.DistinctBindings, CanaryMatches: 3, FailuresToDemote: d.FailuresToDemote, InsufficientToDemote: d.InsufficientToDemote, RetireAfterDays: d.RetireAfterDays}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newLadderStore()
			st.policy, st.policyE = tc.rows, tc.err
			if got := st.provider().policy(asOwner(ladderOwner)); got != tc.want {
				t.Fatalf("policy = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// builtinAnswer runs one of the ladder's builtins and decodes its one node, failing the
// test on a refusal.
func builtinAnswer(t *testing.T, call func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error), ctx context.Context, args map[string]any) map[string]any {
	t.Helper()
	nodes, err := call(ctx, args, 0)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return answerOf(t, nodes)
}

// A move is decided under the policy as it stands: three shadow matches propose when
// the row says three, where the default is five.
func TestAMoveIsDecidedUnderTheCurrentPolicy(t *testing.T) {
	st := newLadderStore(skillFull(ladderOwner, "gs-1", nil))
	st.policy = []map[string]any{{"shadowMatches": float64(3), "distinctBindings": float64(1)}}
	st.listed = []map[string]any{st.skills["gs-1"]}
	p := st.provider()
	args := map[string]any{"goalSignature": st.skills["gs-1"]["goalSignature"], "plan": planArg(torchPlan(4))}
	var proposed []any
	for i := 0; i < 3; i++ {
		got := builtinAnswer(t, p.shadowCompare, asOwner(ladderOwner), args)
		proposed, _ = got["proposed"].([]any)
		if i < 2 && len(proposed) != 0 {
			t.Fatalf("match %d proposed under a policy of three: %v", i+1, got)
		}
	}
	if len(proposed) != 1 || proposed[0] != "gs-1" {
		t.Fatalf("the third match under a policy of three proposed %v, want gs-1", proposed)
	}
}

// One move is one instant: the version, the time a rung changed and the time a promotion
// was proposed are all read off one reading of the clock, not three.
func TestMoveLadderReadsTheClockOnce(t *testing.T) {
	st := newLadderStore(skillFull(ladderOwner, "gs-1", inRung("canary", map[string]any{"canaryMatches": float64(4)})))
	p := st.provider()
	tick := ladderNow
	p.now = func() time.Time { tick = tick.Add(time.Hour); return tick }
	// the fifth clean run makes it trusted: a rung changes, in one move
	if _, err := p.reportSkillOutcome(asOwner(ladderOwner), map[string]any{"skillId": "gs-1", "ok": true}, 0); err != nil {
		t.Fatal(err)
	}
	w := st.writes[0]
	if w["rung"] != "trusted" || w["rungChangedAt"] != w["versionAt"] {
		t.Fatalf("versionAt %v and rungChangedAt %v: want one reading of the clock for both", w["versionAt"], w["rungChangedAt"])
	}
}
