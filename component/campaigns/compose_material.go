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
// compose.CompositionArchiveLimits: no path leaving the archive, no archive
// inside it opened, no link). Anything else, and anything that cannot be read
// as what its name says, is left out with a reason, and a run names each in its
// notes ("Left out <name>: <reason>."). One ZIP member that cannot be used (a
// picture past the pixel limit, an archive inside the archive, text past the
// ZIP's budget) is left out by itself and the rest of the ZIP is used; a ZIP
// the reader refuses (a path leaving it, a link, more than it may unpack to) is
// left out whole, with the reader's sentence.
//
// A picture over 1 MiB can never go in an email (emaildoc.MaxImageBytes), and
// pictures are neither shrunk nor converted, so one is left out too. The picture
// of the finished email is the exception: it is guidance the composer looks at,
// kept up to the per-file limit, and has a handle only when an email could
// carry it.
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
// calls). So the material is read in a render slot (acquireRenderSlot), the
// same few a preview or a save renders in, held by the tools for their whole
// call: a node holds no more material at once than it renders emails. Within a
// read, the spec's limits:
//
//   - a request names at most maxMaterialFiles files;
//   - the attached files are at most maxAttachedBytes together, as the New
//     email page attaches them. It is decided from what their Library rows say,
//     every row read before storage is asked for any file: a file that would
//     take them past it is left out with a reason, and the files after it are
//     still taken while they fit. What storage then gives back is held to the
//     same budget, so a row that says less than its file holds takes nothing
//     past it. The picture of the finished email is not one of them; it is read
//     up to the same per-file limit, being guidance;
//   - a picture whose Library row is larger than an email can carry is left out
//     before it is read;
//   - all that is kept, a ZIP's members unpacked, is at most maxMaterialBytes,
//     past which a file is left out with a reason.
//
// A picture is kept as its bytes. Its data URI is made only when a composition
// puts it in the email (material.images), never for every picture of every read.

const (
	// maxMaterialFiles is the most files a request names (the picture of the
	// finished email aside): what can be attached.
	maxMaterialFiles = 64

	// maxAttachedBytes is the most the attached files hold together, as their
	// Library rows say and as storage gives them back: what the New email page
	// lets a person attach.
	maxAttachedBytes = 16 << 20

	// maxMaterialFileBytes is the most of one file that is read: one attached
	// file is never more than all of them, and the picture of the finished
	// email is read up to the same.
	maxMaterialFileBytes = maxAttachedBytes

	// maxMaterialBytes is the most the files kept from one request hold
	// together: what one ZIP may unpack to.
	maxMaterialBytes = 64 << 20
)

var (
	errTooManyMaterialFiles = fmt.Errorf("An email is composed from at most %d attached files. Attach fewer.", maxMaterialFiles)
	errComposeBusy          = errors.New("Composing is busy right now. Try again in a moment.")

	// errNoUsableMaterial and errMaterialUnreadable are the reasons a failed
	// composition records for the two errors below (compositionReason); the
	// errors' own words, which name the files, are for the composer's tools.
	errNoUsableMaterial   = errors.New("None of the attached files can be used.")
	errMaterialUnreadable = errors.New("A file could not be read from your Library.")
)

// The reasons a file is left out that the archive reader does not give. The
// ones it does give (not a picture or text file, empty file and the rest) are
// written the same way below for a file that is not in a ZIP.
const (
	reasonNotInLibrary  = "it is not in your Library"
	reasonArchived      = "it is archived"
	reasonNoContents    = "its contents are not stored yet"
	reasonFileTooLarge  = "it is larger than 16 MiB"
	reasonAttachedFull  = "the attached files together are larger than 16 MiB"
	reasonMaterialFull  = "what the attached files unpack to is larger than 64 MiB"
	reasonPictureTooBig = "a picture over 1 MiB, more than an email can carry"
	reasonPixels        = "a picture over 24 million pixels"
	reasonTextTooLarge  = "more than 4 MiB of text"
	reasonNotAPicture   = "not a PNG, JPEG or GIF picture"
)

// unusableMaterialError is the answer when files were attached and none of them
// can be used: notes are what was left out and why.
type unusableMaterialError struct{ notes []string }

func (e *unusableMaterialError) Error() string {
	return errNoUsableMaterial.Error() + " " + strings.Join(e.notes, " ")
}

// unreadableMaterialError is a file of the material storage did not give back.
type unreadableMaterialError struct{ name string }

func (e *unreadableMaterialError) Error() string {
	return e.name + " could not be read from your Library. Try again in a moment, or attach it again."
}

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
	// ("memql-asset:<sha256>", compose.EmailAssetHandle) to the picture.
	Handles map[string]materialPicture
	// Skipped is every file left out, under its workspace name, with why.
	Skipped []compose.SkippedMember
	// Reference is the workspace name of the picture of the finished email,
	// or "".
	Reference string
}

// materialPicture is a picture of the material an email can carry, as its
// bytes: its data URI is made when a composition uses it (images).
type materialPicture struct {
	mime string
	data []byte
}

// images maps each handle c puts in the email to its picture's data URI, for
// emaildoc.Compose: only the pictures the email carries are encoded. A handle
// the material does not have is left out, and Compose refuses it by name.
func (m material) images(c emaildoc.Composition) map[string]string {
	images := map[string]string{}
	for _, item := range c.Content {
		if item.Image == nil {
			continue
		}
		handle := strings.TrimSpace(*item.Image)
		if _, done := images[handle]; done {
			continue
		}
		if p, ok := m.Handles[handle]; ok {
			images[handle] = "data:" + p.mime + ";base64," + base64.StdEncoding.EncodeToString(p.data)
		}
	}
	return images
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
// anything is read. Every file's Library row is read, and what is left out for
// what its row says decided (the attached files' budget among it), before
// storage is asked for any. What was left out is noted in the files' order.
// When files were attached and none of them can be used there is nothing to
// write from, and the answer is an error that names each one and why.
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

	r := &materialReader{w: w, m: material{Handles: map[string]materialPicture{}}, taken: map[string]bool{}}
	planned := make([]plannedFile, 0, len(ids))
	attached := 0 // what the rows of the attached files to be read say they hold
	for _, id := range ids {
		p, err := r.plan(ctx, id, id == reference)
		if err != nil {
			return material{}, err
		}
		p.attached = seen[id]
		if p.skip == "" && p.attached {
			if size := integer(p.row, "size"); attached+size > maxAttachedBytes {
				p.skip = reasonAttachedFull
			} else {
				attached += size
			}
		}
		planned = append(planned, p)
	}
	for _, p := range planned {
		if p.skip != "" {
			r.skip(p.name, p.skip)
			continue
		}
		if err := r.read(ctx, p); err != nil {
			return material{}, err
		}
	}
	if len(ids) > 0 && len(r.m.Files) == 0 {
		return material{}, &unusableMaterialError{notes: r.m.notes()}
	}
	return r.m, nil
}

// materialReader builds one request's material.
type materialReader struct {
	w        *Worker
	m        material
	taken    map[string]bool // workspace names, lower-cased
	held     int             // bytes kept so far
	attached int             // bytes storage gave back for the attached files
}

// plannedFile is one file of a request as its Library row says it is: what it
// is, and why it is left out (skip) when its row is enough to tell, before
// storage is asked for it.
type plannedFile struct {
	id, name  string
	row       map[string]any
	kind      int
	reference bool // the picture of the finished email
	attached  bool // one of the attached files, held to their budget
	skip      string
}

// plan reads one Library file's row under the caller, and says what is to be
// done with it. Only a failure to read the row, or a cancelled ctx, is an error.
func (r *materialReader) plan(ctx context.Context, fileID string, reference bool) (plannedFile, error) {
	rows, err := r.w.store.rows(ctx, call("query", "libraryFileById", arg{"fileId", fileID}))
	if err != nil {
		return plannedFile{}, err
	}
	if len(rows) == 0 {
		return plannedFile{id: fileID, name: fileID, reference: reference, skip: reasonNotInLibrary}, nil
	}
	row := rows[0]
	p := plannedFile{id: fileID, name: strings.TrimSpace(str(row, "name")), row: row, reference: reference}
	if p.name == "" {
		p.name = fileID
	}
	// What a file is decides by its name, as for a ZIP member, so a file that
	// cannot be used costs no read of storage.
	p.kind = materialKind(p.name, str(row, "mimeType"))
	switch {
	case boolean(row, "archived"):
		p.skip = reasonArchived
	case reference && p.kind != kindPicture:
		p.skip = reasonNotAPicture
	case p.kind == kindSVG:
		p.skip = "svg is not a picture an email can carry"
	case p.kind == kindOther:
		p.skip = "not a picture or text file"
	case strings.TrimSpace(str(row, "blobUrl")) == "":
		p.skip = reasonNoContents
	case integer(row, "size") > maxMaterialFileBytes:
		p.skip = reasonFileTooLarge
	case p.kind == kindPicture && !reference && integer(row, "size") > emaildoc.MaxImageBytes:
		// It could only be left out once read (keepPicture); the row says so first.
		p.skip = reasonPictureTooBig
	}
	return p, nil
}

// read reads one planned file from storage and keeps what can be used of it.
// Only a failure to read storage, or a cancelled ctx, is an error.
func (r *materialReader) read(ctx context.Context, p plannedFile) error {
	data, err := r.w.readBlobUpTo(ctx, str(p.row, "blobUrl"), str(p.row, "ownerUserId"), maxMaterialFileBytes)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if r.w.logger != nil {
			r.w.logger.Warn("campaigns: a composition's file could not be read", "file", p.id, "error", err)
		}
		return &unreadableMaterialError{name: p.name}
	}
	if p.attached {
		if r.attached+len(data) > maxAttachedBytes {
			r.skip(p.name, reasonAttachedFull)
			return nil
		}
		r.attached += len(data)
	}
	if len(data) == 0 {
		r.skip(p.name, "empty file")
		return nil
	}
	switch p.kind {
	case kindArchive:
		r.addArchive(p.name, data)
	case kindPicture:
		r.addPicture(p.name, data, p.reference)
	case kindText:
		r.addText(p.name, data)
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
		r.m.Handles[compose.EmailAssetHandle(data)] = materialPicture{mime: mime, data: data}
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
