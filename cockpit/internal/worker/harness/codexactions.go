package harness

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
)

// codexactions.go turns both Codex protocols' tool activity into Actions.
//
// The app-server reports a call as a ThreadItem, twice: on item/started
// with status inProgress, and on item/completed with its outcome. Every
// field read below is in the ts-rs types `codex app-server generate-ts`
// printed from codex-cli 0.153.4 on 2026-09-13 (v2/ThreadItem.ts and its
// neighbours), and the command, MCP and failed-exit shapes were recorded
// from that binary driving a real turn the same day (codexactions_test.go
// carries the lines).
//
// The mcp-server fallback reports the same calls as begin / end pairs of
// core events keyed by call_id, with the fields of
// codex-rs/protocol/src/protocol.rs at rust-v0.153.4 -- the last tag that
// ships the crate.
//
// A CODEX READ IS A COMMAND. Codex has no read tool; it runs `cat` or
// `sed -n` through its shell, and says so in the command's own parse
// (`commandActions`, or `parsed_cmd` on the fallback). That parse is the
// only way a Codex read can carry the digest of what it saw, so an exec
// whose parse names a read carries a Content for it -- the call is still
// the exec it was.

// codexItem is the part of an app-server ThreadItem the recording reads.
// A field a variant does not have decodes as its zero value, which every
// reader below takes as "not reported".
type codexItem struct {
	Type              string               `json:"type"`
	ID                string               `json:"id"`
	Status            string               `json:"status"`
	Command           string               `json:"command"`
	Cwd               string               `json:"cwd"`
	CommandActions    []codexCommandAction `json:"commandActions"`
	AggregatedOutput  *string              `json:"aggregatedOutput"`
	ExitCode          *int                 `json:"exitCode"`
	Changes           json.RawMessage      `json:"changes"`
	Server            string               `json:"server"`
	Tool              string               `json:"tool"`
	Arguments         json.RawMessage      `json:"arguments"`
	Result            json.RawMessage      `json:"result"`
	Error             *codexItemError      `json:"error"`
	Query             string               `json:"query"`
	Action            json.RawMessage      `json:"action"`
	Results           json.RawMessage      `json:"results"`
	ContentItems      json.RawMessage      `json:"contentItems"`
	Success           *bool                `json:"success"`
	Path              string               `json:"path"`
	Prompt            *string              `json:"prompt"`
	ReceiverThreadIDs []string             `json:"receiverThreadIds"`
	DurationMs        *int64               `json:"durationMs"`
	Name              string               `json:"name"`
	Namespace         *string              `json:"namespace"`
	Output            json.RawMessage      `json:"output"`
	RevisedPrompt     *string              `json:"revisedPrompt"`
	Failure           json.RawMessage      `json:"failure"`
}

// codexCommandAction is one entry of Codex's own parse of a command:
// v2 CommandAction on the app-server, ParsedCommand on the fallback. Both
// are tagged by `type`, and both call a read "read" and carry its path.
type codexCommandAction struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

type codexItemError struct {
	Message string `json:"message"`
}

// codexNonCallItems are the ThreadItem types that are not tool calls: the
// conversation itself and the app's own state (v2/ThreadItem.ts, 0.153.4).
//
// Every OTHER type is recorded -- a type this build has never seen as
// `other`, whose effects are unknown -- because the two mistakes do not
// cost the same: an extra `other` only makes a replay fall back to the
// app, while a call missing from the recording is one a replay would skip.
var codexNonCallItems = map[string]bool{
	"userMessage":       true,
	"hookPrompt":        true,
	"agentMessage":      true,
	"plan":              true,
	"reasoning":         true,
	"subAgentActivity":  true,
	"enteredReviewMode": true,
	"exitedReviewMode":  true,
	"contextCompaction": true,
}

// codexItemCall builds the call half of an Action from a ThreadItem, and
// reports false for an item that is not a tool call at all -- the
// assistant's prose, its reasoning, its plan.
func codexItemCall(raw json.RawMessage, workspace string) (Action, codexItem, bool) {
	var it codexItem
	if !decodeTolerant(raw, &it) || it.Type == "" || codexNonCallItems[it.Type] {
		return Action{}, it, false
	}
	a := Action{ID: it.ID, AppTool: it.Type, Cwd: workspace}
	switch it.Type {
	case "commandExecution":
		a.Tool = ActionExec
		a.Command = it.Command
		a.Args = argsOf(map[string]any{"command": it.Command})
		if strings.TrimSpace(it.Cwd) != "" {
			a.Cwd = it.Cwd
		}
		a.Contents = codexReadContents(it.CommandActions, a.Cwd)
	case "fileChange":
		a.Tool = ActionFSWrite
		a.Args = argsOf(map[string]any{"changes": rawOrNull(it.Changes)})
		a.Contents = codexChangeContents(it.Changes, a.Cwd)
	case "mcpToolCall":
		a.Tool = ActionMCP
		a.Args = rawOrNull(it.Arguments)
		a.MCP = &MCPTarget{Server: it.Server, Tool: it.Tool}
	case "dynamicToolCall":
		a.Tool = ActionOther
		a.Args = rawOrNull(it.Arguments)
	case "webSearch":
		a.Tool = ActionFetch
		a.Args = argsOf(map[string]any{"query": it.Query, "action": rawOrNull(it.Action)})
		a.Query = it.Query
		a.URL = codexActionURL(it.Action)
	case "imageView":
		a.Tool = ActionFSRead
		a.Args = argsOf(map[string]any{"path": it.Path})
		a.Contents = contentFor(ContentRead, it.Path, a.Cwd)
	case "imageGeneration":
		a.Tool = ActionOther
		a.Args = argsOf(map[string]any{"revisedPrompt": it.RevisedPrompt})
	case "collabAgentToolCall":
		a.Tool = ActionAgent
		a.Args = argsOf(map[string]any{"tool": it.Tool, "prompt": it.Prompt, "receiverThreadIds": it.ReceiverThreadIDs})
	case "sleep":
		a.Tool = ActionAgent
		a.Args = argsOf(map[string]any{"durationMs": it.DurationMs})
	case "functionCallOutput":
		a.Tool = ActionOther
		a.Args = argsOf(map[string]any{"name": it.Name, "namespace": it.Namespace})
	default:
		// Not a type this build knows. Its arguments are not known either,
		// so the item itself stands in for them, whole.
		a.Tool = ActionOther
		a.Args = rawOrNull(raw)
	}
	return a, it, true
}

// codexItemFinish writes what a completed item reported onto its call.
//
// ONLY WHAT THE ITEM REPORTED. `completed` is the one status that means
// the call did what it was asked; failed and declined are the two that do
// not, and anything newer is read the same way, because a status this
// build has not heard of is not evidence of success. An item with no
// status at all -- a web search, an image view, a sleep -- reported no
// verdict, and none is recorded. An item with no result reported no
// result: a null there is recorded as absent, never as the digest of
// null, which would give two different searches one identity.
func codexItemFinish(a *Action, it codexItem) {
	var failed *bool
	verdict := func(f bool) { failed = &f }
	statusFailed := it.Status != "completed"
	var result any
	known := false
	value := func(raw json.RawMessage) {
		if v, ok := decodeValue(raw); ok && v != nil {
			result, known = v, true
		}
	}
	switch it.Type {
	case "commandExecution":
		verdict(statusFailed || (it.ExitCode != nil && *it.ExitCode != 0))
		if it.Status == "declined" {
			// The command never ran: an exit code or an output on the item
			// is not a result of it (the fallback reads its events the same
			// way, in codexCoreFinish).
			break
		}
		a.ExitCode = it.ExitCode
		switch {
		case it.AggregatedOutput != nil:
			result, known = *it.AggregatedOutput, true
		case it.ExitCode != nil:
			// A command that ran and printed nothing reports null, and its
			// result is the empty text it produced -- recorded 2026-09-13
			// from a `printf ... > out.txt` that completed with exitCode 0
			// and aggregatedOutput null. One that never ran (declined) has
			// no exit code and so no result.
			result, known = "", true
		}
	case "fileChange", "collabAgentToolCall":
		verdict(statusFailed)
	case "mcpToolCall":
		verdict(statusFailed || it.Error != nil)
		if it.Error != nil {
			result, known = it.Error.Message, true
		} else if v, ok := decodeValue(it.Result); ok && v != nil {
			result, known = mcpResultValue(v), true
		}
	case "dynamicToolCall":
		verdict(statusFailed || (it.Success != nil && !*it.Success))
		value(it.ContentItems)
	case "imageGeneration":
		switch {
		case !isNull(it.Failure):
			verdict(true)
		case it.Status == "completed":
			verdict(false)
		}
		// The image as the app returned it: digested, never carried.
		value(it.Result)
	case "webSearch":
		value(it.Results)
	case "functionCallOutput":
		value(it.Output)
	}
	a.IsError = failed
	if known {
		a.ResultType, a.ResultDigest = resultShape(result)
	}
	if failed != nil && *failed {
		a.Contents = nil
		return
	}
	// A command whose own parse names exactly one read reported that
	// file's bytes as its output, when it printed the whole file (`cat`)
	// -- and nothing like them when it printed a line of it (`head -1`).
	if it.Type == "commandExecution" && len(a.Contents) == 1 && it.AggregatedOutput != nil {
		a.Contents[0].Seen = []byte(*it.AggregatedOutput)
	}
}

// codexReadContents names the files Codex's own parse of a command says
// it reads. The parse is Codex's, "best-effort" in its own words, and a
// command it could not parse reads nothing here rather than something
// guessed.
func codexReadContents(actions []codexCommandAction, cwd string) []Content {
	var out []Content
	seen := map[string]bool{}
	for _, act := range actions {
		if act.Type != "read" {
			continue
		}
		for _, c := range contentFor(ContentRead, act.Path, cwd) {
			if !seen[c.Path] {
				seen[c.Path] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// codexChangeContents names the files an app-server fileChange touched:
// v2 FileUpdateChange, {path, kind: {type, move_path}, diff}.
func codexChangeContents(raw json.RawMessage, cwd string) []Content {
	var changes []struct {
		Path string `json:"path"`
		Kind struct {
			Type     string `json:"type"`
			MovePath string `json:"move_path"`
		} `json:"kind"`
	}
	if !decodeTolerant(raw, &changes) {
		return nil
	}
	var out []Content
	for _, ch := range changes {
		out = append(out, changeContents(ch.Path, ch.Kind.Type, ch.Kind.MovePath, cwd)...)
	}
	return out
}

// changeContents is one file change as Contents: an add or an update
// writes the file, a delete removes it, and a move is both -- the old path
// is gone and the new one written.
func changeContents(path, kind, movePath, cwd string) []Content {
	switch kind {
	case "delete":
		return contentFor(ContentDelete, path, cwd)
	case "update":
		if strings.TrimSpace(movePath) != "" {
			return append(contentFor(ContentDelete, path, cwd), contentFor(ContentWrite, movePath, cwd)...)
		}
	}
	return contentFor(ContentWrite, path, cwd)
}

// codexActionURL is the page a web-search action opened, when it opened
// one: WebSearchAction's openPage and findInPage carry a url.
func codexActionURL(raw json.RawMessage) string {
	var act struct {
		URL *string `json:"url"`
	}
	if !decodeTolerant(raw, &act) || act.URL == nil {
		return ""
	}
	return *act.URL
}

// --- the mcp-server fallback ----------------------------------------

// codexCoreEvent is the part of a core EventMsg the recording reads.
// Fields are snake_case here, unlike the app-server's.
type codexCoreEvent struct {
	Type             string                     `json:"type"`
	CallID           string                     `json:"call_id"`
	Command          []string                   `json:"command"`
	Cwd              string                     `json:"cwd"`
	ParsedCmd        []codexCommandAction       `json:"parsed_cmd"`
	AggregatedOutput *string                    `json:"aggregated_output"`
	Stdout           *string                    `json:"stdout"`
	Stderr           *string                    `json:"stderr"`
	ExitCode         *int                       `json:"exit_code"`
	Status           string                     `json:"status"`
	Changes          map[string]json.RawMessage `json:"changes"`
	Success          *bool                      `json:"success"`
	Invocation       *codexInvocation           `json:"invocation"`
	Result           json.RawMessage            `json:"result"`
	Query            string                     `json:"query"`
	Action           json.RawMessage            `json:"action"`
	Results          json.RawMessage            `json:"results"`
	Path             string                     `json:"path"`
}

// codexInvocation is McpInvocation: which server, which tool, what
// arguments.
type codexInvocation struct {
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// What a core event is to the recording.
const (
	corePhaseBegin = iota + 1
	corePhaseEnd
	// corePhaseBoth is an event that is a whole call on its own:
	// view_image_tool_call has no begin and no end.
	corePhaseBoth
)

// codexCoreCall builds the call half of an Action from a core event, and
// reports which phase of the call the event is. ok is false for an event
// that is not a call.
//
// An exec's cwd is left EMPTY when the event names none: an older end
// event carries only the call id and the status, and a cwd defaulted here
// would overwrite the one its begin reported (mergeCall keeps the end's
// non-empty fields). The caller fills the workspace in last.
func codexCoreCall(ev codexCoreEvent, workspace string) (Action, int, bool) {
	cwd := codexPath(ev.Cwd)
	base := cwd
	if base == "" {
		base = workspace
	}
	phase := corePhaseEnd
	if strings.HasSuffix(ev.Type, "_begin") {
		phase = corePhaseBegin
	}
	var a Action
	switch ev.Type {
	case "exec_command_begin", "exec_command_end":
		a = Action{ID: ev.CallID, Tool: ActionExec, AppTool: "exec_command", Cwd: cwd}
		if len(ev.Command) > 0 {
			a.Command = shellJoin(ev.Command)
			a.Args = argsOf(map[string]any{"command": ev.Command})
		}
		a.Contents = codexReadContents(ev.ParsedCmd, base)
	case "patch_apply_begin", "patch_apply_end":
		a = Action{ID: ev.CallID, Tool: ActionFSWrite, AppTool: "patch_apply", Cwd: workspace}
		// An older end event carries no changes; leaving these empty is
		// what lets the begin's stand (mergeCall).
		if len(ev.Changes) > 0 {
			a.Args = argsOf(map[string]any{"changes": ev.Changes})
			a.Contents = codexCoreChangeContents(ev.Changes, workspace)
		}
	case "mcp_tool_call_begin", "mcp_tool_call_end":
		a = Action{ID: ev.CallID, Tool: ActionMCP, AppTool: "mcp_tool_call", Cwd: workspace}
		if ev.Invocation != nil {
			a.Args = rawOrNull(ev.Invocation.Arguments)
			a.MCP = &MCPTarget{Server: ev.Invocation.Server, Tool: ev.Invocation.Tool}
		}
	case "web_search_begin", "web_search_end":
		a = Action{ID: ev.CallID, Tool: ActionFetch, AppTool: "web_search", Cwd: workspace,
			Query: ev.Query, URL: codexActionURL(ev.Action)}
		if ev.Type == "web_search_end" {
			a.Args = argsOf(map[string]any{"query": ev.Query, "action": rawOrNull(ev.Action)})
		}
	case "view_image_tool_call":
		path := codexPath(ev.Path)
		a = Action{ID: ev.CallID, Tool: ActionFSRead, AppTool: "view_image_tool_call", Cwd: workspace,
			Args: argsOf(map[string]any{"path": path}), Contents: contentFor(ContentRead, path, workspace)}
		phase = corePhaseBoth
	default:
		return Action{}, 0, false
	}
	return a, phase, true
}

// codexCoreFinish writes what an end event reported onto its call, by
// the rules codexItemFinish keeps: a verdict only where the event gives
// one, a result only where it carries one. A web search and an image view
// report no verdict, and an end event this build cannot read the outcome
// of reports nothing either -- isError stays absent rather than guessing.
func codexCoreFinish(a *Action, ev codexCoreEvent) {
	statusFailed := ev.Status != "" && ev.Status != "completed"
	var failed *bool
	verdict := func(f bool) { failed = &f }
	var result any
	known := false
	printed := func() {
		switch {
		case ev.AggregatedOutput != nil:
			result, known = *ev.AggregatedOutput, true
		case ev.Stdout != nil || ev.Stderr != nil:
			result, known = deref(ev.Stdout)+deref(ev.Stderr), true
		}
	}
	switch ev.Type {
	case "exec_command_end":
		verdict(statusFailed || (ev.ExitCode != nil && *ev.ExitCode != 0))
		if ev.Status == "declined" {
			// The command never ran: an exit code or an output on the event
			// is not a result of it.
			break
		}
		a.ExitCode = ev.ExitCode
		printed()
	case "patch_apply_end":
		verdict(statusFailed || (ev.Success != nil && !*ev.Success))
		// What the patch printed -- "Success. Updated the following files"
		// or the reason it did not apply.
		printed()
	case "mcp_tool_call_end":
		result, failed = codexCoreMCPResult(ev.Result)
		known = failed != nil && result != nil
	case "web_search_end":
		if v, ok := decodeValue(ev.Results); ok && v != nil {
			result, known = v, true
		}
	}
	a.IsError = failed
	if known {
		a.ResultType, a.ResultDigest = resultShape(result)
	}
	if failed != nil && *failed {
		a.Contents = nil
		return
	}
	// As on the app-server: a command whose parse names one read printed
	// that file's bytes to stdout when it printed the whole file.
	if ev.Type == "exec_command_end" && len(a.Contents) == 1 {
		switch {
		case ev.Stdout != nil:
			a.Contents[0].Seen = []byte(*ev.Stdout)
		case ev.AggregatedOutput != nil:
			a.Contents[0].Seen = []byte(*ev.AggregatedOutput)
		}
	}
}

// codexCoreMCPResult reads mcp_tool_call_end's result, which the core
// protocol serialises as a Rust Result: {"Ok": CallToolResult} or
// {"Err": "reason"}. A shape it cannot read reports nothing -- not a
// success and not a failure.
func codexCoreMCPResult(raw json.RawMessage) (any, *bool) {
	var r struct {
		Ok  json.RawMessage `json:"Ok"`
		Err *string         `json:"Err"`
	}
	if !decodeTolerant(raw, &r) {
		return nil, nil
	}
	if r.Err != nil {
		failed := true
		return *r.Err, &failed
	}
	v, ok := decodeValue(r.Ok)
	if !ok {
		return nil, nil
	}
	failed := false
	if obj, ok := v.(map[string]any); ok {
		if flag, ok := obj["isError"].(bool); ok {
			failed = flag
		}
	}
	return mcpResultValue(v), &failed
}

// codexCoreChangeContents names the files a core patch touched, in path
// order: the changes arrive as a map, and map order is not an order.
func codexCoreChangeContents(changes map[string]json.RawMessage, cwd string) []Content {
	paths := make([]string, 0, len(changes))
	for p := range changes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []Content
	for _, p := range paths {
		var kind struct {
			Type     string `json:"type"`
			MovePath string `json:"move_path"`
		}
		_ = decodeTolerant(changes[p], &kind)
		out = append(out, changeContents(p, kind.Type, kind.MovePath, cwd)...)
	}
	return out
}

// codexPath reads a path the core protocol sent. rust-v0.153.4 serialises
// PathUri as a file:// URL (codex-rs/utils/path-uri, `impl Serialize for
// PathUri`), and the older releases this fallback exists for send a plain
// path; both are read, and a URL naming another host is no local path.
func codexPath(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "file://") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Host != "" && u.Host != "localhost") {
		return ""
	}
	return u.Path
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// shellJoin renders an argv as one command line a POSIX shell reads back
// as the same argv.
//
// Only the fallback needs it: its exec events carry the argv as a list,
// where the app-server carries one string and Claude Code's Bash tool is
// handed one. A word made only of characters no shell treats specially
// stays bare, so `bash -lc 'ls -1'` reads the way a person would type it;
// anything else is single-quoted, the one quoting that means the same
// thing in every POSIX shell. FuzzShellJoinRoundTrips holds it to that.
func shellJoin(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = shellWord(a)
	}
	// The FIRST word is the command, and a shell reads two kinds of bare
	// first word as something else: NAME=value is an assignment that runs
	// nothing, and a reserved word opens its own grammar (`if`, `time`).
	// Quoted, either is only a name.
	if len(argv) > 0 && words[0] == argv[0] && (strings.Contains(argv[0], "=") || shellReserved[argv[0]]) {
		words[0] = "'" + argv[0] + "'"
	}
	return strings.Join(words, " ")
}

// shellReserved are the bare words a shell reads as grammar when they
// open a command: POSIX's, and those bash and zsh add. The ones spelled
// with a character shellSafe rejects -- `!`, `{`, `[[` -- are quoted
// already.
var shellReserved = map[string]bool{
	"case": true, "do": true, "done": true, "elif": true, "else": true,
	"esac": true, "fi": true, "for": true, "if": true, "in": true,
	"then": true, "until": true, "while": true,
	"function": true, "select": true, "time": true, "coproc": true,
	"foreach": true, "end": true, "repeat": true, "nocorrect": true, "noglob": true,
}

func shellWord(s string) string {
	if s == "" {
		return "''"
	}
	// A leading `=` is an expansion in zsh, the default shell on a Mac.
	bare := s[0] != '='
	for _, r := range s {
		if !shellSafe(r) {
			bare = false
			break
		}
	}
	if bare {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellSafe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("-_./=:,+@%", r)
}
