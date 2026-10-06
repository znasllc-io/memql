package memql

import (
	"context"
	"errors"
	"testing"
)

func TestWalkQueryPagesRequiresProgressAndExhaustion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cursors []string
		budget  int
		wantErr bool
		visits  int
	}{
		{"continues empty refined pages", []string{"page2", "page3", ""}, 10, false, 3},
		{"same cursor", []string{"page2", "page2"}, 10, true, 1},
		{"cycle", []string{"page2", "page3", "page2"}, 10, true, 2},
		{"budget", []string{"page2", "page3"}, 2, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, visits := 0, 0
			ctx := ContextWithCursor(context.Background(), "unrelated inherited cursor")
			err := WalkQueryPages(ctx, func(ctx context.Context, query string) (*ExecuteResult, error) {
				want := ""
				if calls > 0 {
					want = tc.cursors[calls-1]
				}
				if CursorFromContext(ctx) != want {
					t.Fatalf("cursor %q want %q", CursorFromContext(ctx), want)
				}
				if query != "query stable()" {
					t.Fatal("query changed across pages")
				}
				next := tc.cursors[calls]
				calls++
				return &ExecuteResult{Meta: &ResultMeta{Cursor: next, HasMore: next != ""}}, nil
			}, "query stable()", tc.budget, func(*ExecuteResult) error { visits++; return nil })
			if (err != nil) != tc.wantErr || visits != tc.visits {
				t.Fatalf("err=%v visits=%d", err, visits)
			}
		})
	}
}
func TestWalkQueryPagesPropagatesReadVisitorAndCancellationFailures(t *testing.T) {
	sentinel := errors.New("read or visitor failed")
	visit := func(*ExecuteResult) error { return nil }
	execute := func(context.Context, string) (*ExecuteResult, error) { return nil, sentinel }
	if err := WalkQueryPages(context.Background(), execute, "query x()", 10, visit); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	execute = func(context.Context, string) (*ExecuteResult, error) { return &ExecuteResult{}, nil }
	if err := WalkQueryPages(context.Background(), execute, "query x()", 10, func(*ExecuteResult) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WalkQueryPages(ctx, execute, "query x()", 10, visit); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	execute = func(context.Context, string) (*ExecuteResult, error) {
		return &ExecuteResult{Meta: &ResultMeta{HasMore: true}}, nil
	}
	if err := WalkQueryPages(context.Background(), execute, "query x()", 10, visit); err == nil {
		t.Fatal("missing continuation accepted")
	}
}

func TestEffectiveWindowMatchesTheEvaluatorsActualCeiling(t *testing.T) {
	e := &MemQLEngine{config: engineConfig{MaxResults: 500, MaxWindow: 5000}}
	requested := 1000000
	if got := e.effectiveWindow(&requested, 50); got != 500 {
		t.Fatalf("window %d hides a full clamped page", got)
	}
}
