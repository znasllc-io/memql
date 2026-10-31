package gamespack

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// episodes_test.go -- what needs no database: the kind set is the DSL's, an
// entry is checked against everything the mutation constrains, a refused batch
// writes nothing, and a call with no person behind it is refused first.

// ---- the kinds are spelled once in Go, and the DSL says the same ----

// declaredKindEnum returns the members of the `kind enum(...)` field inside one
// top-level declaration of a DSL file; the declaration runs from its header
// line to the closing brace in column 0.
func declaredKindEnum(t *testing.T, file, header string) []string {
	t.Helper()
	raw, err := fs.ReadFile(Tree(), file)
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "\n"+header+" {\n")
	if start < 0 {
		t.Fatalf("%s declares no %q", file, header)
	}
	body := src[start+1:]
	end := strings.Index(body, "\n}\n")
	if end < 0 {
		t.Fatalf("%s: the %q declaration is not closed", file, header)
	}
	fields := regexp.MustCompile(`\bkind\s+enum\(([^)]*)\)`).FindAllStringSubmatch(body[:end], -1)
	if len(fields) != 1 {
		t.Fatalf("%s: %q declares %d kind enums, want exactly 1", file, header, len(fields))
	}
	var members []string
	for _, m := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(fields[0][1], -1) {
		members = append(members, m[1])
	}
	sort.Strings(members)
	return members
}

// The DSL declares the kinds twice, on the concept and on the mutation. If the
// Go set drifts from either, a batch is refused for a kind the mutation
// accepts, or let through to a mutation that refuses it mid-batch.
func TestEpisodeKindsAreTheOnesTheDSLDeclares(t *testing.T) {
	inGo := append([]string(nil), episodeKinds...)
	sort.Strings(inGo)
	inConcept := declaredKindEnum(t, "concepts.memql", "concept episode")
	inMutation := declaredKindEnum(t, "mutations.memql", "mutation episode createGamesEpisode")
	if len(inGo) == 0 || len(inConcept) == 0 || len(inMutation) == 0 {
		t.Fatalf("an empty kind set proves nothing: go %v, concept %v, mutation %v", inGo, inConcept, inMutation)
	}
	if !sameStrings(inGo, inConcept) || !sameStrings(inGo, inMutation) {
		t.Fatalf("the three kind sets must agree: episodeKinds %v, concept episode %v, mutation createGamesEpisode %v", inGo, inConcept, inMutation)
	}
}

// ---- the datetime check itself ----

// isDateTime stands in, up front, for the schema's date-time check at write, and
// it must never be wider than that check. Each bound is pinned HERE, on its
// own: validateEpisode also parses the time before it stores it, and that parse
// refuses minute 60 and second 61 too, so a bound loosened in isDateTime would
// hide behind it in the tables below.
func TestIsDateTimeRefusesEachOutOfRangeFieldOnItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   string
		want bool
	}{
		{"an ordinary time", "2026-10-18T12:00:00Z", true},
		{"a lowercase t and z", "2026-10-18t12:00:00z", true},
		{"a fraction and an offset", "2026-10-18T12:00:00.5+05:30", true},
		{"the last second of a day", "2026-10-18T23:59:59-23:59", true},
		{"the largest offset", "2026-10-18T00:00:00+23:59", true},
		{"nothing", "", false},
		{"an hour of 24", "2026-10-18T24:00:00Z", false},
		{"a minute of 60", "2026-10-18T12:60:00Z", false},
		{"a second of 61", "2026-10-18T12:00:61Z", false},
		// Valid RFC 3339, and the schema takes it; Go's time package cannot hold
		// one, so it is refused by design.
		{"a leap second", "2026-06-30T23:59:60Z", false},
		{"an offset hour of 24", "2026-10-18T12:00:00+24:00", false},
		{"an offset minute of 60", "2026-10-18T12:00:00+00:60", false},
		{"a comma before the fraction", "2026-10-18T12:00:00,5Z", false},
		{"a one-digit hour", "2026-10-18T2:00:00Z", false},
		{"a day that is not on the calendar", "2026-02-30T12:00:00Z", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDateTime(tc.at); got != tc.want {
				t.Fatalf("isDateTime(%q) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

// ---- one entry of a batch ----

// goodEpisode is an entry validateEpisode accepts. Each row of badEpisodes
// spoils exactly one thing in it.
func goodEpisode() map[string]any {
	return map[string]any{
		"episodeId": "e-1", "characterId": "wren", "worldId": "meadow",
		"at": "2026-10-18T12:00:00Z", "kind": "chat",
		"summary": "wren planted a garden", "importance": 4,
	}
}

// spoiled is goodEpisode with one field replaced, or removed when v is nil.
func spoiled(key string, v any) map[string]any {
	e := goodEpisode()
	if v == nil {
		delete(e, key)
	} else {
		e[key] = v
	}
	return e
}

// badEpisodes are the entries a batch must be refused for before anything is
// written, and the sentence each refusal must contain; %d is the entry's index
// in its batch. Each constraint was checked against the real mutation on a live
// engine when the table was written. Blank and missing text is refused by this
// pack as well as by the mutation, as it always was.
var badEpisodes = []struct {
	name  string
	entry any
	want  string
}{
	{"an entry that is not an object", "wren planted a garden", "episode %d is not an object"},
	{"no episodeId", spoiled("episodeId", nil), "episode %d has no episodeId"},
	{"a blank episodeId", spoiled("episodeId", "   "), "episode %d has no episodeId"},
	{"an episodeId of 65 characters", spoiled("episodeId", strings.Repeat("e", 65)), "episode %d episodeId is 65 characters, at most 64"},
	{"an episodeId of 65 multi-byte characters", spoiled("episodeId", strings.Repeat("\u00e9", 65)), "episode %d episodeId is 65 characters, at most 64"},
	// An id the storage gate refuses (colons that are not the concept's own
	// prefix) is refused here, because the gate meets it only after the entries
	// before it were written.
	{"an episodeId with colons", spoiled("episodeId", "wren:tick:7"), "episode %d episodeId must be a bare id with no colons"},
	{"an episodeId qualified for another concept", spoiled("episodeId", "v1:games:player:p-sam"), "episode %d episodeId must be a bare id with no colons"},
	// A NUL byte is a syntax error in the SQL the engine builds around an id,
	// met when it reads the row's latest version, after the entries before it.
	{"an episodeId with a NUL character", spoiled("episodeId", "wren\x00tick"), "episode %d episodeId must be a bare id with no colons"},
	{"an episodeId qualified for the episode concept with a NUL after it", spoiled("episodeId", "v1:games:episode:a\x00b"), "episode %d episodeId must be a bare id with no colons"},
	{"an episodeId qualified for the episode concept with colons after it", spoiled("episodeId", "v1:games:episode:a:b"), "episode %d episodeId must be a bare id with no colons"},
	// A playerId is a reference to a player, canonicalized on write: another
	// concept's id is refused there, mid-batch.
	{"a playerId qualified for another concept", spoiled("playerId", "v1:games:episode:x"), "episode %d playerId must be a bare id with no colons"},
	{"a playerId with colons", spoiled("playerId", "wren:tick:7"), "episode %d playerId must be a bare id with no colons"},
	// Text is a JSON string. A number, array or object would otherwise be stored
	// as its printed form: a summary of 42 as "42", a playerId of 12345 as the
	// player v1:games:player:12345.
	{"an episodeId that is a number", spoiled("episodeId", 7), "episode %d episodeId must be a string"},
	{"a characterId that is an array", spoiled("characterId", []any{"wren"}), "episode %d characterId must be a string"},
	{"a worldId that is an object", spoiled("worldId", map[string]any{"name": "meadow"}), "episode %d worldId must be a string"},
	{"a playerId that is a number", spoiled("playerId", 12345), "episode %d playerId must be a string"},
	{"an at that is a number", spoiled("at", 1760788800), "episode %d at must be a string"},
	{"a kind that is a boolean", spoiled("kind", true), "episode %d kind must be a string"},
	{"a summary that is a number", spoiled("summary", 42), "episode %d summary must be a string"},
	{"no characterId", spoiled("characterId", nil), "episode %d has no characterId"},
	{"a characterId of 65 characters", spoiled("characterId", strings.Repeat("c", 65)), "episode %d characterId is 65 characters, at most 64"},
	{"no worldId", spoiled("worldId", nil), "episode %d has no worldId"},
	{"a worldId of 65 characters", spoiled("worldId", strings.Repeat("w", 65)), "episode %d worldId is 65 characters, at most 64"},
	{"no at", spoiled("at", nil), "episode %d has no at"},
	{"an at that is not a datetime", spoiled("at", "yesterday"), "episode %d at must be an RFC 3339 datetime"},
	{"an at that is only a date", spoiled("at", "2026-10-18"), "episode %d at must be an RFC 3339 datetime"},
	{"an at with no zone", spoiled("at", "2026-10-18T12:00:00"), "episode %d at must be an RFC 3339 datetime"},
	{"an at on a day that is not on the calendar", spoiled("at", "2026-02-30T12:00:00Z"), "episode %d at must be an RFC 3339 datetime"},
	{"an at with an hour of 24", spoiled("at", "2026-10-18T24:00:00Z"), "episode %d at must be an RFC 3339 datetime"},
	{"an at with a minute of 60", spoiled("at", "2026-10-18T12:60:00Z"), "episode %d at must be an RFC 3339 datetime"},
	{"an at with a second of 61", spoiled("at", "2026-10-18T12:00:61Z"), "episode %d at must be an RFC 3339 datetime"},
	// A leap second is valid RFC 3339 and the schema takes it, but Go's time
	// package cannot hold one: it is refused, as isDateTime documents.
	{"an at that is a leap second", spoiled("at", "2026-06-30T23:59:60Z"), "episode %d at must be an RFC 3339 datetime"},
	// Moved to UTC these leave the years 0000 to 9999, which the schema's date
	// check cannot read back.
	{"an at that is in year 10000 once moved to UTC", spoiled("at", "9999-12-31T23:59:59-23:59"), "episode %d at must be an RFC 3339 datetime"},
	{"an at that is in year -1 once moved to UTC", spoiled("at", "0000-01-01T00:00:00+05:30"), "episode %d at must be an RFC 3339 datetime"},
	// The next four are accepted by time.Parse(time.RFC3339) and refused by the
	// schema's date-time format when the row is written: a validator that took
	// them would leave a batch half written.
	{"an at with a comma before the fraction", spoiled("at", "2026-10-18T12:00:00,5Z"), "episode %d at must be an RFC 3339 datetime"},
	{"an at with a one-digit hour", spoiled("at", "2026-10-18T2:00:00Z"), "episode %d at must be an RFC 3339 datetime"},
	{"an at with an offset of +24:00", spoiled("at", "2026-10-18T12:00:00+24:00"), "episode %d at must be an RFC 3339 datetime"},
	{"an at with an offset of +00:60", spoiled("at", "2026-10-18T12:00:00+00:60"), "episode %d at must be an RFC 3339 datetime"},
	{"no kind", spoiled("kind", nil), "episode %d has no kind"},
	{"a kind outside the enum", spoiled("kind", "nonsense"), "episode %d kind must be one of " + strings.Join(episodeKinds, ", ")},
	{"a kind in the wrong case", spoiled("kind", "Chat"), "episode %d kind must be one of " + strings.Join(episodeKinds, ", ")},
	{"no summary", spoiled("summary", nil), "episode %d has no summary"},
	{"a summary of 401 characters", spoiled("summary", strings.Repeat("s", 401)), "episode %d summary is 401 characters, at most 400"},
	{"no importance", spoiled("importance", nil), "episode %d has no importance"},
	{"an importance of 0", spoiled("importance", 0), "episode %d importance must be a whole number from 1 to 10"},
	{"an importance of 11", spoiled("importance", 11), "episode %d importance must be a whole number from 1 to 10"},
	{"a negative importance", spoiled("importance", -3), "episode %d importance must be a whole number from 1 to 10"},
	{"a fractional importance", spoiled("importance", 4.5), "episode %d importance must be a whole number from 1 to 10"},
	{"an importance written as text", spoiled("importance", "5"), "episode %d importance must be a whole number from 1 to 10"},
	{"a boolean importance", spoiled("importance", true), "episode %d importance must be a whole number from 1 to 10"},
}

// The refusal leads with the stable code and names the entry and the field.
func TestValidateEpisodeRefusesWhatTheMutationWouldRefuse(t *testing.T) {
	for _, tc := range badEpisodes {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateEpisode(3, tc.entry)
			if err == nil {
				t.Fatal("accepted")
			}
			if want := fmt.Sprintf(tc.want, 3); !strings.HasPrefix(err.Error(), "games_invalid_argument: ") || !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal = %q, want games_invalid_argument naming %q", err, want)
			}
		})
	}
}

// The edges of what the mutation accepts are accepted: a bound is inclusive,
// and lengths count characters (the engine counts runes), not bytes.
func TestValidateEpisodeAcceptsTheEdgesOfWhatTheMutationAccepts(t *testing.T) {
	type edge struct {
		name  string
		entry map[string]any
	}
	cases := []edge{
		{"ids of exactly 64 characters", func() map[string]any {
			e := goodEpisode()
			for _, k := range []string{"episodeId", "characterId", "worldId"} {
				e[k] = strings.Repeat("i", 64)
			}
			return e
		}()},
		{"ids of exactly 64 multi-byte characters", func() map[string]any {
			e := goodEpisode()
			for _, k := range []string{"episodeId", "characterId", "worldId"} {
				e[k] = strings.Repeat("\u00e9", 64)
			}
			return e
		}()},
		{"a summary of exactly 400 characters", spoiled("summary", strings.Repeat("s", 400))},
		{"a summary of exactly 400 multi-byte characters", spoiled("summary", strings.Repeat("\u00e9", 400))},
		{"the lowest importance", spoiled("importance", 1)},
		{"the highest importance", spoiled("importance", 10)},
		{"an importance written as a whole float", spoiled("importance", 5.0)},
		{"an importance written as a 64-bit integer", spoiled("importance", int64(7))},
		{"an importance written as a JSON number", spoiled("importance", json.Number("7"))},
		{"fractional seconds and a numeric offset", spoiled("at", "2026-10-18T12:00:00.123456+05:30")},
		{"an at with a lowercase t and z, which RFC 3339 allows", spoiled("at", "2026-10-18t12:00:00z")},
		{"an at with the extreme offsets", spoiled("at", "2026-10-18T12:00:00-23:59")},
		{"an at on the last day of a leap february", spoiled("at", "2028-02-29T23:59:59Z")},
		{"an id of 64 characters padded with spaces", spoiled("episodeId", "  "+strings.Repeat("i", 64)+"  ")},
		{"an episodeId qualified for the episode concept", spoiled("episodeId", "v1:games:episode:e-1")},
		{"a bare playerId", spoiled("playerId", "p-sam")},
		{"a playerId qualified for the player concept", spoiled("playerId", "v1:games:player:p-sam")},
		{"a null playerId is no playerId", func() map[string]any {
			e := goodEpisode()
			e["playerId"] = nil
			return e
		}()},
	}
	for _, kind := range episodeKinds {
		cases = append(cases, edge{"the kind " + kind, spoiled("kind", kind)})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateEpisode(0, tc.entry); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// What is returned is what the mutation is called with: trimmed text, the time
// in its one stored form, the importance as an int, and the player only when
// there is one. recordEpisodes reads the typed fields, never an assertion.
func TestValidateEpisodeReturnsTheArgumentsToWrite(t *testing.T) {
	entry := goodEpisode()
	entry["episodeId"] = "  e-1  "
	entry["summary"] = "\twren planted a garden \n"
	entry["importance"] = 4.0
	got, err := validateEpisode(0, entry)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"episodeId": "e-1", "characterId": "wren", "worldId": "meadow",
		"at": "2026-10-18T12:00:00.000Z", "kind": "chat",
		"summary": "wren planted a garden", "importance": 4,
	}
	if !reflect.DeepEqual(got.args(), want) {
		t.Fatalf("arguments = %#v, want %#v", got.args(), want)
	}
	if got.id != "e-1" || got.summary != "wren planted a garden" {
		t.Fatalf("typed fields = id %q, summary %q, want e-1 and the trimmed summary", got.id, got.summary)
	}

	entry["playerId"] = " p-sam "
	got, err = validateEpisode(0, entry)
	if err != nil {
		t.Fatal(err)
	}
	want["playerId"] = "p-sam"
	if !reflect.DeepEqual(got.args(), want) {
		t.Fatalf("arguments with a player = %#v, want %#v", got.args(), want)
	}
}

// An episode's time is stored in ONE form: UTC, milliseconds always written, the
// zone always Z. gamesSessionEpisodes compares it as text, so that is what makes
// text order the same as time order; a lowercase t or z, an offset or a longer
// fraction is accepted but never stored as written. Milliseconds are cut, not
// rounded, so a time is never moved later than it was.
func TestValidateEpisodeStoresTimeInOneFixedWidthForm(t *testing.T) {
	for _, tc := range []struct{ name, at, want string }{
		{"already canonical", "2026-10-18T12:00:00.000Z", "2026-10-18T12:00:00.000Z"},
		{"no fraction", "2026-10-18T12:00:00Z", "2026-10-18T12:00:00.000Z"},
		{"a one-digit fraction", "2026-10-18T12:00:00.5Z", "2026-10-18T12:00:00.500Z"},
		{"a lowercase t and z", "2026-10-18t12:00:00z", "2026-10-18T12:00:00.000Z"},
		{"an offset", "2026-10-18T12:00:00+05:30", "2026-10-18T06:30:00.000Z"},
		{"an offset that crosses midnight", "2026-10-18T01:00:00+05:30", "2026-10-17T19:30:00.000Z"},
		{"an offset that crosses the new year", "2026-12-31T23:59:59-23:59", "2027-01-01T23:58:59.000Z"},
		{"a negative zero offset", "2026-10-18T12:00:00-00:00", "2026-10-18T12:00:00.000Z"},
		{"a twelve-digit fraction", "2026-10-18T12:00:00.123456789123Z", "2026-10-18T12:00:00.123Z"},
		{"a fraction that would round up", "2026-10-18T12:00:00.9999Z", "2026-10-18T12:00:00.999Z"},
		{"padding around it", "  2026-10-18T12:00:00Z  ", "2026-10-18T12:00:00.000Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateEpisode(0, spoiled("at", tc.at))
			if err != nil {
				t.Fatal(err)
			}
			if got.at != tc.want {
				t.Fatalf("stored as %q, want %q", got.at, tc.want)
			}
		})
	}

	// And that form sorts as time does, which is the point of it.
	spellings := []string{
		"2026-10-18T10:00:00+05:30",         // 04:30:00.000Z
		"2026-10-18T05:00:00Z",              // 05:00:00.000Z
		"2026-10-18T06:00:00.123456789123Z", // 06:00:00.123Z
		"2026-10-18t07:00:00z",              // 07:00:00.000Z
	}
	var stored []string
	for _, at := range spellings {
		got, err := validateEpisode(0, spoiled("at", at))
		if err != nil {
			t.Fatal(err)
		}
		stored = append(stored, got.at)
	}
	if !sort.StringsAreSorted(stored) {
		t.Fatalf("stored forms %v are not in text order, though the instants were written in time order", stored)
	}
}

// ---- a batch ----

// writeLog is a rowStore that remembers what it was asked to write.
type writeLog struct{ written []string }

func (w *writeLog) query(context.Context, string, map[string]any) ([]map[string]any, error) {
	return nil, nil
}

func (w *writeLog) write(_ context.Context, _ string, args map[string]any) error {
	w.written = append(w.written, trimmed(args["episodeId"]))
	return nil
}

func (w *writeLog) embedStore(context.Context, string, string, string) error { return nil }

func (w *writeLog) createTrace(context.Context, map[string]any) error { return nil }

func (w *writeLog) createSkill(context.Context, map[string]any) error { return nil }

func (w *writeLog) recordLadder(context.Context, map[string]any) error { return nil }

func (w *writeLog) walk(context.Context, string, map[string]any, func([]map[string]any) error) error {
	return nil
}

// embedLog is an embeddingSpace that remembers what it was asked to embed.
type embedLog struct{ stored []string }

func (e *embedLog) table(context.Context) (string, error) { return "", nil }

func (e *embedLog) embedQuery(context.Context, string) ([]float32, error) { return nil, nil }

func (e *embedLog) store(_ context.Context, nodeID, _, _ string) error {
	e.stored = append(e.stored, nodeID)
	return nil
}

// A batch is all or nothing for bad input: every entry is checked before the
// first is written. The bad entry is always the SECOND one, so a handler that
// writes as it goes has already written the first.
func TestRecordEpisodesWritesNothingWhenAnyEntryIsInvalid(t *testing.T) {
	ctx := asOwner("user-games-validate")
	for _, tc := range badEpisodes {
		t.Run(tc.name, func(t *testing.T) {
			rows, space := &writeLog{}, &embedLog{}
			p := &Provider{store: rows, space: space}
			_, err := p.recordEpisodes(ctx, map[string]any{"episodes": []any{goodEpisode(), tc.entry}}, 0)
			if want := fmt.Sprintf(tc.want, 1); err == nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: ") || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want games_invalid_argument naming %q", err, want)
			}
			if len(rows.written) != 0 || len(space.stored) != 0 {
				t.Fatalf("a refused batch wrote %v and embedded %v, want nothing", rows.written, space.stored)
			}
		})
	}

	// The control: the same batch with a valid second entry is recorded whole,
	// so the empty result above is the refusal's doing.
	rows, space := &writeLog{}, &embedLog{}
	p := &Provider{store: rows, space: space}
	second := goodEpisode()
	second["episodeId"] = "e-2"
	if _, err := p.recordEpisodes(ctx, map[string]any{"episodes": []any{goodEpisode(), second}}, 0); err != nil {
		t.Fatal(err)
	}
	if !sameStrings(rows.written, []string{"e-1", "e-2"}) || len(space.stored) != 2 {
		t.Fatalf("a valid batch wrote %v and embedded %v, want both episodes", rows.written, space.stored)
	}
}

// ---- who is calling ----

// Every games row is somebody's, so a call with no person behind it is refused
// before anything is read or written. The provider is the zero value on
// purpose: reaching its store, its space or its database would panic, so a
// refusal here also proves the actor check runs first.
func TestEpisodeBuiltinsRefuseACallWithNoActor(t *testing.T) {
	handlers := map[string]memql.IntegrationCapability{}
	for _, c := range (&Provider{}).Capabilities() {
		handlers[c.Name] = c
	}
	calls := map[string]map[string]any{
		"recordEpisodes": {"episodes": []any{goodEpisode()}},
		"embedPending":   {"limit": 1},
	}
	nobody := map[string]context.Context{
		"no actor in the context":         context.Background(),
		"an actor whose user id is blank": auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "   "}),
	}
	for name, args := range calls {
		c, ok := handlers[name]
		if !ok {
			t.Fatalf("no capability %s", name)
		}
		for label, ctx := range nobody {
			t.Run(name+"/"+label, func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("the call got past the actor check and reached the provider's store or database: %v", r)
					}
				}()
				if _, err := c.Handler(ctx, args, 0); err == nil || !strings.HasPrefix(err.Error(), "games_unauthenticated: ") {
					t.Fatalf("error = %v, want games_unauthenticated", err)
				}
			})
		}
	}

	// The control: with a signed-in owner the same calls get past the actor
	// check, so the refusals above are the check's doing.
	rows, space := &writeLog{}, &embedLog{}
	p := &Provider{store: rows, space: space, db: func() *sql.DB { return nil }}
	owner := asOwner("user-games-actor")
	if _, err := p.recordEpisodes(owner, calls["recordEpisodes"], 0); err != nil || len(rows.written) != 1 {
		t.Fatalf("recordEpisodes as an owner = %v (wrote %v), want it recorded", err, rows.written)
	}
	if _, err := p.embedPending(owner, calls["embedPending"], 0); err == nil || strings.HasPrefix(err.Error(), "games_unauthenticated") {
		t.Fatalf("embedPending as an owner = %v, want to get past the actor check (and stop at the missing database handle)", err)
	}
}
