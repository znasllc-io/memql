package memql

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/grpclimits"
)

// largeAnswerService answers every query with one string of answerBytes.
type largeAnswerService struct {
	memqlv1.UnimplementedMemqlServiceServer
	answerBytes int
}

func (s *largeAnswerService) Stream(stream memqlv1.MemqlService_StreamServer) error {
	body := strings.Repeat("a", s.answerBytes)
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		exec := msg.GetExecuteQuery()
		if exec == nil {
			continue
		}
		return stream.Send(&memqlv1.MemqlServerMessage{Payload: &memqlv1.MemqlServerMessage_QueryResult{
			QueryResult: &memqlv1.QueryResultChunk{
				RequestId: exec.GetRequestId(),
				Result:    &memqlv1.Result{Data: []*structpb.Value{structpb.NewStringValue(body)}},
				Done:      true,
			},
		}})
	}
}

// The HTTP gateway (POST /memql/query) is a gRPC client of the same server the
// browser bridge is, and its receive limit has the same consequence: gRPC's
// default is 4 MiB, an answer over it is a read error on the stream, and the
// caller gets a 500 with the library's words instead of the answer the server
// was willing to send. It receives what the server will send
// (grpclimits.MaxMessageBytes), whether the caller of NewGateway gives it dial
// options of its own or not.
func TestGatewayCarriesAnAnswerLargerThanGRPCsDefault(t *testing.T) {
	// 6 MiB: over gRPC's 4 MiB default, well under the 32 MiB the server sends.
	const answerBytes = 6 << 20

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.MaxSendMsgSize(grpclimits.MaxMessageBytes), grpc.MaxRecvMsgSize(grpclimits.MaxMessageBytes))
	memqlv1.RegisterMemqlServiceServer(server, &largeAnswerService{answerBytes: answerBytes})
	go func() { _ = server.Serve(lis) }()
	defer server.Stop()

	for name, options := range map[string][]grpc.DialOption{
		"the dial options the app gives it": nil,
		"dial options of the caller's own":  {grpc.WithTransportCredentials(insecure.NewCredentials())},
	} {
		t.Run(name, func(t *testing.T) {
			gateway, err := NewGateway(GatewayOptions{
				Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
				Endpoint: lis.Addr().String(),
				DialOpts: options,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer gateway.Close()
			app := httptest.NewServer(gateway.Middleware()(http.NotFoundHandler()))
			defer app.Close()

			response, err := http.Post(app.URL+GatewayPath, "application/json", strings.NewReader(`{"query":"large"}`))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var body struct {
				Result struct {
					Data []string `json:"data"`
				} `json:"result"`
				Errors []map[string]any `json:"errors"`
				Error  string           `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("decoding the response (HTTP %d): %v", response.StatusCode, err)
			}
			if response.StatusCode != http.StatusOK || body.Error != "" {
				t.Fatalf("HTTP %d for an answer of %d bytes: %q", response.StatusCode, answerBytes, body.Error)
			}
			if len(body.Errors) != 0 || len(body.Result.Data) != 1 || len(body.Result.Data[0]) != answerBytes {
				t.Errorf("the answer did not arrive whole: %d data values, errors %v", len(body.Result.Data), body.Errors)
			}
		})
	}
}

// A caller of NewGateway who sets a receive limit of its own keeps it: the
// shared limit is the default, ahead of the caller's options, not over them.
func TestGatewayKeepsAReceiveLimitTheCallerSets(t *testing.T) {
	const answerBytes = 2 << 20

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	memqlv1.RegisterMemqlServiceServer(server, &largeAnswerService{answerBytes: answerBytes})
	go func() { _ = server.Serve(lis) }()
	defer server.Stop()

	gateway, err := NewGateway(GatewayOptions{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Endpoint: lis.Addr().String(),
		DialOpts: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1 << 20)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	app := httptest.NewServer(gateway.Middleware()(http.NotFoundHandler()))
	defer app.Close()

	response, err := http.Post(app.URL+GatewayPath, "application/json", strings.NewReader(`{"query":"large"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Errorf("HTTP %d for a 2 MiB answer under the caller's 1 MiB limit, want the refusal of that limit (500)", response.StatusCode)
	}
}
