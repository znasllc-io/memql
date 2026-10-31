package gamespack

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// forget_test.go -- what needs no database: the pattern a trace is matched with,
// the names a delete may put into its text, and the calls forget refuses before it
// reads or deletes anything.

// ---- the pattern ----

// The pattern is built from the gamertag, so each character is checked against the
// two languages it passes through: escaped for the JSON text a trace is matched in
// (the quote, the backslash and the control characters), then for the pattern
// language (the thirteen characters it reads). The database test holds the result
// to what the database does with it.
func TestTraceMentionPatternIsTheEscapedNamesBetweenNameBoundaries(t *testing.T) {
	const before, after = `(^|[^[:alnum:]_]|\\[bfnrt]|\\u[0-9a-f]{4})`, `([^[:alnum:]_]|$)`
	for _, tc := range []struct {
		name  string
		names []string
		want  string
	}{
		{"a plain name", []string{"Tag"}, before + `(Tag)` + after},
		{"a leading dot and an underscore", []string{".Kid_Tag"}, before + `(\.Kid_Tag)` + after},
		{"every character of the pattern language", []string{`\.[](){}*+?|^$`}, before + `(\\\\\.\[\]\(\)\{\}\*\+\?\|\^\$)` + after},
		{"a double quote", []string{`Say "Hi"`}, before + `(Say \\"Hi\\")` + after},
		{"a control character", []string{"A\tB", "A\x01B"}, before + `(A\\tB|A\\u0001B)` + after},
		{"letters outside ASCII", []string{"Zo\u00eb"}, before + "(Zo\u00eb)" + after},
		{"several names", []string{"Old", "New"}, before + `(Old|New)` + after},
		{"surrounding space is not part of a name", []string{"  Tag \n"}, before + `(Tag)` + after},
		{"a blank name is no name", []string{"Tag", "   ", ""}, before + `(Tag)` + after},
		{"a name repeated, however it is spaced, is one", []string{"Tag", " Tag", "Tag\n", "Other", "Tag"}, before + `(Tag|Other)` + after},
		{"only blank names", []string{"", " \t"}, ""},
		{"no names", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := traceMentionPattern(tc.names...); got != tc.want {
				t.Errorf("traceMentionPattern(%q) = %q, want %q", tc.names, got, tc.want)
			}
		})
	}
}

// ---- the deletes of the vectors ----

// execLog is the part of a transaction deleteVectorsIn uses, remembering what it was
// asked to run. Every statement deletes `affected` rows.
type execLog struct {
	statements []string
	params     [][]any
	affected   int64
	failOn     string // a statement containing this fails
}

type rowsAffected int64

func (rowsAffected) LastInsertId() (int64, error)   { return 0, errors.New("no insert id") }
func (n rowsAffected) RowsAffected() (int64, error) { return int64(n), nil }

func (e *execLog) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	e.statements = append(e.statements, query)
	e.params = append(e.params, args)
	if e.failOn != "" && strings.Contains(query, e.failOn) {
		return nil, errors.New("the delete failed")
	}
	return rowsAffected(e.affected), nil
}

func idSet(prefix string, n int) map[string]bool {
	ids := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		ids[prefix+strconv.Itoa(i)] = true
	}
	return ids
}

// A DELETE puts the name of the table into its text -- an identifier cannot be a
// parameter -- so it takes the name of a vector table and nothing else, however the
// name was reached. And an erasure must fail closed: a table whose name is none is a
// table whose vectors cannot be deleted, so the delete is an error, and it is an error
// before anything is deleted, wherever in the list the name stands, rather than a
// forget that says it is done with vectors left behind.
func TestDeleteVectorsFailsOnAnyNameThatIsNoVectorTables(t *testing.T) {
	ids := idSet("v1:games:episode:e", 3)
	for _, hostile := range notVectorTables {
		t.Run(fmt.Sprintf("%q", hostile), func(t *testing.T) {
			// In the middle of the list, so that a delete that goes table by table has
			// emptied the ones before it by the time it reaches this one.
			tables := append(append(append([]string{}, vectorTables[:2]...), hostile), vectorTables[2:]...)
			ex := &execLog{affected: 2}
			got, err := deleteVectorsIn(context.Background(), ex, tables, ids)
			if err == nil {
				t.Fatalf("deleted %d vectors and said nothing of the table %q", got, hostile)
			}
			if got != 0 || len(ex.statements) != 0 {
				t.Errorf("ran %d statements, deleting %d vectors, before refusing the table %q:\n%s", len(ex.statements), got, hostile, strings.Join(ex.statements, "\n"))
			}
			if want := fmt.Sprintf("%.60q", hostile); !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to name the table as %s", err, want)
			}
		})
	}

	// The control: with only the names of vector tables every one is emptied, of
	// every id, by statements that are exactly the delete of the three ids.
	ex := &execLog{affected: 2}
	got, err := deleteVectorsIn(context.Background(), ex, vectorTables, ids)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(vectorTables) * 2; got != want {
		t.Errorf("deleted %d vectors, want %d: two from each of the %d vector tables", got, want, len(vectorTables))
	}
	used := map[string]bool{}
	for _, statement := range ex.statements {
		rest, ok := strings.CutPrefix(statement, "DELETE FROM ")
		table, tail, _ := strings.Cut(rest, " ")
		if !ok || tail != "WHERE id IN ($1, $2, $3)" || !isVectorTable(table) {
			t.Errorf("a statement that is not the delete of the vectors of three ids from a vector table: %q", statement)
		}
		used[table] = true
	}
	if len(ex.statements) != len(vectorTables) || len(used) != len(vectorTables) {
		t.Errorf("ran %d statements over the tables %v, want one for each of %v", len(ex.statements), used, vectorTables)
	}
}

// A player with a long history has more ids than a statement should carry, so the
// ids go in batches. Every id is deleted once from every table, in whole batches.
func TestDeleteVectorsGoesInBatches(t *testing.T) {
	const ids = 2*vectorDeleteBatch + 7
	ex := &execLog{affected: 1}
	tables := vectorTables[:2]
	got, err := deleteVectorsIn(context.Background(), ex, tables, idSet("id", ids))
	if err != nil {
		t.Fatal(err)
	}
	if want := len(tables) * 3; len(ex.statements) != want || got != want {
		t.Fatalf("%d statements deleting %d rows, want %d of each: three batches for each of the %d tables", len(ex.statements), got, want, len(tables))
	}
	seen := map[string]int{}
	for i, params := range ex.params {
		if len(params) > vectorDeleteBatch || len(params) == 0 {
			t.Errorf("statement %d carries %d ids, want 1 to %d", i, len(params), vectorDeleteBatch)
		}
		if marks := strings.Count(ex.statements[i], "$"); marks != len(params) {
			t.Errorf("statement %d has %d placeholders for %d ids", i, marks, len(params))
		}
		for _, p := range params {
			seen[fmt.Sprintf("%s|%s", strings.Fields(ex.statements[i])[2], p)]++
		}
	}
	if len(seen) != ids*len(tables) {
		t.Errorf("%d (table, id) pairs deleted, want %d", len(seen), ids*len(tables))
	}
	for pair, times := range seen {
		if times != 1 {
			t.Errorf("%s deleted %d times, want once", pair, times)
		}
	}
}

func TestDeleteVectorsWithNothingToDeleteRunsNothing(t *testing.T) {
	ex := &execLog{}
	if got, err := deleteVectorsIn(context.Background(), ex, vectorTables, nil); got != 0 || err != nil || len(ex.statements) != 0 {
		t.Fatalf("no ids: deleted %d, err %v, ran %d statements; want nothing at all", got, err, len(ex.statements))
	}
}

func TestDeleteVectorsStopsAtTheFirstFailure(t *testing.T) {
	ex := &execLog{failOn: vectorTables[1]}
	_, err := deleteVectorsIn(context.Background(), ex, vectorTables, idSet("i", 1))
	if err == nil || !strings.Contains(err.Error(), "the delete failed") {
		t.Fatalf("error = %v, want the delete's own", err)
	}
	if len(ex.statements) != 2 {
		t.Errorf("ran %d statements, want 2: the one that failed was the last", len(ex.statements))
	}
}

// ---- what forget refuses before it reads or deletes anything ----

// The provider is the zero value on purpose: reaching its store or its database
// would panic, so a refusal here also proves the refusal comes first.
func TestForgetPlayerRefusesWhatItCannotUseBeforeTouchingAnything(t *testing.T) {
	const bareID = "p-sam"
	for _, tc := range []struct {
		name string
		ctx  context.Context
		args map[string]any
		want string
	}{
		{"no actor", context.Background(), map[string]any{"playerId": bareID}, "games_unauthenticated: "},
		{"no playerId", asOwner("user-games-forget-args"), map[string]any{}, "games_invalid_argument: playerId is required"},
		{"a blank playerId", asOwner("user-games-forget-args"), map[string]any{"playerId": "  \t"}, "games_invalid_argument: playerId is required"},
		{"another concept's id", asOwner("user-games-forget-args"), map[string]any{"playerId": "v1:games:episode:" + bareID}, "games_invalid_argument: playerId must be a bare id"},
		{"an id with colons", asOwner("user-games-forget-args"), map[string]any{"playerId": "p:sam:1"}, "games_invalid_argument: playerId must be a bare id"},
		{"a NUL character", asOwner("user-games-forget-args"), map[string]any{"playerId": "p-sam\x00"}, "games_invalid_argument: playerId must be a bare id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("the call got past its refusal and reached the provider's store or database: %v", r)
				}
			}()
			if _, err := (&Provider{}).forgetPlayer(tc.ctx, tc.args, 0); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one that begins %q", err, tc.want)
			}
		})
	}
	// The control: an id the player concept spells either way gets past the refusals
	// and stops at the store this provider does not have.
	for _, id := range []string{bareID, "v1:games:player:" + bareID} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("playerId %q was refused, though it is a player's id", id)
				}
			}()
			_, _ = (&Provider{}).forgetPlayer(asOwner("user-games-forget-args"), map[string]any{"playerId": id}, 0)
		}()
	}
}

func TestIsNoRowsRecognisesTheEmptyAnswerOfASingleRowRead(t *testing.T) {
	if !isNoRows(sql.ErrNoRows) || !isNoRows(fmt.Errorf("lookup: %w", sql.ErrNoRows)) {
		t.Error("sql.ErrNoRows, wrapped or not, is the empty answer")
	}
	if isNoRows(nil) || isNoRows(errors.New("the database is away")) {
		t.Error("anything else is not")
	}
}
