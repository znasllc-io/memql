package library

import (
	"context"
	"fmt"
	"mime"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// A file delivery receipt names a file, not its asynchronously indexed artifact.
// Resolve the current revision under the same cross-replica gate as file writes;
// the caller never has to guess an artifact identity or a version number.
func (i *Integration) handleReadFile(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, ok := auth.SubjectFromContext(ctx); !ok {
		return nil, fmt.Errorf("sign in to read a saved file")
	}
	fileID := strings.TrimSpace(asString(args["fileId"]))
	if fileID == "" {
		return nil, fmt.Errorf("choose a saved file")
	}
	ctx = memql.ContextWithFreshRead(ctx)
	read := func() (map[string]any, error) {
		rows, err := i.historyRows(ctx, "libraryFileById", map[string]any{"fileId": fileID})
		if err != nil {
			return nil, err
		}
		if len(rows) != 1 || boolField(rows[0], "archived") || stringField(rows[0], "ownerUserId") == "" {
			return nil, fmt.Errorf("file is unavailable")
		}
		return rows[0], nil
	}
	row, err := read()
	if err != nil {
		return nil, err
	}
	release, err := i.fileVersionGate(ctx, stringField(row, "id"))
	if err != nil {
		return nil, err
	}
	defer release()
	row, err = read()
	if err != nil {
		return nil, err
	}
	mediaType, _, err := mime.ParseMediaType(stringField(row, "mimeType"))
	if err != nil || !(strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" ||
		mediaType == "application/xml" || mediaType == "application/yaml" || mediaType == "image/svg+xml") {
		return nil, fmt.Errorf("this reader supports saved text files; use a format-specific reader for binary files")
	}
	version, _ := intArg(row["versionNumber"])
	version = max(version, 1)
	doc := reviewDocument{kind: "file", source: stringField(row, "id"), owner: stringField(row, "ownerUserId"),
		version: version, revision: fmt.Sprintf("file:%d", version), backing: row}
	content, revision, err := i.historicalContent(ctx, doc, version)
	if err != nil {
		return nil, err
	}
	return reviewResult(map[string]any{"fileId": memql.BareShortId(doc.source), "name": stringField(row, "name"),
		"mimeType": mediaType, "version": version, "revision": revision, "content": content})
}
