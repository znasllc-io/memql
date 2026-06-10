// Command rolling-drain sends a single operator maintenance/drain trigger
// (memql#1270) to ONE target node and prints the typed result as JSON. It
// is the per-node primitive the ordered-rollout driver
// (scripts/cluster/rolling-drain.sh) calls once per replica.
//
// It funnels into the SAME graceful-drain mechanism the deploy SIGTERM path
// runs (memql#1269): the target node, on receiving NodeMaintenanceMsg, runs
// Draining (advertised in gossip, readiness 503) -> drain delay -> in-flight
// wait bounded by MEMQL_SHUTDOWN_GRACE_PERIOD -> Stopped -> Stop sweep. This
// tool does NOT re-implement any of that; it just initiates it on demand.
//
// Auth: the cluster operator credential (MEMQL_MASTER_KEY), the same
// owner-equivalent credential the secrets tooling uses. The node's
// owner/admin gate admits it as a synthetic cluster owner. No open endpoint.
//
// Usage:
//
//	rolling-drain --endpoint bff-2:50051 [--reason "manual roll 0.9.40"]
//
// Env:
//
//	MEMQL_MASTER_KEY   required -- the operator credential.
//	MEMQL_GRPC_ENDPOINT alternative to --endpoint (same parsing as the
//	                    secrets tool: https:// or :443 => TLS, else plaintext).
//
// Exit codes: 0 = node accepted the drain (now draining or already
// draining); 1 = rejected / unavailable / transport error.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/core/id"
)

func main() {
	var (
		endpoint = flag.String("endpoint", "", "target node gRPC endpoint (overrides MEMQL_GRPC_ENDPOINT)")
		reason   = flag.String("reason", "operator maintenance", "free-form reason recorded in node logs/audit")
		timeout  = flag.Duration("timeout", 30*time.Second, "RPC timeout")
	)
	flag.Parse()

	if strings.TrimSpace(os.Getenv(secret.EnvMasterKey)) == "" {
		fmt.Fprintf(os.Stderr, "ERROR: %s must be set (the cluster operator credential)\n", secret.EnvMasterKey)
		os.Exit(1)
	}

	ep := strings.TrimSpace(*endpoint)
	if ep == "" {
		ep = strings.TrimSpace(os.Getenv("MEMQL_GRPC_ENDPOINT"))
	}
	if ep == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --endpoint or MEMQL_GRPC_ENDPOINT is required")
		os.Exit(1)
	}

	res, err := drain(ep, *reason, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	out, _ := json.Marshal(map[string]any{
		"ok":              res.GetOk(),
		"alreadyDraining": res.GetAlreadyDraining(),
		"nodeId":          res.GetNodeId(),
		"state":           res.GetState(),
		"errorCode":       res.GetErrorCode(),
		"errorMessage":    res.GetErrorMessage(),
	})
	fmt.Println(string(out))

	if !res.GetOk() {
		os.Exit(1)
	}
}

// drain dials the target endpoint, opens a MemqlService stream with the
// operator credential, sends NodeMaintenanceMsg, and returns the result.
func drain(endpoint, reason string, timeout time.Duration) (*memqlv1.NodeMaintenanceResult, error) {
	conn, err := dial(endpoint)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", endpoint, err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ctx = withOperatorAuth(ctx)

	client := memqlv1.NewMemqlServiceClient(conn)
	stream, err := client.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}

	if err := handshake(stream); err != nil {
		return nil, err
	}

	msgID := id.NewShortId()
	if err := stream.Send(&memqlv1.MemqlClientMessage{
		MessageId: msgID,
		Payload: &memqlv1.MemqlClientMessage_NodeMaintenance{
			NodeMaintenance: &memqlv1.NodeMaintenanceMsg{
				RequestId: id.NewShortId(),
				Action:    "drain",
				Reason:    reason,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("send maintenance: %w", err)
	}

	for {
		resp, err := stream.Recv()
		if err != nil {
			return nil, fmt.Errorf("recv: %w", err)
		}
		if res := resp.GetNodeMaintenanceResult(); res != nil {
			return res, nil
		}
		// Ignore any unrelated server-pushed messages (heartbeats, etc.).
	}
}

// dial mirrors the secrets tool's endpoint parsing: https:// or a :443
// suffix => TLS; everything else => plaintext (cluster-mode direct dial).
func dial(endpoint string) (*grpc.ClientConn, error) {
	useTLS := false
	switch {
	case strings.HasPrefix(endpoint, "https://"):
		endpoint = strings.TrimPrefix(endpoint, "https://")
		useTLS = true
	case strings.HasPrefix(endpoint, "http://"):
		endpoint = strings.TrimPrefix(endpoint, "http://")
	}
	if !useTLS && strings.HasSuffix(endpoint, ":443") {
		useTLS = true
	}
	if !strings.Contains(endpoint, ":") {
		if useTLS {
			endpoint += ":443"
		} else {
			endpoint += ":50051"
		}
	}
	if useTLS {
		return grpc.NewClient(endpoint, grpc.WithTransportCredentials(credentials.NewTLS(nil)))
	}
	return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// withOperatorAuth stamps the operator credential onto outgoing metadata.
func withOperatorAuth(ctx context.Context) context.Context {
	key := strings.TrimSpace(os.Getenv(secret.EnvMasterKey))
	if key == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Operator "+key)
}

// handshake completes the ClientHello/ServerHello exchange the stream
// requires before any other message.
func handshake(stream memqlv1.MemqlService_StreamClient) error {
	hello := &memqlv1.MemqlClientMessage{
		MessageId: id.NewShortId(),
		Payload: &memqlv1.MemqlClientMessage_ClientHello{
			ClientHello: &memqlv1.ClientHello{
				ClientId:   "rolling-drain",
				SdkName:    "memql-rolling-drain",
				SdkVersion: "1",
			},
		},
	}
	if err := stream.Send(hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	srvMsg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("recv server hello: %w", err)
	}
	if srvMsg.GetServerHello() == nil {
		return fmt.Errorf("expected ServerHello, got %T", srvMsg.Payload)
	}
	return nil
}
