package workbench

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server/libraryfile"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// Binary delivery uses bounded chunks on the existing authorized dispatch path.
// No file contents enter a model prompt, and the Library receipt outlives the
// ephemeral workspace. The storage adapter verifies the committed bytes.
type exportBlobStore interface {
	CreateVerifiedStream(context.Context, string, string, io.Reader, int64, string, string) (azureblob.VerifiedBlob, error)
}

type exportExecutor struct{ integration *Integration }

func (e exportExecutor) Execute(ctx context.Context, query string) (any, error) {
	return e.integration.engine.Execute(ctx, query)
}

type exportChunk struct {
	Path, Encoding, Content, Version string
	Offset, Size                     int64
	EOF                              bool
}

func (i *Integration) exportFile(ctx context.Context, args map[string]any) (map[string]any, error) {
	runID, _ := args["runId"].(string)
	inner, _ := args["args"].(map[string]any)
	filePath, _ := inner["path"].(string)
	filePath = strings.TrimSpace(filePath)
	if filePath == "" || path.IsAbs(filePath) || strings.Contains(filePath, "\\") || path.Clean(filePath) == ".." || strings.HasPrefix(path.Clean(filePath), "../") {
		return nil, errors.New("fs_export requires a file path relative to this run's workspace")
	}
	filePath = path.Clean(filePath)
	name := path.Base(filePath)
	if name == "." || strings.ContainsAny(name, "\x00\r\n") {
		return nil, errors.New("fs_export requires a regular file with a valid filename")
	}
	if i.engine == nil || !i.store.available() {
		return nil, errors.New("Library is unavailable; no file was exported")
	}
	owner, err := i.workspaceOwner(ctx, runID)
	if err != nil {
		return nil, err
	}
	stepKey := ""
	if run, ok := common.RunFromContext(ctx); ok && run.RunId != "" {
		if memql.BareShortId(run.RunId) != memql.BareShortId(runID) || memql.BareShortId(run.OwnerUserId) != memql.BareShortId(owner) {
			return nil, errors.New("fs_export must belong to the executing owner's work run")
		}
		stepKey = run.StepKey
	}
	uploader, ok := i.uploader.(exportBlobStore)
	if !ok || i.bucket == "" {
		return nil, errors.New("Library byte storage is unavailable; no file was exported")
	}
	read := func(offset int64, version string) (exportChunk, error) {
		readArgs := map[string]any{"path": filePath, "encoding": "base64", "offset": offset, "maxBytes": 256 << 10}
		if version != "" {
			readArgs["version"] = version
		}
		request := make(map[string]any, len(args))
		for k, v := range args {
			request[k] = v
		}
		request["action"], request["args"] = "fs_read", readArgs
		nodes, err := i.handleDispatchHost(ctx, request, 0)
		if err != nil {
			return exportChunk{}, err
		}
		if len(nodes) != 1 {
			return exportChunk{}, errors.New("workspace returned no file receipt")
		}
		var receipt struct {
			OK      bool        `json:"ok"`
			Error   string      `json:"errorMessage"`
			Payload exportChunk `json:"payload"`
		}
		if err = json.Unmarshal(nodes[0].Payload, &receipt); err != nil {
			return exportChunk{}, err
		}
		if !receipt.OK {
			return exportChunk{}, fmt.Errorf("read export: %s", receipt.Error)
		}
		return receipt.Payload, nil
	}
	spool, err := os.CreateTemp("", "memql-workbench-export-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	size, digest, err := copyExport(ctx, spool, read)
	if err != nil {
		return nil, err
	}
	if _, err = spool.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	// The step comes from authenticated execution context, never tool arguments.
	// Separate steps cannot borrow one another's completion receipt.
	identity := genOutputIdEngine.MustFromMap(map[string]any{
		"ownerUserId": owner, "runId": runID, "stepKey": stepKey,
		"path": filePath, "sha256": digest,
	})
	fileID := "workbench-file-" + string(identity)
	key := "library/" + owner + "/" + fileID + "/" + name
	mimeType := mime.TypeByExtension(path.Ext(name))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	actorCtx := withUserActor(ctx, owner)
	stored, err := uploader.CreateVerifiedStream(actorCtx, i.bucket, key, spool, size, digest, mimeType)
	if err != nil {
		return nil, fmt.Errorf("store export: %w", err)
	}
	if stored.Size != size || stored.SHA256 != digest || strings.TrimSpace(stored.URL) == "" {
		return nil, errors.New("stored export failed integrity verification")
	}
	// A lost response can be retried on another replica. Preserve existing file
	// metadata and owner edits; the same run/step/path/bytes name the same receipt.
	rows, err := i.store.exec(actorCtx, "query libraryFileById(fileId:"+langparser.QuoteString(fileID)+")")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		err = libraryfile.NewStore(exportExecutor{i}).Create(actorCtx, map[string]any{
			"fileId": fileID, "name": name, "mimeType": mimeType, "size": size, "sha256": digest,
			"blobUrl": stored.URL, "source": "agent_generated", "producedByRunId": runID, "producedByStepKey": stepKey, "format": formatForExtension(name),
		})
		if err != nil {
			return nil, fmt.Errorf("record export in Library: %w", err)
		}
	}
	if len(rows) == 0 || rows[0]["status"] == "stored" {
		// Verified generated bytes are a finished deliverable. Like composition,
		// export must publish a ready receipt synchronously, without depending on
		// a later content-analysis job (archives may have no analyzable text).
		call, _ := langparser.RenderCall("setLibraryFileStatus", map[string]any{"fileId": fileID, "status": "ready"})
		if _, err := i.store.exec(actorCtx, "mutation "+call); err != nil {
			return nil, fmt.Errorf("finish exported file: %w", err)
		}
	}
	// The index automation may execute on another replica. Return a URL only
	// after the same owner-scoped query the download UI uses can resolve it.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		rows, err = i.store.exec(actorCtx, "query libraryArtifactForFile(fileId:"+langparser.QuoteString(fileID)+")")
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			id, _ := rows[0]["id"].(string)
			if id != "" {
				return map[string]any{"fileId": fileID, "artifactId": id, "name": name, "path": filePath, "size": size, "sha256": digest,
					"downloadUrl": "/_memql/artifacts/" + url.PathEscape(id) + "/content"}, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errors.New("file bytes are saved, but the Library index is not ready; retry fs_export for the same path")
		case <-tick.C:
		}
	}
}

func copyExport(ctx context.Context, dest io.Writer, read func(int64, string) (exportChunk, error)) (int64, string, error) {
	// Library and the storage adapter exchange the standard SHA-256 of the
	// exact file bytes. This checksum is separate from the core/id receipt ID.
	hash := sha256.New()
	version, sourcePath := "", ""
	var offset, size int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, "", err
		}
		chunk, err := read(offset, version)
		if err != nil {
			return 0, "", err
		}
		if chunk.Encoding != "base64" || chunk.Version == "" || chunk.Path == "" || chunk.Offset != offset || chunk.Size < 0 || chunk.Size > azureblob.MaxVerifiedStreamBytes {
			return 0, "", errors.New("workspace cannot provide a complete binary export (maximum 2 GiB); update its runtime if binary reads are unsupported")
		}
		if version == "" {
			version, sourcePath, size = chunk.Version, chunk.Path, chunk.Size
		}
		if version != chunk.Version || sourcePath != chunk.Path || size != chunk.Size {
			return 0, "", errors.New("file changed during export; no partial file was delivered")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(chunk.Content)
		if err != nil || len(data) > 256<<10 || offset+int64(len(data)) > size || (len(data) == 0 && !chunk.EOF) || chunk.EOF != (offset+int64(len(data)) == size) {
			return 0, "", errors.New("workspace returned an incomplete or invalid byte range")
		}
		if _, err = io.MultiWriter(dest, hash).Write(data); err != nil {
			return 0, "", err
		}
		offset += int64(len(data))
		if chunk.EOF {
			return size, hex.EncodeToString(hash.Sum(nil)), nil
		}
	}
}
