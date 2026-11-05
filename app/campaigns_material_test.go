package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/campaigns"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// campaigns_material_test.go -- the Library writer an email composition run
// files its material with, measured at the writer, and its wiring on the node
// where a composition's run executes.

// materialCall is one engine call the filer made.
type materialCall struct {
	query, actor string
	internal     bool
}

// materialEngine is the engine as the filer sees it: the owner's folder, the
// index row promotion writes after `promoteAfter` reads, and every call kept.
type materialEngine struct {
	mu           sync.Mutex
	calls        []materialCall
	folders      map[string]map[string]any
	promoteAfter int
	reads        map[string]int
}

func (e *materialEngine) Execute(ctx context.Context, q string) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ac, _ := auth.AccessFromContext(ctx)
	call := materialCall{query: q, internal: auth.OriginFromContext(ctx).IsInternal()}
	if ac != nil {
		call.actor = ac.UserId
	}
	e.calls = append(e.calls, call)
	switch {
	case strings.HasPrefix(q, "query libraryFolderById"):
		if row, ok := e.folders[materialArg(q, "folderId")]; ok {
			return memqlengine.NewResultWithOutput([]any{row}), nil
		}
	case strings.HasPrefix(q, "mutation createLibraryFolder"):
		folderID := materialArg(q, "folderId")
		e.folders[folderID] = map[string]any{"id": "v1:library:folder:" + folderID, "name": materialArg(q, "name"), "archived": false}
	case strings.HasPrefix(q, "query libraryArtifactBySourceConceptRef"):
		ref := materialArg(q, "sourceConceptRef")
		if e.reads[ref]++; e.promoteAfter >= 0 && e.reads[ref] > e.promoteAfter {
			sum := sha256.Sum256([]byte(ref))
			return memqlengine.NewResultWithOutput([]any{map[string]any{"id": "v1:library:artifact:artifact-" + hex.EncodeToString(sum[:])}}), nil
		}
	}
	return memqlengine.NewResultWithOutput(nil), nil
}

func (e *materialEngine) callsTo(prefix string) []materialCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []materialCall
	for _, c := range e.calls {
		if strings.HasPrefix(c.query, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// materialArg pulls a quoted string argument out of a rendered call.
func materialArg(q, name string) string {
	i := strings.Index(q, name+": \"")
	if i < 0 {
		return ""
	}
	rest := q[i+len(name)+3:]
	return rest[:strings.Index(rest, "\"")]
}

type materialUploader struct {
	mu      sync.Mutex
	objects map[string][]byte
	buckets []string
}

func (u *materialUploader) Upload(_ context.Context, bucket, object string, data []byte, _ string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.objects[object] = append([]byte(nil), data...)
	u.buckets = append(u.buckets, bucket)
	return "https://acct.blob.core.windows.net/" + bucket + "/" + object, nil
}

const materialOwner = "v1:identity:user:u-composer"

func TestCampaignMaterialFilerFilesAndArchives(t *testing.T) {
	engine := &materialEngine{folders: map[string]map[string]any{}, promoteAfter: 2, reads: map[string]int{}}
	uploader := &materialUploader{objects: map[string][]byte{}}
	filer := newCampaignMaterialFiler(engine, uploader, "memql-library")
	filer.poll = time.Millisecond
	ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "v1:work:run:r-1", StepKey: "composeEmail", OwnerUserId: materialOwner})

	data := []byte("Spring is here.")
	first, err := filer.File(ctx, materialOwner, "Email material", "STAGE B - 1__copy.txt", "text/plain", data)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !strings.HasPrefix(first.FileID, "email-material-") || !strings.HasPrefix(first.ArtifactID, "artifact-") {
		t.Fatalf("filed %+v", first)
	}

	// The folder: made once, at the root, by the owner.
	folders := engine.callsTo("mutation createLibraryFolder")
	if len(folders) != 1 || materialArg(folders[0].query, "name") != "Email material" || folders[0].actor != materialOwner {
		t.Fatalf("folder writes %+v", folders)
	}
	folderID := materialArg(folders[0].query, "folderId")

	// The file: the bytes under the owner's own prefix, and a Library row
	// written by the owner through the server's stamp, in that folder, under
	// the workspace name, with the run that filed it.
	creates := engine.callsTo("mutation createLibraryFile")
	if len(creates) != 1 || !creates[0].internal || creates[0].actor != materialOwner {
		t.Fatalf("createLibraryFile calls %+v", creates)
	}
	q := creates[0].query
	for field, want := range map[string]string{
		"fileId": first.FileID, "name": "STAGE B - 1__copy.txt", "mimeType": "text/plain", "source": "derived",
		"folderId": folderID, "producedByRunId": "v1:work:run:r-1", "producedByStepKey": "composeEmail",
	} {
		if got := materialArg(q, field); got != want {
			t.Errorf("createLibraryFile %s = %q, want %q", field, got, want)
		}
	}
	object := "library/u-composer/" + first.FileID + "/STAGE B - 1__copy.txt"
	if string(uploader.objects[object]) != string(data) || uploader.buckets[0] != "memql-library" || !strings.Contains(materialArg(q, "blobUrl"), object) {
		t.Errorf("stored objects %v, blobUrl %q", len(uploader.objects), materialArg(q, "blobUrl"))
	}
	if ready := engine.callsTo("mutation setLibraryFileStatus"); len(ready) != 1 || materialArg(ready[0].query, "status") != "ready" {
		t.Errorf("status writes %+v", ready)
	}

	// A second file goes in the same folder, which is not made again.
	second, err := filer.File(ctx, materialOwner, "Email material", "hero.png", "image/png", []byte("png"))
	if err != nil || second.FileID == first.FileID {
		t.Fatalf("second File: %+v %v", second, err)
	}
	if n := len(engine.callsTo("mutation createLibraryFolder")); n != 1 {
		t.Errorf("the folder was made %d times", n)
	}

	// Archive: the index row and the file, both, by the owner.
	if err := filer.Archive(context.Background(), materialOwner, first); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	artifacts, files := engine.callsTo("mutation archiveArtifact"), engine.callsTo("mutation archiveLibraryFile")
	if len(artifacts) != 1 || materialArg(artifacts[0].query, "artifactId") != first.ArtifactID || artifacts[0].actor != materialOwner {
		t.Errorf("archiveArtifact %+v", artifacts)
	}
	if len(files) != 1 || materialArg(files[0].query, "fileId") != first.FileID || files[0].actor != materialOwner {
		t.Errorf("archiveLibraryFile %+v", files)
	}
}

func TestCampaignMaterialFilerAnswersTheFileWhenItIsNeverListed(t *testing.T) {
	engine := &materialEngine{folders: map[string]map[string]any{}, promoteAfter: -1, reads: map[string]int{}}
	filer := newCampaignMaterialFiler(engine, &materialUploader{objects: map[string][]byte{}}, "memql-library")
	filer.wait, filer.poll = 5*time.Millisecond, time.Millisecond
	got, err := filer.File(context.Background(), materialOwner, "Email material", "copy.txt", "text/plain", []byte("Hi."))
	if err == nil || got.FileID == "" || got.ArtifactID != "" {
		t.Fatalf("File = %+v, %v; want the file and the wait's error", got, err)
	}
	// What was filed can still be archived.
	if err := filer.Archive(context.Background(), materialOwner, got); err != nil {
		t.Fatal(err)
	}
	if files := engine.callsTo("mutation archiveLibraryFile"); len(files) != 1 || len(engine.callsTo("mutation archiveArtifact")) != 0 {
		t.Errorf("archive writes: %d files", len(files))
	}

	// An archived folder is made again rather than filled behind the person.
	engine = &materialEngine{folders: map[string]map[string]any{}, promoteAfter: 0, reads: map[string]int{}}
	filer = newCampaignMaterialFiler(engine, &materialUploader{objects: map[string][]byte{}}, "memql-library")
	if _, err := filer.File(context.Background(), materialOwner, "Email material", "a.txt", "text/plain", []byte("A")); err != nil {
		t.Fatal(err)
	}
	for id, row := range engine.folders {
		row["archived"] = true
		engine.folders[id] = row
	}
	if _, err := filer.File(context.Background(), materialOwner, "Email material", "b.txt", "text/plain", []byte("B")); err != nil {
		t.Fatal(err)
	}
	if n := len(engine.callsTo("mutation createLibraryFolder")); n != 2 {
		t.Errorf("an archived folder was filled: %d folder writes", n)
	}

	// No storage: refused before anything is written.
	engine = &materialEngine{folders: map[string]map[string]any{}, reads: map[string]int{}}
	filer = newCampaignMaterialFiler(engine, nil, "")
	if _, err := filer.File(context.Background(), materialOwner, "Email material", "a.txt", "text/plain", []byte("A")); err == nil || len(engine.calls) != 0 {
		t.Errorf("a node with no storage filed: err %v, %d calls", err, len(engine.calls))
	}
}

func TestCampaignMaterialObjectNameStaysInItsPrefix(t *testing.T) {
	for name, want := range map[string]string{
		"hero.png": "hero.png", "a/../../b.txt": "a-..-..-b.txt", "..": "material", "x\ny.txt": "xy.txt", `c:\w.txt`: "c:-w.txt",
	} {
		if got := materialObjectName(name); got != want {
			t.Errorf("materialObjectName(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestCampaignCompositionIsWiredWhereItsRunExecutes: a composition's run
// executes on the agent node, so that is where the campaigns worker gets its
// material filer, beside the Materializer's wiring and with the same blob
// client and container; and the worker's engine adapter is the tool loop the
// run calls its composer through (component/campaigns resolves it off the
// engine, and an adapter without it refuses every composition).
func TestCampaignCompositionIsWiredWhereItsRunExecutes(t *testing.T) {
	src := readAppFile(t, "transport_agent.go")
	if !strings.Contains(src, "a.wireCampaignMaterial(uploader, blobContainer)") {
		t.Fatal("transport_agent.go does not call wireCampaignMaterial(uploader, blobContainer): every composition with files would fail on the node that runs it")
	}
	compose, wire := strings.Index(src, "a.wireComposeIntegration(uploader, blobContainer)"), strings.Index(src, "a.wireCampaignMaterial(uploader, blobContainer)")
	if compose < 0 || wire < compose {
		t.Error("wireCampaignMaterial should sit beside the Materializer's wiring, after it, with the same blob client and container")
	}
	var adapter any = &CampaignsEngineAdapter{}
	if _, ok := adapter.(interface {
		InvokeAIChatWithFilteredTools(context.Context, string, map[string]any, []string) (string, error)
	}); !ok {
		t.Error("CampaignsEngineAdapter has no tool loop, so the campaigns worker cannot call its composer")
	}
	var _ campaigns.MaterialFiler = newCampaignMaterialFiler(nil, nil, "")
}
