package tools

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

// readOpenedFile never reopens a checked path. Both permission lanes use the
// same bounded reader on their already-authorized descriptor.
func readOpenedFile(ctx context.Context, path string, f *os.File, args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	o, err := permission.ParseReadOptions(args)
	if err != nil {
		return nil, failure("bad_request", err.Error())
	}
	info, err := f.Stat()
	if err != nil {
		return nil, failure("fs_failed", err.Error())
	}
	if !info.Mode().IsRegular() {
		return nil, failure("fs_denied", "File reads require a regular file.")
	}
	if err = ctx.Err(); err != nil {
		return nil, failure("cancelled", err.Error())
	}
	var body []byte
	scanned, offset, next := 0, o.OffsetBytes, o.OffsetBytes
	complete := false
	matches := []map[string]any{}
	if o.StartLine == 0 && o.Search == "" {
		if _, err = f.Seek(int64(offset), io.SeekStart); err != nil {
			return nil, failure("fs_failed", err.Error())
		}
		body, err = io.ReadAll(io.LimitReader(f, int64(o.MaxBytes)))
		scanned = len(body)
		next = offset + len(body)
		complete = int64(next) >= info.Size()
	} else {
		// The scan budget includes bytes skipped before a line range. A bounded
		// prefix cannot establish absence in the rest of a large file.
		reader := bufio.NewReader(io.LimitReader(f, int64(o.MaxScanBytes)))
		line, returned := 0, 0
		for scanned < o.MaxScanBytes {
			if err = ctx.Err(); err != nil {
				break
			}
			value, readErr := reader.ReadString('\n')
			if readErr != nil && readErr != io.EOF {
				err = readErr
				break
			}
			if len(value) == 0 {
				complete = true
				break
			}
			line++
			start := scanned
			scanned += len(value)
			next = scanned
			fullLine := strings.HasSuffix(value, "\n") || int64(scanned) >= info.Size()
			if !fullLine {
				break
			} // Do not report a partial line as a complete match.
			if o.Search != "" && strings.Contains(value, o.Search) {
				if returned+len(value) > o.MaxBytes {
					next = start
					break
				}
				matches = append(matches, map[string]any{"line": line, "offsetBytes": start, "contentOffsetBytes": len(body), "lengthBytes": len(value)})
				body = append(body, []byte(value)...)
				returned += len(value)
				if len(matches) == o.MaxMatches {
					complete = int64(scanned) >= info.Size()
					break
				}
			} else if o.StartLine > 0 && line >= o.StartLine {
				if len(body) == 0 {
					offset = start
				}
				remaining := o.MaxBytes - len(body)
				body = append(body, []byte(value[:min(remaining, len(value))])...)
				next = start + min(remaining, len(value))
				if remaining < len(value) {
					break
				}
				if line >= o.StartLine+o.LineCount-1 {
					complete = true
					break
				}
			}
			if readErr == io.EOF || int64(scanned) >= info.Size() {
				complete = true
				break
			}
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, failure("cancelled", ctx.Err().Error())
		}
		return nil, failure("fs_failed", err.Error())
	}
	if err = ctx.Err(); err != nil {
		return nil, failure("cancelled", err.Error())
	}
	after, err := f.Stat()
	if err != nil {
		return nil, failure("fs_failed", err.Error())
	}
	changed := info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime())
	if changed {
		complete = false
	}
	truncated := !complete
	out := map[string]any{"path": path, "content": string(body), "size": len(body), "fileSize": info.Size(), "modTime": info.ModTime().UTC(), "truncated": truncated, "complete": complete, "changedDuringRead": changed, "offsetBytes": offset, "nextOffsetBytes": next, "scannedBytes": scanned, "sha256": fmt.Sprintf("%x", sha256.Sum256(body))}
	if o.StartLine > 0 {
		out["startLine"], out["lineCount"] = o.StartLine, o.LineCount
	}
	if o.Search != "" {
		out["search"], out["matches"] = o.Search, matches
	}
	preview := fileReadPreview(body, info.Size())
	if permission.ExtendedRead(args) || changed {
		preview = fmt.Sprintf("File inspection: returnedBytes=%d scannedBytes=%d fileSize=%d complete=%t changedDuringRead=%t", len(body), scanned, info.Size(), complete, changed)
	}
	return successJSON(out, 0, 0, len(body), preview), nil
}
