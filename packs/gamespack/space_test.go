package gamespack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// space_test.go -- the part of the embedder space that needs no database.

// A cluster with no embedder bound is the ordinary state of a fresh install, and
// says nothing. A binding that could not be READ is different: the space is not
// known to be empty, only unavailable to this call, and an operator should be
// able to find out why. Either way the answer is "no table" and no error, so a
// write never fails on it.
func TestClusterSpaceTableTellsNothingBoundFromAnUnreadableBinding(t *testing.T) {
	// The binding reader is process-wide; put it back as it was found.
	t.Cleanup(func() { memql.SetEmbedderBindingReader(nil) })

	for _, tc := range []struct {
		name      string
		err       error
		wantWarns int
	}{
		{"a binding that cannot be read", errors.New("the database is away"), 1},
		{"nothing bound", memql.ErrNoEmbedderBound, 0},
		{"nothing bound, wrapped by the reader", fmt.Errorf("reading the binding: %w", memql.ErrNoEmbedderBound), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memql.SetEmbedderBindingReader(func(context.Context) (memql.EmbedderBinding, error) {
				return memql.EmbedderBinding{}, tc.err
			})
			logged := &bytes.Buffer{}
			space := clusterSpace{log: slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug}))}

			table, err := space.table(context.Background())
			if table != "" || err != nil {
				t.Fatalf("table = %q, err = %v, want no table and no error", table, err)
			}
			out := logged.String()
			if got := strings.Count(out, "level=WARN"); got != tc.wantWarns {
				t.Fatalf("logged %d warning(s), want %d: %q", got, tc.wantWarns, out)
			}
			if tc.wantWarns == 0 && out != "" {
				t.Fatalf("an unbound cluster must be silent at every level, logged %q", out)
			}
			if tc.wantWarns > 0 && !strings.Contains(out, tc.err.Error()) {
				t.Fatalf("the warning does not carry the reason: %q", out)
			}
		})
	}
}

// A clusterSpace built without a logger (a test, a tool) falls back to the
// default one rather than panicking when it has something to say.
func TestClusterSpaceWithNoLoggerFallsBackToTheDefaultOne(t *testing.T) {
	if (clusterSpace{}).logger() != slog.Default() {
		t.Fatal("a clusterSpace with no logger must fall back to slog.Default()")
	}
}

// The names a vector table is given, and names that are not one. Every statement
// of the pack that puts a table into its text (recall's join, forget's deletes)
// takes a name that isVectorTable accepts and nothing else, and its tests draw
// their names from these two lists.
var (
	vectorTables = []string{"node_vectors", "node_vectors_1536", "node_vectors_8_0011223344556677", "node_vectors_16_e4dc6287853f59b6"}

	notVectorTables = []string{
		`node_vectors; DROP TABLE "MemoryNodes"`,
		`node_vectors nv ON TRUE; DELETE FROM "MemoryNodes" --`,
		`node_vectors_8_0011223344556677 nv ON TRUE --`,
		"node_vectors_8_0011223344556677\n",
		"node_vectors_8_0011223344556677 ",
		" node_vectors",
		"node_vectors_8_001122334455667",   // fifteen hex digits
		"node_vectors_8_00112233445566778", // seventeen
		"node_vectors_8_0011223344556677Z",
		"node_vectors_8_ABCDEF0123456789", // upper case
		"node_vectors_",
		"node_vectors_x",
		"NODE_VECTORS",
		"public.node_vectors",
		`"node_vectors"`,
		"MemoryNodes",
		"node_vectors_8_0011223344556677_9",
		"",
	}
)

func TestIsVectorTableTakesOnlyTheNameOfAVectorTable(t *testing.T) {
	for _, name := range vectorTables {
		if !isVectorTable(name) {
			t.Errorf("%q is the name of a vector table, and was refused", name)
		}
	}
	for _, name := range notVectorTables {
		if isVectorTable(name) {
			t.Errorf("%q is no vector table's name, and was accepted", name)
		}
	}
}
