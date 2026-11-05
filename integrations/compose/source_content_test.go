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
)

func referencePNG(t *testing.T) []byte {
	t.Helper()
	return colouredPNG(t, color.RGBA{255, 0, 0, 255})
}

// colouredPNG is a 2 x 2 PNG whose first pixel is c: a different colour is a
// different picture, with different bytes.
func colouredPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, c)
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
	if limit != maxReferenceBytes && limit != maxReferenceText {
		panic("unbounded reference read")
	}
	return r.data, nil
}
func TestCaptureRequiresExplicitOptInAndReadableFile(t *testing.T) {
	reader := &referenceReader{data: referencePNG(t)}
	i := &Integration{}
	i.SetSourceDownloader(reader)
	source := []Resolved{{Ref: SourceRef{Kind: KindLibraryFile, Ref: "file"}, Rows: []map[string]any{{"name": "reference.png", "blobUrl": "library/u-alice/file/reference.png", "ownerUserId": "u-alice"}}}}
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

func TestRecipeKeepsAssetPermissionSeparateFromReferencePermission(t *testing.T) {
	selectors := []any{map[string]any{"kind": "library_file", "selector": "file", "content": true, "includeImages": true}}
	refs, err := selectorsToSources(selectors)
	if err != nil || len(refs) != 1 || !refs[0].IncludeImages || !refs[0].Content {
		t.Fatal(refs, err)
	}
	if _, err := parseSourceRefs([]any{map[string]any{"kind": "library_file", "ref": "file", "includeImages": true}}); err == nil {
		t.Fatal("asset without content permission accepted")
	}
}

func TestTemplateCapturesActualContentsAndRefusesMissingOrChangedBytes(t *testing.T) {
	e := newMaterializeEngine(t)
	i := New(e, nil)
	data := []byte("# Template\n{{content}}")
	e.files["template"] = map[string]any{"name": "template.md", "blobUrl": "library/u-alice/template/template.md", "ownerUserId": "u-alice", "sha256": (pure.Result{Bytes: data}).SHA256(), "summary": "This is only metadata"}
	ctx := materializeContext()
	if _, err := i.templateBody(ctx, "template"); err == nil {
		t.Fatal("template silently fell back to metadata")
	}
	i.SetSourceDownloader(&referenceReader{data: data})
	body, err := i.templateBody(ctx, "template")
	if err != nil || body != string(data) {
		t.Fatalf("not actual template: %q %v", body, err)
	}
	i.SetSourceDownloader(&referenceReader{data: []byte("changed")})
	if _, err := i.templateBody(ctx, "template"); err == nil {
		t.Fatal("changed template silently accepted")
	}
}

// A reference's bytes and a template's are read with the node's storage
// credential, so the row's key is held to its owner's own library/{owner}/
// prefix before storage is asked -- the row being the caller's says nothing
// about whose object its blobUrl names.
func TestReferenceAndTemplateReadsRefuseAKeyOutsideTheOwnersLibrary(t *testing.T) {
	foreign := "https://acct.blob.core.windows.net/memql-library/library/u-bob/f-1/brief.md"
	reader := &referenceReader{data: []byte("# Bob's brief")}
	i := &Integration{}
	i.SetSourceDownloader(reader)
	source := []Resolved{{Ref: SourceRef{Kind: KindLibraryFile, Ref: "file", Label: "Brief", Content: true},
		Rows: []map[string]any{{"name": "brief.md", "blobUrl": foreign, "ownerUserId": "v1:identity:user:u-alice"}}}}
	if err := i.captureSourceContents(context.Background(), source); err == nil {
		t.Fatal("a reference naming another owner's key was read")
	}
	if reader.calls != 0 {
		t.Fatalf("storage was asked %d time(s) before the refusal", reader.calls)
	}

	e := newMaterializeEngine(t)
	ti := New(e, nil)
	ti.SetSourceDownloader(reader)
	e.files["template"] = map[string]any{"name": "template.md", "blobUrl": foreign, "ownerUserId": "u-alice"}
	if _, err := ti.templateBody(materializeContext(), "template"); err == nil {
		t.Fatal("a template naming another owner's key was read")
	}
	if reader.calls != 0 {
		t.Fatalf("storage was asked %d time(s) before the refusal", reader.calls)
	}
}

func TestReferencesShareOneExpandedBudgetWithTheOriginalSentence(t *testing.T) {
	budget := &referenceBudget{}
	half := bytes.Repeat([]byte("x"), 10<<20)
	if _, err := readReference("a.zip", "", referenceZIP(t, map[string][]byte{"a.txt": half}, ""), &referenceBudget{bytes: 0}); err == nil {
		t.Fatal("10 MiB of text exceeds the text cap and must fail on its own")
	}
	img := bytes.Repeat([]byte{0}, 9<<20)
	budget.bytes = len(img) // a first reference already used 9 MiB
	_, err := readReference("b.zip", "", referenceZIP(t, map[string][]byte{"b.csv": bytes.Repeat([]byte("y"), 8<<20)}, ""), budget)
	if err == nil || err.Error() != "reference contents exceed 16 MiB expanded" {
		t.Fatalf("got %v", err)
	}
	budget.bytes = maxReferenceBytes
	_, err = readReference("c.zip", "", referenceZIP(t, map[string][]byte{"c.txt": []byte("z")}, ""), budget)
	if err == nil || err.Error() != "reference contents exceed 16 MiB expanded" {
		t.Fatalf("empty remainder: got %v", err)
	}
}
