package library

import (
	"context"
	"errors"
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
	row, mediaType, release, unavailable, err := i.readableTextFile(ctx, fileID)
	if err != nil {
		return nil, err
	}
	if unavailable != "" {
		return nil, errors.New(unavailable)
	}
	defer release()
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

// readableTextFile resolves a saved text file's current row for both readers:
// read under the caller, so row admission decides who may see it; read again
// under the file's version gate, so the row and the bytes read next are one
// revision; and held to text types. release ends the gate and is set only when
// the file is readable. unavailable is the sentence for a file this caller
// cannot read here; err is any other failure.
func (i *Integration) readableTextFile(ctx context.Context, fileID string) (row map[string]any, mediaType string, release func(), unavailable string, err error) {
	read := func() (map[string]any, string, error) {
		rows, err := i.historyRows(ctx, "libraryFileById", map[string]any{"fileId": fileID})
		if err != nil {
			return nil, "", err
		}
		if len(rows) != 1 || boolField(rows[0], "archived") || stringField(rows[0], "ownerUserId") == "" {
			return nil, "file is unavailable", nil
		}
		return rows[0], "", nil
	}
	if row, unavailable, err = read(); err != nil || unavailable != "" {
		return nil, "", nil, unavailable, err
	}
	if release, err = i.fileVersionGate(ctx, stringField(row, "id")); err != nil {
		return nil, "", nil, "", err
	}
	if row, unavailable, err = read(); err != nil || unavailable != "" {
		release()
		return nil, "", nil, unavailable, err
	}
	mediaType, _, perr := mime.ParseMediaType(stringField(row, "mimeType"))
	if perr != nil || !(strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" ||
		mediaType == "application/xml" || mediaType == "application/yaml" || mediaType == "image/svg+xml") {
		release()
		return nil, "", nil, "this reader supports saved text files; use a format-specific reader for binary files", nil
	}
	return row, mediaType, release, "", nil
}
