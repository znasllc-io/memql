package memql

import (
	"context"
	"github.com/znasllc-io/memql/component/auth"
)

// DSL/API/Nexus intake needs the same post-RotateAuth credential ceiling as
// Ask's AI forward. The stream-open claims can name an older badge/operator.
func (s *streamSession) bindWorkIntakeAuthority(ctx context.Context) context.Context {
	principal, err := s.forwardedPrincipal()
	return auth.ContextWithExecutionAuthority(ctx, principal.Authority, err)
}
