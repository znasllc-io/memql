package compose

import (
	"archive/zip"
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"

	pure "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/core/common"
)

func referencePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func referenceZIP(t *testing.T, files map[string][]byte, symlink string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, data := range files {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if name == symlink {
			header.SetMode(os.ModeSymlink | 0600)
		}
		f, err := w.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestReferenceZIPUsesActualImageAndTextContents(t *testing.T) {
	image := referencePNG(t)
	data := referenceZIP(t, map[string][]byte{"brand/reference.png": image, "brief.md": []byte("# Autumn launch"), "__MACOSX/._brief.md": []byte("metadata")}, "")
	files, err := readReference("resources.zip", "application/zip", data, &referenceBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files=%d", len(files))
	}
	var hasImage, hasText bool
	for _, file := range files {
		hasImage = hasImage || (bytes.Equal(file.Image, image) && file.MimeType == "image/png" && file.SHA256 == (pure.Result{Bytes: image}).SHA256())
		hasText = hasText || file.Text == "# Autumn launch"
	}
	if !hasImage || !hasText {
		t.Fatal("reference content was replaced with metadata")
	}
}
func TestReferenceArchivesRefuseUnsafeAndOversizedInputs(t *testing.T) {
	for _, name := range []string{"../outside.txt", "/absolute.txt", "folder/../../outside.txt", "C:/escape.txt", "folder\\escape.txt", "./relative.txt"} {
		t.Run(name, func(t *testing.T) {
			_, err := readReference("x.zip", "", referenceZIP(t, map[string][]byte{name: []byte("x")}, ""), &referenceBudget{})
			if err == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
	for _, c := range []struct {
		name    string
		files   map[string][]byte
		symlink string
	}{
		{"symlink", map[string][]byte{"link.txt": []byte("/secret")}, "link.txt"},
		{"nested", map[string][]byte{"nested.zip": referenceZIP(t, map[string][]byte{"note.txt": []byte("x")}, "")}, ""},
		{"executable", map[string][]byte{"install.sh": []byte("echo must-not-run")}, ""},
		{"expanded", map[string][]byte{"too-large.txt": bytes.Repeat([]byte("x"), maxReferenceBytes+1)}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := readReference("x.zip", "", referenceZIP(t, c.files, c.symlink), &referenceBudget{})
			if err == nil {
				t.Fatal("invalid bundle accepted")
			}
		})
	}
	budget := &referenceBudget{}
	for n := 0; n < 8; n++ {
		if _, err := readReference("x.png", "", referencePNG(t), budget); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := readReference("ninth.png", "", referencePNG(t), budget); err == nil {
		t.Fatal("aggregate image limit ignored")
	}
}

type referenceReader struct {
	data  []byte
	calls int
}

func (r *referenceReader) DownloadURLWithLimit(_ context.Context, _ string, limit int64) ([]byte, error) {
	r.calls++
	if limit != maxReferenceBytes {
		panic("unbounded reference read")
	}
	return r.data, nil
}
func TestCaptureRequiresExplicitOptInAndReadableFile(t *testing.T) {
	reader := &referenceReader{data: referencePNG(t)}
	i := &Integration{}
	i.SetSourceDownloader(reader)
	source := []Resolved{{Ref: SourceRef{Kind: KindLibraryFile, Ref: "file"}, Rows: []map[string]any{{"name": "reference.png", "blobUrl": "stored-object"}}}}
	if err := i.captureSourceContents(context.Background(), source); err != nil || reader.calls != 0 {
		t.Fatal("merely selecting metadata downloaded file content")
	}
	source[0].Ref.Content = true
	if err := i.captureSourceContents(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if reader.calls != 1 || !bytes.Equal(source[0].Files[0].Image, reader.data) {
		t.Fatal("image not captured")
	}
	source[0].Rows = nil
	if err := i.captureSourceContents(context.Background(), source); err == nil || reader.calls != 1 {
		t.Fatal("read denied file reached object storage")
	}
}
func TestRecipeRetainsReferenceProcessingChoice(t *testing.T) {
	refs, err := selectorsToSources([]any{map[string]any{"kind": "library_file", "selector": "file", "label": "Brand", "content": true}})
	if err != nil || !refs[0].Content {
		t.Fatalf("content choice lost: %v %v", refs, err)
	}
	_, err = parseSourceRefs([]any{map[string]any{"kind": "query", "ref": "anything()", "content": true}})
	if err == nil || !strings.Contains(err.Error(), "library_file") {
		t.Fatal("query was allowed to download arbitrary row URLs")
	}
}

func TestEmailReferencesSurviveDispatchToAnotherReplica(t *testing.T) {
	i, e, u := materializeFixture(t)
	e.account = map[string]any{"id": "client", "status": "active"}
	png := referencePNG(t)
	reader := &referenceReader{data: png}
	i.SetSourceDownloader(reader)
	e.files["reference"] = map[string]any{"name": "example.png", "blobUrl": "stored-object", "sha256": (pure.Result{Bytes: png}).SHA256()}
	opener := &directMaterializeGoal{}
	i.SetGoalOpener(opener)
	args := materializeArgs{Name: "Client welcome", Statement: "Use our supplied example", OutputKind: "email_template", Format: pure.FormatJSON, AccountIds: []string{"client"}, Sources: []SourceRef{{Kind: KindLibraryFile, Ref: "reference", Label: "Visual example", Content: true}}}
	result, err := i.materialize(materializeContext(), "u-alice", "", args)
	if err != nil {
		t.Fatal(err)
	}
	reader.data = []byte("changed source after dispatch")
	// A fresh integration has no reference reader or in-memory input state.
	worker := New(e, nil)
	worker.SetUploader(u, "files")
	worker.SetComposer(materializeComposerFunc(func(_ context.Context, req ComposeRequest) (ComposeReply, error) {
		if req.OutputKind != "email_template" || len(req.Sources) != 1 || len(req.Sources[0].Files) != 1 || !bytes.Equal(req.Sources[0].Files[0].Image, png) {
			t.Fatal("replica did not receive the original reference snapshot")
		}
		return ComposeReply{Draft: pure.Draft{Body: `{"subject":"Welcome","textBody":"Hello","htmlBody":"<table><tr><td>Hello</td></tr></table>"}`}}, nil
	}))
	ctx := common.ContextWithRun(materializeContext(), common.RunContext{RunId: "v1:work:run:direct", GoalId: "v1:work:goal:direct", StepKey: "compose", OwnerUserId: "u-alice"})
	if _, err := worker.handleExecute(ctx, opener.goal.Input, 0); err != nil {
		t.Fatal(err)
	}
	row := e.compositions[stringOf(result["compositionId"])]
	file := e.files[stringOf(row["outputFileId"])]
	if row["status"] != "ready" || !strings.HasSuffix(stringOf(file["name"]), ".email.json") || stringOf(file["mimeType"]) != "application/json" {
		t.Fatalf("wrong email output receipt: %+v", file)
	}
	if _, err := pure.RenderEmailTemplate(string(u.bytes)); err != nil {
		t.Fatal(err)
	}
	if reader.calls != 1 {
		t.Fatal("references were read again on another replica")
	}
}

func TestEmailMaterializationRequiresOneAccessibleOrganization(t *testing.T) {
	for _, accounts := range [][]string{nil, {}, {"one", "two"}, {"private"}} {
		i, e, _ := materializeFixture(t)
		_, err := i.materialize(materializeContext(), "u-alice", "", materializeArgs{Name: "Email", Format: pure.FormatJSON, OutputKind: "email_template", AccountIds: accounts})
		if err == nil || len(e.compositions) != 0 || len(e.inputs) != 0 {
			t.Fatal("invalid organization started work")
		}
	}
}
