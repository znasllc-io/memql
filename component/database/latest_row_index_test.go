package database

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
)

// THE LATEST-ROW INDEX'S POLICY, WITHOUT A DATABASE (memql#5252).
//
// decideLatestRowIndex is the whole of what the migration decides; the
// db-gated suite (latest_row_index_db_test.go) proves the catalog reading and
// the builds against real TimescaleDB and plain layouts. These pin the policy
// itself, so a change to what counts as "complete" or "equivalent" is a
// visible edit to a table here rather than a behaviour nobody decided.

// latestRowShaped is a complete index of the latest-row shape.
func latestRowShaped(name string) indexFacts {
	return indexFacts{
		Name: name, Method: "btree", Valid: true, Ready: true, Live: true,
		KeyColumns:    []string{"concept", "id", "createdAt"},
		KeyDescending: []bool{false, false, true},
		KeyPlain:      []bool{true, true, true},
	}
}

func with(f indexFacts, change func(*indexFacts)) indexFacts {
	f.KeyColumns = append([]string(nil), f.KeyColumns...)
	f.KeyDescending = append([]bool(nil), f.KeyDescending...)
	f.KeyPlain = append([]bool(nil), f.KeyPlain...)
	change(&f)
	return f
}

func TestTheLatestRowShapeIsExactlyTheKeyTheReadNeeds(t *testing.T) {
	base := latestRowShaped("idx")
	cases := []struct {
		name string
		f    indexFacts
		want string // substring of the problem; "" means it answers the read
	}{
		{"the canonical key", base, ""},
		{"a hash index", with(base, func(f *indexFacts) { f.Method = "hash" }), "hash"},
		{"a partial index", with(base, func(f *indexFacts) { f.Partial = true }), "partial"},
		{"an expression index", with(base, func(f *indexFacts) { f.Expression = true }), "expression"},
		{"createdAt ascending", with(base, func(f *indexFacts) { f.KeyDescending[2] = false }), "its key is"},
		{"concept descending", with(base, func(f *indexFacts) { f.KeyDescending[0] = true }), "its key is"},
		{"id before concept", with(base, func(f *indexFacts) { f.KeyColumns[0], f.KeyColumns[1] = "id", "concept" }), "its key is"},
		{"a fourth key column", with(base, func(f *indexFacts) {
			f.KeyColumns = append(f.KeyColumns, "payload")
			f.KeyDescending = append(f.KeyDescending, false)
			f.KeyPlain = append(f.KeyPlain, true)
		}), "its key is"},
		{"only (id, createdAt DESC)", with(base, func(f *indexFacts) {
			f.KeyColumns, f.KeyDescending, f.KeyPlain = []string{"id", "createdAt"}, []bool{false, true}, []bool{true, true}
		}), "its key is"},
		{"concept COLLATE \"C\" or a pattern opclass", with(base, func(f *indexFacts) { f.KeyPlain[0] = false }), "operator class or collation"},
		{"arrays of different lengths", with(base, func(f *indexFacts) { f.KeyPlain = f.KeyPlain[:2] }), "its key is"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.f.shapeProblem()
			switch {
			case c.want == "" && got != "":
				t.Fatalf("shapeProblem = %q, want the index to answer the read", got)
			case c.want != "" && !strings.Contains(got, c.want):
				t.Fatalf("shapeProblem = %q, want it to mention %q", got, c.want)
			}
		})
	}
}

func TestOnlyAValidReadyLiveIndexIsUsable(t *testing.T) {
	base := latestRowShaped("idx")
	for _, c := range []struct {
		name string
		f    indexFacts
		want string
	}{
		{"valid, ready, live", base, ""},
		{"invalid (interrupted or running build)", with(base, func(f *indexFacts) { f.Valid = false }), "invalid"},
		{"not ready", with(base, func(f *indexFacts) { f.Ready = false }), "not ready"},
		{"being dropped", with(base, func(f *indexFacts) { f.Live = false }), "being dropped"},
	} {
		got := c.f.usableProblem()
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: usableProblem = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDecideLatestRowIndex(t *testing.T) {
	const canon = "canon_idx"
	complete := latestRowShaped(canon)
	invalid := with(complete, func(f *indexFacts) { f.Valid = false })
	squatter := with(complete, func(f *indexFacts) { f.Partial = true })
	operator := latestRowShaped("operator_idx")
	unrelated := with(latestRowShaped("memory_nodes_id_created_at_desc_idx"), func(f *indexFacts) {
		f.KeyColumns, f.KeyDescending, f.KeyPlain = []string{"id", "createdAt"}, []bool{false, true}, []bool{true, true}
	})

	chunk := func(name string, idx ...indexFacts) chunkFacts { return chunkFacts{Name: name, Indexes: idx} }
	coveredChunk := func(name string) chunkFacts { return chunk(name, latestRowShaped(name+"_"+canon)) }

	cases := []struct {
		name        string
		st          latestRowIndexState
		outcome     latestRowIndexOutcome
		covering    string
		drop        bool
		build       bool
		incomplete  string // substring expected among the reasons, when set
		exempt      int
		equivalents int
	}{
		{
			name:    "plain table, complete canonical: nothing to do",
			st:      latestRowIndexState{Root: []indexFacts{unrelated, complete}},
			outcome: latestRowIndexValid, covering: canon,
		},
		{
			name:    "plain table, nothing there: build",
			st:      latestRowIndexState{Root: []indexFacts{unrelated}},
			outcome: latestRowIndexBuilt, build: true, incomplete: "no index named",
		},
		{
			name:    "invalid canonical: drop it and build again",
			st:      latestRowIndexState{Root: []indexFacts{invalid}},
			outcome: latestRowIndexRebuilt, drop: true, build: true, incomplete: "invalid",
		},
		{
			name:    "a partial index squatting on the name: replaced",
			st:      latestRowIndexState{Root: []indexFacts{squatter}},
			outcome: latestRowIndexRebuilt, drop: true, build: true, incomplete: "partial",
		},
		{
			name:    "an operator's equivalent index and no canonical: accepted, no duplicate",
			st:      latestRowIndexState{Root: []indexFacts{operator}},
			outcome: latestRowIndexEquivalent, covering: "operator_idx", equivalents: 1, incomplete: "no index named",
		},
		{
			name:    "an operator's equivalent index beside an invalid canonical: the canonical is dropped, nothing built",
			st:      latestRowIndexState{Root: []indexFacts{invalid, operator}},
			outcome: latestRowIndexEquivalent, covering: "operator_idx", drop: true, equivalents: 1,
		},
		{
			name:    "an operator's equivalent index beside a squatter: the squatter is dropped",
			st:      latestRowIndexState{Root: []indexFacts{squatter, operator}},
			outcome: latestRowIndexEquivalent, covering: "operator_idx", drop: true, equivalents: 1,
		},
		{
			name:    "an operator's INVALID equivalent is no equivalent at all: build",
			st:      latestRowIndexState{Root: []indexFacts{with(operator, func(f *indexFacts) { f.Valid = false })}},
			outcome: latestRowIndexBuilt, build: true,
		},
		{
			name:    "complete canonical beside an operator's equivalent: valid, the other reported as redundant",
			st:      latestRowIndexState{Root: []indexFacts{complete, operator}},
			outcome: latestRowIndexValid, covering: canon, equivalents: 1,
		},
		{
			name: "hypertable, every chunk covered: valid",
			st: latestRowIndexState{Hypertable: true, Root: []indexFacts{complete},
				Chunks: []chunkFacts{coveredChunk("c1"), coveredChunk("c2")}},
			outcome: latestRowIndexValid, covering: canon,
		},
		{
			name: "hypertable, a valid root and ONE chunk without its index: rebuilt",
			st: latestRowIndexState{Hypertable: true, Root: []indexFacts{complete},
				Chunks: []chunkFacts{coveredChunk("c1"), chunk("c2", unrelated)}},
			outcome: latestRowIndexRebuilt, drop: true, build: true, incomplete: "chunk c2",
		},
		{
			name: "hypertable, a chunk whose only latest-row index is invalid: rebuilt",
			st: latestRowIndexState{Hypertable: true, Root: []indexFacts{complete},
				Chunks: []chunkFacts{coveredChunk("c1"), chunk("c2", with(latestRowShaped("c2_"+canon), func(f *indexFacts) { f.Valid = false }))}},
			outcome: latestRowIndexRebuilt, drop: true, build: true, incomplete: "chunk c2",
		},
		{
			name: "hypertable, a COMPRESSED chunk without the index: valid, and reported",
			st: latestRowIndexState{Hypertable: true, Root: []indexFacts{complete},
				Chunks: []chunkFacts{coveredChunk("c1"), {Name: "c2", Exempt: "compressed"}}},
			outcome: latestRowIndexValid, covering: canon, exempt: 1,
		},
		{
			// Coverage is judged by shape: 2.29 records no link from a chunk's
			// index to the hypertable index it was made for, and the planner
			// uses whichever latest-row index a chunk carries.
			name: "hypertable, a chunk covered only by the operator's chunk index: covered",
			st: latestRowIndexState{Hypertable: true, Root: []indexFacts{complete, operator},
				Chunks: []chunkFacts{coveredChunk("c1"), chunk("c2", latestRowShaped("c2_operator_idx"))}},
			outcome: latestRowIndexValid, covering: canon, equivalents: 1,
		},
		{
			name: "hypertable, an uncovered chunk and only an operator's index: the canonical is built",
			st: latestRowIndexState{Hypertable: true, Root: []indexFacts{operator},
				Chunks: []chunkFacts{chunk("c1")}},
			outcome: latestRowIndexBuilt, build: true, equivalents: 1, incomplete: "chunk c1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decideLatestRowIndex(c.st, canon)
			if d.Outcome != c.outcome {
				t.Fatalf("outcome = %s, want %s (incomplete: %v)", d.Outcome, c.outcome, d.Incomplete)
			}
			if d.Covering != c.covering {
				t.Errorf("covering = %q, want %q", d.Covering, c.covering)
			}
			if (d.Drop != nil) != c.drop {
				t.Errorf("drop = %v, want %v", d.Drop != nil, c.drop)
			}
			if d.Drop != nil && d.Drop.Name != canon {
				t.Errorf("the decision would drop %q: only the canonical name is this migration's to drop", d.Drop.Name)
			}
			if d.Build != c.build {
				t.Errorf("build = %v, want %v", d.Build, c.build)
			}
			if c.incomplete != "" && !strings.Contains(strings.Join(d.Incomplete, "; "), c.incomplete) {
				t.Errorf("incomplete = %v, want a reason mentioning %q", d.Incomplete, c.incomplete)
			}
			if c.outcome == latestRowIndexValid && len(d.Incomplete) != 0 {
				t.Errorf("a valid index carries reasons it is incomplete: %v", d.Incomplete)
			}
			if len(d.ExemptWithoutIndex) != c.exempt {
				t.Errorf("exempt = %v, want %d", d.ExemptWithoutIndex, c.exempt)
			}
			if len(d.Equivalents) != c.equivalents {
				t.Errorf("equivalents = %v, want %d", d.Equivalents, c.equivalents)
			}
		})
	}
}

func TestOnlyTheDriversReadDeadlineCountsAsAnOrphanedStatement(t *testing.T) {
	readDeadline := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"pgdriver's read deadline", readDeadline, true},
		{"the read deadline, wrapped", fmt.Errorf("exec: %w", readDeadline), true},
		{"a context that ended before the statement was sent", context.DeadlineExceeded, false},
		{"a cancelled context", context.Canceled, false},
		{"a server error", errors.New("ERROR: canceling statement due to lock timeout (SQLSTATE=55P03)"), false},
		{"no error", nil, false},
	} {
		if got := isClientReadDeadline(c.err); got != c.want {
			t.Errorf("%s: isClientReadDeadline = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTheBuildLockIsPerTableAndStable(t *testing.T) {
	if latestRowIndexLockKey(memoryNodesTableName) != latestRowIndexLockKey(memoryNodesTableName) {
		t.Fatal("the build lock key is not stable: two nodes migrating the same table would not contend")
	}
	if latestRowIndexLockKey(memoryNodesTableName) == latestRowIndexLockKey(secretMemoryNodesTableName) {
		t.Fatal("two tables share a build lock key")
	}
	if latestRowIndexLockKey(memoryNodesTableName) == timescaleExtensionAdvisoryLockKey {
		t.Fatal("the build lock key collides with the extension-create lock")
	}
}

func TestTheBuildStatementCarriesTheInspectedKey(t *testing.T) {
	if got, want := latestRowIndexColumnsSQL(), `"concept", "id", "createdAt" DESC`; got != want {
		t.Fatalf("the build's key is %s, want %s -- the key the inspection accepts", got, want)
	}
}
