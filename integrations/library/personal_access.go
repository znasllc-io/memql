package library

import "context"

// documentAuthority is supplied by the engine, which resolves current grants
// with organization scope. A missing authority port fails closed.
type documentAuthority interface {
	GlobalDataCapable(context.Context, string) bool
	PersonalAppCapable(context.Context, string) bool
}

// This only admits the action. Every document handler subsequently resolves its
// artifact and backing row under the unchanged caller's owner-scoped context.
func (i *Integration) canManageDocument(ctx context.Context, verb string) bool {
	return i != nil && i.authority != nil && (i.authority.GlobalDataCapable(ctx, verb) || i.authority.PersonalAppCapable(ctx, "files"))
}
