package procedure

import (
	"path"
	"strings"
	"unicode"
)

// relativize.go -- a recording's workspace, written as "the workspace" (epic
// memql#5408; the coordinator's decision recorded in the plan's Task 4 notes).
//
// EVERY RECORDING RUNS IN ITS OWN WORKSPACE, <workspaceRoot>/<runId>, and the
// apps write ABSOLUTE paths: Claude Code's Write names /ws/r1/out/report-a.txt,
// the next session's names /ws/r2/out/report-b.txt. Canonicalized as they
// were recorded, the workspace segment differs between every pair of
// recordings, so the template grew a FREE hole there -- one no goal input
// supplies, because no goal ever named a workspace. A procedure with such a
// hole can be compared in shadow (the app's own action binds it) and can
// never replay for real (nothing does), so no real procedure could ever be
// trusted.
//
// So the recording's workspace W is written as the workspace itself, BEFORE
// the corpus is canonicalized:
//
//	exactly W            ->  "."
//	W + "/" + rest       ->  "./" + rest
//	inside a command     ->  every occurrence of W bounded by path separators,
//	                         the same two ways (`cd /ws/r1 && npm test` ->
//	                         `cd . && npm test`)
//
// THE REWRITE PRESERVES WHAT THE ACTION DOES because every replay executes
// with its working directory set to its OWN fresh workspace -- the workbench
// keys one per replay run, and the machine dispatcher does the same -- so
// "./out" names, for the replay, exactly what /ws/r1/out named for the
// recording. And it is what makes recordings from different workspaces
// agree, so the one real parameter (the file name) is the only hole left.
//
// A PATH THAT MERELY SHARES THE PREFIX IS NOT THE WORKSPACE: /ws/r10 is not
// inside /ws/r1, and /tmp/ws/r1 is not /ws/r1. An occurrence counts only when
// nothing that could continue a path segment touches it on either side.
//
// The SAME function relativizes the app's actions in a shadow comparison, by
// construction: the comparison loads the recording through the corpus loader.

// actionWorkspace is the workspace one recorded action is read against: the
// recording's fingerprint cwd -- the session's own directory, which is what a
// command's `cd` elsewhere does not change -- and, for a recording that
// carried no fingerprint, the directory the action itself reported running
// in. With neither, nothing is relativized, and an absolute path stays what
// it was: the machine's (target.go).
func actionWorkspace(recordingWorkspace string, observation map[string]any) string {
	if ws := strings.TrimSpace(recordingWorkspace); ws != "" {
		return ws
	}
	return strings.TrimSpace(str(obj(observation, "data"), "cwd"))
}

// relativizeArgs rewrites every string leaf of an action's arguments against
// the workspace, returning a new map; the input is not modified. A workspace
// that is not an absolute directory below the root rewrites nothing: there is
// nothing to be relative TO, and "/" contains every path.
func relativizeArgs(args map[string]any, workspace string) map[string]any {
	ws := workspaceRoot(workspace)
	if ws == "" || args == nil {
		return args
	}
	out, _ := relativizeValue("", args, ws).(map[string]any)
	return out
}

// workspaceRoot is the cleaned workspace, or "" when there is none to use.
func workspaceRoot(workspace string) string {
	ws := strings.TrimSpace(workspace)
	if ws == "" || !path.IsAbs(ws) {
		return ""
	}
	ws = path.Clean(ws)
	if ws == "/" {
		return ""
	}
	return ws
}

// relativizeValue walks one value. key is the name the value arrived under,
// which is what decides whether a string is a COMMAND LINE (rewritten inside)
// or a value (rewritten only when it is the workspace or a path below it).
func relativizeValue(key string, v any, ws string) any {
	switch t := v.(type) {
	case string:
		if relativizedCommandKeys[key] {
			return relativizeCommand(t, ws)
		}
		return relativizeWhole(t, ws)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = relativizeValue(k, e, ws)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			// An ARGUMENT VECTOR under a command key keeps its key: each
			// element is one argument, and a shell's `-c` script element is
			// a command line of its own, which the inside rewrite handles.
			out[i] = relativizeValue(key, e, ws)
		}
		return out
	default:
		return v
	}
}

// relativizedCommandKeys are the argument names whose value is a command line
// -- the same names component/procedure's canonicalization splits as argv, so
// what is rewritten inside a string here is exactly what is parsed as a command
// there.
var relativizedCommandKeys = map[string]bool{"command": true, "cmd": true, "argv": true}

// relativizeWhole rewrites a value that IS the workspace or a path below it.
func relativizeWhole(s, ws string) string {
	switch {
	case s == ws:
		return "."
	case strings.HasPrefix(s, ws+"/"):
		return "./" + strings.TrimPrefix(s, ws+"/")
	}
	return s
}

// relativizeCommand rewrites every bounded occurrence of the workspace inside
// a command line.
func relativizeCommand(s, ws string) string {
	if !strings.Contains(s, ws) {
		return s
	}
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, ws)
		if i < 0 {
			b.WriteString(rest)
			break
		}
		end := i + len(ws)
		if !boundedBefore(rest, i) || !boundedAfter(rest, end) {
			// Part of a longer path (/tmp/ws/r1, /ws/r10): copied through,
			// and the search resumes one byte on so an overlapping bounded
			// occurrence is still found.
			b.WriteString(rest[:i+1])
			rest = rest[i+1:]
			continue
		}
		b.WriteString(rest[:i])
		if end < len(rest) && rest[end] == '/' {
			b.WriteString("./")
			end++
		} else {
			b.WriteString(".")
		}
		rest = rest[end:]
	}
	return b.String()
}

// boundedBefore reports that nothing that belongs to a path touches the
// occurrence at i from the left: the start of the string, or a character a
// path segment cannot contain (a space, a quote, `=`, `:`, `(` ...). A slash
// is NOT a boundary there -- /x/ws/r1 is a longer path that ends in the
// workspace's name, not the workspace.
func boundedBefore(s string, i int) bool {
	if i == 0 {
		return true
	}
	r := lastRune(s[:i])
	return r != '/' && !pathSegmentRune(r)
}

// boundedAfter reports that the occurrence ending at end is the whole of a
// path component: the end of the string, a slash (a path below it), or a
// character a path segment cannot contain.
func boundedAfter(s string, end int) bool {
	if end >= len(s) {
		return true
	}
	r := firstRune(s[end:])
	return r == '/' || !pathSegmentRune(r)
}

// pathSegmentRune is a character that can continue a path segment: /ws/r1 is
// not the workspace inside /ws/r10, /ws/r1.bak or /ws/r1-old.
func pathSegmentRune(r rune) bool {
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return true
	}
	switch r {
	case '.', '_', '-', '+', '~', '@', '%':
		return true
	}
	return false
}

func lastRune(s string) rune {
	rs := []rune(s)
	if len(rs) == 0 {
		return 0
	}
	return rs[len(rs)-1]
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}
