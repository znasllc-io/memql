package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/campaigns"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/server/libraryfile"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// campaigns_material.go -- the Library writer an email composition run files
// its material with (component/campaigns.MaterialFiler, compose_run.go).
//
// A composition runs whole in Claude Code on the owner's machine, and the
// session reads its material from its workspace, which the cockpit fills from
// the handover's inputs: Library ARTIFACT ids. So every file of the material
// becomes a Library file of the owner's, under the name the composer is told,
// in a folder of its own, and is archived when the run ends. The write is the
// one the server's other writers make (the Materializer's output, the session
// recorder's content, the vision stager's images): the bytes under the
// owner's library/<owner>/<file>/ prefix in the node's container, then
// createLibraryFile through component/server/libraryfile, whose internal-origin
// stamp is the one @serverOnly needs and dies with that call.
//
// THE ARTIFACT ID IS WAITED FOR, not derived: indexFileOnCreate promotes the
// file into the index off the node.created event, and deriving the id would
// copy a DSL expression into Go (app_vision.go's reasoning). A run is not a
// turn somebody watches, so the wait is longer than the vision stager's.
//
// THE FOLDER'S ID IS THE OWNER'S AND THE NAME'S, so two runs of one person
// file into one folder however they race, and a folder the person archived or
// emptied away is made again rather than filled behind their back.

const (
	// materialSource is the Library source of a material file: derived from
	// a file the person attached (a copy of it, or a member of a ZIP).
	materialSource = "derived"

	materialSummary = "Material for an email MemQL is writing. It is archived when MemQL finishes."

	campaignMaterialPromotionWait = 30 * time.Second
	campaignMaterialPromotionPoll = 200 * time.Millisecond
)

// campaignMaterialFiler implements campaigns.MaterialFiler over the engine and
// the node's blob storage.
type campaignMaterialFiler struct {
	engine   server.MemQLExecutor
	uploader server.FileUploader
	bucket   string
	// wait and poll bound the wait for a file's index row; fields so a test
	// about the timeout does not take thirty seconds.
	wait, poll time.Duration
}

var _ campaigns.MaterialFiler = (*campaignMaterialFiler)(nil)

func newCampaignMaterialFiler(engine server.MemQLExecutor, uploader server.FileUploader, bucket string) *campaignMaterialFiler {
	return &campaignMaterialFiler{engine: engine, uploader: uploader, bucket: strings.TrimSpace(bucket),
		wait: campaignMaterialPromotionWait, poll: campaignMaterialPromotionPoll}
}

// File stores one material file as a Library file of owner, in the owner's
// folder named folder, and answers the file and its artifact.
func (f *campaignMaterialFiler) File(ctx context.Context, owner, folder, name, mime string, data []byte) (campaigns.FiledMaterial, error) {
	owner = strings.TrimSpace(owner)
	switch {
	case f == nil || f.engine == nil:
		return campaigns.FiledMaterial{}, errors.New("this node has no engine to file Library files through")
	case f.uploader == nil || f.bucket == "":
		return campaigns.FiledMaterial{}, errors.New("object storage is not configured on this node, so the file has nowhere to go")
	case owner == "":
		return campaigns.FiledMaterial{}, errors.New("a material file has to belong to somebody")
	}
	// The OWNER's authority, borrowed, for every row here: the folder, the
	// file, the index row promotion writes and the archive at the end.
	actor := auth.ContextWithUserActor(ctx, owner)
	folderID, err := f.folder(actor, owner, folder)
	if err != nil {
		return campaigns.FiledMaterial{}, err
	}

	fileID := "email-material-" + id.NewShortId()
	sum := sha256.Sum256(data)
	object := fmt.Sprintf("library/%s/%s/%s", memql.BareShortId(owner), fileID, materialObjectName(name))
	blobURL, err := f.uploader.Upload(ctx, f.bucket, object, data, mime)
	if err != nil {
		return campaigns.FiledMaterial{}, fmt.Errorf("object storage refused the file: %w", err)
	}
	if strings.TrimSpace(blobURL) == "" {
		blobURL = object
	}
	args := map[string]any{
		"fileId": fileID, "name": name, "mimeType": mime, "size": len(data), "sha256": hex.EncodeToString(sum[:]),
		"blobUrl": blobURL, "source": materialSource, "format": server.LibraryFormatForMIME(mime),
		"summary": materialSummary, "folderId": folderID,
	}
	if run, ok := common.RunFromContext(ctx); ok && run.RunId != "" {
		args["producedByRunId"], args["producedByStepKey"] = run.RunId, run.StepKey
	}
	if err := libraryfile.NewStore(f.engine).Create(actor, args); err != nil {
		return campaigns.FiledMaterial{}, err
	}
	filed := campaigns.FiledMaterial{FileID: fileID}
	// Ready, so the Files app does not show it waiting for an analysis that
	// is never started for it. A file left `stored` still serves its bytes.
	if err := f.exec(actor, "setLibraryFileStatus", map[string]any{"fileId": fileID, "status": "ready", "summary": materialSummary}); err != nil {
		return filed, err
	}
	artifactID, err := f.waitForArtifact(actor, fileID)
	if err != nil {
		return filed, err
	}
	filed.ArtifactID = artifactID
	return filed, nil
}

// Archive archives a filed material file: its index row when it has one, and
// the file. Both writes are made here rather than leaving the file to
// archiveFileOnArtifactArchive, the vision stager's reasoning: a cleanup that
// depends on an automation firing has nothing left to check that it did.
func (f *campaignMaterialFiler) Archive(ctx context.Context, owner string, m campaigns.FiledMaterial) error {
	if f == nil || f.engine == nil || strings.TrimSpace(owner) == "" {
		return errors.New("nothing to archive the file through")
	}
	actor := auth.ContextWithUserActor(ctx, owner)
	var errs []error
	if artifactID := strings.TrimSpace(m.ArtifactID); artifactID != "" {
		errs = append(errs, f.exec(actor, "archiveArtifact", map[string]any{"artifactId": artifactID}))
	}
	if fileID := strings.TrimSpace(m.FileID); fileID != "" {
		errs = append(errs, f.exec(actor, "archiveLibraryFile", map[string]any{"fileId": fileID}))
	}
	return errors.Join(errs...)
}

// folder is the owner's folder named name, made at the root when it is
// missing or archived.
func (f *campaignMaterialFiler) folder(ctx context.Context, owner, name string) (string, error) {
	sum := sha256.Sum256([]byte(memql.BareShortId(owner) + "\x00" + name))
	folderID := "material-" + hex.EncodeToString(sum[:16])
	query, err := langparser.RenderCall("libraryFolderById", map[string]any{"folderId": folderID})
	if err != nil {
		return "", err
	}
	res, err := f.engine.Execute(memql.ContextWithFreshRead(ctx), "query "+query)
	if err != nil {
		return "", fmt.Errorf("reading the %s folder: %w", name, err)
	}
	for _, row := range memql.MaterializeRows(res) {
		if id, _ := row["id"].(string); memql.BareShortId(id) == folderID && row["archived"] != true {
			return folderID, nil
		}
	}
	if err := f.exec(ctx, "createLibraryFolder", map[string]any{"folderId": folderID, "name": name}); err != nil {
		return "", fmt.Errorf("making the %s folder: %w", name, err)
	}
	return folderID, nil
}

// waitForArtifact polls for the index row indexFileOnCreate writes for a file,
// and answers its id.
func (f *campaignMaterialFiler) waitForArtifact(ctx context.Context, fileID string) (string, error) {
	query, err := langparser.RenderCall("libraryArtifactBySourceConceptRef", map[string]any{"sourceConceptRef": "v1:library:file:" + fileID})
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(f.wait)
	for {
		res, err := f.engine.Execute(memql.ContextWithFreshRead(ctx), "query "+query)
		if err != nil {
			return "", fmt.Errorf("reading its Library entry: %w", err)
		}
		for _, row := range memql.MaterializeRows(res) {
			if id, _ := row["id"].(string); strings.TrimSpace(id) != "" {
				return memql.BareShortId(strings.TrimSpace(id)), nil
			}
		}
		if !time.Now().Add(f.poll).Before(deadline) {
			return "", fmt.Errorf("its Library entry did not appear within %s", f.wait)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(f.poll):
		}
	}
}

func (f *campaignMaterialFiler) exec(ctx context.Context, mutation string, args map[string]any) error {
	call, err := langparser.RenderCall(mutation, args)
	if err != nil {
		return err
	}
	_, err = f.engine.Execute(ctx, "mutation "+call)
	return err
}

// materialObjectName is the last segment of a material file's object path: its
// name with nothing that could leave the file's own prefix.
func materialObjectName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\':
			return '-'
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	if cleaned = strings.Trim(cleaned, ". "); cleaned == "" {
		return "material"
	}
	return cleaned
}

// wireCampaignMaterial installs the material filer on the campaigns worker, on
// the node where a composition's run executes, with the SAME blob client and
// container the Library's other writers here take. Without it a composition
// that has files fails on this node, saying the node has no Library storage
// wired, rather than running without them.
func (a *App) wireCampaignMaterial(uploader server.FileUploader, container string) {
	if a.engine == nil {
		return
	}
	worker, ok := a.engine.IntegrationByName("campaigns").(*campaigns.Worker)
	if !ok || worker == nil {
		return
	}
	if uploader == nil || strings.TrimSpace(container) == "" {
		a.Logger.Warn("campaigns: no blob storage on this replica, so an email composition with files fails here rather than running without them",
			"component", "campaigns")
		return
	}
	worker.SetMaterialFiler(newCampaignMaterialFiler(&AttachmentEngineAdapter{Engine: a.engine}, uploader, container))
}
