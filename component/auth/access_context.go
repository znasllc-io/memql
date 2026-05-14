package auth

import "context"

// AccessContext is the resolved authorization state for a single gRPC
// stream. It is built once when the middleware sees the first request
// on a connection (after the identity-issued JWT has been validated)
// and attached to every subsequent ctx on that stream.
//
//   UserId         v1:identity:user.id
//   PrimaryEmail   denormalized from the user record
//   Role           cluster-wide role (owner / admin / writer / reader)
//   IdentityId     which v1:identity:identity the caller authenticated with
//   PartitionACL   map partition-name -> per-partition Role
//
// PartitionACL is the hot path: the middleware reads it on every
// message. Cluster-wide owners bypass the ACL check entirely; everyone
// else must have a grant for envelope.partition.
type AccessContext struct {
	UserId       string
	PrimaryEmail string
	Role         Role
	IdentityId   string
	PartitionACL PartitionACL
}

// PartitionACL maps a partition name to the caller's role within it.
// The partition name matches v1:platform:partition.name (and
// v1:identity:partitionAccess.partition).
type PartitionACL map[string]Role

// AccessContextKey is the context key for AccessContext values.
const AccessContextKey contextKey = "accessContext"

// AccessFromContext extracts the AccessContext attached by the auth
// middleware. Returns nil/false if no context has been set -- callers
// should fail closed in that case.
func AccessFromContext(ctx context.Context) (*AccessContext, bool) {
	if ctx == nil {
		return nil, false
	}
	ac, ok := ctx.Value(AccessContextKey).(*AccessContext)
	return ac, ok && ac != nil
}

// ContextWithAccess returns a new context carrying the given
// AccessContext.
func ContextWithAccess(ctx context.Context, ac *AccessContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if ac == nil {
		return ctx
	}
	return context.WithValue(ctx, AccessContextKey, ac)
}

// RoleForPartition returns the caller's role in the named partition.
// Returns "" (and ok=false) if they have no grant.
func (ac *AccessContext) RoleForPartition(partition string) (Role, bool) {
	if ac == nil || ac.PartitionACL == nil {
		return "", false
	}
	role, ok := ac.PartitionACL[partition]
	return role, ok
}

// IsClusterOwner returns true when the caller is a cluster-wide owner.
// Cluster owners bypass the per-partition ACL.
func (ac *AccessContext) IsClusterOwner() bool {
	return ac != nil && ac.Role == RoleOwner
}

// CanReadPartition reports whether the caller may perform a read
// operation against the given partition. Cluster owners can always
// read; otherwise any grant for the partition suffices.
func CanReadPartition(ctx context.Context, partition string) bool {
	ac, ok := AccessFromContext(ctx)
	if !ok {
		return false
	}
	if ac.IsClusterOwner() {
		return true
	}
	_, hasGrant := ac.RoleForPartition(partition)
	return hasGrant
}

// CanWritePartition reports whether the caller may perform a write
// operation against the given partition. Requires owner / admin /
// writer role within that partition (reader cannot write). Cluster
// owners bypass the check.
func CanWritePartition(ctx context.Context, partition string) bool {
	ac, ok := AccessFromContext(ctx)
	if !ok {
		return false
	}
	if ac.IsClusterOwner() {
		return true
	}
	role, hasGrant := ac.RoleForPartition(partition)
	if !hasGrant {
		return false
	}
	switch role {
	case RoleOwner, RoleAdmin, RoleWriter:
		return true
	default:
		return false
	}
}

// AllowedPartitions returns the set of partition names the caller has
// any grant for. Cluster owners get nil (caller should interpret as
// "all partitions" via a separate branch).
func (ac *AccessContext) AllowedPartitions() []string {
	if ac == nil || ac.IsClusterOwner() {
		return nil
	}
	out := make([]string, 0, len(ac.PartitionACL))
	for p := range ac.PartitionACL {
		out = append(out, p)
	}
	return out
}
