package memql

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
)

// Query-triggered model calls and durable intake need the same post-RotateAuth
// credential ceiling as Ask's AI forward. A resolved AccessContext alone cannot
// carry a badge's class, ceiling or expiry across the next model-dispatch hop.
func (s *streamSession) bindQueryAuthority(ctx context.Context) context.Context {
	principal, err := s.forwardedPrincipal()
	access, ok := auth.AccessFromContext(ctx)
	if err == nil && (!ok || access == nil || access.UserId != principal.Authority.Subject || access.Role != principal.Authority.Role) {
		// A concurrent rotation may have changed the session since the query
		// resolved its actor. Refuse forwarding instead of combining grants.
		err = fmt.Errorf("query actor changed while binding execution authority")
	}
	if err == nil {
		ctx = auth.ContextWithForwardedAuthority(ctx, principal.Authority)
	} else {
		// Do not leave an older assertion inherited from the stream usable.
		ctx = auth.ContextWithForwardedAuthority(ctx, auth.ForwardedAuthority{})
	}
	return auth.ContextWithExecutionAuthority(ctx, principal.Authority, err)
}
