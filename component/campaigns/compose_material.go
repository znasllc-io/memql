package campaigns

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"path"
	"strings"
	"unicode/utf8"

	pure "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	"github.com/znasllc-io/memql/integrations/compose"
)

// compose_material.go -- what an email composition is written from: the files
// the person attached and the picture of the finished email, read as the
// caller, unpacked when they are ZIPs, and named as they appear in the
// composer's workspace.
//
// # Whose files
//
// A request names Library files by id. Each is read under the CALLER's actor
// (libraryFileById gates on the file's owner), so a file id is not a way into
// somebody else's uploads; a file the caller cannot see is left out like any
// file that cannot be used. The bytes are then read with the node's own storage
// credential, with a limit, as the CSV import and the picture reader read them.
//
// # What is kept, and what is left out
//
// Pictures (PNG, JPEG or GIF) and text files are kept; a ZIP is read with the
// hardened reader and the limits composition gives it (compose.ReadArchive,
// compose.CompositionArchiveLimits: no path leaving the archive, no nested
// archive, no link). Anything else, and anything that cannot be read as what
// its name says, is left out with a reason in the Materializer's own words, and
// a run names each in its notes ("Left out <name>: <reason>."). A ZIP the
// reader refuses is left out whole, with the reader's sentence.
//
// A picture over 1 MiB can never go in an email (emaildoc.MaxImageBytes), and
// pictures are neither shrunk nor converted, so one is left out too. The picture
// of the finished email is the exception: it is guidance the composer looks at,
// kept whatever its size, and has a handle only when an email could carry it.
//
// # Names
//
// A file keeps its Library name. A ZIP member is "<zip name without
// .zip>__<path with each / as __>", so the email.txt of two folders are two
// files. A name already taken, ignoring case (a workspace on a Mac is), gets
// -2, -3 and so on before its extension.
//
// # What it may cost
//
// Every tool call of a run reads the material again (nothing is kept between
// calls), and one read can hold a few hundred MiB at its peak: the files kept,
// the data URIs of their pictures, a download in flight and a ZIP being
// unpacked. So the material is read in a render slot (acquireRenderSlot), the
// same few a preview or a save renders in, held by the tools for their whole
// call: a node holds no more material at once than it renders emails. Within a
// read: a request names at most maxMaterialFiles files; each is read up to
// maxMaterialFileBytes; a picture whose Library row is larger than an email can
// carry is left out before it is read (the picture of the finished email is
// read up to the per-file limit, being guidance); and all that is kept,
// together, is at most maxMaterialBytes, past which a file is left out with a
// reason.

const (
	// maxMaterialFiles is the most files a request names (the picture of the
	// finished email aside): what can be attached.
	maxMaterialFiles = 64

	// maxMaterialFileBytes is the most of one attached file that is read.
	maxMaterialFileBytes = 64 << 20

	// maxMaterialBytes is the most the files kept from one request hold
	// together: what one ZIP may unpack to.
	maxMaterialBytes = 64 << 20
)

var (
	errTooManyMaterialFiles = fmt.Errorf("An email is composed from at most %d attached files. Attach fewer.", maxMaterialFiles)
	errComposeBusy          = errors.New("Composing is busy right now. Try again in a moment.")
)

// The reasons a file is left out that the archive reader does not give. The
// ones it does give (not a picture or text file, empty file and the rest) are
// written the same way below for a file that is not in a ZIP.
const (
	reasonNotInLibrary  = "it is not in your Library"
	reasonArchived      = "it is archived"
	reasonNoContents    = "its contents are not stored yet"
	reasonFileTooLarge  = "it is larger than 64 MiB"
	reasonMaterialFull  = "the attached files together are larger than 64 MiB"
	reasonPictureTooBig = "a picture over 1 MiB, more than an email can carry"
	reasonPixels        = "a picture over 24 million pixels"
	reasonTextTooLarge  = "more than 4 MiB of text"
	reasonNotAPicture   = "not a PNG, JPEG or GIF picture"
)

// materialFile is one file of the material, under the name it has in the
// composer's workspace. ArtifactID is empty here: it is the Library artifact
// the file is filed as for the session, set by whoever files it.
type materialFile struct {
	Workspace, MIME string
	Data            []byte
	ArtifactID      string
}

// material is what a composition is written from.
type material struct {
	Files []materialFile
	// Handles maps the handle of each picture an email can carry
	// ("memql-asset:<sha256>", compose.EmailAssetHandle) to its data URI.
	Handles map[string]string
	// Skipped is every file left out, under its workspace name, with why.
	Skipped []compose.SkippedMember
	// Reference is the workspace name of the picture of the finished email,
	// or "".
	Reference string
}

// notes are the sentences a finished run records for what it left out.
func (m material) notes() []string {
	notes := make([]string, 0, len(m.Skipped))
	for _, s := range m.Skipped {
		notes = append(notes, "Left out "+s.Path+": "+strings.TrimSuffix(s.Reason, ".")+".")
	}
	return notes
}

// compositionMaterial reads a request's material in a render slot of its own,
// for a caller that holds none (a run, before its session starts). The tools
// hold one slot for their whole call and read with readMaterial inside it.
func (w *Worker) compositionMaterial(ctx context.Context, req compositionRequest) (material, error) {
	release, err := acquireComposeSlot(ctx)
	if err != nil {
		return material{}, err
	}
	defer release()
	return w.readMaterial(ctx, req)
}

// acquireComposeSlot takes a render slot for reading a composition's material
// (and, for a tool, the render that follows), or says composing is busy.
func acquireComposeSlot(ctx context.Context) (func(), error) {
	release, err := acquireRenderSlot(ctx)
	if errors.Is(err, errPreviewBusy) {
		return nil, errComposeBusy
	}
	return release, err
}

// readMaterial reads the files of a request, then its picture of the finished
// email, as the caller; the caller holds a render slot. A file named twice is
// read once, and a request naming more than maxMaterialFiles is refused before
// anything is read. When files were attached and none of them can be used there
// is nothing to write from, and the answer is an error that names each one and
// why.
func (w *Worker) readMaterial(ctx context.Context, req compositionRequest) (material, error) {
	var ids []string // the attached files in order, then the picture of the finished email
	seen := map[string]bool{}
	for _, id := range req.FileIDs {
		if id = bare(strings.TrimSpace(id)); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) > maxMaterialFiles {
		return material{}, errTooManyMaterialFiles
	}
	reference := bare(strings.TrimSpace(req.ReferenceFileID))
	if reference != "" && !seen[reference] {
		ids = append(ids, reference)
	}

	r := &materialReader{w: w, m: material{Handles: map[string]string{}}, taken: map[string]bool{}}
	for _, id := range ids {
		if err := r.read(ctx, id, id == reference); err != nil {
			return material{}, err
		}
	}
	if len(ids) > 0 && len(r.m.Files) == 0 {
		return material{}, fmt.Errorf("None of the attached files can be used. %s", strings.Join(r.m.notes(), " "))
	}
	return r.m, nil
}

// materialReader builds one request's material.
type materialReader struct {
	w     *Worker
	m     material
	taken map[string]bool // workspace names, lower-cased
	held  int             // bytes kept so far
}

// read reads one Library file under the caller and keeps what can be used of
// it. Only a failure to read storage, or a cancelled ctx, is an error.
func (r *materialReader) read(ctx context.Context, fileID string, reference bool) error {
	rows, err := r.w.store.rows(ctx, call("query", "libraryFileById", arg{"fileId", fileID}))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		r.skip(fileID, reasonNotInLibrary)
		return nil
	}
	row := rows[0]
	name := strings.TrimSpace(str(row, "name"))
	if name == "" {
		name = fileID
	}
	// What a file is decides by its name, as for a ZIP member, so a file that
	// cannot be used costs no read of storage.
	kind := materialKind(name, str(row, "mimeType"))
	switch {
	case boolean(row, "archived"):
		r.skip(name, reasonArchived)
		return nil
	case reference && kind != kindPicture:
		r.skip(name, reasonNotAPicture)
		return nil
	case kind == kindSVG:
		r.skip(name, "svg is not a picture an email can carry")
		return nil
	case kind == kindOther:
		r.skip(name, "not a picture or text file")
		return nil
	case strings.TrimSpace(str(row, "blobUrl")) == "":
		r.skip(name, reasonNoContents)
		return nil
	case integer(row, "size") > maxMaterialFileBytes:
		r.skip(name, reasonFileTooLarge)
		return nil
	case kind == kindPicture && !reference && integer(row, "size") > emaildoc.MaxImageBytes:
		// It could only be left out once read (keepPicture); the row says so first.
		r.skip(name, reasonPictureTooBig)
		return nil
	}
	data, err := r.w.readBlobUpTo(ctx, str(row, "blobUrl"), str(row, "ownerUserId"), maxMaterialFileBytes)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if r.w.logger != nil {
			r.w.logger.Warn("campaigns: a composition's file could not be read", "file", fileID, "error", err)
		}
		return fmt.Errorf("%s could not be read from your Library. Try again in a moment, or attach it again.", name)
	}
	if len(data) == 0 {
		r.skip(name, "empty file")
		return nil
	}
	switch kind {
	case kindArchive:
		r.addArchive(name, data)
	case kindPicture:
		r.addPicture(name, data, reference)
	case kindText:
		r.addText(name, data)
	}
	return nil
}

// addArchive keeps the members of a ZIP that can be used, each under its
// workspace name, and leaves out the rest; a ZIP the reader refuses is left out
// whole.
func (r *materialReader) addArchive(name string, data []byte) {
	archive, err := compose.ReadArchive(data, compose.CompositionArchiveLimits)
	if err != nil {
		r.skip(name, err.Error())
		return
	}
	stem := name
	if ext := path.Ext(name); strings.EqualFold(ext, ".zip") {
		stem = strings.TrimSuffix(name, ext)
	}
	member := func(p string) string { return stem + "__" + strings.ReplaceAll(p, "/", "__") }
	for _, m := range archive.Members {
		if strings.HasPrefix(m.MIME, "image/") {
			r.keepPicture(member(m.Path), m.MIME, m.Data, false)
		} else {
			r.keep(member(m.Path), m.MIME, m.Data)
		}
	}
	for _, s := range archive.Skipped {
		r.skip(member(s.Path), s.Reason)
	}
}

// addPicture checks a picture file the way the archive reader checks a member.
func (r *materialReader) addPicture(name string, data []byte, reference bool) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg" && format != "gif") {
		r.skip(name, "not a valid picture")
		return
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > compose.CompositionArchiveLimits.MaxPixels {
		r.skip(name, reasonPixels)
		return
	}
	r.keepPicture(name, "image/"+format, data, reference)
}

// addText checks a text file the way the archive reader checks a member.
func (r *materialReader) addText(name string, data []byte) {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		r.skip(name, "not valid UTF-8 text")
		return
	}
	if int64(len(data)) > compose.CompositionArchiveLimits.MaxTextBytes {
		r.skip(name, reasonTextTooLarge)
		return
	}
	r.keep(name, "text/plain", data)
}

// keepPicture keeps a picture, with a handle when an email can carry it. One
// an email cannot carry is left out, unless it is the picture of the finished
// email, which is kept to look at.
func (r *materialReader) keepPicture(workspace, mime string, data []byte, reference bool) {
	embeddable := len(data) <= emaildoc.MaxImageBytes && pure.EmailImageData(data, mime) == nil
	if !embeddable && !reference {
		reason := "not a valid picture"
		if len(data) > emaildoc.MaxImageBytes {
			reason = reasonPictureTooBig
		}
		r.skip(workspace, reason)
		return
	}
	name, kept := r.keep(workspace, mime, data)
	if !kept {
		return
	}
	if embeddable {
		r.m.Handles[compose.EmailAssetHandle(data)] = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	}
	if reference {
		r.m.Reference = name
	}
}

// keep adds a file under a workspace name nobody has taken, while the
// material has room for it, and answers the name it was given.
func (r *materialReader) keep(workspace, mime string, data []byte) (string, bool) {
	if r.held+len(data) > maxMaterialBytes {
		r.skip(workspace, reasonMaterialFull)
		return "", false
	}
	r.held += len(data)
	name := r.unique(workspace)
	r.m.Files = append(r.m.Files, materialFile{Workspace: name, MIME: mime, Data: data})
	return name, true
}

func (r *materialReader) skip(name, reason string) {
	r.m.Skipped = append(r.m.Skipped, compose.SkippedMember{Path: name, Reason: reason})
}

// unique is name, or the first of name-2, name-3 and so on (before its
// extension) that no file has yet, ignoring case.
func (r *materialReader) unique(name string) string {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	candidate := name
	for n := 2; r.taken[strings.ToLower(candidate)]; n++ {
		candidate = fmt.Sprintf("%s-%d%s", stem, n, ext)
	}
	r.taken[strings.ToLower(candidate)] = true
	return candidate
}

// The kinds of file the material reads.
const (
	kindOther = iota
	kindArchive
	kindPicture
	kindText
	kindSVG
)

// materialKind is what a file is by its name, as the archive reader decides
// for a member, or by its type when its name has no extension.
func materialKind(name, mime string) int {
	switch ext := strings.ToLower(path.Ext(name)); ext {
	case ".zip":
		return kindArchive
	case ".png", ".jpg", ".jpeg", ".gif":
		return kindPicture
	case ".txt", ".md", ".markdown", ".html", ".htm", ".css", ".json", ".csv":
		return kindText
	case ".svg":
		return kindSVG
	case "":
		switch mime = strings.ToLower(strings.TrimSpace(mime)); {
		case mime == "application/zip" || mime == "application/x-zip-compressed":
			return kindArchive
		case mime == "image/png" || mime == "image/jpeg" || mime == "image/gif":
			return kindPicture
		case mime == "image/svg+xml":
			return kindSVG
		case strings.HasPrefix(mime, "text/"):
			return kindText
		}
	}
	return kindOther
}
