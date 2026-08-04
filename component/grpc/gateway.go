package memql

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	GatewayPath            = "/memql/query"
	defaultGatewayTimeout  = 30 * time.Second
	defaultGatewayClientId = "memql-http-gateway"
)

// queryResponse is the internal representation of an HTTP query response.
type queryResponse struct {
	Result *memqlv1.Result       `json:"result,omitempty"`
	Errors []*memqlv1.QueryError `json:"errors,omitempty"`
}

// GatewayOptions configures the HTTP gateway that proxies /memql/query traffic to the gRPC service.
type GatewayOptions struct {
	Logger         *slog.Logger
	Endpoint       string
	DialOpts       []grpc.DialOption
	RequestTimeout time.Duration
}

// Gateway provides an http.Handler-compatible bridge that forwards requests to the MemQL gRPC stream.
type Gateway struct {
	logger         *slog.Logger
	client         memqlv1.MemqlServiceClient
	conn           *grpc.ClientConn
	requestTimeout time.Duration
}

// NewGateway constructs a Gateway instance that forwards HTTP requests to the MemQL gRPC server.
func NewGateway(opts GatewayOptions) (*Gateway, error) {
	dialOpts := opts.DialOpts
	if len(dialOpts) == 0 {
		dialOpts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}

	endpoint := normalizeEndpoint(opts.Endpoint)
	conn, err := grpc.NewClient(endpoint, dialOpts...)
	if err != nil {
		return nil, err
	}

	return &Gateway{
		logger:         opts.Logger,
		client:         memqlv1.NewMemqlServiceClient(conn),
		conn:           conn,
		requestTimeout: resolveGatewayTimeout(opts.RequestTimeout),
	}, nil
}

// InterceptedPaths returns the request paths this gateway serves from
// MIDDLEWARE, ahead of the mux (memql#3004).
//
// # Why this exists
//
// Middleware-mounted routes were a registration channel nothing modelled.
// Everything memql#2939's boot assertion knows about arrives either through
// `a.handleRoute` (recorded in `a.registeredRoutes`) or through
// `server.ContractRoutes()`. This gateway is neither: `Middleware()` is
// appended on EVERY binary including identity, and `shouldHandle` claims the
// request before it ever reaches the mux. So `POST /memql/query` was served on
// a verifier-less binary while appearing in no declaration and failing no
// check -- not because it was judged safe, but because nothing was looking.
//
// Declaring it here puts the channel INTO the model: unauthenticatedSurface-
// Routes folds this in, so the path must appear in PublicPaths() or
// HandlerAuthorizedPaths() or the identity binary refuses to boot. A future
// middleware-mounted route inherits that, which is the actual fix -- the
// specific path was never the problem.
//
// Verified rather than trusted: TestInterceptedPathsMatchShouldHandle drives
// the real predicate, so this list cannot drift from what is actually claimed.
func InterceptedPaths() []string {
	return []string{GatewayPath}
}

// Middleware returns an http middleware that intercepts /memql/query POSTs.
func (g *Gateway) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if g != nil && g.shouldHandle(r) {
				g.handleQuery(w, r)
				return
			}
			if next != nil {
				next.ServeHTTP(w, r)
			}
		})
	}
}

// Close releases resources associated with the gateway.
func (g *Gateway) Close() {
	if g == nil {
		return
	}
	if g.conn != nil {
		_ = g.conn.Close()
	}
}

func (g *Gateway) shouldHandle(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	if r.Method != http.MethodPost {
		return false
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}
	return strings.EqualFold(path, GatewayPath) || strings.HasSuffix(path, GatewayPath)
}

func (g *Gateway) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}

	req, err := decodeQueryRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.GetQuery()) == "" {
		writeJSONError(w, http.StatusBadRequest, "query cannot be empty")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), g.requestTimeout)
	defer cancel()

	ctx = g.attachMetadata(ctx, r.Header)
	stream, err := g.client.Stream(ctx)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	defer stream.CloseSend()

	if err := g.sendClientHello(stream); err != nil {
		writeGRPCError(w, err)
		return
	}

	requestId := id.NewShortId()
	execMsg := &memqlv1.MemqlClientMessage{
		MessageId: id.NewShortId(),
		Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
			ExecuteQuery: &memqlv1.ExecuteQueryMsg{
				RequestId: requestId,
				Query:     req.GetQuery(),
				Variables: req.GetVariables(),
			},
		},
	}
	if err := stream.Send(execMsg); err != nil {
		writeGRPCError(w, err)
		return
	}

	response := &queryResponse{}
	for {
		serverMsg, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			writeGRPCError(w, recvErr)
			return
		}

		switch payload := serverMsg.GetPayload().(type) {
		case *memqlv1.MemqlServerMessage_QueryResult:
			if payload.QueryResult.GetRequestId() != requestId {
				continue
			}
			response.Result = payload.QueryResult.GetResult()
			if payload.QueryResult.GetDone() {
				writeQueryResponse(w, http.StatusOK, response)
				return
			}
		case *memqlv1.MemqlServerMessage_QueryError:
			if payload.QueryError.GetRequestId() != requestId {
				continue
			}
			if payload.QueryError.GetError() != nil {
				response.Errors = append(response.Errors, payload.QueryError.GetError())
			}
			writeQueryResponse(w, http.StatusOK, response)
			return
		}
	}

	writeQueryResponse(w, http.StatusOK, response)
}

func (g *Gateway) sendClientHello(stream memqlv1.MemqlService_StreamClient) error {
	if stream == nil {
		return errors.New("stream unavailable")
	}
	return stream.Send(&memqlv1.MemqlClientMessage{
		MessageId: id.NewShortId(),
		Payload: &memqlv1.MemqlClientMessage_ClientHello{
			ClientHello: &memqlv1.ClientHello{
				ClientId:   defaultGatewayClientId,
				SdkName:    "memql",
				SdkVersion: "gateway",
			},
		},
	})
}

func (g *Gateway) attachMetadata(ctx context.Context, headers http.Header) context.Context {
	if headers == nil {
		return ctx
	}
	var pairs []string
	if auth := strings.TrimSpace(headers.Get("Authorization")); auth != "" {
		pairs = append(pairs, "authorization", auth)
	}
	if len(pairs) == 0 {
		return ctx
	}
	return metadata.NewOutgoingContext(ctx, metadata.Pairs(pairs...))
}

func decodeQueryRequest(r *http.Request) (*memqlv1.ExecuteQueryMsg, error) {
	if r == nil || r.Body == nil {
		return nil, errors.New("request body missing")
	}
	defer r.Body.Close()

	payload, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, errors.New("request body missing")
	}

	var req memqlv1.ExecuteQueryMsg
	if err := protojson.Unmarshal(payload, &req); err != nil {
		return nil, err
	}
	if req.Variables == nil {
		req.Variables = &structpb.Struct{Fields: make(map[string]*structpb.Value)}
	}
	return &req, nil
}

func resolveGatewayTimeout(requestTimeout time.Duration) time.Duration {
	if requestTimeout <= 0 {
		return defaultGatewayTimeout
	}
	return requestTimeout
}

func writeMethodNotAllowed(w http.ResponseWriter) {
	if w == nil {
		return
	}
	w.Header().Set("Allow", http.MethodPost)
	http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
}

func writeJSONError(w http.ResponseWriter, statusCode int, message string) {
	if w == nil {
		return
	}
	type errorPayload struct {
		Error string `json:"error"`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(errorPayload{Error: strings.TrimSpace(message)})
}

func writeQueryResponse(w http.ResponseWriter, statusCode int, resp *queryResponse) {
	if w == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if resp == nil {
		_, _ = w.Write([]byte("{}"))
		return
	}

	// Build a JSON-friendly response structure with omission semantics:
	// - Present fields with data = included
	// - Absent/empty/not-applicable fields = omitted entirely
	output := make(map[string]any)

	// Result: only include if present
	if resp.Result != nil {
		resultMap := make(map[string]any)

		// Bundle: only include if present (not nil)
		if resp.Result.Bundle != nil {
			bundleData, err := protojson.MarshalOptions{}.Marshal(resp.Result.Bundle)
			if err == nil {
				var bundleMap map[string]any
				if json.Unmarshal(bundleData, &bundleMap) == nil {
					// Omit empty arrays inside bundle
					if nodes, ok := bundleMap["nodes"].([]any); ok && len(nodes) == 0 {
						delete(bundleMap, "nodes")
					}
					if edges, ok := bundleMap["edges"].([]any); ok && len(edges) == 0 {
						delete(bundleMap, "edges")
					}
					if rootIds, ok := bundleMap["rootIds"].([]any); ok && len(rootIds) == 0 {
						delete(bundleMap, "rootIds")
					}
					resultMap["bundle"] = bundleMap
				}
			}
		}
		// If bundle is nil, omit it entirely (don't add to resultMap)

		// Data: only include if non-empty
		if len(resp.Result.Data) > 0 {
			dataList := make([]any, 0, len(resp.Result.Data))
			for _, v := range resp.Result.Data {
				if v != nil {
					dataList = append(dataList, v.AsInterface())
				}
			}
			resultMap["data"] = dataList
		}
		// If data is empty, omit it entirely (don't add to resultMap)

		output["result"] = resultMap
	}
	// If result is nil, omit it entirely (don't add to output)

	// Errors: only include if non-empty
	if len(resp.Errors) > 0 {
		errList := make([]map[string]any, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			if e != nil {
				errData, err := protojson.MarshalOptions{}.Marshal(e)
				if err == nil {
					var errMap map[string]any
					if json.Unmarshal(errData, &errMap) == nil {
						errList = append(errList, errMap)
					}
				}
			}
		}
		output["errors"] = errList
	}
	// If no errors, omit the errors field entirely

	data, err := json.Marshal(output)
	if err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(data)
}

func writeGRPCError(w http.ResponseWriter, err error) {
	if err == nil {
		writeJSONError(w, http.StatusInternalServerError, "memql gateway error")
		return
	}
	st := status.Convert(err)
	code := mapGRPCCodeToHTTP(st.Code())
	writeJSONError(w, code, st.Message())
}

func mapGRPCCodeToHTTP(code codes.Code) int {
	switch code {
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.NotFound:
		return http.StatusNotFound
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

func normalizeEndpoint(address string) string {
	trimmed := strings.TrimSpace(address)
	if trimmed == "" {
		return "127.0.0.1" + defaultAddress
	}
	if strings.HasPrefix(trimmed, ":") {
		return "127.0.0.1" + trimmed
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "localhost:") {
		return "127.0.0.1" + trimmed[len("localhost"):]
	}
	return trimmed
}
