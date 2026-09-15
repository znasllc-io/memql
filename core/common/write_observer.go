package common

import "context"

// write_observer.go -- the one seam through which a caller learns that a graph
// write happened beneath it (epic memql#5370, task memql#5372).
//
// A logic called directly -- not as a statement of a run -- is journaled as a
// run of its own only when it WRITES: a read-only call leaves no row behind.
// The runner cannot know ahead of time whether a body will write (a builtin, a
// nested logic, a branch not yet taken), so it installs an observer on the
// context and opens its run at the first write the engine reports. The engine
// reports every committed write through NotifyWrite, from its single write
// path.
//
// The journal's own writes run without the observer (ContextWithoutWriteObserver):
// a run row written because a write happened must not itself count as one.

// WriteObserver is told the concept and id of each committed graph write.
type WriteObserver func(concept, id string)

type writeObserverKey struct{}

// ContextWithWriteObserver installs obs for every write made under ctx. An
// observer ctx already carries is told too, after obs: a write inside a
// nested call is a write its callers made as well.
func ContextWithWriteObserver(ctx context.Context, obs WriteObserver) context.Context {
	if parent, ok := ctx.Value(writeObserverKey{}).(WriteObserver); ok && parent != nil && obs != nil {
		inner := obs
		obs = func(concept, id string) {
			inner(concept, id)
			parent(concept, id)
		}
	}
	return context.WithValue(ctx, writeObserverKey{}, obs)
}

// ContextWithoutWriteObserver masks any observer ctx carries.
func ContextWithoutWriteObserver(ctx context.Context) context.Context {
	return context.WithValue(ctx, writeObserverKey{}, WriteObserver(nil))
}

// NotifyWrite reports one committed write to the observer ctx carries, if
// any.
func NotifyWrite(ctx context.Context, concept, id string) {
	if obs, ok := ctx.Value(writeObserverKey{}).(WriteObserver); ok && obs != nil {
		obs(concept, id)
	}
}
