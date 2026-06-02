// Command deploy-gate-check is the in-cluster deploy-gate client (deployment-v2
// Phase 3, znasllc-io/memql#701/#712). It is the entrypoint of the
// acrmemql.azurecr.io/deploy-gate image referenced by the Argo Rollouts
// AnalysisTemplate (deploy/rollouts/analysis/deploy-gate.yaml). It performs the
// two convergence-safe, IN-CLUSTER checks the gate needs and exits non-zero on
// any failure so the Rollout auto-aborts -> auto-rolls-back:
//
//  1. readiness  -- GET <node>/readyz; asserts the critical schema is present
//     (#657). Proves a migration actually applied, credential-free.
//  2. auth-query -- opens MemqlService.Stream to the node's gRPC with an
//     Authorization: Bearer <service_account JWT> (#691), sends a
//     ClientHello + ExecuteQuery, and asserts the BFF ACCEPTED the
//     token and the engine answered. A gRPC Unauthenticated /
//     PermissionDenied (auth rejected) or Unavailable (BFF down),
//     or no server frame, is a FAIL; a QueryResult or a
//     query-level QueryError both prove the authenticated
//     BFF->engine path is live.
//
// The query string is incidental -- the gate proves the authenticated PATH, so
// even a query-level error counts as auth-success (it means the engine
// processed the request). Mirrors the in-process client in
// component/grpc/gateway.go (which dials the same surface insecurely in-cluster).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
)

func main() {
	addr := flag.String("addr", "bff:50051", "node gRPC address (host:port) to run the authenticated query against")
	jwt := flag.String("jwt", "", "class=service_account JWT bearer (#691). Falls back to $MEMQL_SVC_JWT.")
	query := flag.String("query", "count v1:cognition:space", "MemQL query to run (its validity is incidental; the gate proves the authenticated path)")
	readyzURL := flag.String("readyz-url", "", "override the /readyz URL (default: derive http://<addr-host>:8085/readyz)")
	fanAgent := flag.Bool("fan-agent", false, "reserved: the ExecuteQuery already fans BFF->cognition for cognition-concept reads; logged for parity with the AnalysisTemplate")
	timeout := flag.Duration("timeout", 60*time.Second, "overall deadline")
	useTLS := flag.Bool("tls", false, "dial gRPC over TLS (default: insecure, matching the in-cluster gateway)")
	flag.Parse()

	token := strings.TrimSpace(*jwt)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("MEMQL_SVC_JWT"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := run(ctx, gateConfig{
		addr:      *addr,
		token:     token,
		query:     *query,
		readyzURL: *readyzURL,
		fanAgent:  *fanAgent,
		useTLS:    *useTLS,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "deploy-gate-check: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("deploy-gate-check: PASS")
}

type gateConfig struct {
	addr      string
	token     string
	query     string
	readyzURL string
	fanAgent  bool
	useTLS    bool
}

func run(ctx context.Context, c gateConfig) error {
	if err := checkReadyz(ctx, c.addr, c.readyzURL); err != nil {
		return fmt.Errorf("readiness: %w", err)
	}
	fmt.Println("deploy-gate-check: 1/2 readiness OK (/readyz ready)")

	if c.token == "" {
		return errors.New("authenticated query: no JWT (set --jwt or $MEMQL_SVC_JWT)")
	}
	if err := checkAuthQuery(ctx, c); err != nil {
		return fmt.Errorf("authenticated query: %w", err)
	}
	fmt.Println("deploy-gate-check: 2/2 authenticated query OK (BFF accepted the service_account JWT; engine answered)")
	return nil
}

// readyzURLFor returns the readiness URL: the explicit override, else the
// node's plaintext http /readyz on :8085 derived from the gRPC addr's host.
func readyzURLFor(addr, override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	return fmt.Sprintf("http://%s:8085/readyz", host)
}

// checkReadyz GETs /readyz and asserts a 200 with a ready status (#657).
func checkReadyz(ctx context.Context, addr, override string) error {
	url := readyzURLFor(addr, override)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d (a pre-/readyz image 404s here)", url, resp.StatusCode)
	}
	if !strings.Contains(string(body), `"status":"ready"`) {
		return fmt.Errorf("GET %s: 200 but body not ready: %s", url, strings.TrimSpace(string(body)))
	}
	return nil
}

// checkAuthQuery opens MemqlService.Stream with the bearer, sends ClientHello +
// ExecuteQuery, and asserts the authenticated path is live. Mirrors
// component/grpc/gateway.go.
func checkAuthQuery(ctx context.Context, c gateConfig) error {
	// In-cluster the BFF gRPC surface is plaintext (the in-process gateway dials
	// it insecurely too). If a future surface requires TLS, add a credentials
	// bundle here; --tls is reserved for that.
	if c.useTLS {
		return errors.New("--tls not yet wired; the in-cluster gRPC surface is plaintext (see component/grpc/gateway.go)")
	}
	conn, err := grpc.NewClient(c.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.addr, err)
	}
	defer conn.Close()
	client := memqlv1.NewMemqlServiceClient(conn)

	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+c.token))
	stream, err := client.Stream(ctx)
	if err != nil {
		return classifyGRPC("open stream", err)
	}
	defer stream.CloseSend()

	if err := stream.Send(&memqlv1.MemqlClientMessage{
		MessageId: id.NewShortId(),
		Payload: &memqlv1.MemqlClientMessage_ClientHello{
			ClientHello: &memqlv1.ClientHello{ClientId: "deploy-gate", SdkName: "memql", SdkVersion: "deploy-gate"},
		},
	}); err != nil {
		return classifyGRPC("send ClientHello", err)
	}

	if c.fanAgent {
		fmt.Println("deploy-gate-check: --fan-agent: the query fans BFF->cognition for the cognition concept read")
	}

	requestID := id.NewShortId()
	if err := stream.Send(&memqlv1.MemqlClientMessage{
		MessageId: id.NewShortId(),
		Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
			ExecuteQuery: &memqlv1.ExecuteQueryMsg{RequestId: requestID, Query: c.query},
		},
	}); err != nil {
		return classifyGRPC("send ExecuteQuery", err)
	}

	for {
		msg, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			return errors.New("stream closed before any server frame (BFF rejected the token or the engine never answered)")
		}
		if recvErr != nil {
			return classifyGRPC("recv", recvErr)
		}
		switch p := msg.GetPayload().(type) {
		case *memqlv1.MemqlServerMessage_QueryResult:
			if p.QueryResult.GetRequestId() == requestID {
				return nil // engine answered -> auth + BFF + engine path proven
			}
		case *memqlv1.MemqlServerMessage_QueryError:
			if p.QueryError.GetRequestId() == requestID {
				// A query-level error still proves the authenticated path worked
				// (the engine processed our request). Auth failures surface as a
				// gRPC status, handled above.
				fmt.Printf("deploy-gate-check: engine returned a query-level error (path still proven): %v\n", p.QueryError.GetError())
				return nil
			}
		}
	}
}

// classifyGRPC turns a gRPC transport error into a clear gate failure, calling
// out the auth-rejection and backend-down cases the gate exists to catch.
func classifyGRPC(stage string, err error) error {
	switch status.Code(err) {
	case codes.Unauthenticated, codes.PermissionDenied:
		return fmt.Errorf("%s: AUTH REJECTED (%s) -- the service_account JWT was not accepted on this surface (#691)", stage, status.Code(err))
	case codes.Unavailable:
		return fmt.Errorf("%s: backend UNAVAILABLE (%s) -- the node is down/unready", stage, status.Code(err))
	default:
		return fmt.Errorf("%s: %w", stage, err)
	}
}
