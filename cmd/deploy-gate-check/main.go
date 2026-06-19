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
// The gate proves the authenticated PATH, so even a query-level (execution)
// error counts as auth-success (it means the engine processed the request).
// The query string must still be a WELL-FORMED MemQL query, though: a parse
// failure makes the engine log a `memql query execution failed` ERROR on the
// node on every gate run, which is pure log noise (znasllc-io/memql#1130). The
// default is queryActiveSpaceIds (a real cognition-concept read) for that
// reason. Mirrors the in-process client in component/grpc/gateway.go (which
// dials the same surface insecurely in-cluster).
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

// defaultGateQuery is the MemQL query the gate runs by default. It MUST be a
// well-formed query: the gate proves the authenticated BFF->engine path, but a
// parse error makes the engine log a `memql query execution failed` ERROR on
// the node on every gate run (znasllc-io/memql#1130). queryActiveSpaceIds is the
// id-only projection of active v1:cognition:space rows (dsl/cognition/queries.memql)
// -- a real cognition-concept read that also exercises the BFF->cognition fan.
// TestDefaultGateQueryParses pins that this stays parseable.
const defaultGateQuery = "queryActiveSpaceIds({})"

func main() {
	addr := flag.String("addr", "bff:50051", "node gRPC address (host:port) to run the authenticated query against")
	jwt := flag.String("jwt", "", "class=service_account JWT bearer (#691). Falls back to $MEMQL_SVC_JWT.")
	query := flag.String("query", defaultGateQuery, "MemQL query to run. Must be a well-formed query: the gate proves the authenticated BFF->engine path, and a parse error would emit a spurious `memql query execution failed` ERROR on the node every gate run (znasllc-io/memql#1130). queryActiveSpaceIds is the id-only projection of active v1:cognition:space rows (dsl/cognition/queries.memql) -- a real cognition-concept read that also exercises the BFF->cognition fan.")
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
	if err := checkAuthQueryWithRetry(ctx, c); err != nil {
		return fmt.Errorf("authenticated query: %w", err)
	}
	fmt.Println("deploy-gate-check: 2/2 authenticated query OK (BFF accepted the service_account JWT; engine answered)")
	return nil
}

// errTransientGate marks a gate failure that is worth retrying within a single
// gate run -- a freshly-promoted node whose verifier hasn't yet fetched the
// signing key from the identity JWKS (so a valid service_account JWT is briefly
// rejected as Unauthenticated), or a node that is up but not yet serving
// (Unavailable). A persistent rejection still fails the gate after the retries
// are spent; this only smooths the JWKS/startup warm-up race (#1782).
var errTransientGate = errors.New("transient gate error")

const (
	authRetryAttempts = 6               // ~30s total tolerance for JWKS warm-up
	authRetryDelay    = 5 * time.Second // verifier JWKS refresh-on-unknown-kid is sub-second; 5s spacing is generous
)

// checkAuthQueryWithRetry runs checkAuthQuery, retrying ONLY transient
// (Unauthenticated/Unavailable) failures with bounded backoff so a JWKS/startup
// warm-up race on a fresh color does not abort an otherwise-healthy promotion
// (#1782). Real auth rejections (PermissionDenied) and any other error fail
// immediately.
func checkAuthQueryWithRetry(ctx context.Context, c gateConfig) error {
	var lastErr error
	for attempt := 1; attempt <= authRetryAttempts; attempt++ {
		err := checkAuthQuery(ctx, c)
		if err == nil {
			if attempt > 1 {
				fmt.Printf("deploy-gate-check: authenticated query recovered on attempt %d (warm-up race)\n", attempt)
			}
			return nil
		}
		if !errors.Is(err, errTransientGate) {
			return err
		}
		lastErr = err
		if attempt == authRetryAttempts {
			break
		}
		fmt.Printf("deploy-gate-check: transient auth failure (attempt %d/%d), retrying in %s: %v\n",
			attempt, authRetryAttempts, authRetryDelay, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(authRetryDelay):
		}
	}
	return lastErr
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
	case codes.Unauthenticated:
		// Transient during a promotion: the freshly-promoted color's verifier
		// may not have fetched the token's signing key from the identity JWKS
		// yet. Marked retryable (#1782); a persistent rejection still fails the
		// gate once the in-run retries are spent.
		return fmt.Errorf("%s: AUTH REJECTED (%s) -- service_account JWT not accepted on this surface yet (#691/#1782): %w", stage, status.Code(err), errTransientGate)
	case codes.PermissionDenied:
		// Authorization, not a warm-up race -- a real deny. Do not retry.
		return fmt.Errorf("%s: AUTH REJECTED (%s) -- the service_account JWT was not accepted on this surface (#691)", stage, status.Code(err))
	case codes.Unavailable:
		return fmt.Errorf("%s: backend UNAVAILABLE (%s) -- the node is down/unready: %w", stage, status.Code(err), errTransientGate)
	default:
		return fmt.Errorf("%s: %w", stage, err)
	}
}
