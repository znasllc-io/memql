package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claudegrant_test.go pins what a Claude Code turn is ALLOWED to do, and the
// two environment facts its MCP connection depends on.
//
// The grant is the machine owner's decision, not the user's settings file:
// inside a session Claude Code may edit files and run shell commands inside
// the session workspace, and call MemQL's tools over MCP -- nothing else, and
// nothing a personal ~/.claude/settings.json adds or takes away. Every part
// of that lives in the argv, so the argv is asserted from the fake's own log
// for the reason the rest of this package does: a flag this client believes
// it passed and did not is invisible from this side of the fork, and a
// missing one fails SILENTLY -- the turn runs under whatever the user's
// settings say.

// claudeInitWithMCP is an init event carrying `mcp_servers`.
//
// NOT A RECORDING, unlike the fixtures beside it, and said so: recording one
// means running a real prompt against a real MCP server. The shape is read
// from the claude 2.1.283 binary itself -- its init builder emits
// `mcp_servers: [{name, status}]` beside `cwd`, `session_id`, `model` and
// `permissionMode`, and its status enum is connected / failed / needs-auth /
// pending / disabled. STATUS is substituted per case.
const claudeInitWithMCP = `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-mcp","tools":["Bash","Edit","Read","mcp__memql__query"],"mcp_servers":[{"name":"memql","status":"STATUS"}],"model":"claude-haiku-4-5-20251001","permissionMode":"dontAsk"}`

// claudeAfterInit is the rest of a turn that got past its init event.
const claudeAfterInit = `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"pong"}]},"parent_tool_use_id":null,"session_id":"sess-mcp"}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"session_id":"sess-mcp","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1},"result":"pong"}`

func initWithMCPStatus(server, status string) string {
	line := strings.Replace(claudeInitWithMCP, "STATUS", status, 1)
	return strings.Replace(line, `"name":"memql"`, `"name":"`+server+`"`, 1)
}

// separatorAt is the index of the `--` that ends option parsing.
func separatorAt(t *testing.T, argv []string) int {
	t.Helper()
	for i, a := range argv {
		if a == "--" {
			return i
		}
	}
	t.Fatalf("no -- separator in %v", argv)
	return -1
}

// TestClaudeHeadlessArgvCarriesTheSessionGrant is the owner's permission
// decision, flag by flag.
func TestClaudeHeadlessArgvCarriesTheSessionGrant(t *testing.T) {
	bin, log := fakeClaude(t, prints(claudePongTurn))
	spec := claudeSpec(t, bin)
	h := startClaude(t, spec)

	if _, err := h.Turn(context.Background(), "say pong", &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	argv := recordedArgv(t, log)[0]
	options := argv[:separatorAt(t, argv)]

	// No settings FILE is read: not the user's, not the workspace's. An
	// empty source list is the app's own "none" (its parser returns [] for
	// ""), and the = form is the one Claude Code itself respawns with, so
	// the empty value cannot be mistaken for a missing one.
	if !hasArg(options, "--setting-sources=") {
		t.Errorf("--setting-sources= is missing, so ~/.claude/settings.json decides what this session may do: %v", argv)
	}
	// No MCP server but the session's own. A user's global servers would
	// ride a session somebody else started, on their credentials.
	if !hasArg(options, "--strict-mcp-config") {
		t.Errorf("--strict-mcp-config is missing, so the user's own MCP servers load too: %v", argv)
	}
	// Anything not granted below is REFUSED rather than asked about: there
	// is nobody at a `claude -p` to answer.
	if got := argValue(t, options, "--permission-mode"); got != "dontAsk" {
		t.Errorf("--permission-mode = %q, want dontAsk", got)
	}
	// The file tools are granted under the workspace only (a CLI rule's "/"
	// is the working directory), and the MemQL server whole. Bash is not
	// here on purpose: it is approved only through the sandbox below.
	if got := argValue(t, options, "--allowedTools"); got != "Edit(/**) Read(/**) mcp__memql" {
		t.Errorf("--allowedTools = %q, want exactly the workspace file tools and the MemQL server", got)
	}

	var settings struct {
		Sandbox map[string]any `json:"sandbox"`
	}
	raw := argValue(t, options, "--settings")
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		t.Fatalf("--settings is not inline JSON: %q: %v", raw, err)
	}
	for key, want := range map[string]bool{
		"enabled":                  true,
		"failIfUnavailable":        true,
		"autoAllowBashIfSandboxed": true,
		"allowUnsandboxedCommands": false,
	} {
		if got, ok := settings.Sandbox[key].(bool); !ok || got != want {
			t.Errorf("sandbox.%s = %v, want %v (settings %s)", key, settings.Sandbox[key], want, raw)
		}
	}

	// Nothing that widens the grant past what is written above.
	for _, forbidden := range []string{"--dangerously-skip-permissions", "--allow-dangerously-skip-permissions", "bypassPermissions", "--add-dir"} {
		if hasArg(argv, forbidden) {
			t.Errorf("%s widens the session past its workspace: %v", forbidden, argv)
		}
	}
	// And every one of those flags is an option, not a prompt.
	if got := argv[separatorAt(t, argv)+1:]; len(got) != 1 || got[0] != "say pong" {
		t.Errorf("the prompt must be the only thing after --, got %v", got)
	}
}

// TestClaudeHeadlessGrantHoldsWithoutAnMCPConfig: a session with no MCP
// configuration still loads NO MCP server -- strict with nothing to load is
// "none", where dropping the flag would be "the user's".
func TestClaudeHeadlessGrantHoldsWithoutAnMCPConfig(t *testing.T) {
	bin, log := fakeClaude(t, prints(claudePongTurn))
	spec := claudeSpec(t, bin)
	spec.MCPConfigPath = ""
	h := startClaude(t, spec)

	if _, err := h.Turn(context.Background(), "say pong", &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	argv := recordedArgv(t, log)[0]
	if hasArg(argv, "--mcp-config") {
		t.Errorf("--mcp-config with no path: %v", argv)
	}
	if !hasArg(argv, "--strict-mcp-config") {
		t.Errorf("--strict-mcp-config is missing, so the user's own MCP servers load: %v", argv)
	}
}

// TestClaudeHeadlessPinsTheCertStore: Claude Code must trust the system
// store as well as its bundled roots, because a local cluster's MCP endpoint
// is signed by an mkcert CA that lives only in the system keychain. It is
// PINNED -- the worker's own environment does not get a say, because a
// narrower value there is a session that silently has no MemQL tools.
func TestClaudeHeadlessPinsTheCertStore(t *testing.T) {
	t.Setenv("CLAUDE_CODE_CERT_STORE", "bundled")
	seen := filepath.Join(t.TempDir(), "cert-store")
	bin, _ := fakeClaude(t, "printf '%s' \"$CLAUDE_CODE_CERT_STORE\" > '"+seen+"'\n"+prints(claudePongTurn))
	h := startClaude(t, claudeSpec(t, bin))

	if _, err := h.Turn(context.Background(), "say pong", &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the fake recorded no environment: %v", err)
	}
	if string(got) != "bundled,system" {
		t.Errorf("CLAUDE_CODE_CERT_STORE = %q, want bundled,system whatever the worker's own environment says", got)
	}
}

// TestClaudeHeadlessMCPServerThatCannotServeFailsTheTurn: an init event that
// says MemQL's server FAILED or NEEDS AUTH ends the turn at once, naming the
// server and the status. The process is stopped rather than left to run a
// whole turn without a single MemQL tool -- on somebody's subscription, to
// produce an answer the caller would read as MemQL's.
func TestClaudeHeadlessMCPServerThatCannotServeFailsTheTurn(t *testing.T) {
	for _, status := range []string{"failed", "needs-auth"} {
		t.Run(status, func(t *testing.T) {
			// After the init line the fake would sit for a minute; only a
			// turn that stopped it returns in time.
			bin, _ := fakeClaude(t, prints(initWithMCPStatus("memql", status))+"exec sleep 60\n")
			h := startClaude(t, claudeSpec(t, bin))

			done := make(chan error, 1)
			go func() {
				_, err := h.Turn(context.Background(), "use a memql tool", &recorder{})
				done <- err
			}()
			var err error
			select {
			case err = <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("the turn kept running after MemQL's MCP server reported it cannot serve")
			}
			if err == nil {
				t.Fatalf("a turn whose MemQL server reported %q reported success", status)
			}
			for _, want := range []string{`"memql"`, status} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to name %s", err, want)
				}
			}
		})
	}
}

// TestClaudeHeadlessMCPServerStillConnectingIsNotAFailure: `pending` is a
// server still connecting when the init event was printed. Claude Code
// finishes the connection on its own, and failing on it would fail every
// turn whose MCP endpoint answers a little slower than the app starts.
func TestClaudeHeadlessMCPServerStillConnectingIsNotAFailure(t *testing.T) {
	for _, status := range []string{"pending", "connected"} {
		t.Run(status, func(t *testing.T) {
			bin, _ := fakeClaude(t, prints(initWithMCPStatus("memql", status)+"\n"+claudeAfterInit))
			h := startClaude(t, claudeSpec(t, bin))
			res, err := h.Turn(context.Background(), "use a memql tool", &recorder{})
			if err != nil {
				t.Fatalf("a %s MemQL server failed the turn: %v", status, err)
			}
			if res.Text != "pong" {
				t.Errorf("Text = %q, want the turn's answer", res.Text)
			}
		})
	}
}

// TestClaudeHeadlessOnlyMemQLsServerDecides: a server that is not MemQL's
// (a machine's managed configuration can add one) failing does not cost the
// session MemQL's tools, so it does not fail the turn.
func TestClaudeHeadlessOnlyMemQLsServerDecides(t *testing.T) {
	bin, _ := fakeClaude(t, prints(initWithMCPStatus("someone-elses", "failed")+"\n"+claudeAfterInit))
	h := startClaude(t, claudeSpec(t, bin))
	if _, err := h.Turn(context.Background(), "use a memql tool", &recorder{}); err != nil {
		t.Fatalf("another server's failure failed the turn: %v", err)
	}
}

// claudeToolRules splits a --allowedTools / --disallowedTools value the way
// Claude Code 2.1.283 does (its exported `Hp`, read from the binary): on
// commas and spaces OUTSIDE parentheses, so a rule whose path holds a space
// stays one rule. A path holding a parenthesis would not, which is why the
// grant writes no rule for one.
func claudeToolRules(value string) []string {
	var rules []string
	var cur strings.Builder
	inside := false
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			rules = append(rules, s)
		}
		cur.Reset()
	}
	for _, r := range value {
		switch {
		case r == '(':
			inside = true
			cur.WriteRune(r)
		case r == ')':
			inside = false
			cur.WriteRune(r)
		case (r == ',' || r == ' ') && !inside:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return rules
}

// TestClaudeHeadlessArgvDeniesTheProtectedPaths: the paths a session must
// never touch -- this worker's own directory (the worker tokens, and
// policy.yaml, whose apps.allow is the app consent gate) and this machine's
// fs.deny list -- are denied to the app for READING as well as writing, on
// both of the app's surfaces:
//
//   - the file tools, as Read and Edit deny rules (a deny rule beats the
//     workspace's allow rule, and a bare path covers everything under it,
//     as a gitignore entry does);
//   - every shell command, as the sandbox's own filesystem.denyRead and
//     denyWrite, in the plain absolute form the sandbox documents. The
//     sandbox confines writes to the workspace already; reads it confines
//     only here, and without this a sandboxed `cat ~/.memql/worker.yaml >
//     out.txt` would leave the worker token in a file the session then
//     pushes to the Library.
//
// All of it before `--`, where it is an option and not part of the prompt.
func TestClaudeHeadlessArgvDeniesTheProtectedPaths(t *testing.T) {
	deny := []string{
		"/home/ada/.memql",
		"/home/ada/.ssh",
		"/home/ada/Library/Application Support/Google/Chrome",
		"/etc/passwd",
		"/home/ada/odd (dir)",
	}
	bin, log := fakeClaude(t, prints(claudePongTurn))
	spec := claudeSpec(t, bin)
	spec.DenyPaths = deny
	h := startClaude(t, spec)

	if _, err := h.Turn(context.Background(), "say pong", &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	argv := recordedArgv(t, log)[0]
	options := argv[:separatorAt(t, argv)]

	rules := map[string]bool{}
	for _, rule := range claudeToolRules(argValue(t, options, "--disallowedTools")) {
		rules[rule] = true
	}
	for _, path := range deny[:4] {
		for _, tool := range []string{"Read", "Edit"} {
			// A rule's "//" prefix is an absolute path; a single "/" would
			// be the working directory, which is the workspace.
			if want := tool + "(/" + path + ")"; !rules[want] {
				t.Errorf("no %s deny rule %q in %v", tool, want, rules)
			}
		}
	}
	for rule := range rules {
		if strings.Contains(rule, "odd") {
			t.Errorf("a path holding a parenthesis became rule %q; Claude Code's splitter would cut it apart", rule)
		}
	}

	var settings struct {
		Sandbox struct {
			Filesystem struct {
				DenyRead  []string `json:"denyRead"`
				DenyWrite []string `json:"denyWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	raw := argValue(t, options, "--settings")
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		t.Fatalf("--settings is not inline JSON: %q: %v", raw, err)
	}
	// Every path, the parenthesised one included: JSON carries it whole.
	fs := settings.Sandbox.Filesystem
	if strings.Join(fs.DenyRead, "\n") != strings.Join(deny, "\n") {
		t.Errorf("sandbox.filesystem.denyRead = %q, want %q", fs.DenyRead, deny)
	}
	if strings.Join(fs.DenyWrite, "\n") != strings.Join(deny, "\n") {
		t.Errorf("sandbox.filesystem.denyWrite = %q, want %q", fs.DenyWrite, deny)
	}
}

// TestClaudeHeadlessNoDenyPathsAddsNoDenyRules: a session with nothing to
// deny carries no deny flag at all, rather than an empty one Claude Code
// would have to read as "deny nothing".
func TestClaudeHeadlessNoDenyPathsAddsNoDenyRules(t *testing.T) {
	bin, log := fakeClaude(t, prints(claudePongTurn))
	h := startClaude(t, claudeSpec(t, bin))

	if _, err := h.Turn(context.Background(), "say pong", &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	argv := recordedArgv(t, log)[0]
	if hasArg(argv, "--disallowedTools") {
		t.Errorf("--disallowedTools with nothing to deny: %v", argv)
	}
	if strings.Contains(argValue(t, argv, "--settings"), "filesystem") {
		t.Errorf("a sandbox filesystem block with nothing in it: %v", argv)
	}
}
