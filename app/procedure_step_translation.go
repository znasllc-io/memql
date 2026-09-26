package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/num"
)

// procedure_step_translation.go -- a learned procedure's step, in the APP'S
// own spelling, becomes host actions; what the host did becomes the
// executor-independent observation a replay is judged on (epic memql#5408,
// task memql#5411; the plan's "3. Stream notes").
//
// PURE. Nothing in this file dispatches. The dispatchers drive a host through
// these functions, so every translation and every observation rule is tested
// without one.
//
// THE ARGUMENTS ARE THE APP'S. A procedure is learned from what an app did,
// so a step carries Claude Code's `Bash {command}` / `Write {file_path,
// content}` / `Edit {file_path, old_string, new_string, replace_all}` /
// `MultiEdit {file_path, edits}` / `Read {file_path, offset, limit}` /
// `WebFetch {url, prompt}`, Codex's `command_execution {command: argv}`, and
// the MCP recorder's `{tool, arguments}`. The step's TYPE (exec, fs_write,
// fs_read, fetch, mcp) is the recorder's normalization of the tool name; the
// spelling of its arguments is not normalized by anyone but this file.
//
// ONLY WHAT A REPLAY CAN REPORT IS OBSERVED, and exactly what the lift
// expects of each tool (integrations/procedure/corpus.go stepEvidenceOf):
//
//	exec              isError, exitCode, resultType (of stdout)
//	fetch, mcp        isError, resultType
//	fs_write, fs_read isError, contents [{op, path, digest}]
//
// Paths are reported WORKSPACE-RELATIVE (the recordings were relativized
// against their workspace before learning) and digests are lowercase hex
// SHA-256 over the bytes written or read -- the Library file row's spelling,
// which component/work compares with the cockpit's "sha256:<hex>" as one
// value. A replay that reported anything the recording did not would be held
// to it; one that left out anything the recording carried would be refused,
// because an absent measurement is never a match.

// procedureHostReply is what one surface's dispatchHost answered for one
// action: the workbench's {ok, payload, errorCode, errorMessage} and the
// worker's {ok, output, errorCode, errorMessage}, read into one shape.
type procedureHostReply struct {
	OK           bool
	ErrorCode    string
	ErrorMessage string
	// Payload is the action's own output: exec's {exitCode, stdout, stderr},
	// fs_read's {content, truncated}, http_fetch's {status, body}, ...
	Payload map[string]any
}

// procedureRefusedBeforeRunning are the error codes with which a surface
// says NOTHING RAN -- a gate, a missing peer, an argument it would not take.
//
// The distinction is the one the replay's side-effect accounting rests on. A
// step that did not run is returned as a Go error, and the runner does not
// list it among the steps the app must not redo; a step that ran and failed
// -- or may have run, like a timeout or a lost answer -- is an observation
// with isError set, and the comparison is what stops the replay. Guessing
// "did not run" for a call that ran would hand the app a step to repeat.
var procedureRefusedBeforeRunning = map[string]bool{
	// The workbench (integrations/workbench).
	"unknown_action":             true,
	"invalid_environment_hint":   true,
	"environment_mismatch":       true,
	"no_workbench_peer":          true,
	"no_forwarded_authority":     true,
	"encode_args":                true,
	"workspace_owner_unresolved": true,
	"missing_arg":                true,
	"command_not_allowed":        true,
	"invalid_path":               true,
	"too_large":                  true,
	"invalid_request":            true,
	"exec_failed":                true,
	// The worker's gates and router (integrations/agent/worker), all decided
	// before a dispatch leaves this node.
	"denied_no_per_task_approval": true,
	"kill_switch_engaged":         true,
	"authorization_lookup_failed": true,
	"denied_by_scope":             true,
	"denied_by_classifier":        true,
	"denied_by_policy":            true,
	"no_worker_available":         true,
	"worker_busy":                 true,
	"worker_unreachable":          true,
}

// procedureRefusal is the Go error for a host that refused a step before
// running it.
func procedureRefusal(surface, action string, reply procedureHostReply) error {
	msg := strings.TrimSpace(reply.ErrorMessage)
	if msg == "" {
		msg = "no reason given"
	}
	return fmt.Errorf("%s refused %s before running it (%s): %s", surface, action, reply.ErrorCode, msg)
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

// procedureCommandLine is a recorded command in one of its two spellings: a
// command LINE (Claude Code's Bash, and any command the template materialized
// from an argv-form string), or an argument VECTOR executed with no shell
// between its elements (Codex).
type procedureCommandLine struct {
	Line string
	Argv []string
}

// procedureCommandKeys are the argument names a recorded command rides under,
// in the order they are tried. The same set component/procedure reads as
// command keys.
var procedureCommandKeys = []string{"command", "cmd", "argv"}

// procedureCommandOf reads the command an exec step ran.
func procedureCommandOf(args map[string]any) (procedureCommandLine, error) {
	for _, key := range procedureCommandKeys {
		v, present := args[key]
		if !present {
			continue
		}
		switch c := v.(type) {
		case string:
			if strings.TrimSpace(c) == "" {
				return procedureCommandLine{}, fmt.Errorf("its %s is empty", key)
			}
			return procedureCommandLine{Line: c}, nil
		case []any:
			argv := make([]string, 0, len(c))
			for i, e := range c {
				word, ok := procedureScalarWord(e)
				if !ok {
					return procedureCommandLine{}, fmt.Errorf("its %s[%d] is a %T, not an argument", key, i, e)
				}
				argv = append(argv, word)
			}
			if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
				return procedureCommandLine{}, fmt.Errorf("its %s is an empty argument vector", key)
			}
			return procedureCommandLine{Argv: argv}, nil
		case []string:
			if len(c) == 0 || strings.TrimSpace(c[0]) == "" {
				return procedureCommandLine{}, fmt.Errorf("its %s is an empty argument vector", key)
			}
			return procedureCommandLine{Argv: append([]string(nil), c...)}, nil
		default:
			return procedureCommandLine{}, fmt.Errorf("its %s is a %T, neither a command line nor an argument vector", key, v)
		}
	}
	return procedureCommandLine{}, fmt.Errorf("it names no command (its arguments are %s)", procedureKeyList(args))
}

// procedureScalarWord spells one argument-vector element. A materialized
// number arrives as float64 and is spelled the way the recording wrote it.
func procedureScalarWord(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	case json.Number:
		return t.String(), true
	case int:
		return strconv.Itoa(t), true
	}
	return "", false
}

// procedureShellSafeWord matches an argument a POSIX shell reads back as
// itself: nothing that splits, quotes, expands, globs or redirects.
var procedureShellSafeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// procedureShellQuote quotes one argument so a shell reads it back verbatim.
//
// STRICTER THAN component/procedure's argv quoting, and on purpose. That rule
// re-spells a command LINE the recording already wrote for a shell, where a
// bare `>` or `*` was the author's operator and must stay one. An argument
// VECTOR was executed with no shell at all, so every element is literal: `*`
// was a filename and `;` was a character, and joining them into a line for
// `/bin/sh -c` must keep them so.
func procedureShellQuote(s string) string {
	if s != "" && procedureShellSafeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// procedureJoinArgv is an argument vector as the one command line a shell
// executes identically.
func procedureJoinArgv(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = procedureShellQuote(a)
	}
	return strings.Join(words, " ")
}

// procedureShells are the shells whose `-c` script is itself the command.
var procedureShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

// procedureUnwrapShellScript reads the script out of `[<shell>, -c|-lc,
// <script>]` -- the shape Codex runs EVERY command in. ok is false for any
// other vector, including one with arguments after the script (they would be
// $0 and $1 to it, which a bare script cannot express).
//
// Used on the WORKBENCH only, whose exec allowlist admits no shell binary: a
// Codex step asked for as `bash -lc '...'` would be refused there without
// running. The script runs under the workbench's own `/bin/sh -c` instead,
// which is not a login shell and not bash -- a script that depended on either
// fails its comparison rather than passing, and stays in shadow.
func procedureUnwrapShellScript(argv []string) (string, bool) {
	if len(argv) != 3 {
		return "", false
	}
	shell := path.Base(strings.TrimSpace(argv[0]))
	if !procedureShells[shell] {
		return "", false
	}
	switch argv[1] {
	case "-c", "-lc", "-cl":
	default:
		return "", false
	}
	if strings.TrimSpace(argv[2]) == "" {
		return "", false
	}
	return argv[2], true
}

// procedureLeadingCd matches a command line that opens by changing into one
// directory: `cd <word> && <rest>`, with the word bare or single-quoted.
var procedureLeadingCd = regexp.MustCompile(`^\s*cd\s+('[^']*'|[A-Za-z0-9_@%+=:,./-]+)\s*&&\s*`)

// procedureSplitLeadingCd moves a command's leading `cd <relative dir> &&`
// into a working directory: `cd ./app && npm test` becomes `npm test` run in
// `app`. Repeated prefixes compose. The directory is returned relative to the
// workspace, "." when there was none; ok is false when a prefix names a
// directory that is absolute or leaves the workspace, and the command is then
// returned untouched.
//
// WHY. Every recording ran with the workspace as its working directory, and
// relativization turned `cd /ws/run1 && npm test` into `cd . && npm test`. On
// the workbench `cd` is a shell builtin its exec allowlist does not admit, so
// the command would be refused whole; running `npm test` in the directory the
// prefix named is the same command. It is the same only for a directory that
// exists: a `cd` into a missing one would have failed the recording's command,
// and here the step is refused before it runs.
func procedureSplitLeadingCd(line string) (rest, dir string, ok bool) {
	rest, dir = line, "."
	for {
		m := procedureLeadingCd.FindStringSubmatch(rest)
		if m == nil {
			return rest, dir, true
		}
		target := m[1]
		if strings.HasPrefix(target, "'") {
			target = strings.TrimSuffix(strings.TrimPrefix(target, "'"), "'")
		}
		if target == "" || path.IsAbs(target) || strings.HasPrefix(target, "~") {
			return line, ".", false
		}
		next := path.Clean(path.Join(dir, target))
		if next == ".." || strings.HasPrefix(next, "../") {
			return line, ".", false
		}
		dir = next
		rest = rest[len(m[0]):]
	}
}

// procedureTimeoutSec reads a recorded command's own time limit. Claude
// Code's Bash `timeout` is MILLISECONDS; the hosts take seconds, rounded up so
// a limit never shrinks. Zero means none was recorded, and the host's default
// applies.
func procedureTimeoutSec(args map[string]any) int {
	ms, ok := procedurePayloadInt(args["timeout"])
	if !ok || ms <= 0 {
		return 0
	}
	return (ms + 999) / 1000
}

// ---------------------------------------------------------------------------
// Files
// ---------------------------------------------------------------------------

// procedurePathKeys are the argument names a file step's path rides under.
var procedurePathKeys = []string{"file_path", "path", "filePath", "file"}

// procedureFilePathOf reads the file a step names.
func procedureFilePathOf(args map[string]any) (string, error) {
	for _, key := range procedurePathKeys {
		if s, ok := args[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s), nil
		}
	}
	return "", fmt.Errorf("it names no file (its arguments are %s)", procedureKeyList(args))
}

// procedureWorkspaceRelative is a path inside the workspace, cleaned: "./a/b"
// is "a/b" and "." is the workspace itself. An absolute path, a home path and
// one that climbs out are refused -- a workspace-relative procedure never
// wrote one, so a step that names one was learned from a machine.
func procedureWorkspaceRelative(p string) (string, error) {
	p = strings.TrimSpace(p)
	switch {
	case p == "":
		return "", fmt.Errorf("the path is empty")
	case path.IsAbs(p):
		return "", fmt.Errorf("%q is absolute, and only a path inside the replay's workspace can be run here", p)
	case strings.HasPrefix(p, "~"):
		return "", fmt.Errorf("%q names a home directory, which is not inside the replay's workspace", p)
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%q leaves the replay's workspace", p)
	}
	return clean, nil
}

// procedureFileEdit is one exact-string replacement, in Claude Code's terms.
type procedureFileEdit struct {
	Old        string
	New        string
	ReplaceAll bool
}

// procedureWritePlan is what an fs_write step writes: the whole content (a
// Write), or edits applied to what is there (an Edit or a MultiEdit).
type procedureWritePlan struct {
	Kind    string // "write" | "edit" | "multiEdit"
	Content string
	Edits   []procedureFileEdit
}

// procedureWritePlanOf reads an fs_write step's arguments. The SHAPE decides,
// because the step type does not: the recorder normalizes Write, Edit and
// MultiEdit to one step type, and only their arguments tell them apart.
func procedureWritePlanOf(args map[string]any) (procedureWritePlan, error) {
	if raw, present := args["edits"]; present {
		list, ok := raw.([]any)
		if !ok || len(list) == 0 {
			return procedureWritePlan{}, fmt.Errorf("its edits are not a non-empty list")
		}
		plan := procedureWritePlan{Kind: "multiEdit"}
		for i, e := range list {
			m, ok := e.(map[string]any)
			if !ok {
				return procedureWritePlan{}, fmt.Errorf("its edits[%d] is a %T, not an edit", i, e)
			}
			edit, err := procedureFileEditOf(m)
			if err != nil {
				return procedureWritePlan{}, fmt.Errorf("its edits[%d]: %w", i, err)
			}
			plan.Edits = append(plan.Edits, edit)
		}
		return plan, nil
	}
	if _, present := args["old_string"]; present {
		edit, err := procedureFileEditOf(args)
		if err != nil {
			return procedureWritePlan{}, err
		}
		return procedureWritePlan{Kind: "edit", Edits: []procedureFileEdit{edit}}, nil
	}
	if raw, present := args["content"]; present {
		content, ok := raw.(string)
		if !ok {
			return procedureWritePlan{}, fmt.Errorf("its content is a %T, not text", raw)
		}
		return procedureWritePlan{Kind: "write", Content: content}, nil
	}
	return procedureWritePlan{}, fmt.Errorf("it carries no content, old_string or edits (its arguments are %s)", procedureKeyList(args))
}

func procedureFileEditOf(m map[string]any) (procedureFileEdit, error) {
	oldS, ok := m["old_string"].(string)
	if !ok {
		return procedureFileEdit{}, fmt.Errorf("old_string is a %T, not text", m["old_string"])
	}
	newS, ok := m["new_string"].(string)
	if !ok {
		return procedureFileEdit{}, fmt.Errorf("new_string is a %T, not text", m["new_string"])
	}
	all, _ := m["replace_all"].(bool)
	return procedureFileEdit{Old: oldS, New: newS, ReplaceAll: all}, nil
}

// applyProcedureEdits applies edits in order to a file's content, exactly as
// the app's own Edit / MultiEdit would, and REFUSES exactly where it would.
//
// The error is the APP'S refusal, not a dispatch failure: the step ran and
// the tool said no, which is an observation with isError set -- the same
// answer the recording would have carried had the app met this file. So:
//
//   - an old_string that is not in the file is refused;
//   - an old_string found more than once is refused unless replace_all says
//     every one is meant;
//   - an edit that changes nothing is refused;
//   - an empty old_string creates a file that does not exist (or is empty),
//     and is refused on one that has content.
//
// A MultiEdit is all or nothing: any refused edit refuses the whole step, and
// the file is not written.
func applyProcedureEdits(current string, exists bool, edits []procedureFileEdit) (string, error) {
	content := current
	for i, e := range edits {
		where := ""
		if len(edits) > 1 {
			where = fmt.Sprintf("edit %d: ", i+1)
		}
		if e.Old == e.New {
			return "", fmt.Errorf("%sno changes to make: old_string and new_string are exactly the same", where)
		}
		if e.Old == "" {
			if exists && content != "" {
				return "", fmt.Errorf("%scannot create a new file: the file already exists", where)
			}
			content, exists = e.New, true
			continue
		}
		if !exists {
			return "", fmt.Errorf("%sthe file does not exist", where)
		}
		n := strings.Count(content, e.Old)
		switch {
		case n == 0:
			return "", fmt.Errorf("%sthe string to replace was not found in the file", where)
		case n > 1 && !e.ReplaceAll:
			return "", fmt.Errorf("%sfound %d matches of the string to replace, but replace_all is false", where, n)
		case e.ReplaceAll:
			content = strings.ReplaceAll(content, e.Old, e.New)
		default:
			content = strings.Replace(content, e.Old, e.New, 1)
		}
	}
	return content, nil
}

// procedureDigest is the lowercase hex SHA-256 of the bytes, which is how the
// Library file row spells a content digest.
func procedureDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// procedureSandboxWriteCommand writes stdin to a workspace-relative file
// through exec rather than fs_write, making its parent directory first.
//
// A SHADOW REPLAY USES IT, AND ONLY A SHADOW REPLAY. The workbench promotes
// every successful fs_write into its run owner's Library as a generated
// output -- a real MemQL row, which a shadow must never write -- and the
// bytes this lands on disk are the same bytes. `mkdir`, `cat` and the
// redirect are all the exec allowlist needs to admit.
func procedureSandboxWriteCommand(rel string) string {
	target := procedureShellQuote(rel)
	if dir := path.Dir(rel); dir != "." && dir != "" {
		return "mkdir -p " + procedureShellQuote(dir) + " && cat > " + target
	}
	return "cat > " + target
}

// ---------------------------------------------------------------------------
// Observations
// ---------------------------------------------------------------------------

// procedureExecObservation reads an exec reply: the exit code when the
// command ran to one, the error flag, and the type of what it printed.
//
// A reply with no exit code ran and was stopped -- a timeout, a signal, a
// lost answer -- so it is an error with no code and no type: absent
// measurements the comparison refuses, which is the answer for a command
// that did not finish.
func procedureExecObservation(reply procedureHostReply) (work.StepObservation, map[string]any) {
	out := map[string]any{"action": "exec"}
	if reply.ErrorCode != "" {
		out["errorCode"] = reply.ErrorCode
		out["errorMessage"] = reply.ErrorMessage
	}
	code, hasCode := procedurePayloadInt(reply.Payload["exitCode"])
	if !hasCode {
		isError := true
		return work.StepObservation{IsError: &isError}, out
	}
	isError := code != 0 || reply.ErrorCode != ""
	stdout, _ := reply.Payload["stdout"].(string)
	resultType := work.InferTextType(stdout)
	out["exitCode"] = code
	out["resultType"] = resultType
	out["stdoutBytes"] = len(stdout)
	out["stdoutDigest"] = procedureDigest(stdout)
	if isError {
		if stderr, _ := reply.Payload["stderr"].(string); stderr != "" {
			out["stderrTail"] = procedureTail(stderr, procedureStderrTail)
		}
	}
	return work.StepObservation{IsError: &isError, ExitCode: &code, ResultType: resultType}, out
}

// procedureStderrTail bounds the stderr a failed step's receipt keeps: enough
// to say why, and never the output itself (design D5).
const procedureStderrTail = 1024

// procedureFileObservation is an fs_write or fs_read step's observation: the
// error flag, and on success the one content it touched.
func procedureFileObservation(op string, ok bool, reported, digest string) work.StepObservation {
	isError := !ok
	obs := work.StepObservation{IsError: &isError}
	if ok {
		obs.Contents = []work.ContentDigest{{Op: op, Path: reported, Digest: digest}}
	}
	return obs
}

// procedureFetchObservation reads an http_fetch reply. A response is typed by
// its body the way the cockpit types a text result; a request that got no
// response is an error with no type.
func procedureFetchObservation(reply procedureHostReply) (work.StepObservation, map[string]any) {
	out := map[string]any{"action": "http_fetch"}
	if reply.ErrorCode != "" {
		out["errorCode"] = reply.ErrorCode
		out["errorMessage"] = reply.ErrorMessage
	}
	status, hasStatus := procedurePayloadInt(reply.Payload["status"])
	if !hasStatus {
		isError := true
		return work.StepObservation{IsError: &isError}, out
	}
	body, _ := reply.Payload["body"].(string)
	isError := !reply.OK || status >= 400
	resultType := work.InferTextType(body)
	out["status"] = status
	out["resultType"] = resultType
	out["bytes"] = len(body)
	out["digest"] = procedureDigest(body)
	return work.StepObservation{IsError: &isError, ResultType: resultType}, out
}

// ---------------------------------------------------------------------------
// Probes
// ---------------------------------------------------------------------------

// procedureToolWord matches a tool name the prober will run: one command word,
// nothing a shell would read as anything else. A learned name is read off a
// recorded command, and a name that is not a plain word is left unmeasured
// rather than put on a command line.
var procedureToolWord = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// procedureVersionCommand is how a tool reports its version. Go is the one
// that takes no flag, and the one whose line also names the platform --
// which component/procedure's normalizer strips on both sides.
func procedureVersionCommand(tool string) string {
	if tool == "go" {
		return "go version"
	}
	return procedureShellQuote(tool) + " --version"
}

// procedureVersionLine is the first non-empty line a version command printed,
// on stdout or -- for a tool that writes it there -- on stderr.
func procedureVersionLine(payload map[string]any) string {
	for _, key := range []string{"stdout", "stderr"} {
		text, _ := payload[key].(string)
		for _, line := range strings.Split(text, "\n") {
			if l := strings.TrimSpace(line); l != "" {
				return l
			}
		}
	}
	return ""
}

// procedurePlatformOf reads `uname -s -m` in Go's names, which are the names
// the cockpit's fingerprint records (it reports runtime.GOOS / GOARCH).
func procedurePlatformOf(line string) (goos, goarch string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", "", false
	}
	goos = strings.ToLower(fields[0])
	switch strings.ToLower(fields[len(fields)-1]) {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	case "i386", "i686":
		goarch = "386"
	case "armv6l", "armv7l", "arm":
		goarch = "arm"
	default:
		goarch = strings.ToLower(fields[len(fields)-1])
	}
	return goos, goarch, goos != "" && goarch != ""
}

// ---------------------------------------------------------------------------
// Small readers
// ---------------------------------------------------------------------------

// procedurePayloadInt reads a decoded payload number as an int.
func procedurePayloadInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return num.ClampInt64(n), true
	case float64:
		whole, ok := num.WholeInt64(n)
		if !ok {
			return 0, false
		}
		return num.ClampInt64(whole), true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		whole, ok := num.WholeInt64(f)
		if !ok {
			return 0, false
		}
		return num.ClampInt64(whole), true
	}
	return 0, false
}

// procedureTail is the last n bytes of s.
func procedureTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// procedureKeyList names a map's keys, sorted, for a message.
func procedureKeyList(m map[string]any) string {
	if len(m) == 0 {
		return "empty"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
