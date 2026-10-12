package campaigns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// email_render_test.go -- the emailRender builtin: a preview of an email
// document that saves and sends nothing.
//
// What matters about a preview is who it may read for and what it may cost. It
// is run by someone who writes an email, so the pictures it embeds are the ones
// that person can read and no others; and it is run on a node that does other
// work, so a document cannot make it expensive. The first is proved here
// against an engine that models the owner gate of the two Library reads, and
// against the real one in email_render_db_test.go; the second by the limits of
// the handler (the document, the sample, the pictures, the issues, how many
// renders at once).

const (
	renderOrg = "acme"

	// The sentences a person reads, spelled out here and not taken from the
	// handler so that a change of wording is a change of a test.
	notAccessible   = "organization is not active or accessible"
	needsReadAccess = "You need read access to this organization's data to preview an email."
	previewBusy     = "Previews are busy right now. Try again in a moment."
	tooLargeToWrite = "This email is larger than 8 MiB once it is written out. Shorten it or use fewer pictures."
	sampleTooLarge  = "The sample values are too large. Use up to 100 fields of up to 1,000 characters each."
	fileTooLarge    = "This email file is larger than 2 MiB. Use fewer or smaller pictures."
	controlRefused  = "This email contains a character that can't be saved. Remove it and try again."
)

// previewEngine is the package's fake engine plus a record of the organization
// decisions asked of it and a way to say no.
type previewEngine struct {
	*fakeEngine
	mu      sync.Mutex
	asked   []layoutAsk
	capable func(account, verb, resource string) bool // nil allows everything
	// pictureOwner, when set, models the owner gate of libraryArtifactById and
	// libraryFileById: only that user is shown the rows. Every other caller is
	// answered as the real queries answer them, with nothing.
	pictureOwner string
}

func (e *previewEngine) Execute(ctx context.Context, q string) (any, error) {
	res, err := e.fakeEngine.Execute(ctx, q)
	if e.pictureOwner != "" && (strings.HasPrefix(q, "query libraryArtifactById") || strings.HasPrefix(q, "query libraryFileById")) {
		if ac, _ := auth.AccessFromContext(ctx); ac == nil || ac.UserId != e.pictureOwner {
			return rowsEnvelope(nil), nil
		}
	}
	return res, err
}

func (e *previewEngine) OrganizationCapable(_ context.Context, account, verb, resource string) bool {
	e.mu.Lock()
	e.asked = append(e.asked, layoutAsk{account, verb, resource})
	e.mu.Unlock()
	return e.capable == nil || e.capable(account, verb, resource)
}

// callsTo returns the queries and mutations the engine was asked, in order,
// that begin with prefix.
func (e *previewEngine) callsTo(prefix string) []recordedCall {
	e.fakeEngine.mu.Lock()
	defer e.fakeEngine.mu.Unlock()
	var out []recordedCall
	for _, c := range e.fakeEngine.calls {
		if strings.HasPrefix(c.query, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// recordingBlobs is object storage that records how it was read. Reading a
// picture must be a limited read, so the unlimited one is counted rather than
// refused: a test that fails on a count says what went wrong.
type recordingBlobs struct {
	mu        sync.Mutex
	objects   map[string][]byte
	err       error
	limits    []int64 // the limit of each limited read
	unbounded int     // reads that named no limit
	inflight  int
	peak      int
	// hold, when set, parks every limited read until it is closed; entered gets
	// one value for each read that has started.
	hold    chan struct{}
	entered chan struct{}
}

func (b *recordingBlobs) Download(_ context.Context, _ string, key string) ([]byte, error) {
	b.mu.Lock()
	b.unbounded++
	body, ok := b.objects[key]
	b.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no object %q", key)
	}
	return body, nil
}

func (b *recordingBlobs) DownloadWithLimit(ctx context.Context, _ string, key string, maxBytes int64) ([]byte, error) {
	b.mu.Lock()
	b.limits = append(b.limits, maxBytes)
	b.inflight++
	if b.inflight > b.peak {
		b.peak = b.inflight
	}
	hold, entered, failure := b.hold, b.entered, b.err
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.inflight--
		b.mu.Unlock()
	}()
	if entered != nil {
		entered <- struct{}{}
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if failure != nil {
		return nil, failure
	}
	b.mu.Lock()
	body, ok := b.objects[key]
	b.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no object %q", key)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("object exceeds download limit of %d bytes", maxBytes)
	}
	return body, nil
}

func (b *recordingBlobs) reads() (limited int, unbounded int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.limits), b.unbounded
}

func (b *recordingBlobs) peakInflight() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peak
}

func (b *recordingBlobs) currentInflight() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inflight
}

// previewFixture is a worker over a fake engine that knows one active
// organization, and storage that records how it is read.
func previewFixture(t *testing.T) (*Worker, *previewEngine, *recordingBlobs) {
	t.Helper()
	engine := &previewEngine{fakeEngine: &fakeEngine{
		accounts: map[string]map[string]any{
			renderOrg: {"id": "v1:accounts:account:" + renderOrg, "status": "active", "name": "Acme Co"},
		},
		libraryArtifacts: map[string]map[string]any{},
		libraryFiles:     map[string]map[string]any{},
	}}
	blobs := &recordingBlobs{objects: map[string][]byte{}}
	w := newTestWorker(t, engine, &recordingSender{})
	w.newBlobReader = func(context.Context) (blobReader, string, error) { return blobs, "memql", nil }
	return w, engine, blobs
}

// previewPNG is a small valid picture. Two with different sizes are two
// different pictures.
func previewPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// previewPicture is a small picture of the format, which is "png", "jpeg" or
// "gif".
func previewPicture(t *testing.T, format string) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	var err error
	switch format {
	case "png":
		err = png.Encode(&buf, img)
	case "jpeg":
		err = jpeg.Encode(&buf, img, nil)
	case "gif":
		err = gif.Encode(&buf, img, nil)
	default:
		t.Fatalf("no format %q", format)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// libraryPicture makes an artifact the Library shows its owner: the index row
// and the file row it names. It returns the storage key of the file's bytes.
func libraryPicture(engine *previewEngine, artifact, mime string, size int) string {
	file := "file-" + artifact
	key := "library/" + testOwner + "/" + file + "/" + artifact
	engine.libraryArtifacts[artifact] = map[string]any{
		"id": "v1:library:artifact:" + artifact, "kind": "file", "sourceConceptRef": "v1:library:file:" + file, "title": artifact,
	}
	engine.libraryFiles[file] = map[string]any{
		"id": "v1:library:file:" + file, "name": artifact, "mimeType": mime, "size": size, "blobUrl": key,
	}
	return key
}

// addPicture is libraryPicture with the bytes in storage.
func addPicture(engine *previewEngine, blobs *recordingBlobs, artifact, mime string, data []byte) {
	blobs.objects[libraryPicture(engine, artifact, mime, len(data))] = data
}

// setBlock changes the block with the id, wherever it sits in the document.
func setBlock(t *testing.T, doc map[string]any, id string, change func(block map[string]any)) {
	t.Helper()
	var walk func(blocks []any) bool
	walk = func(blocks []any) bool {
		for _, item := range blocks {
			block := item.(map[string]any)
			if block["id"] == id {
				change(block)
				return true
			}
			if inner, ok := block["blocks"].([]any); ok && walk(inner) {
				return true
			}
			if columns, ok := block["columns"].([]any); ok {
				for _, column := range columns {
					if inner, ok := column.(map[string]any)["blocks"].([]any); ok && walk(inner) {
						return true
					}
				}
			}
		}
		return false
	}
	if !walk(doc["blocks"].([]any)) {
		t.Fatalf("the document has no block %q", id)
	}
}

// customDocument is a custom-mode document with the HTML.
func customDocument(html string) map[string]any {
	return map[string]any{"subject": "A plain note", "preheader": "", "textBody": "Hello there.", "htmlBody": html}
}

// renderedPreview is what a client reads back.
type renderedPreview struct {
	Subject string           `json:"subject"`
	HTML    string           `json:"html"`
	Text    string           `json:"text"`
	Issues  []emaildoc.Issue `json:"issues"`
}

// has reports whether the preview carries an issue of the code about the block
// ("" for any block).
func (p renderedPreview) has(code, block string) bool {
	for _, issue := range p.Issues {
		if issue.Code == code && (block == "" || issue.Block == block) {
			return true
		}
	}
	return false
}

func (p renderedPreview) count(code string) int {
	n := 0
	for _, issue := range p.Issues {
		if issue.Code == code {
			n++
		}
	}
	return n
}

func decodePreview(t *testing.T, nodes []memorynodes.MemoryNode) renderedPreview {
	t.Helper()
	if len(nodes) != 1 || nodes[0].Concept != "integration:campaigns:campaignEmailRendered" {
		t.Fatalf("expected one campaignEmailRendered node, got %+v", nodes)
	}
	var out renderedPreview
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatalf("decoding the preview: %v", err)
	}
	return out
}

func renderArgs(doc map[string]any, sample map[string]any) map[string]any {
	args := map[string]any{"document": doc, "accountId": renderOrg}
	if sample != nil {
		args["sample"] = sample
	}
	return args
}

// preview renders and fails the test on an error.
func preview(t *testing.T, w *Worker, ctx context.Context, doc map[string]any, sample map[string]any) renderedPreview {
	t.Helper()
	nodes, err := w.handleRenderEmail(ctx, renderArgs(doc, sample), 0)
	if err != nil {
		t.Fatalf("emailRender: %v", err)
	}
	return decodePreview(t, nodes)
}

// neverWrites fails the test if the engine was asked to change anything: a
// preview saves and sends nothing.
func neverWrites(t *testing.T, engine *previewEngine) {
	t.Helper()
	for _, c := range engine.fakeEngine.calls {
		if !strings.HasPrefix(c.query, "query ") {
			t.Errorf("a preview asked the engine to change something: %s", c.query)
		}
	}
}

func TestEmailRenderReturnsPreviewHTMLTextAndIssues(t *testing.T) {
	w, engine, _ := previewFixture(t)
	doc := layoutDocument(t, "builtin:letter")
	setBlock(t, doc, "headline", func(b map[string]any) { b["text"] = "Thanks for joining {{accountName}}" })

	got := preview(t, w, importCtx(), doc, nil)

	if got.Subject != "A note from our founder" {
		t.Errorf("subject = %q", got.Subject)
	}
	// A preview marks every block so an editor can select by clicking.
	if !strings.Contains(got.HTML, `data-block="headline"`) {
		t.Errorf("the preview does not mark the headline block:\n%.600s", got.HTML)
	}
	if strings.TrimSpace(got.Text) == "" {
		t.Error("the plain-text part is empty")
	}
	// The organization's own name is the sample for {{accountName}}, in both parts.
	if !strings.Contains(got.Text, "Thanks for joining Acme Co") || strings.Contains(got.Text, "{{accountName}}") {
		t.Errorf("the text part was not sampled:\n%s", got.Text)
	}
	if !strings.Contains(got.HTML, "Thanks for joining Acme Co") {
		t.Errorf("the HTML part was not sampled:\n%.600s", got.HTML)
	}
	// The built-in letter has pictures to choose: it is a preview, so that is an
	// issue to show and not a failure.
	if !got.has(emaildoc.IssueImageMissing, "logo") {
		t.Errorf("no image_missing issue for the logo: %+v", got.Issues)
	}
	// The link tags are filled when the email is sent, per recipient, and stay as
	// written in a preview.
	if !strings.Contains(got.HTML, "{{unsubscribeUrl}}") {
		t.Error("the unsubscribe link tag was filled in a preview")
	}
	neverWrites(t, engine)
}

func TestEmailRenderReturnsAnEmptyListWhenThereAreNoIssues(t *testing.T) {
	w, _, _ := previewFixture(t)
	nodes, err := w.handleRenderEmail(importCtx(), renderArgs(customDocument(""), nil), 0)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(nodes[0].Payload, &raw); err != nil {
		t.Fatal(err)
	}
	// An empty list and not a missing key or null: a client reads `issues.length`.
	if string(raw["issues"]) != "[]" {
		t.Errorf("issues = %s, want []", raw["issues"])
	}
	for _, key := range []string{"subject", "html", "text"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("the result has no %q", key)
		}
	}
}

func TestEmailRenderEmbedsPicturesTheCallerCanRead(t *testing.T) {
	w, engine, _ := previewFixture(t)
	data := previewPNG(t, 4, 3)
	key := libraryPicture(engine, "art-logo", "image/png", len(data))
	w.newBlobReader = func(context.Context) (blobReader, string, error) {
		return stubBlobs{objects: map[string][]byte{key: data}}, "memql", nil
	}
	doc := layoutDocument(t, "builtin:letter")
	setBlock(t, doc, "logo", func(b map[string]any) { b["src"] = "artifact:art-logo" })

	got := preview(t, w, importCtx(), doc, nil)

	if !strings.Contains(got.HTML, "data:image/png;base64,") {
		t.Fatalf("the picture was not embedded:\n%.600s", got.HTML)
	}
	for _, issue := range got.Issues {
		if issue.Block == "logo" {
			t.Errorf("the logo has an issue although its picture was read: %+v", issue)
		}
	}
	// The other pictures are still to choose.
	if !got.has(emaildoc.IssueImageMissing, "signature-photo") {
		t.Errorf("the other pictures lost their issue: %+v", got.Issues)
	}
	neverWrites(t, engine)
}

func TestEmailRenderTurnsUnreadablePicturesIntoIssues(t *testing.T) {
	good := previewPNG(t, 4, 3)
	const key = "library/user-owner-1/file-pic/pic"
	for _, tc := range []struct {
		name  string
		setup func(engine *previewEngine, blobs *recordingBlobs)
		// reads is how many reads of storage the picture is allowed to cost.
		reads int
	}{
		{"an artifact nobody can see", func(*previewEngine, *recordingBlobs) {}, 0},
		{"a file that is not a picture", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "text/csv", []byte("a,b\n1,2\n"))
		}, 0},
		{"a vector picture", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`))
		}, 0},
		{"an archived artifact", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/png", good)
			e.libraryArtifacts["pic"]["archived"] = true
		}, 0},
		{"an archived file", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/png", good)
			e.libraryFiles["file-pic"]["archived"] = true
		}, 0},
		{"an artifact that is not a file", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/png", good)
			e.libraryArtifacts["pic"]["kind"] = "document"
		}, 0},
		{"a file whose recorded size is over a picture's", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/png", good)
			e.libraryFiles["file-pic"]["size"] = emaildoc.MaxImageBytes + 1
		}, 0},
		{"a file with no storage location", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/png", good)
			e.libraryFiles["file-pic"]["blobUrl"] = ""
		}, 0},
		// The record of a size is not the size: the read is what is limited.
		{"bytes over a picture's although the record says they are small", func(e *previewEngine, b *recordingBlobs) {
			libraryPicture(e, "pic", "image/png", 100)
			b.objects[key] = append(append([]byte{}, good...), make([]byte, emaildoc.MaxImageBytes)...)
		}, 1},
		{"bytes that are not a picture", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/png", []byte("this is not a picture"))
		}, 1},
		{"a picture of another kind than its type says", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/jpeg", good)
		}, 1},
		{"storage that cannot be read", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/png", good)
			b.err = errors.New("container unreachable")
		}, 1},
		{"a file that is not in storage", func(e *previewEngine, b *recordingBlobs) {
			libraryPicture(e, "pic", "image/png", len(good))
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, engine, blobs := previewFixture(t)
			tc.setup(engine, blobs)
			doc := layoutDocument(t, "builtin:letter")
			setBlock(t, doc, "logo", func(b map[string]any) { b["src"] = "artifact:pic" })

			// Not an error: the person is shown which picture to choose again.
			got := preview(t, w, importCtx(), doc, nil)

			if !got.has(emaildoc.IssueImageUnavailable, "logo") {
				t.Errorf("no image_unavailable issue for the logo: %+v", got.Issues)
			}
			if got.has(emaildoc.IssueImageMissing, "logo") {
				t.Errorf("an unreadable picture was reported as a missing one: %+v", got.Issues)
			}
			if strings.Contains(got.HTML, "data:image") {
				t.Error("a picture the caller cannot read was embedded")
			}
			if limited, unbounded := blobs.reads(); limited != tc.reads || unbounded != 0 {
				t.Errorf("storage was read %d times with a limit and %d without, want %d and 0", limited, unbounded, tc.reads)
			}
		})
	}

	t.Run("a custom email says which picture", func(t *testing.T) {
		w, _, _ := previewFixture(t)
		got := preview(t, w, importCtx(), customDocument(`<p><img src="artifact:pic" alt="Our shop"></p>`), nil)
		if !got.has(emaildoc.IssueImageUnavailable, "") || !strings.Contains(got.Issues[0].Message, `"Our shop"`) {
			t.Errorf("issues = %+v", got.Issues)
		}
	})
}

// artifactImages is what publishing reads pictures with too, so its contract is
// pinned apart from the preview that uses it: a picture is what an email can
// carry, or the reason it is not.
func TestArtifactImagesAnswerOnlyWithWhatAnEmailCanCarry(t *testing.T) {
	for _, format := range []string{"png", "jpeg", "gif"} {
		t.Run("a "+format+" is a picture", func(t *testing.T) {
			w, engine, blobs := previewFixture(t)
			data := previewPicture(t, format)
			addPicture(engine, blobs, "pic", "image/"+format, data)

			got, err := artifactImages{w}.ResolveImage(importCtx(), "pic")

			if err != nil || got.MIMEType != "image/"+format || !bytes.Equal(got.Data, data) {
				t.Fatalf("got %q with %d bytes, %v; want image/%s with %d bytes", got.MIMEType, len(got.Data), err, format, len(data))
			}
		})
	}

	good := previewPicture(t, "png")
	for _, tc := range []struct {
		name  string
		setup func(engine *previewEngine, blobs *recordingBlobs)
		want  string // a part of the reason
	}{
		{"nothing is visible under the id", func(*previewEngine, *recordingBlobs) {}, "no Library artifact"},
		{"an archived artifact", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/png", good)
			e.libraryArtifacts["pic"]["archived"] = true
		}, "archived"},
		{"an archived file", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/png", good)
			e.libraryFiles["file-pic"]["archived"] = true
		}, "archived"},
		{"a spreadsheet", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "text/csv", []byte("a,b\n"))
		}, "not a picture an email can carry"},
		{"a picture type in other letters", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "IMAGE/PNG", good)
		}, "not a picture an email can carry"},
		{"a recorded size over a picture's", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/png", good)
			e.libraryFiles["file-pic"]["size"] = emaildoc.MaxImageBytes + 1
		}, "over the"},
		{"bytes that are not a picture of the type", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/gif", good)
		}, "must contain valid PNG, JPEG, or GIF bytes"},
		{"bytes that are not a picture at all", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/png", []byte("hello"))
		}, "must contain valid PNG, JPEG, or GIF bytes"},
		{"no bytes", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/png", []byte{})
		}, "email image"},
		{"storage that cannot be read", func(e *previewEngine, b *recordingBlobs) {
			addPicture(e, b, "pic", "image/png", good)
			b.err = errors.New("container unreachable")
		}, "container unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, engine, blobs := previewFixture(t)
			tc.setup(engine, blobs)
			got, err := artifactImages{w}.ResolveImage(importCtx(), "pic")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one about %q", err, tc.want)
			}
			if got.Data != nil || got.MIMEType != "" {
				t.Errorf("a picture came back with the error: %+v", got)
			}
		})
	}
}

func TestEmailRenderRefusesAnInaccessibleOrganization(t *testing.T) {
	archived := map[string]map[string]any{renderOrg: {"id": "v1:accounts:account:" + renderOrg, "status": "archived", "name": "Acme Co"}}
	for _, tc := range []struct {
		name     string
		accounts map[string]map[string]any // nil: the fixture's
		account  string
	}{
		{"an organization nobody can see", nil, "elsewhere"},
		{"an archived organization", archived, renderOrg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, engine, blobs := previewFixture(t)
			if tc.accounts != nil {
				engine.accounts = tc.accounts
			}
			addPicture(engine, blobs, "pic", "image/png", previewPNG(t, 2, 2))
			// A document that could not be parsed is not parsed for someone who
			// may not use the organization: they get the refusal, and the work
			// that follows is not done.
			args := map[string]any{"document": map[string]any{"subject": ""}, "accountId": tc.account}
			_, err := w.handleRenderEmail(importCtx(), args, 0)
			if err == nil || err.Error() != notAccessible {
				t.Fatalf("error = %v, want %q", err, notAccessible)
			}
			if got := engine.callsTo("query library"); len(got) != 0 {
				t.Errorf("a picture was looked for in an organization the caller cannot use: %+v", got)
			}
			neverWrites(t, engine)
		})
	}

	t.Run("the canonical id of an organization names it too", func(t *testing.T) {
		w, _, _ := previewFixture(t)
		args := map[string]any{"document": customDocument(""), "accountId": "v1:accounts:account:" + renderOrg}
		if _, err := w.handleRenderEmail(importCtx(), args, 0); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("no organization named", func(t *testing.T) {
		w, engine, _ := previewFixture(t)
		for _, account := range []any{nil, "", "  "} {
			args := map[string]any{"document": customDocument(""), "accountId": account}
			if _, err := w.handleRenderEmail(importCtx(), args, 0); err == nil || err.Error() != "Choose the organization this email is for." {
				t.Errorf("accountId %v: error = %v", account, err)
			}
		}
		if len(engine.fakeEngine.calls) != 0 {
			t.Errorf("the engine was asked about no organization: %+v", engine.fakeEngine.calls)
		}
	})
}

func TestEmailRenderRequiresReadAccessToTheOrganization(t *testing.T) {
	t.Run("a member without read access to its data", func(t *testing.T) {
		w, engine, blobs := previewFixture(t)
		addPicture(engine, blobs, "pic", "image/png", previewPNG(t, 2, 2))
		engine.capable = func(string, string, string) bool { return false }
		doc := customDocument(`<p><img src="artifact:pic"></p>`)

		_, err := w.handleRenderEmail(importCtx(), renderArgs(doc, nil), 0)

		if err == nil || err.Error() != needsReadAccess {
			t.Fatalf("error = %v, want %q", err, needsReadAccess)
		}
		// Read, on the organization's data: what the builtin's description says
		// and nothing more, since a preview changes nothing.
		if want := []layoutAsk{{renderOrg, auth.VerbRead, auth.ResourceData}}; len(engine.asked) != 1 || engine.asked[0] != want[0] {
			t.Errorf("asked %+v, want %+v", engine.asked, want)
		}
		if got := engine.callsTo("query library"); len(got) != 0 {
			t.Errorf("a picture was looked for although the caller may not preview here: %+v", got)
		}
		if limited, unbounded := blobs.reads(); limited+unbounded != 0 {
			t.Error("storage was read although the caller may not preview here")
		}
	})

	t.Run("nobody signed in", func(t *testing.T) {
		w, engine, _ := previewFixture(t)
		_, err := w.handleRenderEmail(context.Background(), renderArgs(customDocument(""), nil), 0)
		if err == nil || err.Error() != "Sign in to preview an email." {
			t.Fatalf("error = %v", err)
		}
		if len(engine.fakeEngine.calls) != 0 || len(engine.asked) != 0 {
			t.Error("the engine was asked on behalf of nobody")
		}
	})
}

func TestEmailRenderReportsParseErrorsAsErrors(t *testing.T) {
	w, engine, _ := previewFixture(t)
	for _, tc := range []struct {
		name string
		doc  any
	}{
		{"an empty subject", map[string]any{"subject": ""}},
		{"a field the format does not have", map[string]any{"subject": "Hi", "blocks": []any{}, "mystery": true}},
		{"no document", nil},
		{"a document that is not an object", "just text"},
		{"both modes", map[string]any{"subject": "Hi", "textBody": "x", "htmlBody": "", "blocks": []any{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.doc)
			if err != nil {
				t.Fatal(err)
			}
			_, want := emaildoc.Parse(encoded)
			if want == nil {
				t.Fatal("the fixture is a document the parser accepts")
			}
			_, err = w.handleRenderEmail(importCtx(), map[string]any{"document": tc.doc, "accountId": renderOrg}, 0)
			// The parser's own sentence: it already names the block and the problem.
			if err == nil || err.Error() != want.Error() {
				t.Fatalf("error = %v, want the parser's: %v", err, want)
			}
		})
	}

	t.Run("a character that cannot be saved", func(t *testing.T) {
		doc := customDocument("")
		doc["textBody"] = "Hello\x00 there"
		_, err := w.handleRenderEmail(importCtx(), renderArgs(doc, nil), 0)
		if err == nil || err.Error() != controlRefused {
			t.Fatalf("error = %v, want %q", err, controlRefused)
		}
	})
	neverWrites(t, engine)
}

func TestEmailRenderRefusesAFileOverTheLimitBeforeSpendingAnything(t *testing.T) {
	w, engine, _ := previewFixture(t)
	doc := customDocument("")
	doc["textBody"] = strings.Repeat("a", emaildoc.MaxBytes)

	_, err := w.handleRenderEmail(importCtx(), renderArgs(doc, nil), 0)

	if err == nil || err.Error() != fileTooLarge || !errors.Is(err, emaildoc.ErrFileTooLarge) {
		t.Fatalf("error = %v, want %q", err, fileTooLarge)
	}
	// Before the organization is looked up, before a render slot is taken and
	// before the parser reads a byte of it.
	if len(engine.fakeEngine.calls) != 0 || len(engine.asked) != 0 || len(renderSlots) != 0 {
		t.Errorf("a file over the limit still cost something: %d engine calls, %d questions, %d slots",
			len(engine.fakeEngine.calls), len(engine.asked), len(renderSlots))
	}
}

// The file is measured as the file it is, not as the escaped JSON of it: a
// default JSON encoder writes each "<" as six bytes, and a 1.5 MiB email of
// them would be refused as nine.
func TestEmailRenderMeasuresTheFileNotItsEscapedForm(t *testing.T) {
	w, _, _ := previewFixture(t)
	doc := customDocument("")
	doc["textBody"] = strings.Repeat("<", emaildoc.MaxBytes*3/4)
	got := preview(t, w, importCtx(), doc, nil)
	if len(got.Text) != emaildoc.MaxBytes*3/4 {
		t.Errorf("the text part has %d bytes", len(got.Text))
	}
}

func TestEmailRenderReportsAnEmailTooLargeToWriteAsASentence(t *testing.T) {
	w, _, _ := previewFixture(t)
	// 130,000 tags of 15 bytes are 1.9 MiB, a file inside the limit; each is
	// filled with 100 characters, which is 13 MB once it is written out.
	doc := customDocument("<p>" + strings.Repeat("{{displayName}}", 130000) + "</p>")
	sample := map[string]any{"displayName": strings.Repeat("n", 100)}

	_, err := w.handleRenderEmail(importCtx(), renderArgs(doc, sample), 0)

	if err == nil || err.Error() != tooLargeToWrite {
		t.Fatalf("error = %v, want %q", err, tooLargeToWrite)
	}
	if errors.Is(err, emaildoc.ErrTooLarge) {
		t.Error("the renderer's own sentence went out; it is not one for a person")
	}
	if len(renderSlots) != 0 {
		t.Error("the render slot was not given back")
	}
}

func TestEmailRenderSamplesMergeValues(t *testing.T) {
	w, _, _ := previewFixture(t)
	doc := customDocument(`<p>Dear {{displayName}}, plan {{fields.plan}}, seats {{fields.seats}}</p>`)
	doc["subject"] = "Hello {{displayName}} from {{accountName}}"
	doc["textBody"] = "Dear {{displayName}} <{{email}}>, plan {{fields.plan}}, seats {{fields.seats}}, tags {{fields.tags}}, " +
		"nested {{fields.nested}}, flag {{fields.ok}}, blank {{fields.blank}}, campaign {{campaignName}}. {{unsubscribeUrl}}"
	sample := map[string]any{
		"displayName":  "Ada <Lovelace> & Co",
		"email":        "ada@example.test",
		"campaignName": "Spring",
		"fields": map[string]any{
			"plan": "pro", "seats": 5, "tags": []any{"a"}, "nested": map[string]any{"x": 1}, "ok": true, "blank": "  ",
		},
	}

	got := preview(t, w, importCtx(), doc, sample)

	// The subject and the text part take the values as written; the HTML part
	// escapes them.
	if got.Subject != "Hello Ada <Lovelace> & Co from Acme Co" {
		t.Errorf("subject = %q", got.Subject)
	}
	// Only a string is a sample value: a number, a list, an object and a flag are
	// dropped, and so is one that is only spaces, and their tags stay as written.
	wantText := "Dear Ada <Lovelace> & Co <ada@example.test>, plan pro, seats {{fields.seats}}, tags {{fields.tags}}, " +
		"nested {{fields.nested}}, flag {{fields.ok}}, blank {{fields.blank}}, campaign Spring. {{unsubscribeUrl}}"
	if got.Text != wantText {
		t.Errorf("text =\n%s\nwant\n%s", got.Text, wantText)
	}
	if want := "Dear Ada &lt;Lovelace&gt; &amp; Co, plan pro, seats {{fields.seats}}"; !strings.Contains(got.HTML, want) {
		t.Errorf("HTML lacks %q:\n%s", want, got.HTML)
	}

	t.Run("the organization's name unless the sample names another", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			sample map[string]any
			want   string
		}{
			{"no sample", nil, "From Acme Co"},
			{"a sample without it", map[string]any{"displayName": "Ada"}, "From Acme Co"},
			{"another name", map[string]any{"accountName": "Other Co"}, "From Other Co"},
			{"a blank name", map[string]any{"accountName": "   "}, "From Acme Co"},
			{"a name that is not text", map[string]any{"accountName": 7}, "From Acme Co"},
		} {
			d := customDocument("")
			d["subject"] = "From {{accountName}}"
			if got := preview(t, w, importCtx(), d, tc.sample); got.Subject != tc.want {
				t.Errorf("%s: subject = %q, want %q", tc.name, got.Subject, tc.want)
			}
		}
	})
}

func TestEmailRenderBoundsTheSample(t *testing.T) {
	// The sentence says the numbers the limits are.
	if !strings.Contains(sampleTooLarge, fmt.Sprint(maxSampleFields)) || !strings.Contains(sampleTooLarge, "1,000") || maxSampleChars != 1000 {
		t.Fatalf("the sentence %q does not describe the limits (%d fields, %d characters)", sampleTooLarge, maxSampleFields, maxSampleChars)
	}
	fields := func(n int, key, value string) map[string]any {
		out := map[string]any{}
		for i := 0; i < n; i++ {
			out[fmt.Sprintf("%s%d", key, i)] = value
		}
		return out
	}
	long := strings.Repeat("é", maxSampleChars) // characters, not bytes
	for _, tc := range []struct {
		name   string
		sample any
		want   string // "" is accepted
	}{
		{"as many fields as allowed", map[string]any{"fields": fields(maxSampleFields, "k", "v")}, ""},
		{"one field too many", map[string]any{"fields": fields(maxSampleFields+1, "k", "v")}, sampleTooLarge},
		{"a value of as many characters as allowed", map[string]any{"fields": map[string]any{"k": long}, "displayName": long}, ""},
		{"a field value one character too long", map[string]any{"fields": map[string]any{"k": long + "x"}}, sampleTooLarge},
		{"a name one character too long", map[string]any{"displayName": long + "x"}, sampleTooLarge},
		{"an email one character too long", map[string]any{"email": long + "x"}, sampleTooLarge},
		{"a field name one character too long", map[string]any{"fields": map[string]any{long + "x": "v"}}, sampleTooLarge},
		{"a field name of as many as allowed", map[string]any{"fields": map[string]any{long: "v"}}, ""},
		// Dropped values do not count against the characters, but the fields do.
		{"many fields that are not text", map[string]any{"fields": func() map[string]any {
			out := map[string]any{}
			for i := 0; i <= maxSampleFields; i++ {
				out[fmt.Sprintf("n%d", i)] = i
			}
			return out
		}()}, sampleTooLarge},
		{"fields that are not an object", map[string]any{"fields": "plan=pro"}, "The sample fields must be an object of column values."},
		{"a sample that is not an object", "displayName=Ada", "The sample values must be an object."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _, _ := previewFixture(t)
			args := renderArgs(customDocument(""), nil)
			args["sample"] = tc.sample
			_, err := w.handleRenderEmail(importCtx(), args, 0)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// The pictures are read as the caller. The engine here models the owner gate of
// the two Library reads, as the real queries have it: a file belongs to the
// person who uploaded it, and its artifact id is not a way to someone else's.
func TestEmailRenderReadsPicturesUnderTheCallersActor(t *testing.T) {
	w, engine, blobs := previewFixture(t)
	engine.pictureOwner = testOwner
	addPicture(engine, blobs, "private", "image/png", previewPNG(t, 4, 3))
	doc := customDocument(`<p><img src="artifact:private" alt="Private"></p>`)

	t.Run("the owner's own picture is embedded", func(t *testing.T) {
		got := preview(t, w, importCtx(), doc, nil)
		if !strings.Contains(got.HTML, "data:image/png;base64,") || got.has(emaildoc.IssueImageUnavailable, "") {
			t.Errorf("the owner's picture was not embedded: %+v\n%.400s", got.Issues, got.HTML)
		}
	})

	t.Run("somebody else's is not, and nothing of it is read", func(t *testing.T) {
		before, _ := blobs.reads()
		colleague := auth.ContextWithUserActor(context.Background(), "colleague-1")
		got := preview(t, w, colleague, doc, nil)

		if strings.Contains(got.HTML, "data:image") {
			t.Fatal("a colleague previewed another person's picture")
		}
		if !got.has(emaildoc.IssueImageUnavailable, "") {
			t.Errorf("no image_unavailable issue: %+v", got.Issues)
		}
		if after, _ := blobs.reads(); after != before {
			t.Error("storage was read for a picture the caller cannot see")
		}
	})

	t.Run("every Library read ran as the person who asked", func(t *testing.T) {
		seen := map[string]int{}
		for _, prefix := range []string{"query libraryArtifactById", "query libraryFileById"} {
			for _, c := range engine.callsTo(prefix) {
				seen[c.actorID]++
				if c.isOwner {
					t.Errorf("%s ran as a cluster owner", c.query)
				}
			}
		}
		if len(seen) != 2 || seen[testOwner] == 0 || seen["colleague-1"] == 0 {
			t.Errorf("the reads ran as %v, want the owner and the colleague and nobody else", seen)
		}
	})
	neverWrites(t, engine)
}

func TestEmailRenderReadsAPictureWithALimitedReader(t *testing.T) {
	w, engine, blobs := previewFixture(t)
	addPicture(engine, blobs, "pic", "image/png", previewPNG(t, 4, 3))

	preview(t, w, importCtx(), customDocument(`<p><img src="artifact:pic"></p>`), nil)

	blobs.mu.Lock()
	defer blobs.mu.Unlock()
	// One read, with the most a picture may be as its limit: a blob larger than
	// that is never read into memory, whatever the file's record says.
	if len(blobs.limits) != 1 || blobs.limits[0] != emaildoc.MaxImageBytes || blobs.unbounded != 0 {
		t.Errorf("limits = %v, unbounded reads = %d, want one read limited to %d", blobs.limits, blobs.unbounded, emaildoc.MaxImageBytes)
	}
}

func TestEmailRenderReadsEachPictureOnceAndNoMoreThanItMayCarry(t *testing.T) {
	t.Run("a picture used twenty times is read once", func(t *testing.T) {
		w, engine, blobs := previewFixture(t)
		addPicture(engine, blobs, "pic", "image/png", previewPNG(t, 4, 3))
		html := strings.Repeat(`<p><img src="artifact:pic"></p>`, 20)

		got := preview(t, w, importCtx(), customDocument(html), nil)

		if n := len(engine.callsTo("query libraryArtifactById")); n != 1 {
			t.Errorf("the artifact was looked up %d times", n)
		}
		if limited, _ := blobs.reads(); limited != 1 {
			t.Errorf("storage was read %d times", limited)
		}
		if strings.Count(got.HTML, "data:image/png;base64,") != 20 {
			t.Error("not every use of the picture was embedded")
		}
	})

	t.Run("a document naming forty pictures costs nine lookups", func(t *testing.T) {
		w, engine, _ := previewFixture(t)
		var html strings.Builder
		for i := 0; i < 40; i++ {
			fmt.Fprintf(&html, `<p><img src="artifact:p%d"></p>`, i)
		}

		got := preview(t, w, importCtx(), customDocument(html.String()), nil)

		// The eight an email may carry and the one that shows it has too many.
		want := emaildoc.MaxImages + 1
		if n := len(engine.callsTo("query libraryArtifactById")); n != want {
			t.Errorf("%d artifacts were looked up, want %d", n, want)
		}
		if n := got.count(emaildoc.IssueImageUnavailable); n != 40 {
			t.Errorf("%d pictures reported as unavailable, want all 40", n)
		}
	})

	t.Run("twelve readable pictures: nine are read, and the email says it has too many", func(t *testing.T) {
		w, engine, blobs := previewFixture(t)
		var html strings.Builder
		for i := 0; i < 12; i++ {
			addPicture(engine, blobs, fmt.Sprintf("p%d", i), "image/png", previewPNG(t, i+1, 2))
			fmt.Fprintf(&html, `<p><img src="artifact:p%d"></p>`, i)
		}

		got := preview(t, w, importCtx(), customDocument(html.String()), nil)

		if limited, _ := blobs.reads(); limited != emaildoc.MaxImages+1 {
			t.Errorf("storage was read %d times, want %d", limited, emaildoc.MaxImages+1)
		}
		if n := got.count(emaildoc.IssueImagesLimit); n != 1 {
			t.Errorf("images_limit reported %d times, want once: %+v", n, got.Issues)
		}
		if n := strings.Count(got.HTML, "data:image/png;base64,"); n != emaildoc.MaxImages+1 {
			t.Errorf("%d pictures embedded, want %d", n, emaildoc.MaxImages+1)
		}
	})
}

func TestPreviewIssuesAreEachOnceAndBounded(t *testing.T) {
	issue := func(code, block, message string) emaildoc.Issue {
		return emaildoc.Issue{Code: code, Block: block, Message: message}
	}
	t.Run("an issue said twice is said once, in the order it first came", func(t *testing.T) {
		got := previewIssues([]emaildoc.Issue{
			issue("link_missing", "a", "x"), issue("image_missing", "b", "y"), issue("link_missing", "a", "x"),
			issue("link_missing", "c", "x"), issue("image_missing", "b", "y"),
		})
		want := []emaildoc.Issue{issue("link_missing", "a", "x"), issue("image_missing", "b", "y"), issue("link_missing", "c", "x")}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("a flood of one kind leaves the others", func(t *testing.T) {
		var all []emaildoc.Issue
		for i := 0; i < 3*maxIssuesPerCode; i++ {
			all = append(all, issue("image_unavailable", "", fmt.Sprintf("picture %d", i)))
		}
		all = append(all, issue("images_limit", "", "too many"), issue("unsubscribe_missing", "", "add one"), issue("email_too_large", "", "big"))
		got := previewIssues(all)
		counts := map[string]int{}
		for _, i := range got {
			counts[i.Code]++
		}
		want := map[string]int{"image_unavailable": maxIssuesPerCode, "images_limit": 1, "unsubscribe_missing": 1, "email_too_large": 1}
		if fmt.Sprint(counts) != fmt.Sprint(want) {
			t.Errorf("counts = %v, want %v", counts, want)
		}
		// The ones kept are the first ones.
		if got[0].Message != "picture 0" || got[maxIssuesPerCode-1].Message != fmt.Sprintf("picture %d", maxIssuesPerCode-1) {
			t.Errorf("the issues kept are not the first: %v ... %v", got[0], got[maxIssuesPerCode-1])
		}
	})
	t.Run("none is an empty list and not nil", func(t *testing.T) {
		for _, in := range [][]emaildoc.Issue{nil, {}} {
			if got := previewIssues(in); got == nil || len(got) != 0 {
				t.Errorf("previewIssues(%v) = %#v", in, got)
			}
		}
	})
}

func TestEmailRenderBoundsTheIssuesOfAHugeDocument(t *testing.T) {
	w, _, _ := previewFixture(t)
	t.Run("three hundred different pictures that cannot be resolved", func(t *testing.T) {
		var html strings.Builder
		for i := 0; i < 300; i++ {
			fmt.Fprintf(&html, `<img src="cid:image%d">`, i)
		}
		got := preview(t, w, importCtx(), customDocument(html.String()), nil)
		if n := got.count(emaildoc.IssueImageUnavailable); n != maxIssuesPerCode {
			t.Errorf("%d issues, want %d", n, maxIssuesPerCode)
		}
	})
	t.Run("three hundred uses of one", func(t *testing.T) {
		got := preview(t, w, importCtx(), customDocument(strings.Repeat(`<img src="cid:same">`, 300)), nil)
		if n := got.count(emaildoc.IssueImageUnavailable); n != 1 {
			t.Errorf("%d issues, want 1", n)
		}
	})
}

// The render slots are the node's protection from a document that is expensive
// to read and write: 4 at a time in a process, the rest wait under their own
// deadline.
func TestEmailRenderLimitsConcurrentRenders(t *testing.T) {
	if maxConcurrentRenders != 4 {
		t.Fatalf("a process renders %d previews at once, want 4", maxConcurrentRenders)
	}
	w, engine, blobs := previewFixture(t)
	addPicture(engine, blobs, "pic", "image/png", previewPNG(t, 4, 3))
	blobs.hold = make(chan struct{})
	blobs.entered = make(chan struct{}, 32)
	var release sync.Once
	letGo := func() { release.Do(func() { close(blobs.hold) }) }
	t.Cleanup(letGo)
	doc := customDocument(`<p><img src="artifact:pic"></p>`)

	// Twelve people preview at once. Four get in and are parked reading a picture.
	// What they get is read here, on the test's own goroutine.
	type outcome struct {
		nodes []memorynodes.MemoryNode
		err   error
	}
	done := make(chan outcome, 12)
	for i := 0; i < 12; i++ {
		go func() {
			nodes, err := w.handleRenderEmail(importCtx(), renderArgs(doc, nil), 0)
			done <- outcome{nodes, err}
		}()
	}
	for i := 0; i < maxConcurrentRenders; i++ {
		select {
		case <-blobs.entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d renders started", i)
		}
	}
	// Give the others every chance to get in wrongly.
	time.Sleep(100 * time.Millisecond)
	if got := blobs.currentInflight(); got != maxConcurrentRenders {
		t.Fatalf("%d renders are in progress, want exactly %d", got, maxConcurrentRenders)
	}
	if len(renderSlots) != maxConcurrentRenders {
		t.Errorf("%d slots are held, want %d", len(renderSlots), maxConcurrentRenders)
	}

	// A thirteenth gives up under its own deadline, with a sentence for a person,
	// having asked nothing of storage.
	short, cancel := context.WithTimeout(importCtx(), 50*time.Millisecond)
	defer cancel()
	_, err := w.handleRenderEmail(short, renderArgs(doc, nil), 0)
	if err == nil || err.Error() != previewBusy {
		t.Fatalf("a preview that waited past its deadline: %v, want %q", err, previewBusy)
	}
	// Someone who may not use the organization does not wait for a slot to be
	// told so: the organization is looked at first.
	outsider, leave := context.WithTimeout(importCtx(), 50*time.Millisecond)
	defer leave()
	_, err = w.handleRenderEmail(outsider, map[string]any{"document": doc, "accountId": "elsewhere"}, 0)
	if err == nil || err.Error() != notAccessible {
		t.Fatalf("an outsider waited for a slot: %v, want %q", err, notAccessible)
	}
	// Reading the document is part of what the slots limit, and the most costly
	// part: one that cannot be read is not read until a slot is free.
	waiting, stop := context.WithTimeout(importCtx(), 50*time.Millisecond)
	defer stop()
	_, err = w.handleRenderEmail(waiting, map[string]any{"document": map[string]any{"subject": ""}, "accountId": renderOrg}, 0)
	if err == nil || err.Error() != previewBusy {
		t.Fatalf("a document that cannot be read was read before a slot was free: %v, want %q", err, previewBusy)
	}

	// Let them go: all twelve finish, one after another in fours, and nothing
	// was ever above the limit.
	letGo()
	for i := 0; i < 12; i++ {
		select {
		case out := <-done:
			if out.err != nil {
				t.Errorf("a waiting preview failed: %v", out.err)
			} else if !strings.Contains(decodePreview(t, out.nodes).HTML, "data:image/png") {
				t.Error("a waiting preview lost its picture")
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of 12 previews finished", i)
		}
	}
	if peak := blobs.peakInflight(); peak != maxConcurrentRenders {
		t.Errorf("the most renders at once was %d, want %d", peak, maxConcurrentRenders)
	}
	if len(renderSlots) != 0 {
		t.Errorf("%d slots were not given back", len(renderSlots))
	}
	// The slots are free again.
	if got := preview(t, w, importCtx(), customDocument(""), nil); got.Subject == "" {
		t.Error("a preview after the others failed")
	}
}

func TestEmailRenderGivesBackItsSlotWhenItFails(t *testing.T) {
	w, _, _ := previewFixture(t)
	for i := 0; i < 3*maxConcurrentRenders; i++ {
		// A parse error after the slot is taken, and one before it.
		if _, err := w.handleRenderEmail(importCtx(), map[string]any{"document": map[string]any{"subject": ""}, "accountId": renderOrg}, 0); err == nil {
			t.Fatal("the document was accepted")
		}
	}
	if len(renderSlots) != 0 {
		t.Fatalf("%d slots are still held", len(renderSlots))
	}
	// A render that is cancelled before it starts is cancelled, and holds nothing.
	cancelled, cancel := context.WithCancel(importCtx())
	cancel()
	if _, err := w.handleRenderEmail(cancelled, renderArgs(customDocument(""), nil), 0); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled preview: %v", err)
	}
	if len(renderSlots) != 0 {
		t.Fatalf("%d slots are still held after a cancellation", len(renderSlots))
	}
}

func TestEmailRenderCapabilityIsRegistered(t *testing.T) {
	w, _, _ := previewFixture(t)
	for _, c := range w.Capabilities() {
		if c.Name != "renderEmail" {
			continue
		}
		if c.Handler == nil {
			t.Fatal("renderEmail has no handler")
		}
		if c.PreserveOrder {
			t.Error("renderEmail answers one preview; there is no order to preserve")
		}
		return
	}
	t.Fatal("renderEmail is not registered; the builtin emailRender names it as its executor")
}
