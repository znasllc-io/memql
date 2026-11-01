package library

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// excerptScanTime bounds the scanning of one call's files, all of them.
var excerptScanTime = 2 * time.Minute

// handleReadFileExcerpts serves libraryReadFileExcerpts: bounded excerpts of
// several saved text files under one budget (design record
// docs/superpowers/specs/2026-10-29-failed-run-diagnosis-evidence-design.md,
// section 4). Its first caller is the failed-run diagnosis, which reads up to
// three failed steps' retained logs in one call.
//
// EVERY FILE IS AUTHORIZED AS libraryReadFile AUTHORIZES IT (readableTextFile),
// and bytes are opened only after that, through openFileBytes. A file this
// caller cannot read is reported in its own entry with libraryReadFile's
// sentence, so one unreadable log does not cost a diagnosis the others; so is a
// file whose bytes no attempt could read (excerptBytesFailed). A database or gate
// failure, or storage that may recover, fails the call, so the caller's own
// failure handling can retry it. Malformed input is refused before any row is
// read.
//
// ONE BUDGET, MAX-MIN FAIR BY ACTUAL USE: files are read in ascending stored
// size, each given what is left divided by the files still unread, so a small
// file comes back whole and the larger ones share what it did not use. The
// stored size only orders the reads (an unlocked read under the caller); no
// excerpt exceeds its share whatever the bytes turn out to be. A share is never
// below the even split, which the input check holds to excerptMinBytes a file.
//
// THE VERSION GATE COVERS THE ROW READS ONLY. It is taken one file at a time
// and released before the file's bytes are opened: a version's bytes are never
// overwritten (a new version is a new blob), so the row read under the gate
// already names exactly the bytes to read, and a scan that held the gate would
// hold a gate-pool connection for as long as a caller's file and patterns make
// it last. The scan has a deadline of its own (excerptScanTime) for the same
// reason: any signed-in caller chooses both. A file the deadline stops is
// reported as not excerpted in time, which a retry would not change.
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
	if !ok || maxBytes < excerptMinBytes*len(files) || maxBytes > excerptMaxBytes {
		return nil, fmt.Errorf("maxBytes must be at least %d for each file, %d for these %d, and at most %d",
			excerptMinBytes, excerptMinBytes*len(files), len(files), excerptMaxBytes)
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
	scanCtx, cancel := context.WithTimeout(ctx, excerptScanTime)
	defer cancel()
	results := make([]map[string]any, len(files))
	remaining := maxBytes
	for done, k := range order {
		entry, used, err := i.excerptOne(ctx, scanCtx, files[k].fileID, patterns, remaining/(len(order)-done))
		if err != nil {
			return nil, err
		}
		entry["label"] = files[k].label
		results[k] = entry
		remaining -= used
	}
	return reviewResult(map[string]any{"files": results})
}

// excerptOne reads one file's excerpt within share: its row under ctx, its
// bytes under scanCtx. It returns the entry and the bytes it used; an
// unavailable file uses none. Every entry answers with the bare id, whichever
// way the caller named the file.
func (i *Integration) excerptOne(ctx, scanCtx context.Context, fileID string, patterns []*regexp.Regexp, share int) (map[string]any, int, error) {
	bare := memql.BareShortId(fileID)
	row, _, release, unavailable, err := i.readableTextFile(ctx, fileID)
	if err != nil {
		return nil, 0, err
	}
	if unavailable != "" {
		return map[string]any{"fileId": bare, "unavailable": unavailable}, 0, nil
	}
	release()
	if err := scanCtx.Err(); err != nil {
		return i.excerptBytesFailed(ctx, scanCtx, bare, err)
	}
	stream, err := i.openFileBytes(scanCtx, stringField(row, "ownerUserId"), stringField(row, "blobUrl"))
	if err != nil {
		return i.excerptBytesFailed(ctx, scanCtx, bare, err)
	}
	defer stream.Close()
	ex, err := excerptText(scanCtx, stream, patterns, share)
	if err != nil {
		return i.excerptBytesFailed(ctx, scanCtx, bare, err)
	}
	version, _ := intArg(row["versionNumber"])
	return map[string]any{
		"fileId": memql.BareShortId(stringField(row, "id")), "name": stringField(row, "name"), "version": max(version, 1),
		"excerpt": ex.Text, "complete": ex.Complete, "totalBytes": ex.TotalBytes, "totalLines": ex.TotalLines,
		"matchedLines": ex.MatchedLines, "scanCapped": ex.ScanCapped,
	}, len(ex.Text), nil
}

// excerptLate is the entry of a file the scan's deadline stopped.
const excerptLate = "this file could not be excerpted in time"

// excerptBytesFailed answers a failed open or read of one file's bytes, and
// logs the cause under the file's id, since no answer below carries it.
//
// AN ENTRY IS AN ANSWER NO ATTEMPT CHANGES. The diagnosis recipe reads a log with
// an entry as gone and, when every log has one, ends for good with a result and
// no retry. So a failure is an entry only when another attempt would get the same
// answer: the scan's deadline, a stored key openFileBytes refuses, or a blob that
// is absent (azureblob.IsBlobAbsent; a retired log is the case it was written
// for). The deadline is judged by the scan's own context and never by the error,
// because storage's own timeout says the same words and may well pass.
//
// ANYTHING ELSE MAY CLEAR, SO IT FAILS THE CALL, and with it any other file's
// result, which the next attempt reads again at no cost beyond the read. Storage
// refusing the node's credential fails here too: it is no blip, but the failure
// path's retry budget bounds it and a person is then asked, which is better than
// a result saying the log is gone.
//
// THE ERROR IS A FIXED SENTENCE, NOT THE CAUSE, because the failure path acts on
// its words (component/work/symptom.go): "temporarily unavailable" is what
// transient.unavailable retries. Storage's own text is no use to that table. A
// 500 reaches no rule at all, so it would cost a model call, and a path or a
// request id can hold a "403" or a "404", which send a run to a person with the
// wrong question. TestReadFileExcerptsFailsTheCallWhenStorageFails holds the
// sentence to the table.
func (i *Integration) excerptBytesFailed(ctx, scanCtx context.Context, fileID string, cause error) (map[string]any, int, error) {
	if ctx.Err() != nil {
		return nil, 0, ctx.Err()
	}
	i.log().Warn("library: could not read a saved file's bytes for an excerpt", "fileId", fileID, "error", cause)
	switch {
	case scanCtx.Err() != nil:
		return map[string]any{"fileId": fileID, "unavailable": excerptLate}, 0, nil
	case errors.Is(cause, errFileKeyRefused), azureblob.IsBlobAbsent(cause):
		return map[string]any{"fileId": fileID, "unavailable": "could not read this saved version"}, 0, nil
	}
	return nil, 0, fmt.Errorf("saved file %s could not be read: document storage may be temporarily unavailable", fileID)
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
