package identity

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"testing"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// githubconnect_test.go -- what githubConnectBegin answers, and what it
// refuses (epic memql#4912, decision C4).
//
// The three reply keys are a WIRE CONTRACT the OS builds against, so they are
// asserted by name rather than by shape: a renamed key is a client that renders
// an empty card with nothing failing on either side.

// beginFakeEngine records the MemQL text the capability issues.
//
// IntegrationEngineAccess is a wide interface and this flow uses exactly one
// method of it, so the rest is EMBEDDED AS A NIL INTERFACE: an unexpected call
// panics with the method name rather than returning a plausible zero value,
// which is the difference between a test that reports a new dependency and one
// that quietly tolerates it.
type beginFakeEngine struct {
	memqlengine.IntegrationEngineAccess

	mu         sync.Mutex
	statements []string
	err        error
}

func (f *beginFakeEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statements = append(f.statements, q)
	if f.err != nil {
		return nil, f.err
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func (f *beginFakeEngine) statement(t *testing.T, prefix string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.statements {
		if strings.HasPrefix(q, prefix) {
			return q
		}
	}
	t.Fatalf("no statement starting %q. Executed:\n  %s", prefix, strings.Join(f.statements, "\n  "))
	return ""
}

// callerContext builds the access envelope a signed-in stream call carries.
func callerContext(userId string) context.Context {
	return componentAuth.ContextWithUserActor(context.Background(), userId)
}

// beginReply decodes the capability's single result node.
func beginReply(t *testing.T, nodes []memorynodes.MemoryNode, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("githubConnectBegin: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d result nodes, want exactly 1", len(nodes))
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return out
}

func configureApp(t *testing.T) {
	t.Helper()
	t.Setenv(githubconnect.EnvAppID, "123456")
	t.Setenv(githubconnect.EnvAppSlug, "memql-example")
	t.Setenv(githubconnect.EnvClientID, "Iv1.exampleclientid")
	t.Setenv(githubconnect.EnvClientSecret, "example-client-secret")
	t.Setenv(githubconnect.EnvPrivateKeyB64, "LS0tLS1CRUdJTiBFWEFNUExF")
	t.Setenv(githubconnect.EnvWebhookSecret, "example-webhook-secret")
	t.Setenv("MEMQL_IDENTITY_BASE_URL", "https://identity.example.test")
}

func unconfigureApp(t *testing.T) {
	t.Helper()
	// Explicitly empty rather than merely unset: a developer with a real app
	// exported must not be able to make this pass or fail.
	t.Setenv(githubconnect.EnvAppID, "")
	t.Setenv(githubconnect.EnvAppSlug, "")
	t.Setenv(githubconnect.EnvClientID, "")
	t.Setenv(githubconnect.EnvClientSecret, "")
	t.Setenv(githubconnect.EnvPrivateKeyB64, "")
	t.Setenv(githubconnect.EnvWebhookSecret, "")
	t.Setenv("MEMQL_IDENTITY_BASE_URL", "https://identity.example.test")
}

// TestGithubConnectBeginAnswersAnAuthorizeURL is the happy path, and it pins
// three things at once: the state row is WRITTEN, the URL carries the state,
// and the row carries only its DIGEST.
func TestGithubConnectBeginAnswersAnAuthorizeURL(t *testing.T) {
	configureApp(t)
	eng := &beginFakeEngine{}
	i := NewIdentityIntegrationWithEngine(eng, nil, nil)

	nodes, err := i.handleGithubConnectBegin(
		callerContext("v1:identity:user:asked"),
		map[string]any{"returnPath": "/packages/new"}, 0)
	reply := beginReply(t, nodes, err)

	if reply["reason"] != "ok" {
		t.Fatalf("reason = %v, want ok", reply["reason"])
	}
	authorizeURL, _ := reply["authorizeUrl"].(string)
	if !strings.HasPrefix(authorizeURL, "https://github.com/login/oauth/authorize?") {
		t.Fatalf("authorizeUrl = %q", authorizeURL)
	}
	if !strings.Contains(authorizeURL, "redirect_uri=https%3A%2F%2Fidentity.example.test%2Fauth%2Fgithub%2Fcallback") {
		t.Errorf("the redirect URI is not derived from this cluster's own identity base URL:\n  %s", authorizeURL)
	}
	if reply["installUrl"] != "https://github.com/apps/memql-example/installations/new" {
		t.Errorf("installUrl = %v", reply["installUrl"])
	}

	// THE PLAINTEXT STATE LIVES ONLY IN THE URL. Pull it back out and check
	// the row stored the digest instead -- a row read is not a credential.
	m := regexp.MustCompile(`[?&]state=([^&]+)`).FindStringSubmatch(authorizeURL)
	if m == nil {
		t.Fatal("the authorize URL carries no state")
	}
	plaintext := m[1]
	write := eng.statement(t, "mutation createGithubConnectState(")
	if strings.Contains(write, plaintext) {
		t.Errorf("the PLAINTEXT state value was written to the row:\n  %s\n"+
			"Only its digest may be stored: anyone who could read this row would otherwise be able "+
			"to complete somebody else's connect and land a grant on their account.", write)
	}
	// The positive control for that grep: the digest really is there, so an
	// empty statement or a renamed field fails here rather than passing.
	if !strings.Contains(write, `stateHash: "`) {
		t.Errorf("the row carries no state digest at all, so the leak check above scanned nothing:\n  %s", write)
	}
	if !strings.Contains(write, `userId: "v1:identity:user:asked"`) {
		t.Errorf("the state row does not name the caller:\n  %s", write)
	}
	if !strings.Contains(write, `returnPath: "/packages/new"`) {
		t.Errorf("the return path did not reach the row:\n  %s", write)
	}
}

// TestGithubConnectBeginAnswersNotConfigured is the whole point of a TYPED
// reason: on a cluster with no GitHub App the OS must offer the pasted-token
// path, not render a failure.
func TestGithubConnectBeginAnswersNotConfigured(t *testing.T) {
	unconfigureApp(t)
	eng := &beginFakeEngine{}
	i := NewIdentityIntegrationWithEngine(eng, nil, nil)

	nodes, err := i.handleGithubConnectBegin(
		callerContext("v1:identity:user:asked"), map[string]any{}, 0)
	reply := beginReply(t, nodes, err)

	if reply["reason"] != "github_app_not_configured" {
		t.Fatalf("reason = %v, want github_app_not_configured", reply["reason"])
	}
	if reply["authorizeUrl"] != "" {
		t.Errorf("authorizeUrl = %v, want empty -- there is no app to send anybody to", reply["authorizeUrl"])
	}
	if reply["installUrl"] != "" {
		t.Errorf("installUrl = %v, want empty -- a link composed from a blank slug 404s on github.com",
			reply["installUrl"])
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.statements) != 0 {
		t.Errorf("an unconfigured cluster wrote %d statement(s): %v", len(eng.statements), eng.statements)
	}
}

// TestGithubConnectBeginRefusesAnEmptyActor. The state row names the only
// account the callback may land a grant on, so a call with no actor is refused
// rather than answered.
func TestGithubConnectBeginRefusesAnEmptyActor(t *testing.T) {
	configureApp(t)
	eng := &beginFakeEngine{}
	i := NewIdentityIntegrationWithEngine(eng, nil, nil)

	_, err := i.handleGithubConnectBegin(context.Background(), map[string]any{}, 0)
	if err == nil {
		t.Fatal("a call carrying no actor was answered. The state row would name nobody, and the " +
			"grant the callback writes under it would be owned by nobody -- readable by nobody, " +
			"including the person who pressed Connect.")
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.statements) != 0 {
		t.Errorf("the refusal came after %d write(s): %v", len(eng.statements), eng.statements)
	}
}

// TestGithubConnectBeginDropsAnUnsafeReturnPath. The value is client-supplied
// and lands in a Location header at the far end of a third-party round trip.
func TestGithubConnectBeginDropsAnUnsafeReturnPath(t *testing.T) {
	configureApp(t)
	for _, hostile := range []string{
		"https://evil.test/steal",
		"//evil.test/steal",
		"/\\evil.test",
		"/ok\r\nSet-Cookie: a=b",
	} {
		t.Run(hostile, func(t *testing.T) {
			eng := &beginFakeEngine{}
			i := NewIdentityIntegrationWithEngine(eng, nil, nil)
			nodes, err := i.handleGithubConnectBegin(
				callerContext("v1:identity:user:asked"),
				map[string]any{"returnPath": hostile}, 0)
			beginReply(t, nodes, err)

			write := eng.statement(t, "mutation createGithubConnectState(")
			if !strings.Contains(write, `returnPath: ""`) {
				t.Errorf("an unsafe return path reached the row:\n  %s", write)
			}
		})
	}
}

// TestGithubConnectBeginAnswersStateInvalidWhenTheRowCannotBeWritten.
//
// A reason rather than an error, so the OS never has to parse one -- and the
// row was NOT written, which is what makes retrying safe.
func TestGithubConnectBeginAnswersStateInvalidWhenTheRowCannotBeWritten(t *testing.T) {
	configureApp(t)
	eng := &beginFakeEngine{err: errWriteRefused}
	i := NewIdentityIntegrationWithEngine(eng, nil, nil)

	nodes, err := i.handleGithubConnectBegin(
		callerContext("v1:identity:user:asked"), map[string]any{}, 0)
	reply := beginReply(t, nodes, err)

	if reply["reason"] != "connect_state_invalid" {
		t.Fatalf("reason = %v, want connect_state_invalid", reply["reason"])
	}
	if reply["authorizeUrl"] != "" {
		t.Errorf("authorizeUrl = %v; sending a browser to GitHub with a state no row backs would "+
			"fail at the callback instead of here", reply["authorizeUrl"])
	}
}

// TestGithubConnectBeginIsRegistered pins the half that
// AuditIntegrationExecutors turns into a boot ERROR: the DSL declares
// integration.identity.githubConnectBegin on every node, and a declared
// capability the registry does not carry fails the whole node's boot.
func TestGithubConnectBeginIsRegistered(t *testing.T) {
	caps := NewIdentityIntegration().Capabilities()
	for _, c := range caps {
		if c.Name == "githubConnectBegin" {
			if c.Handler == nil {
				t.Fatal("githubConnectBegin is declared with a nil handler")
			}
			if _, ok := c.ArgsSchema["returnPath"]; !ok {
				t.Errorf("githubConnectBegin declares no returnPath argument: %v", c.ArgsSchema)
			}
			return
		}
	}
	t.Fatal("githubConnectBegin is not in Capabilities(). dsl/identity/builtins.memql declares " +
		"@executor(\"integration.identity.githubConnectBegin\") and the identity plug-in registers " +
		"on EVERY node type, so a declared-but-unimplemented capability is a boot ERROR everywhere " +
		"(component/memql/integration_executor_audit.go).")
}

var errWriteRefused = &writeRefusedError{}

type writeRefusedError struct{}

func (*writeRefusedError) Error() string { return "the engine refused the write" }
