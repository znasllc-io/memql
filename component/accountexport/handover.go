package accountexport

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
)

// A hand-over is how a component that keeps account data in tables of its
// own puts that data in the file (spec B, "Outside the row table"). The
// component registers one from an init(); the run calls every registered one
// against its snapshot, in name order, so the file is the same whichever
// order the init()s ran in.

// Querier is the run's snapshot: a transaction at one instant. Every query a
// hand-over makes goes through it, so its lines come from the same instant as
// the rows.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// HandOverInput tells a hand-over which account, and which rows travel.
type HandOverInput struct {
	// Account is the account's bare id.
	Account string
	// RowIDs answers the bare ids of the carried rows of a kind. A hand-over
	// limits itself to these, so it writes nothing about a row the file does
	// not hold.
	RowIDs func(concept string) []string
}

// HandOverSink is the part of the writer a hand-over may write to.
type HandOverSink interface {
	// Parted opens a parted folder under tables/.
	Parted(dir string) LineSink
	// Line adds a line to a hand-over's single member (blocks.jsonl).
	Line(member string, v any) error
}

// LineSink takes JSON lines.
type LineSink interface {
	Write(v any) error
	Close() error
}

// HandOver writes one component's members.
type HandOver func(ctx context.Context, q Querier, in HandOverInput, out HandOverSink) error

var handOvers = struct {
	sync.Mutex
	m map[string]HandOver
}{m: map[string]HandOver{}}

// RegisterHandOver is called from an init(). A duplicate name panics: two
// components claiming one name would silently drop one's data.
func RegisterHandOver(name string, h HandOver) {
	if name == "" || h == nil {
		panic("accountexport: RegisterHandOver needs a name and a function")
	}
	handOvers.Lock()
	defer handOvers.Unlock()
	if _, dup := handOvers.m[name]; dup {
		panic(fmt.Sprintf("accountexport: hand-over %q registered twice", name))
	}
	handOvers.m[name] = h
}

// HandOvers answers the registered names, sorted.
func HandOvers() []string {
	handOvers.Lock()
	defer handOvers.Unlock()
	out := make([]string, 0, len(handOvers.m))
	for name := range handOvers.m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// RunHandOver runs one registered hand-over.
func RunHandOver(ctx context.Context, name string, q Querier, in HandOverInput, out HandOverSink) error {
	handOvers.Lock()
	h, ok := handOvers.m[name]
	handOvers.Unlock()
	if !ok {
		return fmt.Errorf("accountexport: no hand-over named %q", name)
	}
	if err := h(ctx, q, in, out); err != nil {
		return fmt.Errorf("accountexport: hand-over %q: %w", name, err)
	}
	return nil
}
