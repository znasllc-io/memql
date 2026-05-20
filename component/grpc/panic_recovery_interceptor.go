package memql

import (
	"fmt"
	"log/slog"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NewPanicRecoveryStreamInterceptor wraps `base` (the rest of the
// interceptor chain) in a `defer recover()`. A panic anywhere downstream
// (any sub-interceptor, any handler, any engine call) is caught, the full
// panic value + stack trace are logged against an error id, and the
// client surface is a `codes.Internal` with the safe message
// "internal server error [ERR-xxxxxx]" — never the raw panic value
// (which can include sensitive locals from closures).
//
// Place this interceptor at the outermost layer of the chain so it sees
// panics from every inner interceptor + every handler.
func NewPanicRecoveryStreamInterceptor(base grpc.StreamServerInterceptor, logger *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				eid := generateErrorId()
				if logger != nil {
					logger.Error("grpc stream panic recovered",
						"errorId", eid,
						"panic", fmt.Sprintf("%v", rec),
						"method", info.FullMethod,
						"stack", string(debug.Stack()),
					)
				}
				err = status.Errorf(codes.Internal, "internal server error [%s]", eid)
			}
		}()
		if base == nil {
			return handler(srv, ss)
		}
		return base(srv, ss, info, handler)
	}
}
