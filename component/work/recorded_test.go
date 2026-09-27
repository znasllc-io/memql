package work

import (
	"reflect"
	"strings"
	"testing"
)

// The entries below are spelled exactly as component/worker's storeContents
// writes them: "<path>: <err>" for a Library write that failed, and
// "<path>: <omitted> (sha256 <hex>)" for a content above the per-file cap.
func TestParseOmittedReadsThePathTheReasonAndTheDigest(t *testing.T) {
	cases := []struct {
		entry                 string
		wantPath, wantWhy, wd string
	}{
		{"src/main.go: above the 1 MiB per-file cap (sha256 ABCDEF0123)", "src/main.go", "above the 1 MiB per-file cap", "abcdef0123"},
		{"notes.txt: library unreachable", "notes.txt", "library unreachable", ""},
		{"/abs/path/data.csv: this node stores no content", "/abs/path/data.csv", "this node stores no content", ""},
		// A reason that itself contains ": " keeps everything after the first
		// separator: the path is what comes before it.
		{"a.txt: write failed: disk full", "a.txt", "write failed: disk full", ""},
	}
	for _, tc := range cases {
		path, why, digest := ParseOmitted(tc.entry)
		if path != tc.wantPath || why != tc.wantWhy || digest != tc.wd {
			t.Errorf("ParseOmitted(%q) = (%q, %q, %q), want (%q, %q, %q)", tc.entry, path, why, digest, tc.wantPath, tc.wantWhy, tc.wd)
		}
	}
}

func TestParseOmittedRefusesAnEntryItCannotRead(t *testing.T) {
	for _, entry := range []string{"", "no separator here", "   "} {
		if path, why, digest := ParseOmitted(entry); path != "" || why != "" || digest != "" {
			t.Errorf("ParseOmitted(%q) = (%q, %q, %q); an entry the recorder never writes must name no path", entry, path, why, digest)
		}
	}
}

func TestRecordedPathsReadsOnlyNamedPathArguments(t *testing.T) {
	args := map[string]any{
		"file_path":  " src/app.go ",
		"old_string": "// src/not-a-path.go",
		"content":    "package main",
		"changes": []any{
			map[string]any{"path": "b.txt", "kind": "update"},
			map[string]any{"path": "a.txt", "kind": "add"},
			"not an object",
		},
		"options": map[string]any{"cwd": "/work"},
		"count":   3,
	}
	got := RecordedPaths(args)
	want := []string{"/work", "a.txt", "b.txt", "src/app.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RecordedPaths = %v, want %v -- a string that merely looks like a path is content, not a file the action touched", got, want)
	}
	if got := RecordedPaths(nil); len(got) != 0 {
		t.Errorf("RecordedPaths(nil) = %v, want none", got)
	}
}

func TestRecordedArgsDecodesTheStoredStringAndRefusesATruncatedOne(t *testing.T) {
	args, ok := RecordedArgs(map[string]any{"args": `{"file_path":"x.go","content":"y"}`})
	if !ok || args["file_path"] != "x.go" {
		t.Fatalf("RecordedArgs of a whole JSON string = (%v, %v)", args, ok)
	}
	if args, ok := RecordedArgs(map[string]any{"args": `{"file_path":"x.go","con`, "argsTruncated": true}); ok || args != nil {
		t.Errorf("a truncated argument set read as reproducible: (%v, %v)", args, ok)
	}
	if args, ok := RecordedArgs(map[string]any{}); !ok || len(args) != 0 {
		t.Errorf("absent arguments = (%v, %v), want no arguments and reproducible", args, ok)
	}
	args, ok = RecordedArgs(map[string]any{"args": "ls -la"})
	if !ok || args["_raw"] != "ls -la" {
		t.Errorf("a non-object argument must be kept as _raw rather than dropped, got (%v, %v)", args, ok)
	}
	args, ok = RecordedArgs(map[string]any{"args": map[string]any{"path": "p"}})
	if !ok || args["path"] != "p" {
		t.Errorf("an already-decoded object = (%v, %v)", args, ok)
	}
}

// The three helpers compose into what a snapshot needs from one recorded
// action: the file the content belongs to, from the arguments of the SAME
// action.
func TestARecordedWriteNamesTheFileItsContentBelongsTo(t *testing.T) {
	data := map[string]any{
		"tool":           "Write",
		"args":           `{"file_path":"reports/q3.md","content":"..."}`,
		"contentRefs":    []any{"v1:library:file:f1"},
		"contentOmitted": []any{"reports/big.bin: above the 1 MiB per-file cap (sha256 00ff)"},
	}
	args, ok := RecordedArgs(data)
	if !ok {
		t.Fatal("the arguments did not decode")
	}
	if paths := RecordedPaths(args); len(paths) != 1 || paths[0] != "reports/q3.md" {
		t.Errorf("paths = %v", paths)
	}
	omitted, _ := data["contentOmitted"].([]any)
	path, why, _ := ParseOmitted(omitted[0].(string))
	if path != "reports/big.bin" || !strings.Contains(why, "cap") {
		t.Errorf("omitted = (%q, %q)", path, why)
	}
}
