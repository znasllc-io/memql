package packages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
)

// credentials_test.go -- the D10 properties of epic memql#4885 that a fake
// engine CAN measure: what the create handler puts on the wire, in the log
// and in its reply; what the resolver does with zero rows, a revoked row and
// an active one; and under whose actor the sealed read is issued. What a fake
// cannot measure -- that the real read gate refuses another user's credential
// over real rows -- is credentials_db_test.go.

// testMasterKey is 32 bytes of hex, which is all secret.Encrypt asks of
// MEMQL_MASTER_KEY. A test that seals must set it, because Encrypt refuses to
// seal under no key rather than writing cleartext.
const testMasterKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

const testToken = "ghp_SUPERSECRETVALUEabcd1234"

// actorEngine is a recordingEngine that ALSO records, per construct, the actor
// and the origin each Execute arrived under. The two facts this file turns on
// are exactly those: the sealed read must run as the package OWNER, and the
// create must run as the CALLER.
type actorEngine struct {
	recordingEngine
	amu     sync.Mutex
	actors  map[string]string
	origins map[string]auth.CallOrigin
}

func (e *actorEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	e.amu.Lock()
	if e.actors == nil {
		e.actors = map[string]string{}
		e.origins = map[string]auth.CallOrigin{}
	}
	name := callName(query)
	if ac, ok := auth.AccessFromContext(ctx); ok {
		e.actors[name] = ac.UserId
	} else {
		e.actors[name] = ""
	}
	e.origins[name] = auth.OriginFromContext(ctx)
	e.amu.Unlock()
	return e.recordingEngine.Execute(ctx, query)
}

// capturedLog is a logger writing into a buffer the test can grep.
func capturedLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// credentialHarness wires an Integration over an explicit store, so the
// production resolve() -- which would build an Azure client -- never runs.
func credentialHarness(t *testing.T, engine Engine, logger *slog.Logger) (*Integration, *store) {
	t.Helper()
	i := NewIntegration(engine, logger)
	s := &store{engine: engine, logger: logger}
	i.depsOnce.Do(func() {
		i.deps = &Deps{Store: s, Credentials: s.resolveCredential, PeekCredentials: s.peekCredential, Logger: logger}
	})
	return i, s
}

func callerCtx(userId string) context.Context {
	return auth.ContextWithUserActor(context.Background(), userId)
}

func replyPayload(t *testing.T, nodes []memorynodes.MemoryNode) map[string]any {
	t.Helper()
	if len(nodes) != 1 {
		t.Fatalf("want one reply node, got %d", len(nodes))
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatalf("reply payload is not JSON: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// sourceCredentialCreate
// ---------------------------------------------------------------------------

// TestSourceCredentialCreateNeverReturnsOrLogsTheToken is section G's second
// bullet measured: the token crosses the wire once and appears in no row, no
// log line and no reply. The rendered mutation is the only statement, and the
// one place the token could land on a row is its encryptedValue -- which must
// be a real seal of it and not the value dressed up.
func TestSourceCredentialCreateNeverReturnsOrLogsTheToken(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	logger, logs := capturedLog()
	engine := &actorEngine{}
	i, _ := credentialHarness(t, engine, logger)

	nodes, err := i.handleSourceCredentialCreate(callerCtx("v1:identity:user:alice"), map[string]any{
		"host":  "github.com",
		"label": "work laptop",
		"token": testToken,
	}, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The reply: an id and a fingerprint, and nothing that is the token.
	reply := replyPayload(t, nodes)
	id, _ := reply["credentialId"].(string)
	if !strings.HasPrefix(id, sourceCredentialConcept+":") {
		t.Fatalf("the reply must carry an engine-minted credential id, got %q", id)
	}
	fp, _ := reply["fingerprint"].(string)
	if fp != secret.Fingerprint(testToken) || !strings.HasSuffix(fp, testToken[len(testToken)-4:]) {
		t.Fatalf("the fingerprint must be the token's last four characters, got %q", fp)
	}
	if raw := string(nodes[0].Payload); strings.Contains(raw, testToken) {
		t.Fatalf("the token reached the reply: %s", raw)
	}

	// The wire: exactly one statement, the @serverOnly create, carrying the
	// ciphertext and never the plaintext.
	stmts := engine.statements()
	if len(stmts) != 1 || !strings.HasPrefix(stmts[0], "mutation createSourceCredential(") {
		t.Fatalf("want exactly the createSourceCredential statement, got %v", stmts)
	}
	if strings.Contains(stmts[0], testToken) {
		t.Fatalf("the token VALUE reached a row: %s", stmts[0])
	}
	sealed := regexp.MustCompile(`encryptedValue: "([^"]+)"`).FindStringSubmatch(stmts[0])
	if sealed == nil {
		t.Fatalf("no encryptedValue on the rendered statement: %s", stmts[0])
	}
	if strings.Contains(sealed[1], testToken) {
		t.Fatal("encryptedValue contains the plaintext")
	}
	// The reachable positive for the two assertions above: the ciphertext is
	// a REAL seal of the token, so a renderer that stopped sealing -- or
	// sealed something else -- fails here rather than passing on "does not
	// contain".
	if got, derr := secret.Decrypt(sealed[1]); derr != nil || got != testToken {
		t.Fatalf("encryptedValue does not unseal to the token: %q, %v", got, derr)
	}
	// ownerUserId is STAMPED from the actor inside the mutation and must not
	// be an argument -- a caller-supplied owner is what
	// TestDeclaredOwnerFieldsAreServerStamped exists to refuse.
	args := callArgNames(stmts[0])
	for _, a := range args {
		if a == "ownerUserId" {
			t.Fatal("ownerUserId is passed as an argument; it must be stamped from actor.userId inside the mutation")
		}
	}
	if want := []string{"credentialId", "host", "label", "encryptedValue", "fingerprint"}; strings.Join(args, ",") != strings.Join(want, ",") {
		t.Fatalf("rendered arguments %v, want %v", args, want)
	}

	// The actor: the CALLER's, so the owner stamp is the person who asked;
	// the origin: internal, because the mutation is @serverOnly.
	if got := engine.actors["createSourceCredential"]; got != "v1:identity:user:alice" {
		t.Fatalf("the create must run under the caller's own actor, got %q", got)
	}
	if got := engine.origins["createSourceCredential"]; got != auth.OriginInternal {
		t.Fatalf("the create reaches a @serverOnly mutation and must be stamped internal, got %v", got)
	}

	// The log: nothing this handler logs may carry the value.
	if strings.Contains(logs.String(), testToken) {
		t.Fatalf("the token reached the log: %s", logs.String())
	}
}

func TestSourceCredentialCreateRefusesAnotherHost(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	logger, _ := capturedLog()
	engine := &actorEngine{}
	i, _ := credentialHarness(t, engine, logger)

	_, err := i.handleSourceCredentialCreate(callerCtx("v1:identity:user:alice"), map[string]any{
		"host": "gitlab.com", "label": "work", "token": testToken,
	}, 0)
	if got := RefusalCode(err); got != CodeSourceHostUnsupported {
		t.Fatalf("want %s, got %s (%v)", CodeSourceHostUnsupported, got, err)
	}
	if !strings.Contains(err.Error(), "only github.com today, or upload a zip") {
		t.Fatalf("the refusal must name the two ways forward, got: %v", err)
	}
	if len(engine.statements()) != 0 {
		t.Fatalf("a refused host must write nothing: %v", engine.statements())
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatal("the refusal carries the token")
	}
}

// The two spellings GitHub answers on collapse to the one the fetcher matches
// against; anything else is refused by name.
func TestCredentialHostNormalization(t *testing.T) {
	for _, raw := range []string{"github.com", "GitHub.com", " www.github.com ", "https://github.com/"} {
		got, err := normalizeCredentialHost(raw)
		if err != nil || got != credentialHostGitHub {
			t.Errorf("%q: got %q, %v; want %q", raw, got, err, credentialHostGitHub)
		}
	}
	for _, raw := range []string{"", "gitlab.com", "github.com.evil.example", "bitbucket.org"} {
		if _, err := normalizeCredentialHost(raw); RefusalCode(err) != CodeSourceHostUnsupported {
			t.Errorf("%q was admitted (%v)", raw, err)
		}
	}
}

func TestSourceCredentialCreateRefusesAnEmptyActor(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	logger, _ := capturedLog()
	engine := &actorEngine{}
	i, _ := credentialHarness(t, engine, logger)

	_, err := i.handleSourceCredentialCreate(context.Background(), map[string]any{
		"host": "github.com", "label": "work", "token": testToken,
	}, 0)
	if err == nil {
		t.Fatal("a call with no actor must be refused: the row would be owned by nobody and readable by nobody")
	}
	if len(engine.statements()) != 0 {
		t.Fatalf("nothing may be written for an empty actor: %v", engine.statements())
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatal("the refusal carries the token")
	}
}

// ---------------------------------------------------------------------------
// sourceCredentialRevoke
// ---------------------------------------------------------------------------

// The revoke is the caller's own owned write: one unstamped statement under
// the caller's actor, so the write guard -- not this package -- decides.
func TestSourceCredentialRevokeIsTheCallersOwnWrite(t *testing.T) {
	logger, _ := capturedLog()
	engine := &actorEngine{}
	i, _ := credentialHarness(t, engine, logger)

	nodes, err := i.handleSourceCredentialRevoke(callerCtx("v1:identity:user:alice"), map[string]any{
		"credentialId": "v1:platform:sourceCredential:abc",
	}, 0)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	reply := replyPayload(t, nodes)
	if reply["status"] != credentialStatusRevoked || reply["credentialId"] != "v1:platform:sourceCredential:abc" {
		t.Fatalf("reply %v", reply)
	}
	stmts := engine.statements()
	if len(stmts) != 1 || stmts[0] != `mutation revokeSourceCredential(credentialId: "v1:platform:sourceCredential:abc")` {
		t.Fatalf("want exactly the revoke statement, got %v", stmts)
	}
	if got := engine.actors["revokeSourceCredential"]; got != "v1:identity:user:alice" {
		t.Fatalf("the revoke must run under the caller's actor, got %q", got)
	}
	if got := engine.origins["revokeSourceCredential"]; got != auth.OriginClient {
		t.Fatalf("the revoke is an owned write and must NOT be stamped internal, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// resolveCredential
// ---------------------------------------------------------------------------

func sealedRow(t *testing.T, status string) map[string]any {
	t.Helper()
	ciphertext, _, err := secret.Encrypt(testToken)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return map[string]any{
		"id":             "v1:platform:sourceCredential:abc",
		"ownerUserId":    "v1:identity:user:alice",
		"host":           "github.com",
		"status":         status,
		"encryptedValue": ciphertext,
	}
}

func TestResolveCredentialRefusesZeroRowsAsNotFound(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	logger, _ := capturedLog()
	engine := &actorEngine{} // no rows canned: the owner cannot read it
	s := &store{engine: engine, logger: logger}

	_, err := s.resolveCredential(context.Background(), "v1:platform:sourceCredential:abc", "v1:identity:user:alice")
	if got := RefusalCode(err); got != CodeCredentialNotFound {
		t.Fatalf("want %s, got %s (%v)", CodeCredentialNotFound, got, err)
	}
	for _, want := range []string{"the package's owner cannot read it", "refused by name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must say %q, got: %v", want, err)
		}
	}
	if engine.sawStatement("mutation touchSourceCredential") {
		t.Fatal("a credential that did not resolve must not be touched")
	}
}

func TestResolveCredentialRefusesARevokedRow(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	logger, _ := capturedLog()
	engine := &actorEngine{}
	engine.rows = map[string][]map[string]any{"query sourceCredentialSealedById": {sealedRow(t, credentialStatusRevoked)}}
	s := &store{engine: engine, logger: logger}

	_, err := s.resolveCredential(context.Background(), "v1:platform:sourceCredential:abc", "v1:identity:user:alice")
	if got := RefusalCode(err); got != CodeCredentialRevoked {
		t.Fatalf("want %s, got %s (%v)", CodeCredentialRevoked, got, err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatal("the refusal carries the token")
	}
	if engine.sawStatement("mutation touchSourceCredential") {
		t.Fatal("a revoked credential must not be touched: it was not used")
	}
}

// TestResolveCredentialUnsealsUnderThePackageOwnerAndStampsTheHeartbeat is
// the resolver's positive path, and the actor assertion is the one that
// matters: the sealed read runs as the PACKAGE OWNER, not as whoever holds
// the inbound context -- here, nobody -- so a cluster owner deploying
// somebody's package fetches under that package's credential.
func TestResolveCredentialUnsealsUnderThePackageOwnerAndStampsTheHeartbeat(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	logger, logs := capturedLog()
	engine := &actorEngine{}
	engine.rows = map[string][]map[string]any{"query sourceCredentialSealedById": {sealedRow(t, credentialStatusActive)}}
	s := &store{engine: engine, logger: logger}

	// The inbound context is a DIFFERENT person -- the cluster owner clicking
	// deploy on alice's package.
	inbound := callerCtx("v1:identity:user:operator")
	token, err := s.resolveCredential(inbound, "v1:platform:sourceCredential:abc", "v1:identity:user:alice")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if token != testToken {
		t.Fatalf("the resolver must answer the plaintext to its one caller, got %q", token)
	}
	if got := engine.actors["sourceCredentialSealedById"]; got != "v1:identity:user:alice" {
		t.Fatalf("the sealed read must run under the PACKAGE OWNER's actor, got %q (the inbound caller was the operator)", got)
	}
	if got := engine.origins["sourceCredentialSealedById"]; got != auth.OriginInternal {
		t.Fatalf("the sealed read is @serverOnly and must be stamped internal, got %v", got)
	}
	if !engine.sawStatement("mutation touchSourceCredential(credentialId: \"v1:platform:sourceCredential:abc\", lastUsedAt: \"") {
		t.Fatalf("a successful unseal must stamp lastUsedAt; statements: %v", engine.statements())
	}
	if got := engine.origins["touchSourceCredential"]; got != auth.OriginInternal {
		t.Fatalf("the touch is @serverOnly and must be stamped internal, got %v", got)
	}
	for _, q := range engine.statements() {
		if strings.Contains(q, testToken) {
			t.Fatalf("the token VALUE reached a row: %s", q)
		}
	}
	if strings.Contains(logs.String(), testToken) {
		t.Fatalf("the token reached the log: %s", logs.String())
	}
}

// A failed heartbeat is bookkeeping: logged at Warn, naming the credential
// and never the value, and the fetch still gets its token.
func TestResolveCredentialSurvivesAFailedHeartbeat(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	logger, logs := capturedLog()
	engine := &actorEngine{}
	engine.rows = map[string][]map[string]any{"query sourceCredentialSealedById": {sealedRow(t, credentialStatusActive)}}
	engine.fail = map[string]error{"mutation touchSourceCredential": errors.New("the row moved")}
	s := &store{engine: engine, logger: logger}

	token, err := s.resolveCredential(context.Background(), "v1:platform:sourceCredential:abc", "v1:identity:user:alice")
	if err != nil {
		t.Fatalf("a failed heartbeat must not fail the fetch: %v", err)
	}
	if token != testToken {
		t.Fatalf("got %q", token)
	}
	// The reachable positive for every "the log does not contain the token"
	// assertion in this file: a line WAS written, it names the credential
	// and the error, and it does not carry the value.
	out := logs.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "v1:platform:sourceCredential:abc") || !strings.Contains(out, "the row moved") {
		t.Fatalf("want a WARN naming the credential and the error, got: %s", out)
	}
	if strings.Contains(out, testToken) {
		t.Fatalf("the token reached the log: %s", out)
	}
}

// A ciphertext this node cannot unseal -- the wrong master key, a rotated one
// -- is a refusal that names the key variable, not a silent anonymous fetch.
func TestResolveCredentialRefusesACiphertextItCannotUnseal(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	row := sealedRow(t, credentialStatusActive)
	t.Setenv(secret.EnvMasterKey, strings.Repeat("ff", 32))
	logger, _ := capturedLog()
	engine := &actorEngine{}
	engine.rows = map[string][]map[string]any{"query sourceCredentialSealedById": {row}}
	s := &store{engine: engine, logger: logger}

	_, err := s.resolveCredential(context.Background(), "v1:platform:sourceCredential:abc", "v1:identity:user:alice")
	if err == nil {
		t.Fatal("a ciphertext sealed under another key must not unseal")
	}
	if !strings.Contains(err.Error(), secret.EnvMasterKey) {
		t.Fatalf("the refusal must name the key an operator would check, got: %v", err)
	}
	if engine.sawStatement("mutation touchSourceCredential") {
		t.Fatal("a credential that did not unseal was not used and must not be touched")
	}
}

// Both capabilities are on the surface the builtins in
// dsl/platform/builtins.memql name by executor.
func TestSourceCredentialCapabilitiesAreRegistered(t *testing.T) {
	i := NewIntegration(&recordingEngine{}, discardLogger())
	found := map[string]bool{}
	for _, c := range i.Capabilities() {
		found[c.Name] = true
	}
	for _, name := range []string{"sourceCredentialCreate", "sourceCredentialRevoke"} {
		if !found[name] {
			t.Errorf("capability %q is not registered; dsl/platform/builtins.memql names integration.packages.%s", name, name)
		}
	}
}
