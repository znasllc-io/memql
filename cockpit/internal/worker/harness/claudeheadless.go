package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// claudeheadless.go drives Claude Code through its own headless
// protocol: ONE PROCESS PER TURN, resumed by session id.
//
// That is the shape the app offers rather than a limitation worked
// around. `claude -p` takes one prompt, runs the agent loop to
// completion and exits; continuity is `--resume <session_id>` on the
// NEXT process. There is nothing to keep alive between turns, which is
// why Start forks nothing and why a mid-session credential renewal
// lands honestly -- the replacement .mcp.json is read when the next
// process starts, not pretended into a running one.
//
// THE SESSION ID IS THE ONLY THREAD. Every event carries `session_id`
// and this client keeps the last one it saw, including from a turn that
// died before its result event. Lose it and the next turn starts a
// fresh conversation that looks identical from the outside and has
// forgotten everything -- the failure the TurnResult.AppSessionRef
// comment names.

const (
	// claudeReadBufBytes is one read from a child stream. stream-json
	// lines are routinely tens of kilobytes (a system/init event listing
	// every tool and MCP server is ~10 KB on a loaded machine), so a
	// small buffer would spend the whole run refilling.
	claudeReadBufBytes = 64 << 10

	// claudeMaxEventBytes caps ONE logical line before the client gives
	// up on parsing it and streams it out as stdout instead.
	//
	// It exists because nothing bounds what an app prints on a line: a
	// tool_result carrying a large file arrives as a single JSON object
	// with no newline in it, and a client that accumulates until the
	// newline is an out-of-memory kill on somebody's laptop. The value
	// mirrors appsession/process.go's maxLineBytes so the two readers
	// give up at the same size rather than at two sizes nobody wrote
	// down. Over it the bytes are still DELIVERED (as stdout, in
	// pieces); only the parse is abandoned.
	claudeMaxEventBytes = 1 << 20

	// claudeStderrKeepBytes is how much of the tail of stderr is held to
	// explain a failed turn. Claude Code puts the actionable sentence
	// there and nowhere else -- a bad --resume id prints "No
	// conversation found with session ID: ..." on stderr while stdout
	// carries only a machine-readable result event -- so an error that
	// named the exit code alone would send an operator hunting.
	claudeStderrKeepBytes = 4 << 10
)

// Claude Code stream-json event types, as claude 2.1.263 emits them
// under `-p --output-format stream-json --verbose` (verified
// 2026-09-07). Anything not named here is structure this client does
// not interpret and forwards whole; the set is open on purpose, because
// Claude Code adds event types between releases and a client that
// dropped what it did not recognise would go quiet on the new ones.
const (
	claudeTypeAssistant = "assistant"
	claudeTypeUser      = "user"
	claudeTypeResult    = "result"
	// claudeTypeSystem with subtype init opens every turn and names the
	// model the session runs on (2.1.270: `"model":"claude-haiku-4-5-..."`)
	// and the working directory it runs in, which the recording uses.
	claudeTypeSystem  = "system"
	claudeSubtypeInit = "init"
)

// Content block types inside an assistant or user message. The last two
// are the recording's (claudeactions.go): a call, and its result.
const (
	claudeBlockText       = "text"
	claudeBlockThinking   = "thinking"
	claudeBlockToolUse    = "tool_use"
	claudeBlockToolResult = "tool_result"
)

// claudeHeadless is the Harness for `claude -p`.
//
// The struct holds a SESSION, not a process: the spec every turn forks
// from, and the app's own session id as it currently stands. A turn's
// mutable state lives in claudeTurn instead, so two turns cannot see
// each other's half-parsed stream.
type claudeHeadless struct {
	mu   sync.Mutex
	spec Spec
	// knobs are the session's level as this app spells it, settled once in
	// Start and passed to every turn's process -- a resumed turn included,
	// because each turn is a fresh process that knows only its own argv.
	knobs   Knobs
	ref     string
	started bool
	closed  bool
	running bool
	// rec is the session's recording. It outlives every turn's process,
	// because the action seq runs across the whole session.
	rec *recording
}

// Name implements Harness.
func (h *claudeHeadless) Name() string { return HarnessClaudeHeadless }

// Start records the spec. It forks NOTHING, because a turn is a process
// and there is no turn yet.
//
// It therefore does not check that Spec.Binary exists, and that is a
// decision rather than an omission. A probe here would be a DIFFERENT
// check from the one that matters: the launcher owns process creation
// and its environment, so a binary this package can resolve is not
// necessarily one the launcher can, nor the other way round -- and a
// Start that answered either way would still have to be re-answered at
// the first Turn. So the honest report is the one the first Turn makes,
// which carries the app's real exit status and its own stderr. What
// Start does refuse is the pair of programming errors that would
// otherwise surface far from their cause: a nil Launch (a nil-pointer
// panic inside the first turn, or worse, a silent fall back to forking
// outside the app-session supervisor) and an empty Binary (an argv[0]
// of "" that the launcher reports as a mysterious exec failure).
//
// Workspace is deliberately NOT re-validated. The session runner
// resolves and refuses it before it ever builds a Spec -- absolute,
// existing, and past this machine's own policy check -- and a second
// copy of that rule here would be a second place to keep in step with
// it.
func (h *claudeHeadless) Start(_ context.Context, spec Spec) error {
	if spec.Launch == nil {
		return errors.New("harness: claude-headless was given no Launch function; " +
			"every process must come from the app-session supervisor")
	}
	if strings.TrimSpace(spec.Binary) == "" {
		return errors.New("harness: claude-headless was given no binary to run")
	}
	// The level is settled here rather than at the first turn, so a level
	// this app cannot run at fails while the session is still starting --
	// before a process, and before a turn that would read as a bad prompt.
	knobs, err := spec.knobs(HarnessClaudeHeadless)
	if err != nil {
		return fmt.Errorf("harness: claude-headless: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.spec = spec
	h.knobs = knobs
	h.ref = strings.TrimSpace(spec.ResumeRef)
	h.rec = newRecording()
	h.started = true
	h.closed = false
	return nil
}

// Close releases nothing, because between turns this harness holds
// nothing: no process, no pipe, no file. It still latches, so that a
// follow-up arriving after the session ended is REFUSED rather than
// quietly starting an agent on somebody's machine with nobody left
// watching it.
func (h *claudeHeadless) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	return nil
}

// Turn runs one prompt to completion in its own process.
//
// A turn that ran and failed still returns what it gathered: the real
// exit code, the session ref so the conversation stays resumable, and
// whatever the result event managed to say. The error names what
// failed.
//
// Turns are refused rather than queued while one is in flight. Two
// processes resuming a single Claude Code session id both append to the
// one transcript on disk and neither sees the other's turn, so a
// concurrent follow-up would not be a slow answer -- it would be a
// conversation that silently forgot half of itself.
func (h *claudeHeadless) Turn(ctx context.Context, prompt string, sink Sink) (TurnResult, error) {
	h.mu.Lock()
	spec, knobs, ref, started, closed, running, rec := h.spec, h.knobs, h.ref, h.started, h.closed, h.running, h.rec
	if started && !closed && !running {
		h.running = true
	}
	h.mu.Unlock()

	switch {
	case !started:
		return TurnResult{ExitCode: -1}, errors.New("harness: claude-headless was asked for a turn before Start")
	case closed:
		return TurnResult{ExitCode: -1, AppSessionRef: ref},
			errors.New("harness: claude-headless was asked for a turn after Close; the session is over")
	case running:
		return TurnResult{ExitCode: -1, AppSessionRef: ref},
			errors.New("harness: claude-headless is already running a turn; a session's turns are a sequence")
	}
	defer func() {
		h.mu.Lock()
		h.running = false
		h.mu.Unlock()
	}()
	rec.startTurn()

	argv := claudeArgv(spec, knobs, ref)
	proc, err := spec.Launch(ctx, spec.Workspace, argv, claudeEnv(spec.Env), true)
	if err != nil {
		return TurnResult{ExitCode: -1, AppSessionRef: ref},
			fmt.Errorf("harness: could not start %s: %w", spec.Binary, err)
	}

	turn := &claudeTurn{sink: sink, rec: rec, cwd: spec.Workspace, inferenceOnly: spec.InferenceOnly}
	if strings.TrimSpace(spec.MCPConfigPath) != "" && !spec.InferenceOnly {
		// Only a session that configured MemQL's server can be let down
		// by it; the stop is the process group, as a cancel's is.
		turn.mcpServer = MCPServerName
		turn.stop = proc.Terminate
	}

	// The context is watched here as well as by the launcher because
	// Process.Terminate is the seam that stops the whole process GROUP.
	// A claude turn spawns tools which spawn compilers; a cancel that
	// reaped only the direct child would leave those running with
	// nothing watching them.
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			proc.Terminate()
		case <-watchDone:
		}
	}()

	// Text stdin carries one complete prompt, then EOF. Putting the prompt in
	// argv can exceed the host's per-argument exec limit before Claude starts.
	// This private pipe is not an interactive engine-input channel. Drain both
	// outputs while writing so a child that prints before reading cannot
	// deadlock on full pipes. A failed delivery stops the supervised group;
	// never retry a partial prompt as a second turn or accept its answer.
	inputDone := make(chan error, 1)
	go func() {
		err := writeClaudePrompt(proc.Stdin(), prompt)
		if err != nil {
			proc.Terminate()
		}
		inputDone <- err
	}()

	var pumps sync.WaitGroup
	pumps.Add(1)
	go func() {
		defer pumps.Done()
		turn.pumpStderr(proc.Stderr())
	}()
	turn.pumpStdout(proc.Stdout())
	pumps.Wait()

	// A call begun in this turn and never answered never will be: the next
	// turn is a new process with no memory of this one's ids. It is
	// recorded as incomplete rather than dropped (recording.flush).
	for _, a := range rec.flush() {
		turn.record(a)
	}

	waitErr := proc.Wait()
	inputErr := <-inputDone
	close(watchDone)

	code := proc.ExitCode()
	res := TurnResult{ExitCode: code, AppSessionRef: turn.sessionID}
	if res.AppSessionRef == "" {
		res.AppSessionRef = ref
	}
	h.rememberRef(res.AppSessionRef)

	ev := turn.result
	if ev != nil {
		res.Text = ev.Result
		res.Usage = ev.usage()
	}
	// What the app says served this turn -- for a failed turn too, since a
	// run that spent tokens on a model before it failed still ran on it.
	// Effort is left empty on purpose: Claude Code states none anywhere in
	// its stream (2.1.270), so the only value that could go here is the
	// --effort this client passed, and a request is not a report.
	res.Model = turn.servedModel()

	// A turn stopped because MemQL's server could not serve it failed for
	// THAT reason, whatever status the stopped process exited with: the
	// signal is this client's doing, and naming it would send an operator
	// looking for a crash.
	if turn.mcpFailure != "" {
		return res, errors.New(turn.mcpFailure)
	}

	// The exit status is the first question -- but a failed turn can still
	// carry the structured answer the schema asked for, and it goes back
	// beside the failure rather than being dropped with it: Claude Code can
	// answer and still exit non-zero, and AppSessionEnd keeps result_json
	// apart from the error precisely so the one readable part of such a run
	// survives. structured() accepts only a JSON object or array, so an
	// error message in `result` is never mistaken for one.
	ctxErr := ctx.Err()
	if inputErr != nil || ctxErr != nil || code != 0 || waitErr != nil || (ev != nil && ev.IsError) {
		if ev != nil && strings.TrimSpace(spec.ResponseSchema) != "" {
			if structured, ok := ev.structured(); ok {
				res.ResultJSON = structured
			}
		}
		if ctxErr != nil {
			return res, fmt.Errorf("harness: claude-headless turn stopped: %w", ctxErr)
		}
		if inputErr != nil {
			return res, fmt.Errorf("harness: could not deliver the complete Claude prompt: %w", inputErr)
		}
		return res, turn.failure(spec, code, waitErr)
	}
	if ev == nil {
		return res, fmt.Errorf("harness: the %s turn ended with status 0 but printed no result event, "+
			"so nothing is known about what it answered or spent", spec.Binary)
	}
	if strings.TrimSpace(spec.ResponseSchema) == "" {
		return res, nil
	}
	structured, ok := ev.structured()
	if !ok {
		return res, ErrNoStructuredResult
	}
	res.ResultJSON = structured
	return res, nil
}

// rememberRef carries the app's session id into the next turn.
func (h *claudeHeadless) rememberRef(ref string) {
	if strings.TrimSpace(ref) == "" {
		return
	}
	h.mu.Lock()
	h.ref = ref
	h.mu.Unlock()
}

// writeClaudePrompt owns this turn's finite input. Closing even on failure
// makes EOF explicit; a short write or failed close is not a delivered prompt.
// The supervisor's group termination unblocks a write during cancellation.
func writeClaudePrompt(stdin io.WriteCloser, prompt string) error {
	if stdin == nil {
		return errors.New("the supervised process has no prompt pipe")
	}
	n, err := io.WriteString(stdin, prompt)
	if err == nil && n != len(prompt) {
		err = io.ErrShortWrite
	}
	return errors.Join(err, stdin.Close())
}

// claudeArgv builds the options for one print-mode turn. The complete prompt
// travels separately over text stdin, which Claude documents as an alternative
// to an argument (https://code.claude.com/docs/en/errors). This avoids both
// exec's argument-size ceiling and interpreting a leading dash as an option.
// `--` still terminates the variadic options; nothing follows it.
//
// `--verbose` is not decoration either. Under `--print`,
// `--output-format=stream-json` is REFUSED without it ("When using
// --print, --output-format=stream-json requires --verbose"), so
// dropping it turns every turn into an immediate failure.
//
// The level's knobs go straight after it, before --mcp-config, and each
// is a single-value option (`--model <model>`, `--effort <level>` on
// 2.1.270), so neither can swallow a following argument the way the
// variadic can. CheckKnobs has already refused a value that begins with
// a dash, which is the one value that would read as a flag of its own.
//
// Then the session's GRANT (claudeGrantArgs), which is the machine
// owner's decision and not the user's settings file.
func claudeArgv(spec Spec, knobs Knobs, ref string) []string {
	argv := []string{spec.Binary, "-p", "--input-format", "text", "--output-format", "stream-json", "--verbose"}
	if knobs.Model != "" {
		argv = append(argv, "--model", knobs.Model)
	}
	if knobs.Effort != "" {
		argv = append(argv, "--effort", knobs.Effort)
	}
	if spec.InferenceOnly {
		argv = append(argv, "--tools", "", "--strict-mcp-config", "--include-partial-messages", "--system-prompt", "Follow the supplied prompt and return its requested answer.")
	} else {
		argv = append(argv, claudeGrantArgs(spec.DenyPaths)...)
	}
	if path := strings.TrimSpace(spec.MCPConfigPath); path != "" {
		argv = append(argv, "--mcp-config", path)
	}
	if r := strings.TrimSpace(ref); r != "" {
		argv = append(argv, "--resume", r)
	}
	// A schema the caller did not ask for changes what the app says, so
	// the flag appears only when Spec carries one.
	if schema := strings.TrimSpace(spec.ResponseSchema); schema != "" {
		argv = append(argv, "--json-schema", schema)
	}
	return append(argv, "--")
}

// claudeGrantArgs is what a session's Claude Code may do, as flags.
//
// THE GRANT IS THE MACHINE OWNER'S DECISION, not the user's settings file:
// inside a session Claude Code may edit files and run shell commands INSIDE
// THE SESSION WORKSPACE, and call MemQL's tools over MCP -- nothing else,
// and nothing a personal ~/.claude/settings.json can widen or narrow.
// Before this, `claude -p` ran with no permission flags at all, so what a
// session could do was whatever that file happened to say on this machine.
//
// Each flag below was checked against `claude --help` and the binary of
// 2.1.275 and 2.1.283; none of it has been run against a real prompt here.
//
//   - `--setting-sources=` loads NO settings file: not the user's, not the
//     workspace's .claude/settings.json, not settings.local.json (the app's
//     own parser returns an empty list for ""). What still loads is the
//     settings passed on this command line -- the grant -- and the machine's
//     MANAGED settings, which are its administrator's to impose. The `=`
//     form is the one Claude Code respawns itself with, and it keeps the
//     empty value one token rather than an argument that could go missing.
//   - `--strict-mcp-config` loads no MCP server but the ones --mcp-config
//     names, which is the session's own. Without it the user's global
//     servers ride along on a session somebody else started.
//   - `--permission-mode dontAsk` REFUSES whatever is not granted below
//     rather than asking. Nobody is at a `claude -p` to answer, and `-p`
//     already refused (the recorded 2.1.270 refusals in
//     claudeactions_test.go); the mode says so instead of relying on it.
//   - `--allowedTools` grants `Edit(/**)` and `Read(/**)` -- a command-line
//     rule's leading "/" is the working directory, which is the workspace,
//     so both stop at its edge. Edit rules govern every file-writing tool
//     (Edit, Write) and Read rules the reading ones (Read, Glob, Grep). And
//     `mcp__memql`, every tool of MemQL's server. It is variadic like
//     --mcp-config, which is fine only because every flag precedes `--`.
//   - Bash has NO rule of its own. The sandbox approves it
//     (claudeSandboxSettings), and approves a command BECAUSE it runs
//     sandboxed -- so a command the sandbox did not take has no approval
//     in this grant, and dontAsk refuses it.
//   - `--disallowedTools` denies the file tools every one of deny --
//     Spec.DenyPaths: this worker's own files and this machine's fs.deny
//     list -- for reading and for writing (claudeDenyRules). A deny rule
//     beats the workspace's allow rule, so a path reached through a
//     symlink the session made in its workspace is refused all the same.
//     Absent when there is nothing to deny.
//
// `--tools` is deliberately NOT passed. It would narrow the built-in set to
// the grant's tools, but it would take ToolSearch with it -- the tool Claude
// Code loads a deferred MCP tool with before it can call one (the recorded
// turn in claudeactions_test.go) -- and whether it spares the synthetic
// StructuredOutput tool a --json-schema answer rides could not be verified
// without a real run. The permission mode is what refuses everything else.
func claudeGrantArgs(deny []string) []string {
	args := []string{
		"--setting-sources=",
		"--strict-mcp-config",
		"--permission-mode", "dontAsk",
		"--allowedTools", "Edit(/**) Read(/**) mcp__" + MCPServerName,
	}
	if rules := claudeDenyRules(deny); rules != "" {
		args = append(args, "--disallowedTools", rules)
	}
	return append(args, "--settings", claudeSandboxSettings(deny))
}

// claudeDenyRules is the file tools' half of the deny list: a Read and an
// Edit rule for every path, space-separated as --disallowedTools takes them.
//
// `//` is an absolute path in a rule (a single `/` would be the working
// directory). The path is written BARE, not as `path/**`: a rule matches the
// way a gitignore entry does, so a directory's rule covers everything under
// it and a file's covers the file -- and a glob rule is what the Linux
// sandbox cannot use (it merges these rules into its own lists, and skips or
// expands a glob there).
//
// A path the rule syntax would misread gets NO rule rather than a wrong
// one: a parenthesis ends the rule early (Claude Code splits the list on
// spaces and commas outside parentheses, 2.1.283), and a glob character
// matches more or less than the path. Such a path is still denied to every
// shell command by the sandbox's own list, which is JSON and carries any
// path whole; that the file tools could reach it depends on the workspace
// overlapping it, which the session refuses.
func claudeDenyRules(deny []string) string {
	var rules []string
	for _, path := range deny {
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "()*?[]\\") {
			continue
		}
		rules = append(rules, "Read(/"+path+")", "Edit(/"+path+")")
	}
	return strings.Join(rules, " ")
}

// claudeSandboxSettings turns on Claude Code's own sandbox for every shell
// command a session runs (Seatbelt on macOS, bubblewrap on Linux), passed
// inline as `--settings` -- the one settings source the grant keeps.
//
// The sandbox confines WRITES to the working directory (plus the temporary
// directory it always allows), keeps the app's mandatory write protections
// (.mcp.json -- this session's bearer --, .git/hooks, .git/config, shell rc
// files), and sends a command's network through its own proxy, which
// approves domains one at a time; this grant approves none. READS it
// confines only away from deny (Spec.DenyPaths), and that is what keeps a
// sandboxed `cat ~/.memql/worker.yaml > out.txt` from leaving the worker
// token in a file the session then pushes to the Library. Elsewhere a
// sandboxed command can still read outside the workspace.
//
//   - failIfUnavailable: a machine that cannot sandbox fails the turn at
//     start in the app's own words (a Linux box without bubblewrap, say),
//     rather than warning and running every command unsandboxed.
//   - autoAllowBashIfSandboxed: the approval Bash gets instead of a rule.
//   - allowUnsandboxedCommands=false: the Bash tool's dangerouslyDisableSandbox
//     parameter is ignored, so there is no way out of the sandbox from a
//     single call.
//   - filesystem.denyRead / denyWrite: deny, in the plain absolute form the
//     sandbox's settings document (Claude Code's own test harness passes
//     its lists the same way). Denied for writing as well, although writes
//     stop at the workspace already: the temporary directory is writable by
//     every sandboxed command, and a deny entry there stays unwritten. The
//     workspace itself never overlaps one (the session refuses it), so this
//     takes nothing from the grant. On Linux a denied directory is covered
//     by an empty one and a denied FILE by /dev/null -- the default
//     fs.deny's /etc/passwd included, so a sandboxed command there cannot
//     look up a user name. On macOS a denied read fails with EPERM, which
//     git treats as fatal for an XDG config under a denied ~/.config
//     (checked with sandbox-exec; docs/local-apps.md says what to do).
func claudeSandboxSettings(deny []string) string {
	type filesystem struct {
		DenyRead  []string `json:"denyRead"`
		DenyWrite []string `json:"denyWrite"`
	}
	type sandbox struct {
		Enabled                  bool        `json:"enabled"`
		FailIfUnavailable        bool        `json:"failIfUnavailable"`
		AutoAllowBashIfSandboxed bool        `json:"autoAllowBashIfSandboxed"`
		AllowUnsandboxedCommands bool        `json:"allowUnsandboxedCommands"`
		Filesystem               *filesystem `json:"filesystem,omitempty"`
	}
	sb := sandbox{Enabled: true, FailIfUnavailable: true, AutoAllowBashIfSandboxed: true}
	if len(deny) > 0 {
		sb.Filesystem = &filesystem{DenyRead: deny, DenyWrite: deny}
	}
	// Strings and booleans only, which Marshal cannot fail on.
	body, _ := json.Marshal(struct {
		Sandbox sandbox `json:"sandbox"`
	}{sb})
	return string(body)
}

// claudeCertStoreEnv is set on every turn: TLS trusts Claude Code's bundled
// roots AND the system store.
//
// A local cluster's MCP endpoint is signed by an mkcert CA that lives only in
// the system keychain, so an app trusting its bundled roots alone reaches no
// MemQL tool and says only that the server failed. It is PINNED, appended
// after the worker's own environment -- the launcher starts the child with
// os.Environ() and then this, and the last value of a key is the one a
// process gets -- because a narrower value inherited from the worker is that
// same silent failure.
const claudeCertStoreEnv = "CLAUDE_CODE_CERT_STORE=bundled,system"

// claudeEnv is the environment one turn's process adds to the worker's:
// the session's own entries, then the pinned certificate store.
func claudeEnv(env []string) []string {
	return append(append([]string(nil), env...), claudeCertStoreEnv)
}

// claudeTurn is one turn's mutable state.
//
// Only sinkMu is a mutex, and it guards the Sink alone: two goroutines
// (stdout and stderr) call it, and interleaving two chunks would
// scramble a transcript a person reads. Everything else here is written
// by exactly one goroutine and read only after both have been joined,
// which is a cheaper guarantee than a lock and a stricter one than a
// comment.
type claudeTurn struct {
	inferenceOnly bool
	sink          Sink
	sinkMu        sync.Mutex
	// rec is the session's recording, shared by every turn.
	rec *recording

	// Written by the stdout pump only.
	sessionID string
	result    *claudeResultEvent
	// initModel is the model the turn's init event says the session runs
	// on. It is not a report on its own -- it is a setting, printed before
	// anything has run -- and servedModel uses it only to pick the
	// session's own model out of a result that names several.
	initModel string
	// cwd is the working directory every call this turn makes is recorded
	// in: the workspace, until the app's own init event names it.
	cwd string
	// mcpFailure is why MemQL's MCP server cannot serve this turn, as the
	// init event reported it; empty while it can.
	mcpFailure string

	// Set before the pumps start and only read after. mcpServer is the
	// server whose status decides the turn -- empty for a session that
	// configured none -- and stop ends the turn's process group.
	mcpServer string
	stop      func()

	// Written by the stderr pump only.
	stderrTail []byte
}

func (t *claudeTurn) emit(stream string, data []byte) {
	if len(data) == 0 || t.sink == nil {
		return
	}
	t.sinkMu.Lock()
	defer t.sinkMu.Unlock()
	t.sink.Chunk(stream, data)
}

// record hands one finished call to the sink, under the lock every chunk
// takes, so an action never lands between two halves of something else.
func (t *claudeTurn) record(a Action) {
	if t.sink == nil {
		return
	}
	t.sinkMu.Lock()
	defer t.sinkMu.Unlock()
	t.sink.Record(a)
}

// pumpStdout reads the stream-json protocol to EOF.
func (t *claudeTurn) pumpStdout(src io.Reader) {
	if src == nil {
		return
	}
	readClaudeLines(src,
		t.route,
		func(part []byte) { t.emit(StreamStdout, part) },
	)
}

// pumpStderr forwards stderr verbatim and keeps its tail.
//
// The tail is kept because it is the only place the app explains itself
// when it fails: stdout's result event carries a machine-readable
// subtype and, for some failures, nothing else at all.
func (t *claudeTurn) pumpStderr(src io.Reader) {
	if src == nil {
		return
	}
	keep := func(line []byte) {
		t.stderrTail = append(t.stderrTail, line...)
		if over := len(t.stderrTail) - claudeStderrKeepBytes; over > 0 {
			t.stderrTail = append(t.stderrTail[:0], t.stderrTail[over:]...)
		}
		t.emit(StreamStderr, line)
	}
	readClaudeLines(src, keep, keep)
}

// route classifies one complete stdout line onto a Sink stream.
//
// The mapping, and the reason behind each part of it:
//
//   - An assistant or user message is unwrapped to its content blocks.
//     Text and thinking become StreamText, because they are what a
//     person reads. An envelope carrying anything else -- a tool_use, a
//     tool_result, or whatever block type Claude Code adds next -- goes
//     out WHOLE on StreamTool, because the envelope is where
//     `parent_tool_use_id` lives and that field is the only link
//     between a subagent's tool call and the call that spawned it.
//   - Every other event type is structure the engine turns into a
//     progress event, so it goes out verbatim on StreamEvent. Verbatim
//     rather than summarised: a live view that is confidently wrong
//     about what the agent did is worse than a plain one.
//   - A line that is not stream-json goes to StreamStdout rather than
//     being dropped. An app that dies while printing a stack trace, or
//     a package manager that warns on the way up, prints it there.
func (t *claudeTurn) route(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	var ev claudeEvent
	if trimmed[0] != '{' || json.Unmarshal(trimmed, &ev) != nil || ev.Type == "" {
		t.emit(StreamStdout, line)
		return
	}
	if id := strings.TrimSpace(ev.SessionID); id != "" {
		t.sessionID = id
	}
	if ev.Type == claudeTypeSystem {
		var init claudeInitEvent
		if json.Unmarshal(trimmed, &init) == nil && init.Subtype == claudeSubtypeInit {
			if m := strings.TrimSpace(init.Model); m != "" {
				t.initModel = m
			}
		}
		// The app names its working directory once, on init, and every
		// call it makes is recorded there (see Action.Cwd for what that
		// does and does not promise).
		var env claudeRecordEnvelope
		if decodeTolerant(trimmed, &env) && env.Subtype == claudeSubtypeInit && strings.TrimSpace(env.Cwd) != "" {
			t.cwd = env.Cwd
		}
		t.checkMCP(trimmed)
	}

	switch ev.Type {
	case "stream_event":
		if t.inferenceOnly {
			var partial struct {
				Event struct {
					Type  string `json:"type"`
					Delta struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"delta"`
				} `json:"event"`
			}
			if json.Unmarshal(trimmed, &partial) == nil && partial.Event.Type == "content_block_delta" && partial.Event.Delta.Type == "text_delta" {
				t.emit(StreamAnswer, []byte(partial.Event.Delta.Text))
			}
		}
		t.emit(StreamEvent, line)
	case claudeTypeAssistant, claudeTypeUser:
		var env claudeRecordEnvelope
		_ = decodeTolerant(trimmed, &env)
		if t.routeBlocks(ev.Message, env, line) {
			return
		}
		t.emit(StreamEvent, line)
	case claudeTypeResult:
		var res claudeResultEvent
		if json.Unmarshal(trimmed, &res) == nil {
			t.result = &res
		}
		t.emit(StreamEvent, line)
	default:
		t.emit(StreamEvent, line)
	}
}

// claudeMCPServers is the init event's `mcp_servers` (2.1.283:
// `[{"name":"memql","status":"connected"}]`, the status one of connected,
// failed, needs-auth, pending or disabled). It is decoded apart from every
// other read of the init line, so a release that changed its shape costs
// this check and nothing else.
type claudeMCPServers struct {
	Subtype    string `json:"subtype"`
	MCPServers []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
}

// claudeMCPUnservable is what each status that fails a turn means, in the
// words the End carries.
//
// Exactly two statuses say MemQL's server cannot serve the turn. `pending`
// is NOT one of them: it is a server still connecting when the init event
// was printed, which Claude Code finishes on its own, and failing on it
// would fail every turn whose endpoint answers a little slower than the app
// starts. The rest leave the turn to say for itself what it had.
var claudeMCPUnservable = map[string]string{
	"failed": "Claude Code could not connect to it -- an endpoint it cannot reach, " +
		"or a certificate it does not trust",
	"needs-auth": "it did not accept the session's credential",
}

// checkMCP fails the turn when its init event says MemQL's server cannot
// serve it.
//
// The process is STOPPED rather than left to finish: a turn with none of
// MemQL's tools still runs, on somebody's subscription, and its answer would
// be read as one made with them.
func (t *claudeTurn) checkMCP(line []byte) {
	if t.mcpServer == "" || t.mcpFailure != "" {
		return
	}
	var init claudeMCPServers
	if !decodeTolerant(line, &init) || init.Subtype != claudeSubtypeInit {
		return
	}
	for _, server := range init.MCPServers {
		if server.Name != t.mcpServer {
			continue
		}
		status := strings.TrimSpace(server.Status)
		why, ok := claudeMCPUnservable[status]
		if !ok {
			return
		}
		t.mcpFailure = fmt.Sprintf("harness: Claude Code reported MemQL's MCP server %q as %s when the turn "+
			"started (%s), so the turn was stopped rather than run without MemQL's tools", server.Name, status, why)
		if t.stop != nil {
			t.stop()
		}
		return
	}
}

// routeBlocks splits a message's content across the streams and reports
// whether it understood the shape.
//
// A false return means the caller forwards the whole envelope instead:
// `content` is a plain string in some message shapes, and guessing a
// stream for a shape this client does not recognise is how a chunk ends
// up on the wrong side of the text / structure split.
//
// It is also where the RECORDING is fed: a tool_use block opens a call, a
// tool_result block closes it, and a call that closed goes to the sink as
// an Action right after the envelope that reported it.
func (t *claudeTurn) routeBlocks(msg json.RawMessage, env claudeRecordEnvelope, line []byte) bool {
	if len(msg) == 0 {
		return false
	}
	var message claudeMessage
	if err := json.Unmarshal(msg, &message); err != nil || len(message.Content) == 0 {
		return false
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(message.Content, &raws); err != nil || len(raws) == 0 {
		return false
	}
	blocks := make([]*claudeContentBlock, len(raws))
	results := 0
	for i, raw := range raws {
		var b claudeContentBlock
		if !decodeTolerant(raw, &b) {
			continue
		}
		blocks[i] = &b
		if b.Type == claudeBlockToolResult {
			results++
		}
	}
	parent := ""
	if env.ParentToolUseID != nil {
		parent = strings.TrimSpace(*env.ParentToolUseID)
	}
	// The envelope's structured record belongs to ONE result. With several
	// on one message nothing says which it describes, so none of them
	// reads it -- which costs a Bash call its exit status of 0 and never
	// invents one.
	var record json.RawMessage
	if results == 1 {
		record = env.ToolUseResult
	}

	structural := false
	var finished []Action
	for _, b := range blocks {
		if b == nil {
			structural = true
			continue
		}
		switch b.Type {
		case claudeBlockText:
			t.emit(StreamText, []byte(b.Text))
		case claudeBlockThinking:
			// The block's `signature` is an opaque blob with no reader
			// on this side and a length that would dominate a
			// transcript, so only the prose leaves.
			t.emit(StreamText, []byte(b.Thinking))
		case claudeBlockToolUse:
			structural = true
			t.rec.begin(claudeCall(b.ID, b.Name, b.Input, parent, t.cwd))
		case claudeBlockToolResult:
			structural = true
			if a, ok := t.rec.complete(b.ToolUseID, func(a *Action) {
				if a.ParentID == "" {
					a.ParentID = parent
				}
				if a.Cwd == "" {
					a.Cwd = t.cwd
				}
				claudeFinish(a, b.Content, b.IsError, record)
			}); ok {
				finished = append(finished, a)
			}
		default:
			structural = true
		}
	}
	if structural {
		t.emit(StreamTool, line)
	}
	// After the app's own report of the call, never before: a reader of
	// the transcript sees the call, then what the recording made of it.
	for _, a := range finished {
		t.record(a)
	}
	return true
}

// failure builds the error for a turn whose process did not succeed.
//
// The detail is the app's OWN words, preferred in the order it is
// likely to have said something useful: the result event's `errors`
// array first, then the tail of stderr. Redacting it is the caller's
// job -- the session applies its redactor to the message it sends on --
// and nothing this client puts in the string comes from the MCP config
// it never reads.
func (t *claudeTurn) failure(spec Spec, code int, waitErr error) error {
	detail := ""
	if t.result != nil && len(t.result.Errors) > 0 {
		detail = strings.Join(t.result.Errors, "; ")
	}
	if detail == "" && t.result != nil && t.result.IsError {
		// Quota refusals can say subtype=success, is_error=true and carry
		// their only explanation in result (no errors array or stderr).
		detail = strings.TrimSpace(t.result.Result)
	}
	if detail == "" {
		detail = strings.TrimSpace(string(t.stderrTail))
	}
	if detail == "" && waitErr != nil {
		detail = waitErr.Error()
	}

	// An app that exits 0 and then declares its own failure is not the
	// same event as one that died, and saying "exited 0" alone would
	// read as a success somebody has to reconcile against a failed
	// session.
	what := fmt.Sprintf("exited %d", code)
	if code == 0 && t.result != nil && t.result.IsError {
		what = fmt.Sprintf("exited 0 and then reported %s", t.result.Subtype)
	}
	if detail == "" {
		return fmt.Errorf("harness: the %s turn %s and printed no reason", spec.Binary, what)
	}
	return fmt.Errorf("harness: the %s turn %s: %s", spec.Binary, what, detail)
}

// readClaudeLines cuts src into newline-terminated lines.
//
// bufio.Scanner is not used because its answer to a line longer than
// its buffer is to STOP, which would silently truncate the rest of a
// run at the first big tool result. ReadSlice reports a full buffer as
// a recoverable condition instead, so an oversize line can be streamed
// out through onOversize and the reader can carry on with the next one
// -- including the result event that follows it.
//
// Both callbacks receive a slice they OWN and nothing here reads again.
// That is what lets a Sink hold a chunk past the call: a reader that
// handed out a window onto its own buffer would corrupt whatever the
// session had queued, in a way that shows up as a garbled transcript
// long after the read that caused it.
func readClaudeLines(src io.Reader, onLine, onOversize func([]byte)) {
	r := bufio.NewReaderSize(src, claudeReadBufBytes)
	var line []byte
	oversize := false

	for {
		frag, err := r.ReadSlice('\n')
		complete := err == nil
		if errors.Is(err, bufio.ErrBufferFull) {
			err = nil
		}
		if len(frag) > 0 {
			switch {
			case oversize:
				onOversize(bytes.Clone(frag))
			case len(line)+len(frag) > claudeMaxEventBytes:
				oversize = true
				if len(line) > 0 {
					onOversize(line)
					line = nil
				}
				onOversize(bytes.Clone(frag))
			default:
				line = append(line, frag...)
			}
		}
		if complete || err != nil {
			if !oversize && len(line) > 0 {
				onLine(line)
			}
			line = nil
			oversize = false
		}
		if err != nil {
			return
		}
	}
}

// claudeEvent is the envelope every stream-json line shares.
//
// Only the three fields this client routes on are declared. The rest is
// forwarded verbatim rather than modelled, so a Claude Code release that
// adds a field costs this package nothing.
//
// `message` stays RAW so that an unfamiliar shape inside it cannot fail
// the whole envelope: a decode error on the outer object would send a
// perfectly good event to stdout as unrecognised narration, which is the
// one classification an operator cannot tell from a crash. The init
// event's model is read in a second pass (claudeInitEvent) for the same
// reason: declaring it here would put every line at the mercy of a later
// release that gave some other event a `model` that is not a string.
type claudeEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id"`
	Message   json.RawMessage `json:"message"`
}

// claudeInitEvent is the init event's statement of the model the session
// runs on (2.1.270: `{"type":"system","subtype":"init","model":...}`),
// decoded only from system lines.
type claudeInitEvent struct {
	Subtype string `json:"subtype"`
	Model   string `json:"model"`
}

// claudeMessage keeps `content` raw for the same reason one level down:
// its shape varies, an array of blocks on the messages this client
// splits and a plain string on some synthetic ones.
type claudeMessage struct {
	Content json.RawMessage `json:"content"`
}

type claudeContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
	// A call (tool_use) and its result (tool_result), for the recording.
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// claudeRecordEnvelope is what the recording reads off an envelope. It is
// decoded APART from claudeEvent, so that a field of an unexpected type
// here can cost the recording a fact but can never cost a line its route.
type claudeRecordEnvelope struct {
	Subtype         string          `json:"subtype"`
	Cwd             string          `json:"cwd"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	ToolUseResult   json.RawMessage `json:"tool_use_result"`
}

// claudeResultEvent is the last line of a turn: the app's own statement
// of what it answered, what it spent and which session it was.
//
// `result` is the app's final answer, and it is what TurnResult.Text
// carries even when a schema was asked for -- Claude Code sets it to
// the SERIALISED structured value in that case, so taking Text from the
// last assistant text block instead would make this client disagree
// with the app about what the app said.
type claudeResultEvent struct {
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	Errors           []string        `json:"errors"`
	Usage            *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	// ModelUsage is the app's own account of which models SPENT this
	// turn's tokens, keyed by model id (2.1.270:
	// `{"claude-haiku-4-5-20251001":{"inputTokens":909,"outputTokens":284,
	// "costUSD":0.02,...}}`). It is the report servedModel reads: a key here
	// is a model that ran, where the init event's model is only the one the
	// session was set up with.
	ModelUsage map[string]claudeModelUsage `json:"modelUsage"`
}

// claudeModelUsage is the one figure of a modelUsage entry this client
// reads: the output that model produced, which decides between several.
type claudeModelUsage struct {
	OutputTokens int64 `json:"outputTokens"`
}

// servedModel is the model the app REPORTED this turn running on.
//
// A turn can spend on more than one model -- a subagent, or the small model
// Claude Code uses for its own housekeeping -- so the session's OWN model,
// the one the init event names, wins when it is among them. Otherwise the
// model that produced the most output is the best statement there is, with
// the name as the tie-break so the answer never depends on map order.
//
// No modelUsage, or an empty one, is NO report. The recorded failed resume
// says `"modelUsage":{}` because no model ran, and naming the init event's
// model for it would report a model that never did; a result from a Claude
// Code that printed no modelUsage at all is the same silence.
func (t *claudeTurn) servedModel() string {
	if t.result == nil || len(t.result.ModelUsage) == 0 {
		return ""
	}
	// The init event may name the model with the context suffix it was
	// asked for (`claude-sonnet-5[1m]`) where modelUsage keys the model
	// itself, so the bare name is tried too -- and what is reported is the
	// key, the name the app used for the tokens.
	for _, own := range []string{t.initModel, strings.SplitN(t.initModel, "[", 2)[0]} {
		if _, ok := t.result.ModelUsage[own]; ok && own != "" {
			return own
		}
	}
	best, most := "", int64(-1)
	for id, u := range t.result.ModelUsage {
		if u.OutputTokens > most || (u.OutputTokens == most && id < best) {
			best, most = id, u.OutputTokens
		}
	}
	return best
}

// usage returns what the app STATED about its own spend.
//
// Nothing here is computed from event counts, model names or elapsed
// time. A result event that carried neither a usage object nor a cost
// is not a report, and Known stays false -- the engine records that as
// billing "unknown", which is true, where zeroes with Known true would
// be recorded as a free run. The same test appsession/chunks.go applies,
// on purpose: the two paths feed one ledger and must not disagree about
// what silence means.
func (e *claudeResultEvent) usage() Usage {
	if e.Usage == nil && e.TotalCostUSD == 0 {
		return Usage{}
	}
	u := Usage{CostUSD: e.TotalCostUSD, Known: true}
	if e.Usage != nil {
		u.InputTokens = e.Usage.InputTokens
		u.OutputTokens = e.Usage.OutputTokens
	}
	return u
}

// structured returns the turn's structured final answer, and whether
// there was one at all.
//
// `structured_output` is the documented home of a --json-schema answer
// and is what claude 2.1.263 emits on the stream-json result event
// (verified 2026-09-07). The `result` fallback is for an older Claude
// Code that only serialised it there; it is safe ONLY because this is
// reached solely when a schema was asked for, so the app was
// constrained to answer in JSON and prose that happens to look like an
// object is not a case that arises.
//
// Nothing is synthesised when both are absent. An empty object handed
// back here reads downstream as "the app answered nothing", which bills
// and retries differently from "the app answered in prose".
func (e *claudeResultEvent) structured() ([]byte, bool) {
	if out, ok := claudeJSONValue(e.StructuredOutput); ok {
		return out, true
	}
	return claudeJSONValue([]byte(e.Result))
}

// claudeJSONValue accepts a JSON object or array and nothing else.
//
// A bare string or number would satisfy json.Valid and then arrive at
// the engine as a structured result that no schema describes.
func claudeJSONValue(raw []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil, false
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return nil, false
	}
	return compact.Bytes(), true
}
