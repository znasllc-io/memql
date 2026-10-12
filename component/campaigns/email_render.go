package campaigns

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// email_render.go -- the preview of an email document (the emailRender builtin).
//
// An editor shows the email as it will be sent, with the pictures in it and
// sample values where a recipient's would be. This is that render: the same
// emaildoc.Render that publishing runs, in preview mode, over a document the
// caller hands in. It saves nothing and sends nothing.
//
// # Whose pictures
//
// A document names a picture as artifact:<id>, a Library file. The preview
// embeds the pictures the CALLER can read and no others. The artifact and the
// file row it names are read under the caller's own actor, through the Library's
// named reads (libraryArtifactById and libraryFileById), which gate on the
// file's owner and deliberately scope even a cluster owner to their own rows
// (component/memql/library_file_4340_db_test.go, claim 4). So an artifact id is
// not a way to somebody else's upload, whoever asks; and a preview is
// owner-only for the same reason importRecipients is, for privacy. A picture the
// caller cannot read is the same answer as one that does not exist, an
// image_unavailable issue, and storage is not asked for it.
//
// The bytes are then fetched with the node's own storage credential, which has
// no notion of a caller, at the blobUrl the file row records: that read trusts a
// row the caller owns, as the Library's own download route does, and is not
// itself made as the caller. The organization is looked at first: someone
// outside it is refused before anything of the document is read, and someone
// inside needs to read its data.
//
// # What it may cost
//
// A preview is asked for as a person types, by anyone who can write an email,
// and runs on a node that does other work. Reading and writing one document can
// take more than 200 MiB (emaildoc measures the worst it accepts at 228 MiB),
// so nothing here is left to the document:
//
//   - the document is measured as the file it is, and one over MaxBytes is
//     refused before the organization is looked up and before the parser reads
//     any of it;
//   - a process writes maxConcurrentRenders previews at once, and the rest wait
//     under their own deadline and for at most renderSlotWait, since a
//     statement on a stream has none;
//   - a picture is read with a limit of what a picture in an email may be, so a
//     blob larger than that is never held in memory, whatever its file row says,
//     and a read takes at most pictureReadTimeout, so a slow storage cannot hold
//     a slot for ever;
//   - a document can name tens of thousands of pictures, and each read is two
//     queries and a request to storage, so a render reads no more than the
//     eight an email may carry and the one that shows it carries too many;
//   - a document can make as many issues, so the list that goes back has each
//     once and at most maxIssuesPerCode of each kind;
//   - the sample is a handful of values, not a second document;
//   - the answer is plain JSON (resultNode), where encoding/json's default would
//     write each "<", ">" and "&" of the email as six bytes: 47.6 MiB for an
//     email of 1.9 MiB, and the engine and the wire read it again after the slot
//     is given back.
//
// What the renderer refuses as too large (more than MaxRenderBytes once it is
// written out) goes back as a sentence for a person, not the renderer's error.

// The limits of a preview.
const (
	// maxConcurrentRenders is how many previews one process writes at once.
	maxConcurrentRenders = 4

	// maxPictureReads is the most pictures one render reads: the MaxImages an
	// email may carry, and one more, which is how a render learns that it carries
	// too many and says so (images_limit). Past it a picture is unavailable.
	maxPictureReads = emaildoc.MaxImages + 1

	// maxIssuesPerCode is the most issues of one kind a preview lists. Kinds are
	// counted apart so that a flood of one (a hundred pictures that cannot be
	// read) does not hide a single other (no unsubscribe link).
	maxIssuesPerCode = 50

	// maxSampleFields is the most field values a sample has, and maxSampleChars
	// the most characters in one of them, in a field's name and in a value.
	maxSampleFields = 100
	maxSampleChars  = 1000
)

// renderSlots holds one value for each preview being written in this process.
var renderSlots = make(chan struct{}, maxConcurrentRenders)

// The waits of a preview. A statement that arrives on a stream carries no
// deadline, so a preview sets its own: it waits at most renderSlotWait for a
// slot, and one picture read takes at most pictureReadTimeout. They are
// variables so that a test can shorten them.
var (
	renderSlotWait     = 10 * time.Second
	pictureReadTimeout = 10 * time.Second
)

var (
	errPreviewBusy     = errors.New("Previews are busy right now. Try again in a moment.")
	errNoPreviewAccess = errors.New("You need read access to this organization's data to preview an email.")
	errSampleTooLarge  = errors.New("The sample values are too large. Use up to 100 fields of up to 1,000 characters each.")
	errPreviewTooLarge = fmt.Errorf("This email is larger than %d MiB once it is written out. Shorten it or use fewer pictures.", emaildoc.MaxRenderBytes>>20)
)

// handleRenderEmail implements integration.campaigns.renderEmail.
func (w *Worker) handleRenderEmail(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	accountID := bare(argString(args, "accountId"))
	if accountID == "" {
		return nil, errors.New("Choose the organization this email is for.")
	}
	if callerUserID(ctx) == "" {
		return nil, errors.New("Sign in to preview an email.")
	}

	// What costs nothing comes first, and only then what costs something: the
	// file's size, the sample, the organization, a slot, and last the parse.
	encoded, err := encodeDocument(args["document"])
	if err != nil {
		return nil, fmt.Errorf("The email document could not be read: %w", err)
	}
	if len(encoded) > emaildoc.MaxBytes {
		return nil, emaildoc.ErrFileTooLarge
	}
	sample, err := sampleValues(args["sample"])
	if err != nil {
		return nil, err
	}

	accounts, err := w.store.rows(ctx, call("query", "clientAccountById", arg{"accountId", accountID}))
	if err != nil {
		return nil, err
	}
	if len(accounts) != 1 || str(accounts[0], "status") != "active" {
		return nil, errors.New("organization is not active or accessible")
	}
	if !w.store.engine.OrganizationCapable(ctx, accountID, auth.VerbRead, auth.ResourceData) {
		return nil, errNoPreviewAccess
	}

	release, err := acquireRenderSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	doc, err := emaildoc.Parse(encoded)
	if err != nil {
		return nil, err
	}
	// {{accountName}} is the organization's own name, unless the sample says
	// another.
	if _, named := sample["accountName"]; !named {
		if name := strings.TrimSpace(str(accounts[0], "name")); name != "" {
			sample["accountName"] = name
		}
	}
	result, err := emaildoc.Render(ctx, doc, &pictureReads{inner: artifactImages{w}, left: maxPictureReads},
		emaildoc.RenderOptions{Preview: true, Sample: sample})
	if err != nil {
		if errors.Is(err, emaildoc.ErrTooLarge) {
			return nil, errPreviewTooLarge
		}
		return nil, err
	}
	return resultNode("campaignEmailRendered", map[string]any{
		"subject": result.Subject,
		"html":    result.HTML,
		"text":    result.Text,
		"issues":  previewIssues(result.Issues),
	})
}

// encodeDocument writes the document the way its file is written, for measuring
// it and handing it to the parser: HTML is left as it is, where a JSON encoder's
// default writes each "<", ">" and "&" as six bytes and would measure a 1.5 MiB
// email of them as nine. Every place that measures a document as a file measures
// it with this, so that what one accepts the other does not refuse as too large.
func encodeDocument(document any) ([]byte, error) {
	return plainJSON(document)
}

// sampleValues flattens the sample a client sends ({displayName, email,
// accountName, campaignName, fields: {column: value}}) into the keys the
// renderer fills: the four, and fields.<column> for each column.
//
// Only a string is a value. A number, a list, an object or a flag is dropped,
// and so is a string that is empty or only spaces: its tag stays as written
// (and {{accountName}} is the organization's name), which shows the person that
// the email has a merge field there. What is not dropped is bounded: a sample
// is a few values to see the email with, and the renderer builds a replacer
// from every one of them.
func sampleValues(raw any) (map[string]string, error) {
	sample := map[string]string{}
	if raw == nil {
		return sample, nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("The sample values must be an object.")
	}
	for _, key := range []string{"displayName", "email", "accountName", "campaignName"} {
		text, kept, err := sampleText(object[key])
		if err != nil {
			return nil, err
		}
		if kept {
			sample[key] = text
		}
	}
	rawFields, present := object["fields"]
	if !present || rawFields == nil {
		return sample, nil
	}
	fields, ok := rawFields.(map[string]any)
	if !ok {
		return nil, errors.New("The sample fields must be an object of column values.")
	}
	if len(fields) > maxSampleFields {
		return nil, errSampleTooLarge
	}
	for column, value := range fields {
		if utf8.RuneCountInString(column) > maxSampleChars {
			return nil, errSampleTooLarge
		}
		text, kept, err := sampleText(value)
		if err != nil {
			return nil, err
		}
		if kept {
			sample["fields."+column] = text
		}
	}
	return sample, nil
}

// sampleText is a sample value as text: kept when it is a string with something
// in it, and an error when it is too long.
func sampleText(value any) (text string, kept bool, err error) {
	text, ok := value.(string)
	if !ok {
		return "", false, nil
	}
	if utf8.RuneCountInString(text) > maxSampleChars {
		return "", false, errSampleTooLarge
	}
	return text, strings.TrimSpace(text) != "", nil
}

// acquireRenderSlot waits for one of the maxConcurrentRenders slots, under the
// caller's own deadline and for no longer than renderSlotWait whatever that is:
// a statement that arrives on a stream carries no deadline of its own, and a
// preview that waits for ever holds a connection and a goroutine for nobody. It
// returns the function that gives the slot back. A caller whose deadline passes
// while it waits, or that waits the whole time, is told the previews are busy;
// one that has gone away is told nothing it will read.
func acquireRenderSlot(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, waitRefusal(err)
	}
	wait := time.NewTimer(renderSlotWait)
	defer wait.Stop()
	select {
	case renderSlots <- struct{}{}:
		return func() { <-renderSlots }, nil
	case <-ctx.Done():
		return nil, waitRefusal(ctx.Err())
	case <-wait.C:
		return nil, errPreviewBusy
	}
}

func waitRefusal(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errPreviewBusy
	}
	return err
}

// previewIssues is the list of issues a preview returns: each issue once, in
// the order it was found, and at most maxIssuesPerCode of any one kind. A
// document the size of the limit can have a hundred thousand issues, and the
// answer to it should not be larger than the document. The list is never nil.
func previewIssues(all []emaildoc.Issue) []emaildoc.Issue {
	out := make([]emaildoc.Issue, 0, min(len(all), 16))
	seen := map[emaildoc.Issue]bool{}
	perCode := map[string]int{}
	for _, issue := range all {
		if seen[issue] || perCode[issue.Code] >= maxIssuesPerCode {
			continue
		}
		seen[issue] = true
		perCode[issue.Code]++
		out = append(out, issue)
	}
	return out
}

// pictureReads lets a render read at most so many pictures. Render asks for a
// picture once for each different one, from the goroutine that called it. A
// picture past the limit is answered with emaildoc.ErrTooManyPictures, which the
// render reports as the email having more pictures than it can carry
// (images_limit, once) and not as a picture that could not be read: the picture
// is fine, and the email has too many.
type pictureReads struct {
	inner emaildoc.ImageResolver
	left  int
}

func (p *pictureReads) ResolveImage(ctx context.Context, artifactID string) (emaildoc.Image, error) {
	if p.left <= 0 {
		return emaildoc.Image{}, emaildoc.ErrTooManyPictures
	}
	p.left--
	return p.inner.ResolveImage(ctx, artifactID)
}

// artifactImages reads the Library pictures a document names, as the caller of
// the render: emaildoc asks it for an artifact id, and it answers with the
// picture's bytes or says why not (the renderer turns that into an
// image_unavailable issue for the block).
type artifactImages struct{ w *Worker }

// ResolveImage reads one picture. The artifact and its file are read under the
// caller's actor (see LibraryFileForArtifact), so a picture the caller cannot
// see is not found; the file must not be archived, must be a PNG, JPEG or GIF
// of at most emaildoc.MaxImageBytes, and its bytes are read with that as the
// limit and checked as sending checks them.
func (a artifactImages) ResolveImage(ctx context.Context, artifactID string) (emaildoc.Image, error) {
	// A read has its own time, from the first lookup to the last byte of storage:
	// a slow one does not hold the render, and with it a slot, for ever. The
	// render's own context is not touched, so a read that runs out of time is a
	// picture that could not be read and the render goes on.
	read, cancel := context.WithTimeout(ctx, pictureReadTimeout)
	defer cancel()
	image, err := a.read(read, artifactID)
	if err != nil && ctx.Err() == nil && a.w.logger != nil {
		a.w.logger.Debug("campaigns: a picture in an email could not be read", "artifact", artifactID, "reason", err)
	}
	return image, err
}

func (a artifactImages) read(ctx context.Context, artifactID string) (emaildoc.Image, error) {
	ref, refusal, err := a.w.store.LibraryFileForArtifact(ctx, artifactID)
	if err != nil {
		return emaildoc.Image{}, err
	}
	if refusal != "" {
		return emaildoc.Image{}, errors.New(refusal)
	}
	if ref.Archived {
		return emaildoc.Image{}, fmt.Errorf("file %q is archived", ref.FileID)
	}
	switch ref.MimeType {
	case "image/png", "image/jpeg", "image/gif":
	default:
		return emaildoc.Image{}, fmt.Errorf("file %q is %q, not a picture an email can carry", ref.FileID, ref.MimeType)
	}
	// The file row's size is a record and may be wrong; it only saves a request
	// to storage. The limit on the read is what keeps a large blob out of memory.
	if ref.Size > emaildoc.MaxImageBytes {
		return emaildoc.Image{}, fmt.Errorf("file %q is %d bytes, over the %d a picture in an email may be", ref.FileID, ref.Size, emaildoc.MaxImageBytes)
	}
	data, err := a.w.readBlobUpTo(ctx, ref.BlobURL, emaildoc.MaxImageBytes)
	if err != nil {
		return emaildoc.Image{}, fmt.Errorf("reading file %q: %w", ref.FileID, err)
	}
	if err := compose.EmailImageData(data, ref.MimeType); err != nil {
		return emaildoc.Image{}, fmt.Errorf("file %q: %w", ref.FileID, err)
	}
	return emaildoc.Image{MIMEType: ref.MimeType, Data: data}, nil
}
