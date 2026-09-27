package procedure

import (
	"fmt"
	"sort"
	"strings"

	proc "github.com/znasllc-io/memql/component/procedure"
)

// replayable.go -- a version no dispatcher can run never climbs (epic
// memql#5408; the coordinator's Task 4 decision).
//
// A learned procedure is promoted on its SHADOW evidence, and shadow evidence
// can be earned by a step no replay could ever perform: a dry comparison of
// arguments matches, the app keeps serving, and the ladder climbs toward a
// canary whose first real replay refuses. Worse, the promotion approval would
// ask a person to vouch for something the cluster cannot do. So one pure
// predicate decides, per step, whether the dispatchers (seams.go) run it, and
// the SAME predicate is asked in two places:
//
//	the lift     a version with any step it refuses stays a CANDIDATE, its
//	             reason naming the step and the tool
//	the runner   refuses to start such a version, before the first step
//
// THE FORMS ARE THE DISPATCHER CONTRACT. What is accepted here is exactly what
// a Dispatcher must translate (DispatchRequest.Args is the app's own spelling):
//
//	exec      {command}: a command line, or an argument vector (Codex), whose
//	          PROGRAM is a literal. `run_in_background: true` is refused: a
//	          command the app left running reported no result to compare.
//	fs_write  Claude Code's Write {file_path, content}, Edit {file_path,
//	          old_string, new_string[, replace_all]}, MultiEdit {file_path,
//	          edits: [{old_string, new_string[, replace_all]}]}. Codex's
//	          fileChange {changes: [...]} is REFUSED, an `add` included: no
//	          dispatcher reads `changes`, so a procedure made of one climbed
//	          on dry shadow comparisons, asked a person to approve it, and
//	          failed every canary.
//	fs_read   {file_path} or {path} -- one file. A listing (a Glob or Grep
//	          `pattern`) reads no one file's content.
//	fetch     {url}. A web search names no URL to fetch again.
//	mcp       {tool, arguments} as the MCP node records a call back into
//	          MemQL, the tool NAMED by a literal: a goal input must never
//	          choose which tool runs.
//
// Everything else -- every level-2 `automation:<name>` step, the app's answer,
// a tool this package has never heard of -- is not replayable.

// replayable reports whether a dispatcher can run one template step, and why
// not when it cannot.
func replayable(s proc.TemplateStep) (bool, string) {
	tool := strings.TrimSpace(s.Tool)
	args := s.Args
	if strings.HasPrefix(tool, "automation:") {
		return false, fmt.Sprintf("it calls the automation %s, and no action dispatcher runs an automation", strings.TrimPrefix(tool, "automation:"))
	}
	if args != nil && args.Kind != proc.KindObject {
		return false, "its arguments are not an object, so no dispatcher can read them"
	}
	switch tool {
	case "exec":
		return replayableExec(args)
	case "fs_write":
		return replayableWrite(args)
	case "fs_read":
		if has(args, "pattern") {
			return false, "it is a listing (a pattern), not a read of one file whose content a replay can compare"
		}
		if !has(args, "file_path") && !has(args, "path") {
			return false, "it names no file to read (file_path or path)"
		}
		return true, ""
	case "fetch":
		if !has(args, "url") {
			return false, "it names no URL (a search is not a fetch a dispatcher can repeat)"
		}
		return true, ""
	case "mcp":
		name, ok := literalAt(args, "tool")
		if !ok || strings.TrimSpace(name) == "" {
			return false, "it does not name the MemQL tool it called with a literal, so a replay could not know which tool to call"
		}
		return true, ""
	case "":
		return false, "it names no tool"
	}
	return false, fmt.Sprintf("no dispatcher runs the tool %q", tool)
}

// replayableExec is the exec half of the contract.
func replayableExec(args *proc.Node) (bool, string) {
	cmd, ok := at(args, "command")
	if !ok {
		return false, "it carries no command (the app's call was not a shell command)"
	}
	if background, ok := literalAt(args, "run_in_background"); ok && background == "true" {
		return false, "the app ran it in the background, so it reported no result a replay could be compared with"
	}
	switch cmd.Kind {
	case proc.KindLit:
		if strings.TrimSpace(cmd.Lit) == "" {
			return false, "its command is empty"
		}
		return true, ""
	case proc.KindArray:
		if len(cmd.Kids) == 0 || cmd.Kids[0] == nil {
			return false, "its command is empty"
		}
		if cmd.Kids[0].Kind != proc.KindLit {
			return false, "the program its command runs is a parameter, and a goal's input must never choose what runs"
		}
		return true, ""
	}
	return false, "its whole command is a parameter, and a goal's input must never choose what runs"
}

// replayableWrite is the fs_write half of the contract.
func replayableWrite(args *proc.Node) (bool, string) {
	if has(args, "changes") {
		return false, "it writes through Codex's file changes (changes), which no dispatcher applies"
	}
	if !has(args, "file_path") {
		if has(args, "notebook_path") {
			return false, "it edits a notebook cell, which no dispatcher applies"
		}
		return false, "it names no file (file_path)"
	}
	switch {
	case has(args, "content"):
		return true, ""
	case has(args, "old_string") && has(args, "new_string"):
		return true, ""
	case has(args, "edits"):
		edits, _ := at(args, "edits")
		if edits.Kind != proc.KindArray || len(edits.Kids) == 0 {
			return false, "its edits are not a list of replacements"
		}
		for _, e := range edits.Kids {
			if e == nil || e.Kind != proc.KindObject || !has(e, "old_string") || !has(e, "new_string") {
				return false, "one of its edits is not an old_string / new_string replacement"
			}
		}
		return true, ""
	}
	return false, fmt.Sprintf("it writes in a form no dispatcher applies (%s)", strings.Join(keysOf(args), ", "))
}

// firstUnreplayable is the first step of a template no dispatcher runs, as
// the sentence the ladder and the runner both say -- numbered from 1, as MemQL
// OS lists a procedure's steps.
func firstUnreplayable(t proc.Template) (string, bool) {
	for idx, s := range t.Steps {
		if ok, why := replayable(s); !ok {
			return fmt.Sprintf("step %d (%s) cannot be replayed: %s", idx+1, oneLine(s.Tool), why), true
		}
	}
	return "", false
}

// at walks object keys.
func at(n *proc.Node, keys ...string) (*proc.Node, bool) {
	if n == nil {
		return nil, false
	}
	child, ok := n.At(keys)
	return child, ok && child != nil
}

func has(n *proc.Node, key string) bool {
	_, ok := at(n, key)
	return ok
}

// literalAt is the literal at a path, or false when it is absent or not a
// literal (a hole, a subtree).
func literalAt(n *proc.Node, keys ...string) (string, bool) {
	child, ok := at(n, keys...)
	if !ok || child.Kind != proc.KindLit {
		return "", false
	}
	return child.Lit, true
}

func keysOf(n *proc.Node) []string {
	if n == nil || n.Kind != proc.KindObject {
		return nil
	}
	out := append([]string(nil), n.Keys...)
	sort.Strings(out)
	return out
}
