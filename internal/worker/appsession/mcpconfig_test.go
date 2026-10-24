package appsession

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
)

const testBearer = "eyJhbGciOiJSUzI1NiJ9.test-session-bearer.signature"

func TestWriteMCPConfig_ClaudeCodeShapeAndMode(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()

	m, err := writeMCPConfig(apps.IDClaudeCode, ws, "https://mcp.example.com/mcp", testBearer, "sess-1", state)
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	defer m.Remove()

	path := m.configPath
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The bearer is in this file. 0600 is not decoration.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("config is not valid JSON: %v\n%s", err, data)
	}
	entry, ok := doc.MCPServers[mcpServerName]
	if !ok {
		t.Fatalf("no %q server in the config: %s", mcpServerName, data)
	}
	if entry.Type != "http" || entry.URL != "https://mcp.example.com/mcp" {
		t.Errorf("entry = %+v", entry)
	}
	if entry.Headers["Authorization"] != "Bearer "+testBearer {
		t.Errorf("Authorization header = %q", entry.Headers["Authorization"])
	}
}

func TestWriteMCPConfig_CodexIsWorkspaceScoped(t *testing.T) {
	ws := t.TempDir()
	m, err := writeMCPConfig(apps.IDCodex, ws, "https://mcp.example.com/mcp", testBearer, "sess-2", t.TempDir())
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	defer m.Remove()

	home := filepath.Dir(m.configPath)
	if _, err := os.Stat(filepath.Join(home, "config.toml")); err != nil {
		t.Fatalf("codex config.toml: %v", err)
	}
	// CODEX_HOME must point INSIDE the workspace. Codex's home defaults
	// to the user profile, which is exactly the global write a per-run
	// credential must never make: it outlives the session, survives a
	// crash, and is shared with every other project on the machine.
	env := m.Env()
	if len(env) != 1 || env[0] != "CODEX_HOME="+home {
		t.Fatalf("env = %v, want CODEX_HOME inside the workspace", env)
	}
	if !strings.HasPrefix(home, ws) {
		t.Errorf("codex home %q escaped the workspace %q", home, ws)
	}
}

// TestWriteMCPConfig_RefusesAnEmptyCredential. An app with no credential
// reaches nothing over MCP and reports that as "MemQL's tools are
// broken". Refusing here names the real cause.
func TestWriteMCPConfig_RefusesAnEmptyCredential(t *testing.T) {
	if _, err := writeMCPConfig(apps.IDClaudeCode, t.TempDir(), "https://mcp.example.com/mcp", "", "s", ""); err == nil {
		t.Fatal("an empty credential must be refused, not written as a blank bearer")
	}
}

// TestRenew_RewritesInPlace: AppSessionControl{renew_credential} arrives
// before the current bearer expires, so no single bearer is ever
// long-lived. The old one must not survive the rewrite.
func TestRenew_RewritesInPlace(t *testing.T) {
	ws := t.TempDir()
	m, err := writeMCPConfig(apps.IDClaudeCode, ws, "https://mcp.example.com/mcp", testBearer, "sess-3", t.TempDir())
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	defer m.Remove()

	const next = "eyJhbGciOiJSUzI1NiJ9.the-renewed-bearer.sig"
	if err := m.Renew(next); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	data, err := os.ReadFile(m.configPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), next) {
		t.Error("the renewed bearer is not in the file")
	}
	if strings.Contains(string(data), testBearer) {
		t.Error("the superseded bearer survived the rewrite")
	}
	info, _ := os.Stat(m.configPath)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode after renewal = %o, want 600", perm)
	}
}

// TestRemove_DeletesEverything is the security control, asserted
// directly. The bearer cannot be revoked -- the engine's verify path is
// JWKS-only and DB-free, so there is no row to strike -- which makes this
// deletion one of the three things standing in for revocation.
func TestRemove_DeletesEverything(t *testing.T) {
	for _, app := range []string{apps.IDClaudeCode, apps.IDCodex} {
		t.Run(app, func(t *testing.T) {
			ws := t.TempDir()
			state := t.TempDir()
			m, err := writeMCPConfig(app, ws, "https://mcp.example.com/mcp", testBearer, "sess-"+app, state)
			if err != nil {
				t.Fatalf("writeMCPConfig: %v", err)
			}
			m.Remove()

			if found := grepTree(t, ws, testBearer); len(found) > 0 {
				t.Errorf("the bearer survived cleanup in: %v", found)
			}
			if found := grepTree(t, state, testBearer); len(found) > 0 {
				t.Errorf("the bearer reached the state directory: %v", found)
			}
			// A second Remove must be harmless: the run finishing and
			// the stream dying are independent events, and either one
			// has to be sufficient on its own.
			m.Remove()
		})
	}
}

// TestLedger_NeverHoldsTheBearer. The ledger exists so a restart can
// delete the file that holds the credential. Writing the credential into
// it would mean two files to clean up instead of one, and the second one
// outside the workspace.
func TestLedger_NeverHoldsTheBearer(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	m, err := writeMCPConfig(apps.IDClaudeCode, ws, "https://mcp.example.com/mcp", testBearer, "sess-4", state)
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	defer m.Remove()

	if found := grepTree(t, state, testBearer); len(found) > 0 {
		t.Fatalf("the ledger carries the bearer: %v", found)
	}
	entries, err := os.ReadDir(filepath.Join(state, ledgerDirName))
	if err != nil || len(entries) != 1 {
		t.Fatalf("ledger entries = %v (%v), want exactly one", entries, err)
	}
}

// TestSweep_CleansUpAfterAKilledCockpit is the test #348 asks for by
// name: no configuration file survives a session that was killed.
//
// A `defer` cannot provide this. A SIGKILLed worker, an OOM, a machine
// that lost power mid-run -- each leaves a bearer on disk and the service
// manager restarts the worker without anything else noticing. Sweeping at
// startup is what bounds that exposure to the downtime rather than to
// forever.
func TestSweep_CleansUpAfterAKilledCockpit(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()

	// A session that was live when the process died: the config is
	// written and Remove is never called, exactly as a SIGKILL leaves it.
	m, err := writeMCPConfig(apps.IDClaudeCode, ws, "https://mcp.example.com/mcp", testBearer, "killed-1", state)
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	if _, err := os.Stat(m.configPath); err != nil {
		t.Fatalf("precondition: the config should exist: %v", err)
	}

	// The next cockpit process starts.
	if removed := Sweep(state); removed != 1 {
		t.Errorf("Sweep removed %d files, want 1", removed)
	}
	if found := grepTree(t, ws, testBearer); len(found) > 0 {
		t.Errorf("a killed session left the bearer behind in: %v", found)
	}
	if _, err := os.Stat(m.configPath); !os.IsNotExist(err) {
		t.Errorf(".mcp.json survived the sweep (%v)", err)
	}
	// The ledger entry goes with it, so the next boot has nothing to do.
	if removed := Sweep(state); removed != 0 {
		t.Errorf("a second sweep removed %d, want 0", removed)
	}
}

// New sessions leave the project configuration intact even after a crash.
func TestSweep_PreservesAPreexistingConfig(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	const mine = `{"mcpServers":{"my-own-server":{}}}`
	if err := os.WriteFile(filepath.Join(ws, ".mcp.json"), []byte(mine), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := writeMCPConfig(apps.IDClaudeCode, ws, "https://mcp.example.com/mcp", testBearer, "killed-2", state); err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	Sweep(state)

	got, err := os.ReadFile(filepath.Join(ws, ".mcp.json"))
	if err != nil {
		t.Fatalf("the user's own config was not restored: %v", err)
	}
	if string(got) != mine {
		t.Errorf("restored config = %s, want %s", got, mine)
	}
}

// TestRemove_PreservesAPreexistingConfig is the same guarantee on the
// clean path. An `open` or `attach` session can point at a real project,
// and destroying a config the user wrote is a worse outcome than
// anything this feature is worth.
func TestRemove_PreservesAPreexistingConfig(t *testing.T) {
	ws := t.TempDir()
	const mine = `{"mcpServers":{"my-own-server":{}}}`
	if err := os.WriteFile(filepath.Join(ws, ".mcp.json"), []byte(mine), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, err := writeMCPConfig(apps.IDClaudeCode, ws, "https://mcp.example.com/mcp", testBearer, "sess-5", t.TempDir())
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	// The private configuration is used while the project config stays intact.
	live, _ := os.ReadFile(m.configPath)
	if !strings.Contains(string(live), mcpServerName) {
		t.Fatalf("the session config is not in place: %s", live)
	}
	m.Remove()

	got, err := os.ReadFile(filepath.Join(ws, ".mcp.json"))
	if err != nil {
		t.Fatalf("the user's own config was not restored: %v", err)
	}
	if string(got) != mine {
		t.Errorf("restored config = %s, want %s", got, mine)
	}
}

// Existing crash ledgers may still name the old shared project config. The
// upgrade must remove that bearer and restore the saved project file.
func TestSweep_RestoresLegacyProjectConfig(t *testing.T) {
	ws, state := t.TempDir(), t.TempDir()
	path := filepath.Join(ws, ".mcp.json")
	for name, body := range map[string]string{path: testBearer, path + backupSuffix: "original project config"} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	legacy := &mcpConfig{sessionID: "legacy", configPath: path, ledger: filepath.Join(state, ledgerDirName, "legacy.json")}
	if err := legacy.writeLedger(); err != nil {
		t.Fatal(err)
	}
	if removed := Sweep(state); removed != 1 {
		t.Fatalf("swept %d files", removed)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "original project config" {
		t.Fatal("legacy project config not restored", err)
	}
}

// TestSanitizeLedgerName keeps a server-minted session id from naming a
// path outside the ledger directory.
func TestSanitizeLedgerName(t *testing.T) {
	cases := map[string]string{
		"sess-abc123":          "sess-abc123",
		"../../etc/passwd":     "______etc_passwd",
		"a/b":                  "a_b",
		"":                     "unnamed",
		"with space and:colon": "with_space_and_colon",
	}
	for in, want := range cases {
		if got := sanitizeLedgerName(in); got != want {
			t.Errorf("sanitizeLedgerName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeLedgerName(strings.Repeat("x", 500)); len(got) != 128 {
		t.Errorf("length = %d, want 128", len(got))
	}
}

// grepTree returns every file under root whose contents contain needle.
func grepTree(t *testing.T, root, needle string) []string {
	t.Helper()
	var found []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if strings.Contains(string(data), needle) {
			found = append(found, path)
		}
		return nil
	})
	return found
}

// ---------------------------------------------------------------------
// The Codex config.toml contract
// ---------------------------------------------------------------------
//
// VERIFIED 2026-09-07 against Codex's own configuration parser and its
// own tests -- `codex-rs/config/src/mcp_types.rs` and
// `codex-rs/config/src/mcp_types_tests.rs` at openai/codex@main -- and
// against the published reference at
// https://learn.chatgpt.com/docs/config-file/config-reference (which
// developers.openai.com/codex/config-reference now redirects to). This
// file previously said the shape was UNVERIFIED, and it was also wrong.
//
// What those sources establish:
//
//   - The table is `[mcp_servers.<id>]`.
//   - A `url` key with no `command` selects the streamable-HTTP
//     transport; neither key is the error "invalid transport".
//   - The credential travels either as `bearer_token_env_var` (the NAME
//     of an environment variable, never the secret) or as a static
//     `http_headers` entry. There is NO literal `bearer_token` key. It
//     exists in the raw config struct only so that its presence can be
//     refused -- `throw_if_set("streamable_http", "bearer_token", ...)`
//     -- and Codex's own test, `deserialize_rejects_inline_bearer_token_field`,
//     asserts the resulting error contains "bearer_token is not
//     supported".
//
// We carry the credential in `http_headers` rather than through an
// environment variable deliberately. The bearer then lives in the one
// file Remove() and Sweep() delete on every exit path, and Renew() can
// replace it there. Moving it into the process environment would put a
// per-run credential somewhere this file's deletion guarantee does not
// reach, and would make renewal meaningless -- an environment is fixed
// when the process starts.
//
// The parser below is DELIBERATELY NARROW: it reads exactly the grammar
// codexMCPBody emits and fails loudly on anything else. That is the
// point. This repository has no TOML library and one is not worth adding
// for a three-line document, and a body that outgrows this parser is a
// body nobody has verified -- so growing the body must mean growing the
// parser, in the same commit, with the source that justifies the new key.

const (
	testCodexEndpoint = "https://mcp.example.com/mcp?cluster=acme"
	codexServerTable  = "mcp_servers." + mcpServerName
)

// TestCodexMCPBody_MatchesCodexsOwnGrammar reads the generated body back
// the way Codex's parser does and asserts each field it will look for.
func TestCodexMCPBody_MatchesCodexsOwnGrammar(t *testing.T) {
	body, err := codexMCPBody(testCodexEndpoint, testBearer)
	if err != nil {
		t.Fatalf("codexMCPBody: %v", err)
	}
	doc := parseNarrowTOML(t, body)

	table, ok := doc[codexServerTable]
	if !ok {
		t.Fatalf("no [%s] table; Codex reads MCP servers from that table and nowhere else:\n%s", codexServerTable, body)
	}
	if got := table["url"]; got != testCodexEndpoint {
		t.Errorf("url = %q, want %q", got, testCodexEndpoint)
	}
	// `url` with no `command` is what selects the streamable-HTTP
	// transport. A `command` here would make Codex try to fork the
	// endpoint as a program.
	if got, ok := table["command"]; ok {
		t.Errorf("command = %q; a command key turns this into a stdio server", got)
	}
	if got := table["http_headers.Authorization"]; got != "Bearer "+testBearer {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer "+testBearer)
	}
}

// TestCodexMCPBody_NeverWritesABearerTokenKey guards the exact bug this
// body used to have.
//
// `bearer_token = "<jwt>"` is not merely ignored by Codex -- it is
// refused, by name, with "bearer_token is not supported for
// streamable_http". The server entry never loads, the app starts with no
// MemQL tools at all, and the run reports that as MemQL's tools being
// broken. Which is precisely the failure mcpconfig.go's own comments
// warn about, arriving through the file those comments are attached to.
func TestCodexMCPBody_NeverWritesABearerTokenKey(t *testing.T) {
	body, err := codexMCPBody(testCodexEndpoint, testBearer)
	if err != nil {
		t.Fatalf("codexMCPBody: %v", err)
	}
	for _, refused := range []string{"bearer_token", "bearer_token_env_var"} {
		if _, ok := parseNarrowTOML(t, body)[codexServerTable][refused]; ok {
			t.Errorf("the config carries %q:\n%s", refused, body)
		}
	}
}

// TestCodexMCPBody_RefusesValuesItCannotRenderHonestly. The renderer is
// hand-written, so a value carrying a quote or a newline could produce a
// document whose meaning is not the one intended -- a second key, a
// truncated URL. Neither a cluster endpoint nor a bearer can legitimately
// contain one, so refusing names the real cause instead of shipping a
// config that means something else.
func TestCodexMCPBody_RefusesValuesItCannotRenderHonestly(t *testing.T) {
	cases := map[string][2]string{
		"quote in endpoint":  {`https://x/"`, testBearer},
		"newline in bearer":  {testCodexEndpoint, "abc\ndef"},
		"carriage return":    {testCodexEndpoint, "abc\rdef"},
		"quote in the token": {testCodexEndpoint, `ab"cd`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := codexMCPBody(tc[0], tc[1]); err == nil {
				t.Error("a value TOML cannot carry here must be refused, not rendered")
			}
		})
	}
}

// TestWriteMCPConfig_CodexFileCarriesTheAuthorizationHeader checks the
// end-to-end write, not just the renderer: the file Codex will actually
// read has to hold the header, at 0600, because the bearer is in it.
func TestWriteMCPConfig_CodexFileCarriesTheAuthorizationHeader(t *testing.T) {
	ws := t.TempDir()
	m, err := writeMCPConfig(apps.IDCodex, ws, testCodexEndpoint, testBearer, "sess-codex-headers", t.TempDir())
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	defer m.Remove()

	path := m.configPath
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := parseNarrowTOML(t, data)[codexServerTable]["http_headers.Authorization"]; got != "Bearer "+testBearer {
		t.Errorf("Authorization header = %q", got)
	}
}

// TestRenew_CodexRewritesTheHeaderInPlace. Renewal is one of the three
// things standing in for a revocation the engine's JWKS-only verify path
// cannot offer, and it only means anything if the superseded bearer stops
// existing on disk. Codex needs its own case because the credential
// travels in a different key from Claude Code's.
func TestRenew_CodexRewritesTheHeaderInPlace(t *testing.T) {
	ws := t.TempDir()
	m, err := writeMCPConfig(apps.IDCodex, ws, testCodexEndpoint, testBearer, "sess-codex-renew", t.TempDir())
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	defer m.Remove()

	const next = "eyJhbGciOiJSUzI1NiJ9.the-renewed-bearer.sig"
	if err := m.Renew(next); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	data, err := os.ReadFile(m.configPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := parseNarrowTOML(t, data)[codexServerTable]["http_headers.Authorization"]; got != "Bearer "+next {
		t.Errorf("Authorization header = %q, want the renewed bearer", got)
	}
	if strings.Contains(string(data), testBearer) {
		t.Error("the superseded bearer survived the rewrite")
	}
}

// parseNarrowTOML reads the exact grammar codexMCPBody emits: comments,
// blank lines, `[table.name]` headers, `key = "string"`, and one level of
// inline table (`key = { "K" = "V" }`, flattened to `key.K`). Anything
// else fails the test rather than being skipped -- a silently ignored
// line is how a body that no longer says what it claims passes.
func parseNarrowTOML(t *testing.T, body []byte) map[string]map[string]string {
	t.Helper()
	doc := map[string]map[string]string{"": {}}
	table := ""
	for n, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				t.Fatalf("line %d: unterminated table header: %q", n+1, raw)
			}
			table = line[1 : len(line)-1]
			if _, ok := doc[table]; !ok {
				doc[table] = map[string]string{}
			}
			continue
		}
		key, rest := scanTOMLKey(t, line)
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "=") {
			t.Fatalf("line %d: not a key/value line: %q", n+1, raw)
		}
		if table == "" && key != "default_permissions" {
			t.Fatalf("line %d: key %q sits outside any table, so Codex would read it as a global setting", n+1, key)
		}
		rest = strings.TrimSpace(rest[1:])
		if strings.HasPrefix(rest, "{") {
			for k, v := range parseInlineTOMLTable(t, rest) {
				doc[table][key+"."+k] = v
			}
			continue
		}
		if rest == "true" || rest == "false" {
			doc[table][key] = rest
			continue
		}
		if strings.HasPrefix(rest, "[") {
			var names []string
			if err := json.Unmarshal([]byte(rest), &names); err != nil {
				t.Fatal(err)
			}
			doc[table][key] = rest
			continue
		}
		value, tail := scanTOMLString(t, rest)
		if strings.TrimSpace(tail) != "" {
			t.Fatalf("line %d: trailing text after the value: %q", n+1, raw)
		}
		doc[table][key] = value
	}
	return doc
}

// parseInlineTOMLTable reads `{ "K" = "V", K2 = "V2" }`.
func parseInlineTOMLTable(t *testing.T, s string) map[string]string {
	t.Helper()
	body := strings.TrimSpace(s)
	if !strings.HasPrefix(body, "{") || !strings.HasSuffix(body, "}") {
		t.Fatalf("not an inline table: %q", s)
	}
	out := map[string]string{}
	body = strings.TrimSpace(body[1 : len(body)-1])
	for body != "" {
		key, rest := scanTOMLKey(t, body)
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "=") {
			t.Fatalf("inline table entry %q has no value", key)
		}
		value, rest := scanTOMLString(t, strings.TrimSpace(rest[1:]))
		out[key] = value
		body = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ","))
	}
	return out
}

// scanTOMLKey reads a bare or quoted key and returns the rest of the line.
func scanTOMLKey(t *testing.T, s string) (string, string) {
	t.Helper()
	if strings.HasPrefix(s, `"`) {
		return scanTOMLString(t, s)
	}
	i := 0
	for i < len(s) {
		c := s[i]
		bare := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
		if !bare {
			break
		}
		i++
	}
	if i == 0 {
		t.Fatalf("expected a key at %q", s)
	}
	return s[:i], s[i:]
}

// scanTOMLString reads one double-quoted string and returns the rest.
// Go's %q and TOML's basic strings agree on the escapes this body can
// produce, so strconv.Unquote is the decoder rather than a hand-rolled
// one that could disagree with the renderer.
func scanTOMLString(t *testing.T, s string) (string, string) {
	t.Helper()
	if !strings.HasPrefix(s, `"`) {
		t.Fatalf("expected a quoted string at %q", s)
	}
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			value, err := strconv.Unquote(s[:i+1])
			if err != nil {
				t.Fatalf("unquote %q: %v", s[:i+1], err)
			}
			return value, s[i+1:]
		}
	}
	t.Fatalf("unterminated string at %q", s)
	return "", ""
}
