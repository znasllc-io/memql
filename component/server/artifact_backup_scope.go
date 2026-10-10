package server

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// Paths are retained only for a requested, caller-owned directory backup.
// Ordinary uploads never enroll themselves by claiming a machine and path.
type libraryBackupScopeStore interface {
	BackupDirectory(context.Context, string, string) (string, error)
}

func (h *ArtifactHandler) backupUploadPath(ctx context.Context, watchID, workerID, sourcePath string) (string, int, string) {
	if watchID == "" {
		return "", 0, ""
	}
	if workerID == "" || sourcePath == "" {
		return "", http.StatusBadRequest, "a folder backup needs its machine and file path"
	}
	store, ok := h.store.(libraryBackupScopeStore)
	if !ok {
		return "", http.StatusServiceUnavailable, "folder backup verification is unavailable"
	}
	root, err := store.BackupDirectory(ctx, watchID, workerID)
	if err != nil {
		h.logger.Error("verify folder backup", "error", err)
		return "", http.StatusInternalServerError, "could not verify the folder backup"
	}
	if root == "" || !withinBackupDirectory(root, sourcePath) {
		return "", http.StatusForbidden, "this file is outside an active folder backup on your machine"
	}
	return sourcePath, 0, ""
}

// Interpret separators from the originating machine, never the server OS.
// Require canonical absolute paths and a strict descendant, not a sibling prefix.
func withinBackupDirectory(root, file string) bool {
	root = strings.ReplaceAll(root, `\`, "/")
	file = strings.ReplaceAll(file, `\`, "/")
	absolute := func(p string) bool { return strings.HasPrefix(p, "/") || len(p) > 2 && p[1] == ':' && p[2] == '/' }
	if !absolute(root) || !absolute(file) {
		return false
	}
	root = strings.TrimRight(root, "/")
	if root == "" {
		root = "/"
	}
	clean := func(p string) string {
		if strings.HasPrefix(p, "//") {
			return "/" + path.Clean(strings.TrimPrefix(p, "/"))
		}
		return path.Clean(p)
	}
	if clean(root) != root || clean(file) != file {
		return false
	}
	return strings.HasPrefix(file, strings.TrimRight(root, "/")+"/") && file != root
}

func (s *EngineLibraryStore) BackupDirectory(ctx context.Context, watchID, workerID string) (string, error) {
	result, err := s.exec(memql.ContextWithFreshRead(ctx), "libraryWatchedFolders", map[string]any{"workerId": workerID})
	if err != nil {
		return "", fmt.Errorf("read folder backups: %w", err)
	}
	for _, row := range memql.MaterializeRows(result) {
		if memql.BareShortId(rowString(row, "id")) == memql.BareShortId(watchID) && memql.BareShortId(rowString(row, "workerId")) == memql.BareShortId(workerID) && rowString(row, "status") == "active" {
			return rowString(row, "localPath"), nil
		}
	}
	return "", nil
}
