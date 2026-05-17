// scripts/secrets is a slim operator tool invoked internally by
// memQL's dev-refresh flow. The authoring of secrets/variables now
// lives in `memql-cockpit genesis init`; this tool's only job is to
// (1) decrypt a genesis.znas blob to a temporary .env for docker
// compose and (2) read that .env and seed manifest-listed entries
// into a running memQL as concept rows.
//
// Subcommands:
//
//	decrypt --in <path>
//	         Open a ZNAS envelope. Writes the plaintext to a fresh
//	         O_EXCL temp file (mode 0600) and prints the path on
//	         stdout. Requires MEMQL_MASTER_KEY in env. Used by
//	         scripts/dev/lib.sh's require_genesis().
//
//	seed --env-file <path>
//	         Read the (decrypted) .env file, walk memql's manifest at
//	         scripts/secrets/manifest.yaml, and upsert manifest-listed
//	         entries into the running memQL as v1:platform:global*
//	         rows. Entries in the .env that are NOT in the manifest
//	         are ignored -- they're bootstrap-only env vars consumed
//	         by docker-compose, not concept rows.
//
//	health   Quick gRPC handshake check against the running memQL.
//	         Used by dev-refresh's wait_for_memql poll loop.
//
// All authoring previously done by `secrets init / set / delete /
// edit / export / variable-set / variable-delete / list / master-key`
// has been retired; use `memql-cockpit genesis init` instead.
//
// Required env:
//   - MEMQL_MASTER_KEY  for decrypt + seed
//   - MEMQL_GRPC_ENDPOINT  default https://bff.${IDENTITY_BOOTSTRAP_DOMAIN:-local.znas.io}:443
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"gopkg.in/yaml.v3"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/secret"
)

// manifestEntry mirrors one row in scripts/secrets/manifest.yaml.
// Secrets and variables share the shape; Kind is meaningful only
// for secrets.
type manifestEntry struct {
	Name        string `yaml:"name"`
	Scope       string `yaml:"scope"` // "global" | "partition"
	Kind        string `yaml:"kind,omitempty"`
	Default     string `yaml:"default,omitempty"`
	Description string `yaml:"description,omitempty"`
}

type manifest struct {
	Secrets   []manifestEntry `yaml:"secrets"`
	Variables []manifestEntry `yaml:"variables"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var err error
	switch sub {
	case "decrypt":
		err = cmdDecrypt(args)
	case "seed":
		err = cmdSeed(logger, args)
	case "health":
		err = cmdHealth()
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", sub)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `memQL secrets seed/decrypt tool (internal -- called by scripts/dev/refresh.sh)

Subcommands:
  decrypt --in <path>
            Open a ZNAS envelope, write plaintext to a fresh temp
            file (mode 0600), print its path on stdout. Requires
            MEMQL_MASTER_KEY.
  seed --env-file <path>
            Push manifest-listed entries from a decrypted .env into
            the running memQL. Requires MEMQL_MASTER_KEY.
  health    Quick gRPC handshake check. Prints "ok" or an error.

Authoring of genesis.znas / .env values lives in
  memql-cockpit genesis init

Env:
  MEMQL_MASTER_KEY      required for decrypt + seed
  MEMQL_GRPC_ENDPOINT   default https://bff.local.znas.io:443
`)
}

// ---------------------------------------------------------------------
// Manifest + env-file loading
// ---------------------------------------------------------------------

func manifestPath() string {
	// The manifest lives next to this main.go in the repo. Callers
	// invoke us via `go run ./scripts/secrets ...` from the memql
	// repo root, so a working-directory relative path is correct.
	wd, _ := os.Getwd()
	return filepath.Join(wd, "scripts", "secrets", "manifest.yaml")
}

func loadManifest() (*manifest, error) {
	b, err := os.ReadFile(manifestPath())
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// loadEnvFile reads a .env file (one KEY=VALUE per line, # comments
// and blanks skipped, matching single/double quotes stripped from
// the value) and returns a name->value map. Used by cmdSeed to
// consume the decrypted temp file produced by the genesis-decrypt
// step in dev-refresh.
func loadEnvFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	out := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("%s:%d: line lacks '=': %q", path, i+1, line)
		}
		name := strings.TrimSpace(line[:eq])
		value := line[eq+1:]
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		out[name] = value
	}
	return out, nil
}

// ---------------------------------------------------------------------
// decrypt
// ---------------------------------------------------------------------

// cmdDecrypt opens a ZNAS envelope and writes the plaintext to a
// freshly-created temp file in $TMPDIR (or /tmp), printing the path
// on stdout. Owning the path here -- rather than letting the caller
// pass --out -- closes a symlink/TOCTOU window: os.CreateTemp uses
// O_CREAT|O_EXCL so a pre-planted symlink at a predictable PID-based
// path cannot trick this code into writing the plaintext to an
// attacker-chosen file.
func cmdDecrypt(args []string) error {
	flags := parseFlags(args)
	in := flags["in"]
	if in == "" {
		return fmt.Errorf("decrypt: --in <path> is required")
	}
	if os.Getenv(secret.EnvMasterKey) == "" {
		return fmt.Errorf("decrypt: %s must be set in env", secret.EnvMasterKey)
	}
	envelope, err := os.ReadFile(in)
	if err != nil {
		return fmt.Errorf("read %s: %w", in, err)
	}
	plaintext, err := secret.OpenBlob(envelope)
	if err != nil {
		return fmt.Errorf("open %s: %w", in, err)
	}
	f, err := os.CreateTemp("", "memql-genesis-*.env")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(f.Name())
		return fmt.Errorf("chmod %s: %w", f.Name(), err)
	}
	if _, err := f.Write(plaintext); err != nil {
		f.Close()
		os.Remove(f.Name())
		return fmt.Errorf("write %s: %w", f.Name(), err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("close %s: %w", f.Name(), err)
	}
	fmt.Println(f.Name())
	return nil
}

// ---------------------------------------------------------------------
// seed
// ---------------------------------------------------------------------

func cmdSeed(logger *slog.Logger, args []string) error {
	flags := parseFlags(args)
	envFile := flags["env-file"]
	if envFile == "" {
		return fmt.Errorf("seed: --env-file <path> is required")
	}
	if os.Getenv(secret.EnvMasterKey) == "" {
		return fmt.Errorf("seed: %s must be set in env to encrypt secret values", secret.EnvMasterKey)
	}

	values, err := loadEnvFile(envFile)
	if err != nil {
		return err
	}
	m, err := loadManifest()
	if err != nil {
		return err
	}

	conn, err := connectGRPC()
	if err != nil {
		return err
	}
	defer conn.Close()
	client := memqlv1.NewMemqlServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stream, err := client.Stream(withOperatorAuth(ctx))
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	defer stream.CloseSend()
	if err := handshake(stream); err != nil {
		return err
	}

	applied := 0
	skipped := 0
	for _, e := range m.Secrets {
		v, ok := values[e.Name]
		if !ok || v == "" {
			skipped++
			continue
		}
		ct, fp, err := secret.Encrypt(v)
		if err != nil {
			return fmt.Errorf("encrypt %s: %w", e.Name, err)
		}
		mutationName, id := secretMutationFor(e)
		payload := map[string]any{
			"id":             id,
			"name":           e.Name,
			"encryptedValue": ct,
			"fingerprint":    fp,
			"kind":           e.Kind,
			"description":    "",
			"addedBy":        "make dev-refresh / secrets seed",
			"active":         true,
		}
		if err := runMutation(stream, mutationName, payload, ""); err != nil {
			return fmt.Errorf("seed secret %s: %w", e.Name, err)
		}
		logger.Info("secret seeded", "name", e.Name, "scope", e.Scope, "fingerprint", fp)
		applied++
	}
	for _, e := range m.Variables {
		v, ok := values[e.Name]
		if !ok || v == "" {
			skipped++
			continue
		}
		mutationName, id := variableMutationFor(e)
		payload := map[string]any{
			"id":          id,
			"name":        e.Name,
			"value":       v,
			"description": "",
			"active":      true,
		}
		if err := runMutation(stream, mutationName, payload, ""); err != nil {
			return fmt.Errorf("seed variable %s: %w", e.Name, err)
		}
		logger.Info("variable seeded", "name", e.Name, "scope", e.Scope)
		applied++
	}

	fmt.Printf("Applied %d entries (skipped %d missing/empty).\n", applied, skipped)
	return nil
}

func secretMutationFor(e manifestEntry) (mutation string, id string) {
	if e.Scope == "global" {
		return "mutationSetGlobalSecret", "secret-global-" + slugify(e.Name)
	}
	return "mutationSetPartitionSecret", "secret-" + slugify(e.Name)
}

func variableMutationFor(e manifestEntry) (mutation string, id string) {
	if e.Scope == "global" {
		return "mutationSetGlobalVariable", "var-global-" + slugify(e.Name)
	}
	return "mutationSetPartitionVariable", "var-" + slugify(e.Name)
}

func slugify(name string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "_", "-"))
}

// ---------------------------------------------------------------------
// health
// ---------------------------------------------------------------------

func cmdHealth() error {
	conn, err := connectGRPC()
	if err != nil {
		fmt.Printf("not-ready: dial: %v\n", err)
		return err
	}
	defer conn.Close()

	client := memqlv1.NewMemqlServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Stream(withOperatorAuth(ctx))
	if err != nil {
		fmt.Printf("not-ready: open stream: %v\n", err)
		return err
	}
	defer stream.CloseSend()

	if err := handshake(stream); err != nil {
		fmt.Printf("not-ready: handshake: %v\n", err)
		return err
	}
	fmt.Println("ok")
	return nil
}

// ---------------------------------------------------------------------
// gRPC plumbing
// ---------------------------------------------------------------------

func parseFlags(args []string) map[string]string {
	out := map[string]string{}
	for _, a := range args {
		a = strings.TrimPrefix(a, "--")
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

func connectGRPC() (*grpc.ClientConn, error) {
	// MEMQL_GRPC_ENDPOINT overrides the default
	// https://bff.${IDENTITY_BOOTSTRAP_DOMAIN:-local.znas.io}:443
	// endpoint. Accepts:
	//
	//   https://bff.<domain>          TLS, port 443 implied
	//   https://bff.<domain>:443      TLS, explicit port
	//   bff.<domain>:443              TLS (auto-detected via :443 suffix)
	//   bff.<domain>:50051            plaintext gRPC (cluster-mode direct)
	//   localhost:50051               plaintext gRPC (legacy / debug)
	endpoint := os.Getenv("MEMQL_GRPC_ENDPOINT")
	useTLS := false
	if endpoint == "" {
		domain := os.Getenv("IDENTITY_BOOTSTRAP_DOMAIN")
		if strings.TrimSpace(domain) == "" {
			domain = "local.znas.io"
		}
		endpoint = "bff." + domain + ":443"
		useTLS = true
	} else {
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
	}
	if useTLS {
		return grpc.NewClient(endpoint, grpc.WithTransportCredentials(credentials.NewTLS(nil)))
	}
	return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// withOperatorAuth stamps the operator credential (MEMQL_MASTER_KEY)
// onto outgoing gRPC metadata so the cluster's operator-aware stream
// interceptor admits the stream as a synthetic cluster owner. When
// MEMQL_MASTER_KEY is empty this is a no-op and the downstream RPC
// fails with codes.Unauthenticated, which is the right outcome.
func withOperatorAuth(ctx context.Context) context.Context {
	key := strings.TrimSpace(os.Getenv(secret.EnvMasterKey))
	if key == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Operator "+key)
}

func handshake(stream memqlv1.MemqlService_StreamClient) error {
	hello := &memqlv1.MemqlClientMessage{
		MessageId: uuid.NewString(),
		Payload: &memqlv1.MemqlClientMessage_ClientHello{
			ClientHello: &memqlv1.ClientHello{
				ClientId:   "make-secrets",
				SdkName:    "memql-secrets-tool",
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

func runMutation(stream memqlv1.MemqlService_StreamClient, mutationName string, args map[string]any, partition string) error {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("marshal args: %w", err)
	}
	query := fmt.Sprintf("%s(%s)", mutationName, string(argsJSON))
	msgId := uuid.NewString()
	msg := &memqlv1.MemqlClientMessage{
		MessageId: msgId,
		Partition: partition,
		Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
			ExecuteQuery: &memqlv1.ExecuteQueryMsg{
				RequestId: uuid.NewString(),
				Query:     query,
			},
		},
	}
	if err := stream.Send(msg); err != nil {
		return fmt.Errorf("send mutation: %w", err)
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		if resp.CorrelateTo == msgId {
			if errorMsg := resp.GetQueryError(); errorMsg != nil {
				return fmt.Errorf("%s", errorMsg.String())
			}
			return nil
		}
	}
}
