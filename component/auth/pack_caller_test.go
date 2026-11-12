package auth

import (
	"context"
	"testing"
)

// AN UNMARKED CONTEXT NAMES NO PACK, and a mark names exactly the pack that
// set it.
func TestAPackCallerIsNamedOnlyWhereItWasMarked(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"nil":             nil,
		"background":      context.Background(),
		"internal origin": ContextWithInternalOrigin(context.Background()),
	} {
		if got := PackCallerFromContext(ctx); got != "" {
			t.Errorf("%s names pack %q", name, got)
		}
	}
	marked := ContextWithPackCaller(context.Background(), "wholesale")
	if got := PackCallerFromContext(marked); got != "wholesale" {
		t.Fatalf("a marked context names %q, want wholesale", got)
	}
	// The mark is not an origin: it says which pack, never that the call is
	// trusted.
	if OriginFromContext(marked).IsInternal() {
		t.Fatal("a pack mark must not stamp internal origin")
	}
	if got := PackCallerFromContext(ContextWithPackCaller(nil, " wholesale ")); got != "wholesale" {
		t.Fatalf("a mark on a nil context names %q", got)
	}
}
