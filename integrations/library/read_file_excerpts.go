package library

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// handleReadFileExcerpts serves libraryReadFileExcerpts: bounded excerpts of
// several saved text files under one budget (design record
// docs/superpowers/specs/2026-10-29-failed-run-diagnosis-evidence-design.md,
// section 4). Its first caller is the failed-run diagnosis, which reads up to
// three failed steps' retained logs in one call.
//
// EVERY FILE IS AUTHORIZED AS libraryReadFile AUTHORIZES IT (readableTextFile),
// and bytes are opened only after that, through openFileBytes. A file this
// caller cannot read, or whose bytes cannot be opened or read, is reported in
// its own entry with libraryReadFile's sentence, so one unreadable log does not
// cost a diagnosis the others. A database or gate failure fails the call, so
// the caller's own failure handling can retry it. Malformed input is refused
// before any row is read.
//
// ONE BUDGET, MAX-MIN FAIR BY ACTUAL USE: files are read in ascending stored
// size, each given what is left divided by the files still unread, so a small
// file comes back whole and the larger ones share what it did not use. The
// stored size only orders the reads (an unlocked read under the caller); no
// excerpt exceeds its share whatever the bytes turn out to be. Gates are taken
// one file at a time, never several at once.
//
// The label is echoed so a recipe can pair a result with what it asked for
// without comparing identifiers. It confers nothing.
func (i *Integration) handleReadFileExcerpts(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, ok := auth.SubjectFromContext(ctx); !ok {
		return nil, fmt.Errorf("sign in to read a saved file")
	}
	files, err := excerptRequests(args["files"])
	if err != nil {
		return nil, err
	}
	raw, ok := excerptStrings(args["patterns"])
	if !ok {
		return nil, fmt.Errorf("patterns must be a list of strings")
	}
	patterns, err := compileExcerptPatterns(raw)
	if err != nil {
		return nil, err
	}
	maxBytes, ok := intArg(args["maxBytes"])
	if !ok || maxBytes < excerptMinBytes || maxBytes > excerptMaxBytes {
		return nil, fmt.Errorf("maxBytes must be %d to %d", excerptMinBytes, excerptMaxBytes)
	}
	if i.blobFetcher == nil {
		return nil, fmt.Errorf("document storage is unavailable on this node")
	}
	ctx = memql.ContextWithFreshRead(ctx)
	for k := range files {
		rows, err := i.historyRows(ctx, "libraryFileById", map[string]any{"fileId": files[k].fileID})
		if err != nil {
			return nil, err
		}
		if len(rows) == 1 {
			files[k].size, _ = intArg(rows[0]["size"])
		}
	}
	order := make([]int, len(files))
	for k := range order {
		order[k] = k
	}
	sort.SliceStable(order, func(a, b int) bool { return files[order[a]].size < files[order[b]].size })
	results := make([]map[string]any, len(files))
	remaining := maxBytes
	for done, k := range order {
		entry, used, err := i.excerptOne(ctx, files[k].fileID, patterns, remaining/(len(order)-done))
		if err != nil {
			return nil, err
		}
		entry["label"] = files[k].label
		results[k] = entry
		remaining -= used
	}
	return reviewResult(map[string]any{"files": results})
}

// excerptOne reads one file's excerpt within share. It returns the entry and
// the bytes it used; an unavailable file uses none.
func (i *Integration) excerptOne(ctx context.Context, fileID string, patterns []*regexp.Regexp, share int) (map[string]any, int, error) {
	row, _, release, unavailable, err := i.readableTextFile(ctx, fileID)
	if err != nil {
		return nil, 0, err
	}
	if unavailable != "" {
		return map[string]any{"fileId": fileID, "unavailable": unavailable}, 0, nil
	}
	defer release()
	stream, err := i.openFileBytes(ctx, stringField(row, "ownerUserId"), stringField(row, "blobUrl"))
	if err != nil {
		return map[string]any{"fileId": fileID, "unavailable": "could not read this saved version"}, 0, nil
	}
	defer stream.Close()
	ex, err := excerptText(stream, patterns, share)
	if err != nil {
		return map[string]any{"fileId": fileID, "unavailable": "could not read this saved version"}, 0, nil
	}
	version, _ := intArg(row["versionNumber"])
	return map[string]any{
		"fileId": memql.BareShortId(stringField(row, "id")), "name": stringField(row, "name"), "version": max(version, 1),
		"excerpt": ex.Text, "complete": ex.Complete, "totalBytes": ex.TotalBytes, "totalLines": ex.TotalLines,
		"matchedLines": ex.MatchedLines, "scanCapped": ex.ScanCapped,
	}, len(ex.Text), nil
}

// excerptRequest is one entry of the files argument.
type excerptRequest struct {
	fileID, label string
	size          int
}

// excerptLabelMax bounds a label, which is echoed on every result.
const excerptLabelMax = 200

func excerptRequests(v any) ([]excerptRequest, error) {
	var list []map[string]any
	switch t := v.(type) {
	case []map[string]any:
		list = t
	case []any:
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("every file is an object with its fileId")
			}
			list = append(list, m)
		}
	}
	if len(list) == 0 || len(list) > excerptMaxFiles {
		return nil, fmt.Errorf("give 1 to %d files", excerptMaxFiles)
	}
	out := make([]excerptRequest, 0, len(list))
	for _, m := range list {
		id := strings.TrimSpace(asString(m["fileId"]))
		label := asString(m["label"])
		if id == "" {
			return nil, fmt.Errorf("every file needs its fileId")
		}
		if len(label) > excerptLabelMax {
			return nil, fmt.Errorf("a file's label is at most %d bytes", excerptLabelMax)
		}
		out = append(out, excerptRequest{fileID: id, label: label})
	}
	return out, nil
}

// excerptStrings reads a list argument that must hold only strings.
func excerptStrings(v any) ([]string, bool) {
	switch t := v.(type) {
	case []string:
		return t, true
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	case nil:
		return nil, true
	}
	return nil, false
}
