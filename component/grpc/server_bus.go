package memql

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/bus"
	busv1 "github.com/znasllc-io/memql/component/bus/gen"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// SetWiring configures the bus wiring for channel-based communication.
// When set, the server can route requests through the bus instead of
// calling engine methods directly.
func (s *Server) SetWiring(w *bus.Wiring) {
	s.wiring = w
}

// executeQueryViaBus sends an EngineExecuteRequest through the bus channel
// and awaits the response. Falls back to direct engine call if wiring is
// not configured.
func (s *service) executeQueryViaBus(ctx context.Context, query string, clientId string) (*memqlv1.Result, error) {
	if s.wiring == nil {
		return s.executeQuery(ctx, query, clientId)
	}

	// Build the bus request
	msg := bus.NewMessage()
	msg.Payload = &busv1.InternalMessage_EngineExecute{
		EngineExecute: &busv1.EngineExecuteRequest{
			Query: query,
		},
	}
	req := bus.NewRequest(msg)

	// Send to engine via channel
	if err := s.wiring.EngineRequests.SendBlocking(ctx, req); err != nil {
		eid := generateErrorId()
		if s.logger != nil {
			s.logger.Error("engine channel unavailable", "errorId", eid, "error", err, "clientId", clientId)
		}
		return nil, status.Errorf(codes.Unavailable, "engine channel unavailable [%s]", eid)
	}

	// Await response with timeout
	resp, err := req.Await(ctx, 30*time.Second)
	if err != nil {
		eid := generateErrorId()
		if s.logger != nil {
			s.logger.Error("engine request timed out", "errorId", eid, "error", err, "clientId", clientId)
		}
		return nil, status.Errorf(codes.DeadlineExceeded, "engine request timed out [%s]", eid)
	}

	// Extract engine response
	execResp := resp.GetEngineExecuteResponse()
	if execResp == nil {
		return nil, status.Error(codes.Internal, "unexpected response type from engine")
	}

	if !execResp.Success {
		// execResp.Error is the engine-side error message; it is already
		// shaped for callers and contains no DB internals (engine
		// wraps DB errors before publishing on the bus). Pass through.
		return nil, status.Error(codes.Internal, execResp.Error)
	}

	// Convert the result Value back to a Result proto
	result, err := convertBusResultToProto(execResp.Result, execResp.TookMs, clientId)
	if err != nil {
		eid := generateErrorId()
		if s.logger != nil {
			s.logger.Error("convert engine result failed", "errorId", eid, "error", err, "clientId", clientId)
		}
		return nil, status.Errorf(codes.Internal, "failed to convert engine result [%s]", eid)
	}

	return result, nil
}

// convertBusResultToProto converts the structpb.Value result from the bus
// response into the gRPC Result proto format.
func convertBusResultToProto(resultValue *structpb.Value, tookMs int64, clientId string) (*memqlv1.Result, error) {
	result := &memqlv1.Result{
		Meta: &memqlv1.ResultMeta{
			TookMs:   tookMs,
			ClientId: clientId,
		},
	}

	if resultValue == nil {
		return result, nil
	}

	// The bus path serializes ExecuteResult through JSON → structpb.Value.
	// We pass it through as data. The direct executeQuery path handles
	// full-fidelity GraphBundle conversion.
	result.Data = []*structpb.Value{resultValue}
	return result, nil
}
