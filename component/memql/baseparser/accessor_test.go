package baseparser

import (
	"errors"
	"testing"
)

// TestClassifyAccessor covers the full classification table from
// the function's doc comment. The table-driven shape doubles as the
// living spec -- adding a new accessor kind means adding a row here.
func TestClassifyAccessor(t *testing.T) {
	cases := []struct {
		literal  string
		wantKind AccessorKind
		wantPath string
		wantErr  error
	}{
		// actor
		{"actor", KindActor, "", nil},
		{"actor.userId", KindActor, "userId", nil},
		{"actor.config.foo", KindActor, "config.foo", nil},
		// args
		{"args", KindArgs, "", nil},
		{"args.spaceId", KindArgs, "spaceId", nil},
		{"args.input.deeply.nested", KindArgs, "input.deeply.nested", nil},
		// ctx
		{"ctx", KindCtx, "", nil},
		{"ctx.spaceId", KindCtx, "spaceId", nil},
		// payload
		{"payload", KindPayload, "", nil},
		{"payload.userId", KindPayload, "userId", nil},
		// caller -- retired in #221, error path
		{"caller", KindNone, "", ErrCallerRetired},
		{"caller.userId", KindNone, "", ErrCallerRetired},
		{"caller.role", KindNone, "", ErrCallerRetired},
		// non-accessors -- fall through (no error)
		{"someFunction", KindNone, "", nil},
		{"v1:cognition:space", KindNone, "", nil},
		{"", KindNone, "", nil},
		// "actorRole" must NOT match KindActor -- the prefix needs
		// the dot, not just the bare-name substring.
		{"actorRole", KindNone, "", nil},
		{"argsRaw", KindNone, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.literal, func(t *testing.T) {
			kind, path, err := ClassifyAccessor(tc.literal)
			if kind != tc.wantKind {
				t.Errorf("kind = %v, want %v", kind, tc.wantKind)
			}
			if path != tc.wantPath {
				t.Errorf("path = %q, want %q", path, tc.wantPath)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
