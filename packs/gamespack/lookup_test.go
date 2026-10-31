package gamespack

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

func TestBestServablePrefersTrustedThenRecentUse(t *testing.T) {
	rows := []map[string]any{
		{"id": "a", "rung": "shadow"},
		{"id": "b", "rung": "canary", "lastReplayAt": "2026-10-18T12:00:00Z"},
		{"id": "c", "rung": "trusted", "lastReplayAt": "2026-10-01T12:00:00Z"},
		{"id": "d", "rung": "trusted", "lastReplayAt": "2026-10-17T12:00:00Z"},
	}
	if best := bestServable(rows); best == nil || best["id"] != "d" {
		t.Fatalf("best = %v, want d", best)
	}
	if best := bestServable(rows[:1]); best != nil {
		t.Fatalf("a shadow skill never serves, got %v", best)
	}
}

// The builtin takes a free-form object (@args(profile="object")), so nothing in the engine
// holds its declared fields to the names the Go half reads: a field renamed on one side
// would be an argument that is sent and silently ignored. The declaration is held here to
// the names every test of the lookup sends through the engine, and to the capability's own
// advisory schema.
func TestFindSkillIsDeclaredWithTheArgumentsItReads(t *testing.T) {
	want := map[string]string{"goalSignature": "string", "trigger": "string", "text": "string", "minSimilarity": "number"}
	raw, err := fs.ReadFile(Tree(), "builtins.memql")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)\nbuiltin gamesFindSkill \{\n(.*?)\n\}`).FindStringSubmatch(string(raw))
	if block == nil {
		t.Fatal("builtins.memql declares no builtin gamesFindSkill")
	}
	declared := map[string]string{}
	for _, line := range strings.Split(block[1], "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			declared[f[0]] = f[1]
		}
	}
	if !reflect.DeepEqual(declared, want) {
		t.Errorf("gamesFindSkill declares %v, want %v: every field optional, and the names the Go half reads", declared, want)
	}
	var advisory map[string]string
	for _, c := range (&Provider{}).Capabilities() {
		if c.Name == "findSkill" {
			advisory = c.ArgsSchema
		}
	}
	if !reflect.DeepEqual(advisory, want) {
		t.Errorf("the findSkill capability's advisory schema is %v, want %v", advisory, want)
	}
}

// ---- the lookup over an in-memory store: what it reads, whose rows it serves ----
//
// The fakes below answer with EVERY owner's rows, as the owned tier's read arm does for the
// cluster's owner when a read does not name the caller's owner. The pack's queries that page
// do name it (queries.memql), so the engine's own lists are the caller's; gamesSkillById does
// not, and answers a skill whoever's it is. The lookup holds every row it reads to the caller
// all the same (ownedBy, both sides canonical) as its own check, on top of the query's, and
// these tests hold it to that: a lookup that did not would serve somebody else's skill the day
// a read stopped naming the owner.

// lookupSig is a goal signature as GoalSignature computes one.
var lookupSig = GoalSignature("Fill the chest!", []string{"spot"})

// lookupTable is a vector table's name, which isVectorTable accepts.
const lookupTable = "node_vectors_16_0123456789abcdef"

// lookupRow is a skill row as the reads answer it.
func lookupRow(id, owner, rung, lastReplayAt string, triggers ...string) map[string]any {
	row := map[string]any{
		"id": id, "ownerUserId": owner, "rung": rung, "goalSignature": lookupSig,
		"triggers": stringsAsAny(triggers), "createdAt": "2026-10-18T00:00:00Z",
	}
	if lastReplayAt != "" {
		row["lastReplayAt"] = lastReplayAt
	}
	return row
}

// lookupStore is a rowStore that answers the three reads a lookup makes from the rows it
// is given, whoever owns them, and counts what it was asked and every write (a lookup
// makes none).
type lookupStore struct {
	bySignature []map[string]any // what gamesSkillsForSignature answers
	trusted     []map[string]any // what gamesTrustedSkills answers
	// gamesSkillById answers the row the lists hold for the skill, unless the test says what
	// the skill is now: byID gives its row (a demotion that landed since the lists were read, a
	// change of owner) and gone says it is not there.
	byID       map[string]map[string]any
	gone       map[string]bool
	byIDErr    error // gamesSkillById fails
	asked      []string
	signatures []string // the goalSignature of each read of a goal's skills
	byIDAsked  []string // the skill of each read of gamesSkillById, in order
	byIDFresh  []bool   // whether each of those reads was marked fresh
	failWith   error    // every read fails with it
	writes     int
}

// askedOf counts the reads of one query.
func (s *lookupStore) askedOf(name string) int {
	n := 0
	for _, q := range s.asked {
		if q == name {
			n++
		}
	}
	return n
}

func (s *lookupStore) query(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	s.asked = append(s.asked, name)
	if s.failWith != nil {
		return nil, s.failWith
	}
	switch name {
	case "gamesSkillsForSignature":
		s.signatures = append(s.signatures, asString(args["goalSignature"]))
		return cloneRows(s.bySignature), nil
	case "gamesTrustedSkills":
		return cloneRows(s.trusted), nil
	case "gamesSkillById":
		id := bare(trimmed(args["skillId"]))
		if strings.Contains(trimmed(args["skillId"]), ":") {
			return nil, fmt.Errorf("lookupStore: skillId %q is not a bare id; clients never compose canonical ids", trimmed(args["skillId"]))
		}
		s.byIDAsked = append(s.byIDAsked, id)
		s.byIDFresh = append(s.byIDFresh, memql.FreshReadFromContext(ctx))
		if s.byIDErr != nil {
			return nil, s.byIDErr
		}
		if s.gone[id] {
			return nil, nil
		}
		if row, ok := s.byID[id]; ok {
			return []map[string]any{cloneRow(row)}, nil
		}
		for _, list := range [][]map[string]any{s.bySignature, s.trusted} {
			for _, r := range list {
				if bare(trimmed(r["id"])) == id {
					return []map[string]any{cloneRow(r)}, nil
				}
			}
		}
		return nil, nil
	}
	return nil, fmt.Errorf("lookupStore: no answer for the query %s", name)
}

func (s *lookupStore) write(context.Context, string, map[string]any) error { s.writes++; return nil }
func (s *lookupStore) embedStore(context.Context, string, string, string) error {
	s.writes++
	return nil
}
func (s *lookupStore) createTrace(context.Context, map[string]any) error  { s.writes++; return nil }
func (s *lookupStore) createSkill(context.Context, map[string]any) error  { s.writes++; return nil }
func (s *lookupStore) recordLadder(context.Context, map[string]any) error { s.writes++; return nil }
func (s *lookupStore) walk(context.Context, string, map[string]any, func([]map[string]any) error) error {
	return errors.New("lookupStore: a lookup walks no pages")
}

func cloneRows(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, len(rows))
	for i, r := range rows {
		out[i] = cloneRow(r)
	}
	return out
}

// lookupSpace is the embedder space as a lookup sees it, with every way it can go wrong.
type lookupSpace struct {
	name     string        // the vector table; "" is nothing bound, or nothing stored
	tableErr error         // the space cannot be read
	embedErr error         // the model is away
	hang     chan struct{} // when set, embedQuery waits on it and on nothing else: a provider that does not watch its context
	panics   bool
	embedded atomic.Int32 // how many texts were handed to the model
}

func (s *lookupSpace) table(context.Context) (string, error) { return s.name, s.tableErr }

func (s *lookupSpace) embedQuery(context.Context, string) ([]float32, error) {
	s.embedded.Add(1)
	if s.panics {
		panic("the embedder broke")
	}
	if s.hang != nil {
		<-s.hang
	}
	if s.embedErr != nil {
		return nil, s.embedErr
	}
	return bagOfWords("fill the chest"), nil
}

func (s *lookupSpace) store(context.Context, string, string, string) error {
	return errors.New("a lookup stores no vector")
}

// refusingConnector is a database handle that connects to nothing: every attempt to use
// it is counted and refused. A provider handed one shows that a statement was never
// run, which is a count of zero.
type refusingConnector struct{ attempts *atomic.Int32 }

func (c refusingConnector) Connect(context.Context) (driver.Conn, error) {
	c.attempts.Add(1)
	return nil, errors.New("this test has no database")
}

func (c refusingConnector) Driver() driver.Driver { return refusingDriver{c} }

type refusingDriver struct{ c refusingConnector }

func (d refusingDriver) Open(string) (driver.Conn, error) {
	d.c.attempts.Add(1)
	return nil, errors.New("this test has no database")
}

// unreachableDB is the database handle func of a Provider, and the count of the times
// anything tried to use it.
func unreachableDB(t *testing.T) (db func() *sql.DB, attempts *atomic.Int32) {
	t.Helper()
	attempts = new(atomic.Int32)
	handle := sql.OpenDB(refusingConnector{attempts})
	t.Cleanup(func() { _ = handle.Close() })
	return func() *sql.DB { return handle }, attempts
}

// lookupProvider is a pack over the store, with an embedder space that has nothing in it.
func lookupProvider(store *lookupStore) *Provider {
	return &Provider{store: store, space: &lookupSpace{}, now: time.Now}
}

// answerRows decodes the rows a lookup answered.
func answerRows(t *testing.T, nodes []memorynodes.MemoryNode) []map[string]any {
	t.Helper()
	out := make([]map[string]any, len(nodes))
	for i, n := range nodes {
		if n.Concept != "integration:games:skillMatch" {
			t.Errorf("answer %d is a %q, want integration:games:skillMatch", i, n.Concept)
		}
		if err := json.Unmarshal(n.Payload, &out[i]); err != nil {
			t.Fatalf("answer %d does not decode: %v", i, err)
		}
	}
	return out
}

// lookupAs runs a lookup as ctx's caller and decodes what it answered.
func lookupAs(t *testing.T, p *Provider, ctx context.Context, args map[string]any) []map[string]any {
	t.Helper()
	nodes, err := p.findSkill(ctx, args, 0)
	if err != nil {
		t.Fatalf("findSkill(%v) = %v", args, err)
	}
	return answerRows(t, nodes)
}

// A lookup is made by one key. Two keys would be two questions, and no key none; each
// is refused before anything is read.
func TestALookupTakesExactlyOneKey(t *testing.T) {
	store := &lookupStore{}
	p := lookupProvider(store)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"no key", map[string]any{}},
		{"keys that are blank", map[string]any{"goalSignature": " ", "trigger": "", "text": "\t\n"}},
		{"keys that are null", map[string]any{"goalSignature": nil, "trigger": nil, "text": nil}},
		{"a signature and a trigger", map[string]any{"goalSignature": lookupSig, "trigger": "initiative"}},
		{"a signature and a text", map[string]any{"goalSignature": lookupSig, "text": "fill the chest"}},
		{"a trigger and a text", map[string]any{"trigger": "initiative", "text": "fill the chest"}},
		{"all three", map[string]any{"goalSignature": lookupSig, "trigger": "initiative", "text": "fill the chest"}},
	} {
		nodes, err := p.findSkill(asOwner("owner-a"), tc.args, 0)
		if err == nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: ") || !strings.Contains(err.Error(), "goalSignature, trigger and text") {
			t.Errorf("%s: err = %v, want games_invalid_argument naming goalSignature, trigger and text", tc.name, err)
		}
		if len(nodes) != 0 {
			t.Errorf("%s: a refused lookup answered %v", tc.name, nodes)
		}
	}
	if len(store.asked) != 0 || store.writes != 0 {
		t.Errorf("refused lookups read %v and wrote %d times; they must do neither", store.asked, store.writes)
	}
}

// Every skill is somebody's, so a lookup with nobody behind it is refused, with the code
// the rest of the pack uses, before anything is read.
func TestALookupNeedsASignedInCaller(t *testing.T) {
	store := &lookupStore{}
	nodes, err := lookupProvider(store).findSkill(context.Background(), map[string]any{"trigger": "initiative"}, 0)
	if err == nil || !strings.HasPrefix(err.Error(), "games_unauthenticated: ") || len(nodes) != 0 {
		t.Fatalf("a lookup with no actor = %v, %v, want games_unauthenticated", nodes, err)
	}
	if len(store.asked) != 0 {
		t.Fatalf("a refused lookup read %v", store.asked)
	}
}

// An argument that cannot mean what the call needs is refused naming it, where reading
// it as its printed form (5 as "5", a list as "[x]", an unreadable floor as the default)
// would answer "no skill" for a runtime that sent the wrong thing, and say nothing.
func TestALookupRefusesArgumentsItCannotMean(t *testing.T) {
	const floor = "minSimilarity must be a number above 0 and at most 1"
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"a number as the signature", map[string]any{"goalSignature": 5}, "goalSignature must be a string"},
		{"a list as the trigger", map[string]any{"trigger": []any{"initiative"}}, "trigger must be a string"},
		{"a boolean as the text", map[string]any{"text": true}, "text must be a string"},
		{"a number beside a trigger", map[string]any{"trigger": "initiative", "text": 7}, "text must be a string"},
		{"a signature that is too short", map[string]any{"goalSignature": "abc"}, "goalSignature must be 64 lower-case hexadecimal digits"},
		{"a signature in upper case", map[string]any{"goalSignature": strings.ToUpper(lookupSig)}, "goalSignature must be 64 lower-case hexadecimal digits"},
		{"a floor that is a word", map[string]any{"text": "fill the chest", "minSimilarity": "high"}, floor},
		{"a floor of zero", map[string]any{"text": "fill the chest", "minSimilarity": 0.0}, floor},
		{"a floor below zero", map[string]any{"text": "fill the chest", "minSimilarity": -0.5}, floor},
		{"a floor above one", map[string]any{"text": "fill the chest", "minSimilarity": 1.5}, floor},
		{"a floor that is NaN", map[string]any{"text": "fill the chest", "minSimilarity": math.NaN()}, floor},
		{"an infinite floor", map[string]any{"text": "fill the chest", "minSimilarity": math.Inf(1)}, floor},
		{"a boolean as the floor", map[string]any{"text": "fill the chest", "minSimilarity": true}, floor},
	} {
		store := &lookupStore{}
		nodes, err := lookupProvider(store).findSkill(asOwner("owner-a"), tc.args, 0)
		if err == nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: ") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want games_invalid_argument: ...%s", tc.name, err, tc.want)
		}
		if len(nodes) != 0 || len(store.asked) != 0 {
			t.Errorf("%s: a refused lookup answered %v and read %v", tc.name, nodes, store.asked)
		}
	}

	// The controls: the same calls with an argument that means something are accepted.
	for _, floorValue := range []any{0.5, 1.0, 1, int64(1), json.Number("0.9")} {
		store := &lookupStore{}
		if _, err := lookupProvider(store).findSkill(asOwner("owner-a"), map[string]any{"text": "fill the chest", "minSimilarity": floorValue}, 0); err != nil {
			t.Errorf("a floor of %v (%T) was refused: %v", floorValue, floorValue, err)
		}
	}
	if _, err := lookupProvider(&lookupStore{}).findSkill(asOwner("owner-a"), map[string]any{"goalSignature": lookupSig}, 0); err != nil {
		t.Errorf("a well-formed signature was refused: %v", err)
	}
}

// By signature: the trusted skill, else the canary one -- among the CALLER'S skills. The
// store hands every owner's skills for the goal over, as a read that does not name the
// caller's owner would; the rung of another owner's must not outrank the caller's, and
// serving it would run a skill the caller never taught, with the model not standing by.
func TestASignatureLookupServesOnlyTheCallersSkills(t *testing.T) {
	a, b, root := "owner-a", "owner-b", "owner-root"
	store := &lookupStore{bySignature: []map[string]any{
		lookupRow("gs-b-trusted", ownerKey(b), "trusted", "2026-10-18T12:00:00Z"),
		lookupRow(canonical(conceptSkill, "gs-a-canary"), ownerKey(a), "canary", "2026-10-01T12:00:00Z"),
		lookupRow("gs-a-shadow", a, "shadow", ""), // the bare spelling of the owner, as a hand-made row has it
		lookupRow("gs-nobody-trusted", "", "trusted", "2026-10-18T13:00:00Z"),
	}}
	p := lookupProvider(store)
	args := map[string]any{"goalSignature": lookupSig}

	got := lookupAs(t, p, asOwner(a), args)
	if len(got) != 1 || got[0]["id"] != "gs-a-canary" || got[0]["serve"] != "skill" || got[0]["standby"] != true {
		t.Fatalf("owner A has a canary skill and another owner's trusted one is on the page: got %v, want A's canary skill with the model standing by", got)
	}
	got = lookupAs(t, p, asOwner(b), args)
	if len(got) != 1 || got[0]["id"] != "gs-b-trusted" || got[0]["standby"] != false {
		t.Fatalf("owner B has a trusted skill: got %v", got)
	}
	if got := lookupAs(t, p, clusterOwnerOf(root), args); len(got) != 0 {
		t.Fatalf("the cluster's owner has no skill of the goal and was answered %v, want no answer", got)
	}
	if got := lookupAs(t, p, asOwner("owner-nobody"), args); len(got) != 0 {
		t.Fatalf("an owner with no skill for the goal got %v", got)
	}
	// A padded signature is the signature: the read is of the trimmed one.
	if got := lookupAs(t, p, asOwner(a), map[string]any{"goalSignature": "  " + lookupSig + " \n"}); len(got) != 1 || got[0]["id"] != "gs-a-canary" {
		t.Fatalf("a padded signature = %v, want A's canary skill", got)
	}
	// One read of the goal's skills for each lookup, and one fresh read by id for each of the
	// three that found a skill to answer: A's canary skill twice and B's trusted skill once, and
	// no skill of another owner's, which a lookup does not fetch even to discard.
	if store.askedOf("gamesSkillsForSignature") != 5 || !reflect.DeepEqual(store.byIDAsked, []string{"gs-a-canary", "gs-b-trusted", "gs-a-canary"}) {
		t.Fatalf("the lookups read %v, want five reads of the goal's skills and the reads by id of gs-a-canary, gs-b-trusted and gs-a-canary", store.asked)
	}
	for i, sig := range store.signatures {
		if sig != lookupSig {
			t.Errorf("read %d asked for the skills of %q, want %q", i, sig, lookupSig)
		}
	}
	if store.writes != 0 {
		t.Fatalf("a lookup wrote %d times", store.writes)
	}
}

// By trigger: the most recently used TRUSTED skill the CALLER has learned under it. The
// store hands every owner's trusted skills over, as a read that does not name the caller's
// owner would, and the newest of another owner's must not be the answer.
func TestATriggerLookupServesOnlyTheCallersTrustedSkills(t *testing.T) {
	a, b, root := "owner-a", "owner-b", "owner-root"
	store := &lookupStore{trusted: []map[string]any{
		lookupRow("gs-b-1", ownerKey(b), "trusted", "2026-10-18T12:00:00Z", "initiative"), // the newest of all, and B's
		lookupRow("gs-a-greeting", ownerKey(a), "trusted", "2026-10-17T12:00:00Z", "greeting"),
		lookupRow("gs-a-1", ownerKey(a), "trusted", "2026-10-01T12:00:00Z", "initiative"),
		lookupRow("gs-a-2", ownerKey(a), "trusted", "2026-09-01T12:00:00Z", "initiative", "greeting"),
	}}
	p := lookupProvider(store)
	// Whose each skill is: a lookup reads, by id, the skills of its caller and no other's.
	owns := map[string]string{"gs-b-1": b, "gs-a-greeting": a, "gs-a-1": a, "gs-a-2": a}
	idOf := func(who context.Context, owner, trigger string) any {
		before := len(store.byIDAsked)
		defer func() {
			for _, id := range store.byIDAsked[before:] {
				if owns[id] != owner {
					t.Errorf("%s's lookup under %q read the skill %s, which is %s's", owner, trigger, id, owns[id])
				}
			}
		}()
		rows := lookupAs(t, p, who, map[string]any{"trigger": trigger})
		if len(rows) == 0 {
			return nil
		}
		if len(rows) != 1 || rows[0]["serve"] != "skill" || rows[0]["standby"] != false {
			t.Errorf("a trusted skill found by trigger serves alone: %v", rows)
		}
		return rows[0]["id"]
	}

	if got := idOf(asOwner(a), a, "initiative"); got != "gs-a-1" {
		t.Errorf("A under initiative = %v, want A's most recently used, gs-a-1 (B's gs-b-1 is newer and not A's)", got)
	}
	if got := idOf(asOwner(a), a, "greeting"); got != "gs-a-greeting" {
		t.Errorf("A under greeting = %v, want gs-a-greeting", got)
	}
	if got := idOf(asOwner(b), b, "initiative"); got != "gs-b-1" {
		t.Errorf("B under initiative = %v, want gs-b-1", got)
	}
	if got := idOf(clusterOwnerOf(root), root, "initiative"); got != nil {
		t.Errorf("the cluster's owner has no skill under the trigger and was answered %v", got)
	}
	if got := idOf(asOwner(a), a, "init"); got != nil {
		t.Errorf("a trigger is matched whole: init found %v", got)
	}
	if got := idOf(asOwner(a), a, "  initiative \n"); got != "gs-a-1" {
		t.Errorf("a padded trigger = %v, want it read as initiative", got)
	}
	if got := idOf(asOwner(a), a, "Initiative"); got != nil {
		t.Errorf("a trigger is matched as it was recorded, and Initiative found %v", got)
	}
	if store.writes != 0 {
		t.Fatalf("a lookup wrote %d times", store.writes)
	}
}

// "The most recently used" is lastReplayAt, whatever order the read hands the rows in:
// the database sorts a row with no lastReplayAt FIRST when it sorts descending, and a
// lookup that took the first match would answer a skill that was never used before one
// used this morning.
func TestATriggerLookupChoosesTheMostRecentlyUsedWhateverOrderTheRowsComeIn(t *testing.T) {
	a := "owner-a"
	rows := []map[string]any{
		lookupRow("gs-never-used", ownerKey(a), "trusted", "", "initiative"),
		lookupRow("gs-old", ownerKey(a), "trusted", "2026-10-01T12:00:00Z", "initiative"),
		lookupRow("gs-new", ownerKey(a), "trusted", "2026-10-17T12:00:00Z", "initiative"),
		lookupRow("gs-mid", ownerKey(a), "trusted", "2026-10-10T12:00:00Z", "initiative"),
	}
	for shift := range rows {
		rotated := append(append([]map[string]any{}, rows[shift:]...), rows[:shift]...)
		got := lookupAs(t, lookupProvider(&lookupStore{trusted: rotated}), asOwner(a), map[string]any{"trigger": "initiative"})
		if len(got) != 1 || got[0]["id"] != "gs-new" {
			t.Errorf("rows in the order %v..., the answer was %v, want gs-new", rotated[0]["id"], got)
		}
	}
}

// Only a trusted skill runs with no model asked. The read is of trusted skills, but the
// rule is the lookup's: a skill of any lower rung the read hands over (a canary skill
// needs a model standing by, and a trigger lookup has none) is not served.
func TestATriggerLookupNeverServesBelowTrusted(t *testing.T) {
	a := "owner-a"
	store := &lookupStore{trusted: []map[string]any{
		lookupRow("gs-canary", ownerKey(a), "canary", "2026-10-18T12:00:00Z", "initiative"),
		lookupRow("gs-shadow", ownerKey(a), "shadow", "2026-10-18T11:00:00Z", "initiative"),
		lookupRow("gs-retired", ownerKey(a), "retired", "2026-10-18T10:00:00Z", "initiative"),
	}}
	if got := lookupAs(t, lookupProvider(store), asOwner(a), map[string]any{"trigger": "initiative"}); len(got) != 0 {
		t.Fatalf("a skill below trusted was served with no model asked: %v", got)
	}
}

// ---- a lookup answers the skill as it is NOW ----
//
// The lists a lookup picks from are the engine's cached reads. A write made through one replica
// reaches another replica's cache when the invalidation broadcast does, so the skill a list puts
// first may have been demoted a moment ago: it failed twice, it is in shadow, and answering it
// would run, with no model asked, a skill the ladder has taken back. So the best of a list is
// read again by id, past the cache, and judged again; if it is no longer what the lookup wants,
// the next best is tried; and what is answered is the version just read. gamesSkillById is not
// scoped to the caller, so the owner is held on that read too.

// By signature, a skill that is not what its list said it was is not answered; the next best is.
func TestASignatureLookupAnswersWhatTheSkillIsNow(t *testing.T) {
	a, b := "owner-a", "owner-b"
	row := func(id, owner, rung string) map[string]any {
		return lookupRow(id, ownerKey(owner), rung, "2026-10-17T12:00:00Z")
	}
	for _, tc := range []struct {
		name    string
		now     func(s *lookupStore) // what has happened to the skills since the list was read
		want    string               // the skill answered; "" for none
		rung    string               // the rung it is answered at
		standby bool
		read    []string // the skills read by id, in order
	}{
		{"nothing has changed", func(*lookupStore) {}, "gs-trusted", "trusted", false, []string{"gs-trusted"}},
		{"the trusted skill was demoted on another replica", func(s *lookupStore) {
			s.byID = map[string]map[string]any{"gs-trusted": row("gs-trusted", a, "shadow")}
		}, "gs-canary", "canary", true, []string{"gs-trusted", "gs-canary"}},
		{"the trusted skill was retired", func(s *lookupStore) {
			s.byID = map[string]map[string]any{"gs-trusted": row("gs-trusted", a, "retired")}
		}, "gs-canary", "canary", true, []string{"gs-trusted", "gs-canary"}},
		{"the trusted skill is gone", func(s *lookupStore) { s.gone = map[string]bool{"gs-trusted": true} },
			"gs-canary", "canary", true, []string{"gs-trusted", "gs-canary"}},
		{"the trusted skill is not the caller's", func(s *lookupStore) {
			s.byID = map[string]map[string]any{"gs-trusted": row("gs-trusted", b, "trusted")}
		}, "gs-canary", "canary", true, []string{"gs-trusted", "gs-canary"}},
		{"both were demoted", func(s *lookupStore) {
			s.byID = map[string]map[string]any{"gs-trusted": row("gs-trusted", a, "shadow"), "gs-canary": row("gs-canary", a, "shadow")}
		}, "", "", false, []string{"gs-trusted", "gs-canary"}},
		{"the trusted skill is a canary one now", func(s *lookupStore) {
			s.byID = map[string]map[string]any{"gs-trusted": row("gs-trusted", a, "canary")}
		}, "gs-trusted", "canary", true, []string{"gs-trusted"}},
	} {
		store := &lookupStore{bySignature: []map[string]any{row("gs-trusted", a, "trusted"), row("gs-canary", a, "canary")}}
		tc.now(store)
		got := lookupAs(t, lookupProvider(store), asOwner(a), map[string]any{"goalSignature": lookupSig})
		switch {
		case tc.want == "" && len(got) != 0:
			t.Errorf("%s: answered %v, want no skill", tc.name, got)
		case tc.want != "" && (len(got) != 1 || got[0]["id"] != tc.want || got[0]["rung"] != tc.rung || got[0]["standby"] != tc.standby):
			t.Errorf("%s: answered %v, want %s at %s with standby %v", tc.name, got, tc.want, tc.rung, tc.standby)
		}
		if !reflect.DeepEqual(store.byIDAsked, tc.read) {
			t.Errorf("%s: read %v by id, want %v", tc.name, store.byIDAsked, tc.read)
		}
		for i, fresh := range store.byIDFresh {
			if !fresh {
				t.Errorf("%s: read %d by id was served from the result cache; it must be fresh", tc.name, i+1)
			}
		}
		if store.askedOf("gamesSkillsForSignature") != 1 {
			t.Errorf("%s: the goal's skills were read %d times, want once", tc.name, store.askedOf("gamesSkillsForSignature"))
		}
	}

	// What is answered is the version just read, not the one the list held.
	store := &lookupStore{
		bySignature: []map[string]any{row("gs-trusted", a, "trusted")},
		byID:        map[string]map[string]any{"gs-trusted": {"id": "gs-trusted", "ownerUserId": ownerKey(a), "rung": "trusted", "lastReason": "from the database", "lastReplayAt": "2026-10-18T09:00:00Z"}},
	}
	if got := lookupAs(t, lookupProvider(store), asOwner(a), map[string]any{"goalSignature": lookupSig}); len(got) != 1 || got[0]["lastReason"] != "from the database" || got[0]["lastReplayAt"] != "2026-10-18T09:00:00Z" {
		t.Errorf("the answer = %v, want the version read by id (lastReason from the database)", got)
	}
	// A read that fails fails the lookup: it is not an answer of none.
	store = &lookupStore{bySignature: []map[string]any{row("gs-trusted", a, "trusted")}, byIDErr: errors.New("the database is away")}
	if got, err := lookupProvider(store).findSkill(asOwner(a), map[string]any{"goalSignature": lookupSig}, 0); err == nil || !strings.Contains(err.Error(), "the database is away") || len(got) != 0 {
		t.Errorf("with the read by id failing = %v, %v, want the failure", got, err)
	}
}

// By trigger, the same: only a skill that is still trusted, still the caller's and still under
// the trigger is answered, and a canary skill never is.
func TestATriggerLookupAnswersWhatTheSkillIsNow(t *testing.T) {
	a, b := "owner-a", "owner-b"
	row := func(id, owner, rung, lastReplayAt string, triggers ...string) map[string]any {
		return lookupRow(id, ownerKey(owner), rung, lastReplayAt, triggers...)
	}
	const newest, older = "2026-10-17T12:00:00Z", "2026-10-01T12:00:00Z"
	for _, tc := range []struct {
		name string
		now  func(s *lookupStore)
		want string
		read []string
	}{
		{"nothing has changed", func(*lookupStore) {}, "gs-new", []string{"gs-new"}},
		{"the newest was demoted on another replica", func(s *lookupStore) {
			s.byID = map[string]map[string]any{"gs-new": row("gs-new", a, "shadow", newest, "initiative")}
		}, "gs-old", []string{"gs-new", "gs-old"}},
		{"the newest is a canary skill now", func(s *lookupStore) {
			s.byID = map[string]map[string]any{"gs-new": row("gs-new", a, "canary", newest, "initiative")}
		}, "gs-old", []string{"gs-new", "gs-old"}},
		{"the newest is gone", func(s *lookupStore) { s.gone = map[string]bool{"gs-new": true} }, "gs-old", []string{"gs-new", "gs-old"}},
		{"the newest is not the caller's", func(s *lookupStore) {
			s.byID = map[string]map[string]any{"gs-new": row("gs-new", b, "trusted", newest, "initiative")}
		}, "gs-old", []string{"gs-new", "gs-old"}},
		{"both were demoted", func(s *lookupStore) {
			s.byID = map[string]map[string]any{"gs-new": row("gs-new", a, "shadow", newest, "initiative"), "gs-old": row("gs-old", a, "retired", older, "initiative")}
		}, "", []string{"gs-new", "gs-old"}},
	} {
		store := &lookupStore{trusted: []map[string]any{
			row("gs-other", a, "trusted", "2026-10-18T12:00:00Z", "greeting"), // used most lately, under another trigger
			row("gs-new", a, "trusted", newest, "initiative"),
			row("gs-old", a, "trusted", older, "initiative"),
		}}
		tc.now(store)
		got := lookupAs(t, lookupProvider(store), asOwner(a), map[string]any{"trigger": "initiative"})
		switch {
		case tc.want == "" && len(got) != 0:
			t.Errorf("%s: answered %v, want no skill", tc.name, got)
		case tc.want != "" && (len(got) != 1 || got[0]["id"] != tc.want || got[0]["rung"] != "trusted" || got[0]["standby"] != false):
			t.Errorf("%s: answered %v, want the trusted skill %s", tc.name, got, tc.want)
		}
		if !reflect.DeepEqual(store.byIDAsked, tc.read) {
			t.Errorf("%s: read %v by id, want %v", tc.name, store.byIDAsked, tc.read)
		}
		for i, fresh := range store.byIDFresh {
			if !fresh {
				t.Errorf("%s: read %d by id was served from the result cache; it must be fresh", tc.name, i+1)
			}
		}
	}

	store := &lookupStore{
		trusted: []map[string]any{row("gs-new", a, "trusted", newest, "initiative")},
		byID:    map[string]map[string]any{"gs-new": {"id": "gs-new", "ownerUserId": ownerKey(a), "rung": "trusted", "triggers": []any{"initiative"}, "lastReason": "from the database"}},
	}
	if got := lookupAs(t, lookupProvider(store), asOwner(a), map[string]any{"trigger": "initiative"}); len(got) != 1 || got[0]["lastReason"] != "from the database" {
		t.Errorf("the answer = %v, want the version read by id (lastReason from the database)", got)
	}
	store = &lookupStore{trusted: []map[string]any{row("gs-new", a, "trusted", newest, "initiative")}, byIDErr: errors.New("the database is away")}
	if got, err := lookupProvider(store).findSkill(asOwner(a), map[string]any{"trigger": "initiative"}, 0); err == nil || !strings.Contains(err.Error(), "the database is away") || len(got) != 0 {
		t.Errorf("with the read by id failing = %v, %v, want the failure", got, err)
	}
}

// What qualifies a skill is judged on a skill that is there: the reads that find none (it is
// gone, or it is not the caller's) drop the candidate, and the judge is never handed an empty
// row to answer for.
func TestPickFreshNeverJudgesASkillThatIsNotThere(t *testing.T) {
	a, b := "owner-a", "owner-b"
	store := &lookupStore{
		gone: map[string]bool{"gs-gone": true},
		byID: map[string]map[string]any{
			"gs-theirs": lookupRow("gs-theirs", ownerKey(b), "trusted", "2026-10-16T12:00:00Z"),
			"gs-here":   lookupRow("gs-here", ownerKey(a), "canary", "2026-10-01T12:00:00Z"),
		},
	}
	candidates := []map[string]any{
		lookupRow("gs-gone", ownerKey(a), "trusted", "2026-10-18T12:00:00Z"),
		lookupRow("gs-theirs", ownerKey(a), "trusted", "2026-10-17T12:00:00Z"),
		lookupRow("gs-here", ownerKey(a), "canary", "2026-10-01T12:00:00Z"),
	}
	judged := []string{}
	got, err := lookupProvider(store).pickFresh(asOwner(a), a, candidates, func(now map[string]any) bool {
		if now == nil {
			t.Error("the judge was asked about a skill that is not there")
			return false
		}
		judged = append(judged, trimmed(now["id"]))
		return true
	})
	if err != nil || got == nil || trimmed(got["id"]) != "gs-here" {
		t.Fatalf("pickFresh = %v, %v, want gs-here: the two before it are gone and another owner's", got, err)
	}
	if !reflect.DeepEqual(judged, []string{"gs-here"}) || !reflect.DeepEqual(store.byIDAsked, []string{"gs-gone", "gs-theirs", "gs-here"}) {
		t.Fatalf("judged %v after the reads %v, want only gs-here judged, after a read of each in turn", judged, store.byIDAsked)
	}
}

// The floor belongs to a lookup by meaning, and only that lookup reads it: another lookup made
// with a floor that a lookup by meaning would refuse is made as if it had none.
func TestAFloorIsReadOnlyByALookupByMeaning(t *testing.T) {
	a := "owner-a"
	for _, floor := range []any{2.0, 0.0, -1.0, "high", true, math.NaN()} {
		store := &lookupStore{
			bySignature: []map[string]any{lookupRow("gs-a", ownerKey(a), "trusted", "2026-10-17T12:00:00Z", "initiative")},
			trusted:     []map[string]any{lookupRow("gs-a", ownerKey(a), "trusted", "2026-10-17T12:00:00Z", "initiative")},
		}
		p := lookupProvider(store)
		for _, key := range []map[string]any{{"goalSignature": lookupSig}, {"trigger": "initiative"}} {
			args := map[string]any{"minSimilarity": floor}
			for k, v := range key {
				args[k] = v
			}
			if got, err := p.findSkill(asOwner(a), args, 0); err != nil || len(got) != 1 {
				t.Errorf("%v with a floor of %v (%T) = %v, %v, want the skill: only a lookup by meaning reads the floor", key, floor, floor, got, err)
			}
		}
		_, err := p.findSkill(asOwner(a), map[string]any{"text": "fill the chest", "minSimilarity": floor}, 0)
		if err == nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: minSimilarity must be a number above 0 and at most 1") {
			t.Errorf("a lookup by meaning with a floor of %v (%T) = %v, want it refused", floor, floor, err)
		}
	}
}

// Skills of one goal that are all trusted embed the same words and are equally near any text.
// The statement orders ties by the skill's id, last, so that a call and the next agree on the
// skill (the database would otherwise pick whichever it met first, which is not a rule).
func TestTheNearestSkillStatementBreaksTiesByID(t *testing.T) {
	const want = "ORDER BY nv.embedding <=> $2::vector, latest.id\nLIMIT 1"
	if !strings.Contains(nearestSkillSQL, want) {
		t.Fatalf("nearestSkillSQL does not end in %q, so equal distances are left to the database:\n%s", want, nearestSkillSQL)
	}
}

// The verdict is component/work's. Trusted serves alone, canary serves with the model
// standing by, and a rung that does not serve says why the model does.
func TestASkillAnswerCarriesDecideServesVerdict(t *testing.T) {
	for _, tc := range []struct {
		rung    string
		serve   string
		standby bool
		reason  bool // whether the answer says why
	}{
		{"trusted", "skill", false, false},
		{"canary", "skill", true, false},
		{"shadow", "model", false, false},
		{"candidate", "model", false, true},
		{"retired", "model", false, true},
		{"", "model", false, false},
		{"unheard-of", "model", false, false},
	} {
		if rung, ok := work.ParseRung(tc.rung); ok && rung != work.RungNone {
			v := work.DecideServe(work.ReplayContext{Mode: "live", ConstructRung: rung})
			if (v.Source == work.ServeConstruct) != (tc.serve == "skill") || v.Standby != tc.standby || (v.Reason != "") != tc.reason {
				t.Fatalf("the table disagrees with work.DecideServe for %q: %+v", tc.rung, v)
			}
		}
		row := lookupRow(canonical(conceptSkill, "gs-x"), ownerKey("owner-a"), tc.rung, "2026-10-17T12:00:00Z", "initiative")
		nodes, err := skillAnswer(row, 0)
		if err != nil || len(nodes) != 1 {
			t.Fatalf("%q: skillAnswer = %v, %v", tc.rung, nodes, err)
		}
		got := answerRows(t, nodes)[0]
		if got["serve"] != tc.serve || got["standby"] != tc.standby {
			t.Errorf("%q: serve=%v standby=%v, want serve=%s standby=%v", tc.rung, got["serve"], got["standby"], tc.serve, tc.standby)
		}
		if reason, has := got["serveReason"]; has != tc.reason || (has && reason == "") {
			t.Errorf("%q: serveReason = %v (present %v), want present=%v and never empty", tc.rung, reason, has, tc.reason)
		}
	}
}

// The answer is the skill's own fields, its id in the bare spelling every answer uses,
// the verdict, and a similarity only for a match by meaning.
func TestASkillAnswerIsTheSkillItsVerdictAndASimilarityOnlyByMeaning(t *testing.T) {
	row := lookupRow(canonical(conceptSkill, "gs-x"), ownerKey("owner-a"), "trusted", "2026-10-17T12:00:00Z", "initiative")
	row["goalText"] = "Fill the chest!"
	row["lastBinding"] = map[string]any{"0.count": float64(4)}
	before := cloneRow(row)

	nodes, err := skillAnswer(row, 0)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("skillAnswer = %v, %v", nodes, err)
	}
	if nodes[0].ID != "gs-x" || nodes[0].Concept != "integration:games:skillMatch" || nodes[0].Type != memorynodes.NodeTypeObject {
		t.Errorf("the node = %q %q %v, want gs-x, integration:games:skillMatch, an object", nodes[0].ID, nodes[0].Concept, nodes[0].Type)
	}
	got := answerRows(t, nodes)[0]
	if got["id"] != "gs-x" || got["goalText"] != "Fill the chest!" || got["ownerUserId"] != ownerKey("owner-a") || !reflect.DeepEqual(got["lastBinding"], map[string]any{"0.count": float64(4)}) {
		t.Errorf("the answer lost the skill's fields: %v", got)
	}
	if sim, has := got["similarity"]; has {
		t.Errorf("a match by signature or trigger carries no similarity, got %v", sim)
	}
	if !reflect.DeepEqual(row, before) {
		t.Errorf("the lookup changed the row it was given: %v, was %v", row, before)
	}

	nodes, err = skillAnswer(row, 0.93)
	if err != nil {
		t.Fatal(err)
	}
	if got := answerRows(t, nodes)[0]; got["similarity"] != 0.93 {
		t.Errorf("a match by meaning must say how near it was: %v", got)
	}
}

// ---- a lookup by meaning, up to the statement ----

// The name of the vector table is put into the text of the statement, because an
// identifier cannot be a parameter, so only the name of a vector table is (isVectorTable:
// the one definition, which recall's join and forget's deletes ask too). Anything else is
// refused before the model is asked and before any statement is run: it is a lookup that
// cannot be made, not a reason to hand the database text.
func TestALookupByMeaningPutsNoUnvettedNameIntoAStatement(t *testing.T) {
	for _, name := range []string{
		"node_vectors; DROP TABLE x",
		lookupTable + " nv ON TRUE --",
		"MemoryNodes",
		`"MemoryNodes"`,
		"node_vectors_16_0123456789ABCDEF",
		"node_vectors_",
		" " + lookupTable,
	} {
		db, attempts := unreachableDB(t)
		space := &lookupSpace{name: name}
		p := &Provider{store: &lookupStore{}, space: space, db: db}
		nodes, err := p.nearestSkill(asOwner("owner-a"), "owner-a", "fill the chest", defaultMinSimilarity)
		if err == nil || !strings.Contains(err.Error(), "no vector table") {
			t.Errorf("the name %q: err = %v, want it refused as no vector table's name", name, err)
		}
		if len(nodes) != 0 {
			t.Errorf("the name %q: answered %v", name, nodes)
		}
		if got := space.embedded.Load(); got != 0 {
			t.Errorf("the name %q: the text was handed to the model %d times for a space that cannot be used", name, got)
		}
		if got := attempts.Load(); got != 0 {
			t.Errorf("the name %q: %d statements were attempted", name, got)
		}
	}

	// The control: a name that is a vector table's gets as far as the model and the statement.
	db, attempts := unreachableDB(t)
	space := &lookupSpace{name: lookupTable}
	p := &Provider{store: &lookupStore{}, space: space, db: db}
	_, err := p.nearestSkill(asOwner("owner-a"), "owner-a", "fill the chest", defaultMinSimilarity)
	if err == nil || !strings.Contains(err.Error(), "nearest skill") {
		t.Fatalf("a vector table's name: err = %v, want the statement to have been attempted", err)
	}
	if space.embedded.Load() != 1 || attempts.Load() != 1 {
		t.Fatalf("a vector table's name: embedded %d, statements attempted %d, want 1 and 1", space.embedded.Load(), attempts.Load())
	}
}

// With no table there is nothing to be near, so no model is asked and no statement run;
// a space that cannot be read is a failure the caller is told of, not "no match".
func TestALookupByMeaningWithNoSpaceAsksNoModel(t *testing.T) {
	db, attempts := unreachableDB(t)
	space := &lookupSpace{}
	p := &Provider{store: &lookupStore{}, space: space, db: db}
	nodes, err := p.nearestSkill(asOwner("owner-a"), "owner-a", "fill the chest", defaultMinSimilarity)
	if err != nil || len(nodes) != 0 {
		t.Fatalf("with nothing bound: %v, %v, want no answer and no error", nodes, err)
	}
	if space.embedded.Load() != 0 || attempts.Load() != 0 {
		t.Fatalf("with nothing bound the model was asked %d times and %d statements attempted", space.embedded.Load(), attempts.Load())
	}

	space = &lookupSpace{tableErr: errors.New("the embedder space could not be read")}
	p = &Provider{store: &lookupStore{}, space: space, db: db}
	if _, err := p.nearestSkill(asOwner("owner-a"), "owner-a", "fill the chest", defaultMinSimilarity); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("a space that cannot be read = %v, want it reported", err)
	}
}

// A node with no database handle cannot run the statement, and says so before it asks
// a model for a vector it could not use.
func TestALookupByMeaningWithNoDatabaseHandleAsksNoModel(t *testing.T) {
	space := &lookupSpace{name: lookupTable}
	p := &Provider{store: &lookupStore{}, space: space, db: func() *sql.DB { return nil }}
	nodes, err := p.nearestSkill(asOwner("owner-a"), "owner-a", "fill the chest", defaultMinSimilarity)
	if err == nil || !strings.Contains(err.Error(), "no database handle") || len(nodes) != 0 {
		t.Fatalf("with no database handle = %v, %v, want it reported", nodes, err)
	}
	if space.embedded.Load() != 0 {
		t.Fatalf("the model was asked %d times for a vector the node could not use", space.embedded.Load())
	}
}

// A text the model cannot embed fails the lookup, saying so and keeping the cause: the
// runtime treats that as no match, and an operator reads why.
func TestALookupByMeaningFailsWhileTheTextCannotBeEmbedded(t *testing.T) {
	cause := errors.New("no model is reachable right now")
	db, attempts := unreachableDB(t)
	p := &Provider{store: &lookupStore{}, space: &lookupSpace{name: lookupTable, embedErr: cause}, db: db}
	nodes, err := p.nearestSkill(asOwner("owner-a"), "owner-a", "fill the chest", defaultMinSimilarity)
	if err == nil || !errors.Is(err, cause) || !strings.Contains(err.Error(), "could not be embedded") || len(nodes) != 0 {
		t.Fatalf("with the model away = %v, %v, want a failure that says the text could not be embedded and wraps the cause", nodes, err)
	}
	if attempts.Load() != 0 {
		t.Fatalf("%d statements were attempted with no vector to ask them with", attempts.Load())
	}
}

// A decision waits on a lookup, so a model that is slow rather than away costs it no more
// than its bound -- whether or not the provider watches its context, as recall's wait is
// bounded -- and the lookup fails then, as it does with the model away, without a
// statement.
func TestALookupByMeaningWaitsForAModelNoLongerThanItsBound(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	db, attempts := unreachableDB(t)
	const bound = 30 * time.Millisecond
	p := &Provider{store: &lookupStore{}, space: &lookupSpace{name: lookupTable, hang: release}, db: db, embedTimeout: bound}

	// The model never answers, so a lookup that returns has given up at a bound, and one that
	// does not would never return. WHICH bound is not a duration to be measured on a machine
	// that may be busy: the error names it (embedRecallQuery says "did not finish within 30ms"),
	// and it must be the one the lookup was given and not another (the default, 2s). The wait
	// below is the point at which the test stops waiting for a lookup that will not return.
	const giveUp = 30 * time.Second
	done := make(chan error, 1)
	go func() {
		_, err := p.nearestSkill(asOwner("owner-a"), "owner-a", "fill the chest", defaultMinSimilarity)
		done <- err
	}()
	select {
	case err := <-done:
		if want := "did not finish within " + bound.String(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to say %q: the bound the lookup was given, and not another", err, want)
		}
	case <-time.After(giveUp):
		t.Fatal("a lookup by meaning waited on a model that never answers; it must give up at its bound")
	}
	if attempts.Load() != 0 {
		t.Errorf("%d statements were attempted with no vector to ask them with", attempts.Load())
	}
}

// An embedder that panics is a failed lookup, with the panic in the error, and not the end
// of the caller.
func TestALookupByMeaningSurvivesAnEmbedderThatPanics(t *testing.T) {
	db, attempts := unreachableDB(t)
	p := &Provider{store: &lookupStore{}, space: &lookupSpace{name: lookupTable, panics: true}, db: db}
	_, err := p.nearestSkill(asOwner("owner-a"), "owner-a", "fill the chest", defaultMinSimilarity)
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err = %v, want the panic reported as a failure", err)
	}
	if attempts.Load() != 0 {
		t.Fatalf("%d statements were attempted", attempts.Load())
	}
}

// ---- the Task 5 carry: the pending scan puts the same name into a statement ----

// gamesEmbedPending builds its statement from the same table name, and so asks the same
// question first: a name that is no vector table's is refused before any statement is
// run (the ledger's carry from Task 5, which named this statement and the lookup's).
func TestEmbedPendingPutsNoUnvettedNameIntoAStatement(t *testing.T) {
	for _, name := range []string{
		"node_vectors; DROP TABLE x",
		lookupTable + " nv WHERE FALSE --",
		"MemoryNodes",
		"node_vectors_16_0123456789ABCDEF",
	} {
		db, attempts := unreachableDB(t)
		p := &Provider{store: &lookupStore{}, space: &lookupSpace{name: name}, db: db}
		nodes, err := p.embedPending(asOwner("owner-a"), map[string]any{}, 0)
		if err == nil || !strings.Contains(err.Error(), "no vector table") {
			t.Errorf("the name %q: err = %v, want it refused as no vector table's name", name, err)
		}
		if len(nodes) != 0 {
			t.Errorf("the name %q: answered %v", name, nodes)
		}
		if got := attempts.Load(); got != 0 {
			t.Errorf("the name %q: %d statements were attempted", name, got)
		}
	}

	// The control: a vector table's name gets as far as the statement.
	db, attempts := unreachableDB(t)
	p := &Provider{store: &lookupStore{}, space: &lookupSpace{name: lookupTable}, db: db}
	if _, err := p.embedPending(asOwner("owner-a"), map[string]any{}, 0); err == nil || !strings.Contains(err.Error(), "pending vectors") || attempts.Load() != 1 {
		t.Fatalf("a vector table's name: err = %v, statements attempted %d, want the pending scan to have been attempted once", err, attempts.Load())
	}
	// No table at all is the ordinary state of a fresh install: every row is pending.
	db, attempts = unreachableDB(t)
	p = &Provider{store: &lookupStore{}, space: &lookupSpace{}, db: db}
	if _, err := p.embedPending(asOwner("owner-a"), map[string]any{}, 0); err == nil || !strings.Contains(err.Error(), "pending vectors") || attempts.Load() != 1 {
		t.Fatalf("no table: err = %v, statements attempted %d, want the pending scan to have been attempted once", err, attempts.Load())
	}
}

// The floor is the engine's own near-match threshold, and a similarity reaches it by being
// at it or above: equal counts, a hair under does not, and a similarity that is not a
// number -- pgvector's answer for the cosine of a vector with no direction -- reaches
// nothing, however low the floor.
func TestASimilarityReachesTheFloorOnlyIfItIsANumberAtOrAboveIt(t *testing.T) {
	if defaultMinSimilarity != 0.82 {
		t.Fatalf("defaultMinSimilarity = %v, want the engine's near-match threshold, 0.82", defaultMinSimilarity)
	}
	for _, tc := range []struct {
		name       string
		similarity float64
		floor      float64
		want       bool
	}{
		{"above", 0.9, 0.82, true},
		{"at", 0.82, 0.82, true},
		{"a hair under", math.Nextafter(0.82, 0), 0.82, false},
		{"far under", 0.3, 0.82, false},
		{"one against a floor of one", 1, 1, true},
		{"not a number", math.NaN(), 0.82, false},
		{"not a number against the lowest floor", math.NaN(), math.SmallestNonzeroFloat64, false},
		{"positive infinity", math.Inf(1), 0.82, false},
		{"negative infinity", math.Inf(-1), 0.82, false},
	} {
		if got := matchesFloor(tc.similarity, tc.floor); got != tc.want {
			t.Errorf("%s: matchesFloor(%v, %v) = %v, want %v", tc.name, tc.similarity, tc.floor, got, tc.want)
		}
	}
}

// The builder is where the name goes into the text of a statement, so it is the builder that
// takes only the name of a vector table, as recall's does: a second caller of it is as safe as
// the first. A name that is not one is an error and no statement.
func TestPendingStatementTakesOnlyTheNameOfAVectorTable(t *testing.T) {
	for _, name := range []string{
		"node_vectors; DROP TABLE x",
		lookupTable + " nv WHERE FALSE --",
		"MemoryNodes",
		"node_vectors_16_0123456789ABCDEF",
		" " + lookupTable,
	} {
		statement, err := pendingStatement(name)
		if err == nil || !strings.Contains(err.Error(), "no vector table") {
			t.Errorf("the name %q: err = %v, want it refused as no vector table's name", name, err)
		}
		if statement != "" {
			t.Errorf("the name %q: built the statement %q", name, statement)
		}
	}
	statement, err := pendingStatement(lookupTable)
	if err != nil || strings.Count(statement, lookupTable) != 1 || !strings.Contains(statement, "FROM "+lookupTable+" nv WHERE nv.id = latest.id") {
		t.Errorf("a vector table's name: %q, %v, want it once, as the table the vectors are looked for in", statement, err)
	}
	statement, err = pendingStatement("")
	if err != nil || !strings.Contains(statement, "WHERE TRUE") {
		t.Errorf("no table: %q, %v, want every row pending", statement, err)
	}
}
