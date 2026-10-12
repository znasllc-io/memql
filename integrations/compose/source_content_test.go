package compose

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"

	pure "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	"github.com/znasllc-io/memql/core/common"
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

// A picture selected for inclusion is captured once, when the request is
// accepted, and the replica that writes the email uses that capture: as the
// Library file it is (artifact:<id>) when the caller can read its artifact, and
// as an embedded copy of the captured bytes when not.
func TestEmailReferencesSurviveDispatchToAnotherReplica(t *testing.T) {
	png := referencePNG(t)
	for name, tc := range map[string]struct {
		artifact map[string]any
		want     string
	}{
		"a picture whose artifact the caller can read": {map[string]any{"id": "v1:library:artifact:a1"}, "artifact:a1"},
		"a picture with no artifact to read":           {nil, "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)},
	} {
		t.Run(name, func(t *testing.T) {
			i, e, u := materializeFixture(t)
			e.account = map[string]any{"id": "client", "status": "active"}
			reader := &referenceReader{data: png}
			i.SetSourceDownloader(reader)
			e.files["reference"] = map[string]any{"name": "example.png", "blobUrl": "library/u-alice/reference/example.png", "ownerUserId": "u-alice", "sha256": (pure.Result{Bytes: png}).SHA256()}
			if tc.artifact != nil {
				e.artifacts["reference"] = tc.artifact
			}
			opener := &directMaterializeGoal{}
			i.SetGoalOpener(opener)
			args := materializeArgs{Name: "Client welcome", Statement: "Use our supplied example", OutputKind: "email_template", Format: pure.FormatJSON, AccountIds: []string{"client"}, Sources: []SourceRef{{Kind: KindLibraryFile, Ref: "reference", Label: "Visual example", Content: true, IncludeImages: true}}}
			result, err := i.materialize(materializeContext(), "u-alice", "", args)
			if err != nil {
				t.Fatal(err)
			}
			reader.data = []byte("changed source after dispatch")
			// A fresh integration has no reference reader or in-memory input state.
			worker := New(e, nil)
			worker.SetUploader(u, "files")
			handle := "memql-asset:" + (pure.Result{Bytes: png}).SHA256()
			worker.SetComposer(materializeComposerFunc(func(_ context.Context, req ComposeRequest) (ComposeReply, error) {
				if req.OutputKind != "email_template" || req.EmailLayout == nil || req.EmailLayout.Layout != "builtin:article" || len(req.Sources) != 1 || !req.Sources[0].Ref.IncludeImages || len(req.Sources[0].Files) != 1 || !bytes.Equal(req.Sources[0].Files[0].Image, png) {
					t.Fatal("replica did not receive the original reference snapshot and the layout to fill")
				}
				alt := "Our shop window"
				return ComposeReply{EmailContent: &emaildoc.Content{Subject: "Welcome", Blocks: []emaildoc.ContentItem{{ID: "banner", Image: &handle, Alt: &alt}}}}, nil
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
			doc := filedEmail(t, u)
			if banner := emailBlock(t, doc, "banner"); banner.Src != tc.want || banner.Alt != "Our shop window" {
				t.Fatalf("banner src = %.80q (alt %q), want %.80q", banner.Src, banner.Alt, tc.want)
			}
			if reader.calls != 1 {
				t.Fatal("references were read again on another replica")
			}
		})
	}
}

// Rulings 12: what a handle the model may use becomes in the email. Only the
// pictures of references selected for inclusion are offered at all; a whole
// Library image file becomes the file itself when the caller can read its
// artifact, and anything else (a ZIP member, a file without a readable
// artifact, a row whose digest says nothing) becomes a copy of the captured
// bytes. The handle is the digest of those bytes, so a capture that changed
// offers a different handle.
func TestEmailImageSourcesOfferOnlyIncludedPictures(t *testing.T) {
	png := referencePNG(t)
	other := colouredPNG(t, color.RGBA{0, 0, 255, 255})
	undigested := colouredPNG(t, color.RGBA{0, 255, 0, 255})
	handle := func(b []byte) string { return "memql-asset:" + (pure.Result{Bytes: b}).SHA256() }
	embedded := func(b []byte) string { return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b) }
	digest := func(b []byte) string { return (pure.Result{Bytes: b}).SHA256() }

	e := newMaterializeEngine(t)
	e.artifacts["whole"] = map[string]any{"id": "v1:library:artifact:whole-art"}
	e.artifacts["archive"] = map[string]any{"id": "v1:library:artifact:zip-art"}
	e.artifacts["undigested"] = map[string]any{"id": "v1:library:artifact:nodigest-art"}
	i := New(e, nil)
	resolved := []Resolved{
		{Ref: SourceRef{Kind: KindLibraryFile, Ref: "whole", Content: true, IncludeImages: true},
			Rows:  []map[string]any{{"name": "logo.png", "sha256": digest(png)}},
			Files: []SourceContent{{Name: "logo.png", MimeType: "image/png", Image: png, SHA256: digest(png)}}},
		{Ref: SourceRef{Kind: KindLibraryFile, Ref: "archive", Content: true, IncludeImages: true},
			Rows:  []map[string]any{{"name": "brand.zip", "sha256": "0000"}},
			Files: []SourceContent{{Name: "brand/photo.png", MimeType: "image/png", Image: other, SHA256: digest(other)}, {Name: "brief.md", MimeType: "text/plain", Text: "Brief"}}},
		{Ref: SourceRef{Kind: KindLibraryFile, Ref: "undigested", Content: true, IncludeImages: true},
			Rows:  []map[string]any{{"name": "mark.png"}},
			Files: []SourceContent{{Name: "mark.png", MimeType: "image/png", Image: undigested, SHA256: "not the digest of anything"}}},
		{Ref: SourceRef{Kind: KindLibraryFile, Ref: "inspiration", Content: true},
			Rows:  []map[string]any{{"name": "mood.png", "sha256": "1111"}},
			Files: []SourceContent{{Name: "mood.png", MimeType: "image/png", Image: colouredPNG(t, color.RGBA{9, 9, 9, 255})}}},
	}
	sources, err := i.emailImageSources(materializeContext(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		handle(png):        "artifact:whole-art",
		handle(other):      embedded(other),
		handle(undigested): embedded(undigested),
	}
	if len(sources) != len(want) {
		t.Fatalf("offered %d pictures, want %d: %v", len(sources), len(want), keysOf(sources))
	}
	for h, src := range want {
		if sources[h] != src {
			t.Errorf("%s -> %.80q, want %.80q", h, sources[h], src)
		}
	}
	// No artifact is read for a picture that is not the whole file, nor for one
	// that is not included.
	for _, call := range e.calls {
		if call != "libraryArtifactForFile" {
			t.Fatalf("unexpected read %s", call)
		}
	}
	if n := len(e.calls); n != 1 {
		t.Fatalf("artifacts read %d times, want once (the whole file's): %v", n, e.calls)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
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

func TestEmailMaterializationRequiresOneAccessibleOrganization(t *testing.T) {
	for _, accounts := range [][]string{nil, {}, {"one", "two"}, {"private"}} {
		i, e, _ := materializeFixture(t)
		_, err := i.materialize(materializeContext(), "u-alice", "", materializeArgs{Name: "Email", Format: pure.FormatJSON, OutputKind: "email_template", AccountIds: accounts})
		if err == nil || len(e.compositions) != 0 || len(e.inputs) != 0 {
			t.Fatal("invalid organization started work")
		}
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
