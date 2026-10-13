package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// runFsRead implements workerHost.fs_read.
func runFsRead(_ context.Context, args map[string]any, policy *Policy) (*memqlv1.Success, *memqlv1.Failure) {
	path := strings.TrimSpace(argString(args, "path"))
	if path == "" {
		return nil, failure("bad_request", "fs_read: path required")
	}
	if err := policy.CheckPath(path); err != nil {
		return nil, failure("fs_denied", err.Error())
	}
	maxBytes := argInt(args, "maxBytes", 5*1024*1024)
	f, err := os.Open(expandHome(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, failure("fs_not_found", err.Error())
		}
		return nil, failure("fs_failed", err.Error())
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, failure("fs_failed", err.Error())
	}
	limited := io.LimitReader(f, int64(maxBytes))
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, failure("fs_failed", err.Error())
	}
	return successJSON(map[string]any{
		"path":      path,
		"content":   string(body),
		"size":      len(body),
		"fileSize":  info.Size(),
		"truncated": info.Size() > int64(len(body)),
	}, 0, 0, len(body), fileReadPreview(body, info.Size())), nil
}

// Keep completeness at the end of the bounded preview, so a retained result
// tail still distinguishes a file prefix from a complete read.
func fileReadPreview(body []byte, fileSize int64) string {
	metadata := fmt.Sprintf("\n[File read: returnedBytes=%d fileSize=%d truncated=%t]", len(body), fileSize, fileSize > int64(len(body)))
	return truncate(string(body), 1024-len(metadata)) + metadata
}

// runFsWrite implements workerHost.fs_write.
func runFsWrite(_ context.Context, args map[string]any, policy *Policy) (*memqlv1.Success, *memqlv1.Failure) {
	path := strings.TrimSpace(argString(args, "path"))
	if path == "" {
		return nil, failure("bad_request", "fs_write: path required")
	}
	if err := policy.CheckPath(path); err != nil {
		return nil, failure("fs_denied", err.Error())
	}
	content := argString(args, "content")
	mode := os.FileMode(0o644)
	if modeStr := argString(args, "mode"); modeStr != "" {
		if parsed, err := strconv.ParseUint(modeStr, 8, 32); err == nil {
			mode = os.FileMode(parsed)
		}
	}
	full := expandHome(path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, failure("fs_failed", err.Error())
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		return nil, failure("fs_failed", err.Error())
	}
	return successJSON(map[string]any{
		"path":  path,
		"bytes": len(content),
	}, 0, len(content), 0, ""), nil
}

// runFsList implements workerHost.fs_list.
func runFsList(_ context.Context, args map[string]any, policy *Policy) (*memqlv1.Success, *memqlv1.Failure) {
	path := strings.TrimSpace(argString(args, "path"))
	if path == "" {
		return nil, failure("bad_request", "fs_list: path required")
	}
	if err := policy.CheckPath(path); err != nil {
		return nil, failure("fs_denied", err.Error())
	}
	entries, err := os.ReadDir(expandHome(path))
	if err != nil {
		return nil, failure("fs_failed", err.Error())
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		info, _ := e.Info()
		row := map[string]any{
			"name": e.Name(),
			"type": typeOf(e),
		}
		if info != nil {
			row["size"] = info.Size()
			row["mode"] = info.Mode().String()
		}
		out = append(out, row)
	}
	return successJSON(map[string]any{
		"path":    path,
		"entries": out,
	}, 0, 0, 0, ""), nil
}

// runFsStat implements workerHost.fs_stat.
func runFsStat(_ context.Context, args map[string]any, policy *Policy) (*memqlv1.Success, *memqlv1.Failure) {
	path := strings.TrimSpace(argString(args, "path"))
	if path == "" {
		return nil, failure("bad_request", "fs_stat: path required")
	}
	if err := policy.CheckPath(path); err != nil {
		return nil, failure("fs_denied", err.Error())
	}
	info, err := os.Stat(expandHome(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, failure("fs_not_found", err.Error())
		}
		return nil, failure("fs_failed", err.Error())
	}
	return successJSON(map[string]any{
		"path":    path,
		"name":    info.Name(),
		"size":    info.Size(),
		"mode":    info.Mode().String(),
		"isDir":   info.IsDir(),
		"modTime": info.ModTime().UTC(),
	}, 0, 0, 0, ""), nil
}

func typeOf(e os.DirEntry) string {
	if e.IsDir() {
		return "dir"
	}
	if e.Type()&os.ModeSymlink != 0 {
		return "symlink"
	}
	return "file"
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
