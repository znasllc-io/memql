package reviewspack

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// fakeWriter records every write a capability makes. A refusal must happen
// BEFORE the writer is reached -- that ordering is the admission boundary the
// internal-origin allowlist entry for this package rests on -- so every
// refusal test asserts the writer was never called.
type fakeWriter struct {
	calls []writeCall
}

type writeCall struct {
	mutation string
	args     map[string]any
	actor    string // the user the write runs as
}

func (f *fakeWriter) Write(ctx context.Context, mutation string, args map[string]any) error {
	actor := ""
	if ac, ok := auth.AccessFromContext(ctx); ok {
		actor = ac.UserId
	}
	f.calls = append(f.calls, writeCall{mutation: mutation, args: args, actor: actor})
	return nil
}

func (f *fakeWriter) only(t *testing.T, mutation string) map[string]any {
	t.Helper()
	if len(f.calls) != 1 || f.calls[0].mutation != mutation {
		t.Fatalf("writes = %+v, want exactly one %s", f.calls, mutation)
	}
	return f.calls[0].args
}

func (f *fakeWriter) none(t *testing.T) {
	t.Helper()
	if len(f.calls) != 0 {
		t.Fatalf("a refused call still wrote: %+v", f.calls)
	}
}
