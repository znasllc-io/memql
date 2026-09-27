package work

// recorded.go -- reading what an app session's recording says about the files
// an action touched (epic memql#5414, design D19; the recording format is
// epic memql#5396's, design D5 and D12).
//
// A recorded action's evidence is a v1:work:observation of kind tool_result.
// Its `data.args` holds the action's arguments as a JSON STRING, whole up to a
// ceiling; its `data.contentRefs` are the Library FILE ids of the contents the
// action read or wrote, and `data.contentOmitted` names a content that was NOT
// stored as "<path>: <why>", with " (sha256 <hex>)" at the end when the digest
// was measured. Two readers need the same three facts out of that shape: the
// procedure corpus (integrations/procedure), which compares a replay's
// contents with the recording's, and a branch or re-run of a session step
// (integrations/work), which rebuilds the workspace the step started against.
// They live here, as pure functions over values, so the two readers cannot
// come to disagree about what one recorded action touched.

import (
	"encoding/json"
	"sort"
	"strings"
)

// ParseOmitted reads one `contentOmitted` entry: "<path>: <why>", with
// " (sha256 <hex>)" at the end when the digest was measured. why is the reason
// without the digest; digest is lower-cased. An entry with no ": " separator
// is not one the recorder writes, and answers three empty strings rather than a
// guessed path.
func ParseOmitted(entry string) (path, why, digest string) {
	p, rest, found := strings.Cut(entry, ": ")
	if !found {
		return "", "", ""
	}
	why = strings.TrimSpace(rest)
	const marker = "(sha256 "
	if i := strings.LastIndex(why, marker); i >= 0 && strings.HasSuffix(why, ")") {
		digest = strings.ToLower(strings.TrimSpace(why[i+len(marker) : len(why)-1]))
		why = strings.TrimSpace(why[:i])
	}
	return strings.TrimSpace(p), why, digest
}

// recordedPathKeys are the argument names whose string value is a file path.
// Named rather than sniffed: an argument that merely LOOKS like a path -- the
// old_string of an edit that begins with "//" -- is content, and reading it as
// a path would send a portable procedure to somebody's machine, or restore a
// file to a place no action ever wrote.
var recordedPathKeys = map[string]bool{
	"path": true, "file": true, "file_path": true, "filePath": true, "filepath": true,
	"filename": true, "targetPath": true, "target_path": true, "directory": true,
	"dir": true, "cwd": true, "notebook_path": true,
}

// RecordedPaths is every path-keyed string in an action's arguments, nested
// ones included (an object inside the arguments, or a list of objects, as a
// multi-file change reports), trimmed and sorted.
func RecordedPaths(args map[string]any) []string {
	var out []string
	var walk func(map[string]any)
	walk = func(m map[string]any) {
		for k, v := range m {
			switch t := v.(type) {
			case string:
				if recordedPathKeys[k] && strings.TrimSpace(t) != "" {
					out = append(out, strings.TrimSpace(t))
				}
			case map[string]any:
				walk(t)
			case []any:
				for _, e := range t {
					if sub, ok := e.(map[string]any); ok {
						walk(sub)
					}
				}
			}
		}
	}
	walk(args)
	sort.Strings(out)
	return out
}

// RecordedArgs reads an action's arguments off its tool_result data.
//
// `data.args` is the arguments as a JSON STRING, whole up to the observation
// ceiling. Past it the string is cut mid-document and `argsTruncated` says so:
// that call is NOT reproducible from the row, and reproducible reports false.
// A value that is not a JSON object is kept as {"_raw": value} -- what the app
// sent, in the one shape a caller can hold -- rather than dropped. An absent
// or blank value is no arguments, which is reproducible.
func RecordedArgs(data map[string]any) (args map[string]any, reproducible bool) {
	if truncated, _ := data["argsTruncated"].(bool); truncated {
		return nil, false
	}
	switch raw := data["args"].(type) {
	case nil:
		return map[string]any{}, true
	case map[string]any:
		return raw, true
	case string:
		if strings.TrimSpace(raw) == "" {
			return map[string]any{}, true
		}
		var decoded any
		if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
			if m, ok := decoded.(map[string]any); ok {
				return m, true
			}
		}
		return map[string]any{"_raw": raw}, true
	default:
		return map[string]any{"_raw": raw}, true
	}
}
