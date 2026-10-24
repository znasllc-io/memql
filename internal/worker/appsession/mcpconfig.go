package appsession

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
)

// mcpconfig.go writes the app's MCP configuration so a delegated run can
// reach MemQL's tools, and deletes it when the session ends
// (memql-cockpit#348).
//
// THE DELETION IS THE SECURITY CONTROL, not housekeeping. The bearer
// cannot be revoked: the engine's verify path is JWKS-only and DB-free by
// design -- that is what lets it work on every node without a lookup -- so
// there is no row to strike, and revoking one token would mean rotating
// the cluster's signing key and invalidating every other token in the
// cluster. Three things stand in for revocation, and this file owns two
// of them: the file being deleted on every exit path, and renewal in
// place so no single bearer is ever long-lived. (The third, an 8h hard
// cap, is enforced at the identity service.)
//
// So: workspace-scoped, never the user's global config. A global write
// outlives the session, survives a crash, and is shared with every other
// project on the machine -- three ways for a per-run credential to stop
// being per-run.
//
// A `defer` is not enough on its own. A cockpit that is SIGKILLed leaves
// the file behind, so every write is recorded in a ledger under the state
// directory and Sweep() clears the wreckage on the next start. The ledger
// records PATHS ONLY -- writing the bearer into a second file to keep
// track of the first one would defeat the entire exercise.

const (
	// mcpServerName is what MemQL calls itself inside the app's config.
	// Aliased from the harness, which names it in Claude Code's
	// permission grant and reads it back off the init event.
	mcpServerName = harness.MCPServerName

	// configFileMode is 0600 on every file this writes. The bearer is in
	// there.
	configFileMode os.FileMode = 0o600
	// configDirMode is 0700 for the same reason.
	configDirMode os.FileMode = 0o700

	// ledgerDirName is where the write ledger lives under the worker's
	// state directory.
	ledgerDirName = "appsessions"

	// backupSuffix marks a file moved aside so a pre-existing one is not
	// destroyed.
	backupSuffix = ".memql-session-backup"
)

// mcpConfig is one session's on-disk MCP configuration.
//
// Every method is safe to call concurrently with the others: renewal
// arrives on the stream while the run is in flight, and cleanup can be
// driven by either the run finishing or the stream dying.
type sessionMCPSettings struct {
	// ResumeRef selects retained provider state, never another session's bearer.
	ResumeRef     string
	InferenceOnly bool
	Tools         []string
	DenyPaths     []string
	RuntimePaths  []string
}

type mcpConfig struct {
	settings  sessionMCPSettings
	app       string
	workspace string
	endpoint  string
	sessionID string
	ledger    string

	mu sync.Mutex
	// configPath is the file carrying the bearer, rewritten on renewal.
	configPath string
	// backupPath is set when a pre-existing config was moved aside.
	backupPath string
	// created are private directories and auth links this made. Remove in
	// reverse order; directories are removed only when empty.
	created []string
	// env are extra environment entries the child process needs.
	env []string
	// removed guards against a double cleanup racing itself.
	removed bool
	// releaseHome excludes simultaneous writers to one resumed conversation.
	releaseHome func()
}

// Env returns the environment entries the app must run with. Empty for
// apps configured purely by a file in the workspace.
func (m *mcpConfig) Env() []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.env...)
}

// paths is where this configuration's bearer lives and, when a person's
// own configuration was moved aside for it, where theirs went.
func (m *mcpConfig) paths() (config, backup string) {
	if m == nil {
		return "", ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.configPath, m.backupPath
}

// writeMCPConfig lays down the app's MCP configuration for one session.
//
// stateDir is the worker's state directory; the ledger lives under it so
// a restart can find what a crash left behind. An empty stateDir disables
// the ledger (tests that do not exercise the sweep), which is stated here
// rather than silently tolerated.
func writeMCPConfig(app, workspace, endpoint, credential, sessionID, stateDir string, settings ...sessionMCPSettings) (*mcpConfig, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, errors.New("mcp config: workspace required")
	}
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("mcp config: mcp_endpoint required")
	}
	if strings.TrimSpace(credential) == "" {
		// An app with no credential reaches nothing over MCP and reports
		// that as "MemQL's tools are broken". Refusing here names the
		// real cause instead.
		return nil, errors.New("mcp config: credential required")
	}

	m := &mcpConfig{
		app:       app,
		workspace: workspace,
		endpoint:  endpoint,
		sessionID: sessionID,
	}
	if len(settings) > 0 {
		m.settings = settings[0]
	}
	if strings.TrimSpace(stateDir) != "" {
		m.ledger = filepath.Join(stateDir, ledgerDirName, sanitizeLedgerName(sessionID)+".json")
	}

	var err error
	switch app {
	case apps.IDClaudeCode:
		err = m.layoutClaudeCode()
	case apps.IDCodex:
		// Codex starts sandboxed filesystem helpers with its own executable.
		// User-installed binaries need an exact read grant beyond :minimal.
		if binary, lookupErr := exec.LookPath("codex"); lookupErr == nil {
			m.settings.RuntimePaths = append(m.settings.RuntimePaths, pathForms(binary)...)
		}
		err = m.layoutCodex()
	default:
		return nil, fmt.Errorf("mcp config: no writer for app %q", app)
	}
	if err != nil {
		m.Remove()
		return nil, err
	}

	// Record the plan BEFORE writing the credential. A crash between the
	// ledger write and the config write leaves a ledger entry pointing at
	// a file that does not exist, which the sweep shrugs off. The reverse
	// order leaves a bearer on disk that nothing knows to delete.
	if err := m.writeLedger(); err != nil {
		m.Remove()
		return nil, err
	}
	if err := m.Renew(credential); err != nil {
		m.Remove()
		return nil, err
	}
	return m, nil
}

// Claude receives this exact path through --mcp-config. Keep each session's
// bearer private even when sessions share a workspace; never move or overwrite
// the person's project .mcp.json. Old crash ledgers still restore their backups.
func (m *mcpConfig) layoutClaudeCode() error {
	dir := filepath.Join(m.workspace, sessionScaffoldDir)
	if err := os.MkdirAll(dir, configDirMode); err != nil {
		return fmt.Errorf("mcp config: workspace: %w", err)
	}
	f, err := os.CreateTemp(dir, "mcp-*.json")
	if err != nil {
		return fmt.Errorf("mcp config: session file: %w", err)
	}
	m.configPath = f.Name()
	m.created = append(m.created, dir)
	return f.Close()
}

// layoutCodex prepares a conversation-scoped CODEX_HOME inside the workspace.
//
// Codex reads MCP servers from config.toml in its home directory, and its
// home defaults to the USER PROFILE -- which is exactly the global write
// this must not do. So the session gets its own home, pointed at by
// CODEX_HOME. Its credential is deleted at session end; provider state remains
// for a later attach in this same workspace, identified by the returned ref.
//
// The user's auth.json is SYMLINKED rather than copied. A copy would put
// a second, longer-lived credential on disk (the app's own, not this
// session's) with no reason to; the link keeps the credential where it
// already lives and where the user manages it.
func (m *mcpConfig) layoutCodex() error {
	dir := filepath.Join(m.workspace, sessionScaffoldDir)
	if err := os.MkdirAll(dir, configDirMode); err != nil {
		return fmt.Errorf("mcp config: session directory: %w", err)
	}
	// New conversations get private homes; attach reuses the indexed provider
	// state under an exclusive lock, with a fresh credential for this session.
	home, err := m.codexHome(dir)
	if err != nil {
		return fmt.Errorf("mcp config: codex home: %w", err)
	}
	// Keep provider state and its lock inode. Only credentials and auth links
	// belong to this session's cleanup ledger.
	m.created = append(m.created, dir)
	m.configPath = filepath.Join(home, "config.toml")
	m.env = []string{"CODEX_HOME=" + home}

	if userHome, err := os.UserHomeDir(); err == nil {
		auth := filepath.Join(userHome, ".codex", "auth.json")
		if _, err := os.Stat(auth); err == nil {
			link := filepath.Join(home, "auth.json")
			// A failed link is not fatal: the app simply runs
			// signed-out, which detection already reported and which the
			// engine already refuses to route to.
			if os.Symlink(auth, link) == nil {
				m.created = append(m.created, link)
			}
		}
	}
	return nil
}

// Renew rewrites the configuration with a new bearer, in place.
//
// AppSessionControl{renew_credential} arrives before the current bearer
// expires so no single one is long-lived. The rewrite is atomic (temp
// file, 0600, rename) so a reader never sees a half-written config.
//
// KNOWN LIMITATION, recorded here and on memql-cockpit#348 rather than
// worked around silently: Claude Code reads its MCP configuration at
// STARTUP. Rewriting the file mid-run therefore serves a restart, not the
// process already running -- an hour-long run whose bearer expires will
// lose MemQL's tools even though the file on disk is current. The engine
// can shorten the run instead; that is its call to make, and it can only
// make it if this says so out loud.
func (m *mcpConfig) Renew(credential string) error {
	if m == nil {
		return errors.New("mcp config: not initialized")
	}
	if strings.TrimSpace(credential) == "" {
		return errors.New("mcp config: empty credential")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.removed {
		return errors.New("mcp config: session already cleaned up")
	}

	var body []byte
	var err error
	switch m.app {
	case apps.IDClaudeCode:
		if m.settings.InferenceOnly {
			body = []byte(`{"mcpServers":{}}`)
		} else {
			body, err = claudeCodeMCPBody(m.endpoint, credential)
		}
	case apps.IDCodex:
		body, err = codexMCPBody(m.endpoint, credential, m.settings)
	default:
		err = fmt.Errorf("mcp config: no writer for app %q", m.app)
	}
	if err != nil {
		return err
	}
	return writeFileAtomic(m.configPath, body, configFileMode)
}

// Remove deletes this session's credentials and clears its ledger entry;
// provider conversation state remains for attach. Safe to call repeatedly --
// which it will be, because the run finishing and the stream dying are
// independent events and either one must be sufficient.
func (m *mcpConfig) Remove() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.removed {
		return
	}
	m.removed = true

	if m.configPath != "" {
		_ = os.Remove(m.configPath)
	}
	if m.backupPath != "" {
		// Put the user's own config back. If this fails, the backup is
		// still on disk under a name that says what it is, which is
		// recoverable; silently discarding it would not be.
		_ = os.Rename(m.backupPath, m.configPath)
	}
	// Deepest first, and only if empty -- these are inside the
	// workspace, which may hold the run's actual output.
	for i := len(m.created) - 1; i >= 0; i-- {
		_ = os.Remove(m.created[i])
	}
	if m.ledger != "" {
		_ = os.Remove(m.ledger)
	}
	if m.releaseHome != nil {
		m.releaseHome()
		m.releaseHome = nil
	}
}

// ledgerEntry is what the sweep reads. PATHS ONLY: recording the bearer
// here to keep track of the file that holds the bearer would defeat the
// whole exercise.
type ledgerEntry struct {
	SessionID string   `json:"sessionId"`
	Paths     []string `json:"paths"`
	Dirs      []string `json:"dirs"`
}

func (m *mcpConfig) writeLedger() error {
	if m.ledger == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.ledger), configDirMode); err != nil {
		return fmt.Errorf("mcp config: ledger dir: %w", err)
	}
	entry := ledgerEntry{SessionID: m.sessionID, Dirs: m.created}
	if m.configPath != "" {
		entry.Paths = append(entry.Paths, m.configPath)
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("mcp config: ledger: %w", err)
	}
	return writeFileAtomic(m.ledger, body, configFileMode)
}

// Sweep deletes the MCP configuration files any previous cockpit process
// left behind, and clears their ledger entries.
//
// This is the half of the deletion guarantee a `defer` cannot provide. A
// SIGKILLed worker, an OOM, a machine that lost power mid-run: all of
// them leave a bearer on disk, and the service manager restarts the
// worker without anything else noticing. Sweeping at startup is what
// bounds that exposure to the downtime rather than to forever.
//
// It returns the number of files removed so the caller can log a count --
// a non-zero one at boot is worth seeing.
func Sweep(stateDir string) int {
	if strings.TrimSpace(stateDir) == "" {
		return 0
	}
	dir := filepath.Join(stateDir, ledgerDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			// Unreadable ledger: drop it. Leaving it means sweeping it
			// again on every boot forever, and it names nothing this can
			// act on.
			_ = os.Remove(path)
			continue
		}
		var entry ledgerEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			_ = os.Remove(path)
			continue
		}
		for _, p := range entry.Paths {
			if err := os.Remove(p); err == nil {
				removed++
			}
			// Restore a config that was moved aside, if the crash
			// happened while ours was in place.
			backup := p + backupSuffix
			if _, err := os.Lstat(backup); err == nil {
				_ = os.Rename(backup, p)
			}
		}
		for i := len(entry.Dirs) - 1; i >= 0; i-- {
			_ = os.Remove(entry.Dirs[i])
		}
		_ = os.Remove(path)
	}
	return removed
}

// claudeCodeMCPBody renders a project-scoped .mcp.json.
//
// The shape mirrors what `claude mcp add --transport http <name> <url>
// --header "Authorization: Bearer <token>"` produces, which is the form
// the engine's own mcp-connect runbook documents.
func claudeCodeMCPBody(endpoint, credential string) ([]byte, error) {
	doc := map[string]any{
		"mcpServers": map[string]any{
			mcpServerName: map[string]any{
				"type": "http",
				"url":  endpoint,
				"headers": map[string]any{
					"Authorization": "Bearer " + credential,
				},
			},
		},
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("mcp config: render claude-code config: %w", err)
	}
	return append(body, '\n'), nil
}

// codexMCPBody renders a config.toml for the per-session CODEX_HOME.
//
// VERIFIED 2026-09-07 against Codex's own configuration parser and its
// own tests (codex-rs/config/src/mcp_types.rs and mcp_types_tests.rs at
// openai/codex@main) and the published reference at
// https://learn.chatgpt.com/docs/config-file/config-reference. The table
// is `[mcp_servers.<id>]`; a `url` with no `command` selects the
// streamable-HTTP transport; and the credential travels either as
// `bearer_token_env_var` -- the NAME of an environment variable, never
// the secret -- or as a static `http_headers` entry.
//
// THE `bearer_token` KEY THIS USED TO WRITE IS REFUSED BY CODEX. It
// survives in the raw config struct only so that its presence can be
// rejected by name, and Codex's own test
// `deserialize_rejects_inline_bearer_token_field` asserts the error
// "bearer_token is not supported". A config carrying it does not load
// unauthenticated -- it does not load at all, and the app then runs with
// no MemQL tools and reports that as MemQL being broken.
//
// The header form is chosen over `bearer_token_env_var` on purpose: it
// keeps the bearer in the ONE file Remove() and Sweep() delete on every
// exit path, and it keeps Renew() meaningful. A credential moved into
// the process environment sits somewhere this file's deletion guarantee
// does not reach, and cannot be replaced in place afterwards.
//
// String values are quoted and the small generated grammar is covered by
// the config tests, including tool grants, workspace bounds and renewal.
func codexMCPBody(endpoint, credential string, options ...sessionMCPSettings) ([]byte, error) {
	if strings.ContainsAny(endpoint, "\"\n\r") || strings.ContainsAny(credential, "\"\n\r") {
		// Neither value can legitimately contain these, and a hand-rolled
		// renderer that let one through would produce a config whose
		// meaning is not the one intended.
		return nil, errors.New("mcp config: endpoint or credential contains a character TOML cannot carry here")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by memql for app session; deleted when the session ends.\n")
	var settings sessionMCPSettings
	if len(options) > 0 {
		settings = options[0]
	}
	fmt.Fprintln(&b, `default_permissions = "memql_session"`)
	if !settings.InferenceOnly {
		fmt.Fprintf(&b, "[mcp_servers.%s]\n", mcpServerName)
		fmt.Fprintln(&b, "required = true")
		if len(settings.Tools) > 0 {
			names, _ := json.Marshal(settings.Tools)
			fmt.Fprintf(&b, "enabled_tools = %s\n", names)
		}
		fmt.Fprintf(&b, "url = %q\n", endpoint)
		fmt.Fprintf(&b, "http_headers = { %q = %q }\n", "Authorization", "Bearer "+credential)
		for _, name := range settings.Tools {
			fmt.Fprintf(&b, "[mcp_servers.%s.tools.%q]\napproval_mode = \"approve\"\n", mcpServerName, name)
		}
	}
	// App consent grants this workspace. Keep Codex's baseline protected
	// directories and confine command reads to runtime paths + the workspace.
	fmt.Fprintln(&b, `[permissions.memql_session]
extends = ":workspace"
[permissions.memql_session.filesystem]
":root" = "deny"
":minimal" = "read"`)
	for _, path := range settings.RuntimePaths {
		fmt.Fprintf(&b, "%q = \"read\"\n", path)
	}
	for _, path := range settings.DenyPaths {
		fmt.Fprintf(&b, "%q = \"deny\"\n", path)
	}
	fmt.Fprintln(&b, `[permissions.memql_session.filesystem.":workspace_roots"]
".memql-session" = "deny"
[permissions.memql_session.network]
enabled = false`)
	return []byte(b.String()), nil
}

// writeFileAtomic writes via a temp file in the same directory and
// renames, so a reader never observes a half-written config. The temp
// file carries the final mode from creation -- a chmod after the write
// would leave a window where the bearer is world-readable.
func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, configDirMode); err != nil {
		return fmt.Errorf("mcp config: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".memql-mcp-*")
	if err != nil {
		return fmt.Errorf("mcp config: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()
	// The two error paths join the close error rather than dropping it,
	// the same way Library.Pull does and for the same reason: a close
	// on a writable handle is where a short write surfaces, and this
	// file is the one carrying the session's bearer. `_ =` would be
	// enough to silence a linter and would still throw away the one
	// message that says the disk is full.
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("mcp config: chmod: %w", errors.Join(err, tmp.Close()))
	}
	if _, err := tmp.Write(body); err != nil {
		return fmt.Errorf("mcp config: write: %w", errors.Join(err, tmp.Close()))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("mcp config: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("mcp config: rename: %w", err)
	}
	return nil
}

// sanitizeLedgerName keeps a server-minted session id from naming a path
// outside the ledger directory.
func sanitizeLedgerName(id string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		}
		return '_'
	}, id)
	if cleaned == "" {
		return "unnamed"
	}
	if len(cleaned) > 128 {
		cleaned = cleaned[:128]
	}
	return cleaned
}
