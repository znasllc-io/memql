package harness

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// claudeactions.go turns Claude Code's tool traffic into Actions.
//
// Claude Code reports a call as a `tool_use` block on an assistant
// message -- id, name, input -- and its result as a `tool_result` block on
// a later user message -- tool_use_id, content, is_error -- with the
// tool's own structured record beside it on the envelope, as
// `tool_use_result`. All three were RECORDED from claude 2.1.270 on
// 2026-09-13 (claudeactions_test.go carries the lines). The pair is
// joined by id, and a call becomes an Action only when its result arrives.

// claudeToolKinds classifies Claude Code's own tool names.
//
// A name missing here is `other`, the fail-closed reading: 2.1.270
// already ships tools that schedule cron jobs, send push notifications
// and trigger remote agents, and a later release will ship more. MCP
// tools are told apart by their prefix instead (claudeKind).
var claudeToolKinds = map[string]string{
	"Bash": ActionExec,

	"Read": ActionFSRead,
	// Glob, Grep and LS read the filesystem but return listings and
	// matches, never one file's content, so they are reads with no
	// Contents.
	"Glob": ActionFSRead,
	"Grep": ActionFSRead,
	"LS":   ActionFSRead,

	"Write":        ActionFSWrite,
	"Edit":         ActionFSWrite,
	"MultiEdit":    ActionFSWrite,
	"NotebookEdit": ActionFSWrite,

	"WebFetch":  ActionFetch,
	"WebSearch": ActionFetch,

	// The app's own bookkeeping. Task and Agent hand work to a sub-agent,
	// whose own calls are recorded separately with this one as their
	// parentId; the rest load deferred tools, keep the to-do list, or are
	// the --json-schema answer itself (StructuredOutput).
	"Task":             ActionAgent,
	"Agent":            ActionAgent,
	"ToolSearch":       ActionAgent,
	"TodoWrite":        ActionAgent,
	"TaskCreate":       ActionAgent,
	"TaskGet":          ActionAgent,
	"TaskList":         ActionAgent,
	"TaskUpdate":       ActionAgent,
	"TaskOutput":       ActionAgent,
	"Skill":            ActionAgent,
	"EnterPlanMode":    ActionAgent,
	"ExitPlanMode":     ActionAgent,
	"AskUserQuestion":  ActionAgent,
	"StructuredOutput": ActionAgent,
}

// claudeMCPPrefix starts the name of every MCP tool Claude Code exposes:
// mcp__<server>__<tool>.
const claudeMCPPrefix = "mcp__"

// claudeKind classifies one tool name.
func claudeKind(name string) string {
	if strings.HasPrefix(name, claudeMCPPrefix) {
		return ActionMCP
	}
	if kind, ok := claudeToolKinds[name]; ok {
		return kind
	}
	return ActionOther
}

// claudeMCPTarget splits mcp__<server>__<tool>.
//
// Claude Code joins the two with a double underscore after rewriting the
// server's name into the characters it allows, so a server whose own name
// holds one cannot be split back unambiguously. The first "__" after the
// prefix ends the server, which is right for every name without one; a
// name that does not split at all names no target rather than a wrong one.
func claudeMCPTarget(name string) *MCPTarget {
	rest, ok := strings.CutPrefix(name, claudeMCPPrefix)
	if !ok {
		return nil
	}
	server, tool, ok := strings.Cut(rest, "__")
	if !ok || server == "" || tool == "" {
		return nil
	}
	return &MCPTarget{Server: server, Tool: tool}
}

// claudeToolInput is the part of a tool's input the recording reads.
type claudeToolInput struct {
	Command         string `json:"command"`
	FilePath        string `json:"file_path"`
	NotebookPath    string `json:"notebook_path"`
	URL             string `json:"url"`
	Query           string `json:"query"`
	RunInBackground bool   `json:"run_in_background"`
}

// claudeCall builds the call half of an Action from a tool_use block.
func claudeCall(id, name string, input json.RawMessage, parent, cwd string) Action {
	a := Action{
		ID:       id,
		ParentID: parent,
		Tool:     claudeKind(name),
		AppTool:  name,
		Args:     rawOrNull(input),
		Cwd:      cwd,
	}
	var in claudeToolInput
	_ = decodeTolerant(input, &in)
	switch a.Tool {
	case ActionExec:
		a.Command = in.Command
	case ActionFetch:
		a.URL, a.Query = in.URL, in.Query
	case ActionMCP:
		a.MCP = claudeMCPTarget(name)
	}
	// Only the whole-file tools name a file whose content is the point of
	// the call.
	switch name {
	case "Read":
		a.Contents = contentFor(ContentRead, in.FilePath, cwd)
	case "Write", "Edit", "MultiEdit":
		a.Contents = contentFor(ContentWrite, in.FilePath, cwd)
	case "NotebookEdit":
		a.Contents = contentFor(ContentWrite, in.NotebookPath, cwd)
	}
	return a
}

// claudeFinish writes what a tool_result reported onto its call.
//
// A FAILED call reads no file: a Write that was refused wrote nothing, and
// what sits at its path is somebody else's content.
func claudeFinish(a *Action, content json.RawMessage, isError bool, record json.RawMessage) {
	failed := isError
	a.IsError = &failed
	value, _ := decodeValue(content)
	result := blocksValue(value)
	a.ResultType, a.ResultDigest = resultShape(result)
	if a.Tool == ActionExec {
		a.ExitCode = claudeExitCode(a.Args, result, isError, record)
	}
	if isError {
		a.Contents = nil
		return
	}
	if a.AppTool == "Read" && len(a.Contents) == 1 {
		a.Contents[0].Seen = claudeReadSeen(record)
	}
}

// claudeReadSeen is the file content a Read's own record says the app
// got: `tool_use_result.file.content`, the raw text without the line
// numbers the model is shown (recorded from 2.1.270:
// {"type":"text","file":{"filePath":...,"content":"alpha\nbeta\n",...}}).
// A window of the file -- an offset, a limit, a truncated long line --
// is still what the app got, and the session compares it with the file:
// only a Read that got the whole file lets the file's bytes travel.
func claudeReadSeen(record json.RawMessage) []byte {
	var rec struct {
		File *struct {
			Content *string `json:"content"`
		} `json:"file"`
	}
	if !decodeTolerant(record, &rec) || rec.File == nil || rec.File.Content == nil {
		return nil
	}
	return []byte(*rec.File.Content)
}

// claudeExitLine is how a failed Bash call reports its status: the
// result's text opens "Exit code <n>" (recorded: "Exit code 3\noops").
var claudeExitLine = regexp.MustCompile(`^Exit code (-?\d+)`)

// claudeExitCode is the exit status Claude Code REPORTED for a Bash call,
// or nil when it reported none.
//
// Claude Code puts no exit status on the wire as a field, so both answers
// below are read out of what it does say, and each is claimed only where
// it is certain -- every case recorded from 2.1.270:
//
//   - a failed call's text opens "Exit code <n>", and n is the status. A
//     failure that says anything else -- a permission refusal, a timeout
//     -- reported no status, and none is claimed.
//   - a call that succeeded exited 0 ONLY when its structured record says
//     it ran to the end in the foreground and Claude Code did not
//     reinterpret the status. `grep` finding nothing exits 1 and comes
//     back as a success carrying returnCodeInterpretation "No matches
//     found"; a call sent to the background has not exited at all.
//     Reading 0 into either would record a fact that is false.
func claudeExitCode(args json.RawMessage, result any, isError bool, record json.RawMessage) *int {
	if isError {
		text, _ := result.(string)
		m := claudeExitLine.FindStringSubmatch(text)
		if m == nil {
			return nil
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return nil
		}
		return &n
	}
	var in claudeToolInput
	if decodeTolerant(args, &in) && in.RunInBackground {
		return nil
	}
	var rec struct {
		Interrupted              *bool  `json:"interrupted"`
		BackgroundTaskID         string `json:"backgroundTaskId"`
		ReturnCodeInterpretation string `json:"returnCodeInterpretation"`
	}
	if json.Unmarshal(record, &rec) != nil || rec.Interrupted == nil || *rec.Interrupted {
		return nil
	}
	if rec.BackgroundTaskID != "" || strings.TrimSpace(rec.ReturnCodeInterpretation) != "" {
		return nil
	}
	zero := 0
	return &zero
}
