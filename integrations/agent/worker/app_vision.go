package worker

// app_vision.go -- the images of a vision call, landed in the session
// workspace and named in the prompt (issue memql#5523, design D11 of
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md).
//
// # Why a stager exists at all
//
// Every other vision provider takes bytes inline: a request carries the image
// and the model sees it. An app is not that. It is Claude Code or Codex,
// headless, on a machine somebody owns, and it reads images NATIVELY -- from
// files, in its own working directory. So "a vision call through the app door"
// is not a message with an image part; it is an image on that machine's disk
// and a prompt that names it.
//
// D11 said this costs no wire change, and it was right: AppSessionStart.inputs
// already carries Library artifact ids the cockpit pulls into the workspace
// before the run starts, and the landing FILENAME is the engine's to choose,
// because `GET /artifacts/{id}/content` sets Content-Disposition from the
// Library file row's own `name` and the cockpit's downloader reads it. What
// D11 did not price is everything below.
//
// # The lifecycle decision, and why it is its own source value
//
// `inputs` takes Library artifact ids and nothing else, so each image becomes
// a real Library file. Left at that, a person's Files app fills with the
// transient inputs of every vision turn, indistinguishable from the files they
// chose to keep. Owner decision: a staged input is a Library file with its OWN
// source value (`vision_input`) and is ARCHIVED when the session ends.
//
// Not `app_session`, which already exists and means something else -- a file
// the SESSION recorded reading or writing. A staged input is the opposite
// direction: something the engine wrote FOR the session to read, which nobody
// asked to keep. Two different facts must not share a spelling, because the
// archive sweep is keyed on one of them.
//
// # The promotion wait is real, and it is on the turn
//
// `indexFileOnCreate` promotes the file into the index off the
// `graph.node.created` event, asynchronously, so the artifact id is WAITED for
// rather than derived -- component/server/artifact_handler.go states why in as
// many words: deriving it would put a copy of a DSL expression in Go, and the
// copy would be the thing that is wrong the day the expression changes. This
// file makes the same trade for the same reason, which puts a bounded wait on
// the critical path of every vision turn. That is a cost, it is the cost the
// issue named, and it is the same wait every human upload already pays.
//
// # A stage that does not land REFUSES
//
// The one rule that is not a trade-off. A prompt naming a file that is not
// there does not fail -- the app answers about an image it never saw, fluently
// and wrongly, and nothing downstream can tell. So a partial stage is a
// refusal too: two of three landing is worse than none, because the answer
// looks complete.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"mime"
	"path"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// visionInputSource is the Library `source` value a staged image carries. It
// must match the enum member on v1:library:file AND on v1:library:artifact --
// the index's enum is the UNION of every backing row's, and a value the index
// refuses is a promotion that never happens, leaving the file staged and the
// artifact id unresolvable.
const visionInputSource = "vision_input"

// visionStagePromotionWait bounds how long a turn waits for
// indexFileOnCreate to produce the index row, and visionStagePromotionPoll is
// how often it asks.
//
// Shorter than the upload route's budget, deliberately. There, a timeout costs
// labels on a file whose bytes are already durable; here it costs the CALL, so
// waiting longer only makes the refusal slower. A vision turn that cannot
// start in a few seconds has already failed the caller.
const (
	visionStagePromotionWait = 6 * time.Second
	visionStagePromotionPoll = 150 * time.Millisecond
)

// visionBlobUploader is the minimal slice of the blob storage surface a stage
// needs. Declared here rather than imported from component/server so this
// package keeps its module boundary -- the same reason
// integrations/workbench declares its own.
type visionBlobUploader interface {
	Upload(ctx context.Context, bucket, objectName string, data []byte, contentType string) (blobUrl string, err error)
}

// visionEngine is the slice of the engine a stage uses: one Execute.
//
// AN INTERFACE so the stager's own behaviour is testable without a database --
// the promotion WAIT in particular, which is a loop whose interesting cases
// are "not there yet, then there" and "never there", neither of which a real
// engine can be made to produce on demand. The lifecycle those calls WRITE is
// pinned against a real store separately
// (component/memql/library_vision_input_5523_db_test.go); what a fake proves
// here is the control flow, and the two halves are only worth having together.
type visionEngine interface {
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
}

// stagedVisionInput is one landed image.
type stagedVisionInput struct {
	// ArtifactId is what rides AppSessionStart.inputs.
	ArtifactId string
	// FileId is the backing v1:library:file row, kept so a release can name
	// the pair even when the index row never appeared.
	FileId string
	// Name is the filename the cockpit will write it under, and the name the
	// prompt refers to. ONE literal, chosen here and used in both places: a
	// name invented for the prompt and a name written by the download would be
	// two strings describing one file.
	Name string
}

// visionStager lands images in a session workspace and archives them after.
type visionStager struct {
	engine   visionEngine
	uploader visionBlobUploader
	// container is the blob container Library bytes live in.
	container string
	logger    *slog.Logger
	clock     func() time.Time
	// wait and poll are the promotion budget, overridable in tests so a case
	// about the timeout does not take six seconds.
	wait time.Duration
	poll time.Duration
}

func newVisionStager(engine visionEngine, uploader visionBlobUploader, container string, logger *slog.Logger, clock func() time.Time) *visionStager {
	if logger == nil {
		logger = slog.Default()
	}
	if clock == nil {
		clock = time.Now
	}
	// A TYPED NIL assigned to the interface above is NOT nil, so a node that
	// resolved no engine would pass every `s.engine == nil` guard and panic
	// inside Execute instead of refusing by name. Normalised here, which is
	// the same reason newBunStore exists in component/memql.
	if e, ok := engine.(*memqlengine.MemQLEngine); ok && e == nil {
		engine = nil
	}
	return &visionStager{
		engine:    engine,
		uploader:  uploader,
		container: container,
		logger:    logger,
		clock:     clock,
		wait:      visionStagePromotionWait,
		poll:      visionStagePromotionPoll,
	}
}

// ready reports whether this replica can stage at all.
//
// Asked BEFORE a session is opened, so an unconfigured node refuses the call
// rather than opening a session it cannot feed. A node with no blob storage is
// a configuration fact, and the refusal names it.
func (s *visionStager) ready() (bool, string) {
	switch {
	case s == nil:
		return false, "this replica has no vision stager wired"
	case s.engine == nil:
		return false, "this replica has no engine to write a Library row through"
	case s.uploader == nil || strings.TrimSpace(s.container) == "":
		return false, "this replica has no blob storage configured, so an image has nowhere to land"
	}
	return true, ""
}

// Stage lands every image as a Library file owned by `owner` and returns them
// in the order given.
//
// ALL OR NOTHING. A partial result is returned alongside the error so the
// caller can release what did land, but it is never usable as a stage: the
// prompt would name files that are not there.
func (s *visionStager) Stage(ctx context.Context, owner, sessionId string, images []common.VisionContent) ([]stagedVisionInput, error) {
	if len(images) == 0 {
		return nil, nil
	}
	if ok, why := s.ready(); !ok {
		return nil, fmt.Errorf("%s", why)
	}
	if strings.TrimSpace(owner) == "" {
		// The Library is owner-tier. A row written under a blank actor is
		// readable by nobody, including the operator asking where the image
		// went -- so this refuses rather than writing one.
		return nil, fmt.Errorf("a vision call has no acting user, and a staged image has to belong to somebody")
	}

	// The OWNER's authority, borrowed. Every row here is theirs: the file, the
	// index row promotion produces, and the archive at the end. Internal
	// origin beside it, because createLibraryFile is @serverOnly.
	writeCtx := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, owner))

	staged := make([]stagedVisionInput, 0, len(images))
	for i, img := range images {
		one, err := s.stageOne(writeCtx, owner, sessionId, i, img)
		if err != nil {
			return staged, err
		}
		staged = append(staged, one)
	}
	return staged, nil
}

func (s *visionStager) stageOne(ctx context.Context, owner, sessionId string, index int, img common.VisionContent) (stagedVisionInput, error) {
	if len(img.Data) == 0 {
		return stagedVisionInput{}, fmt.Errorf("image %d carries no bytes", index+1)
	}
	digest := sha256.Sum256(img.Data)
	hexDigest := hex.EncodeToString(digest[:])

	// The FILE ID is derived from the session and the position, not from the
	// digest. Two identical images in one turn are two references the prompt
	// has to be able to tell apart, and a digest id would collapse them into
	// one file the second reference silently reuses.
	fileId := fmt.Sprintf("vision-%s-%02d", visionSessionSlug(sessionId), index+1)
	name := visionInputName(index, img.MimeType)
	blobPath := fmt.Sprintf("library/%s/%s/%s", owner, fileId, name)

	blobUrl, err := s.uploader.Upload(ctx, s.container, blobPath, img.Data, img.MimeType)
	if err != nil {
		return stagedVisionInput{}, fmt.Errorf("upload image %d: %w", index+1, err)
	}

	call := fmt.Sprintf(
		`mutation createLibraryFile(fileId:%s, name:%s, mimeType:%s, size:%d, sha256:%s, blobUrl:%s, source:%s, format:%s, summary:%s)`,
		langparser.QuoteString(fileId),
		langparser.QuoteString(name),
		langparser.QuoteString(visionMimeOrDefault(img.MimeType)),
		len(img.Data),
		langparser.QuoteString(hexDigest),
		langparser.QuoteString(blobUrl),
		langparser.QuoteString(visionInputSource),
		langparser.QuoteString("image"),
		langparser.QuoteString(fmt.Sprintf("Staged input of a vision call through the app door (session %s).", sessionId)),
	)
	if _, err := s.engine.Execute(ctx, call); err != nil {
		return stagedVisionInput{}, fmt.Errorf("record image %d as a Library file: %w", index+1, err)
	}

	artifactId, err := s.waitForPromotion(ctx, fileId)
	if err != nil {
		return stagedVisionInput{FileId: fileId, Name: name}, err
	}
	return stagedVisionInput{ArtifactId: artifactId, FileId: fileId, Name: name}, nil
}

// waitForPromotion polls libraryArtifactBySourceConceptRef until
// indexFileOnCreate has landed the index row.
//
// Unlike the upload route's version this returns an ERROR on timeout rather
// than an empty id, and the difference is the whole point: there, the bytes
// are durable and the artifact will appear, so failing the request would throw
// away a file over a scheduling detail. Here the artifact id IS the deliverable
// -- it is what `inputs` carries -- so an empty one cannot be passed on.
func (s *visionStager) waitForPromotion(ctx context.Context, fileId string) (string, error) {
	ref := "v1:library:file:" + fileId
	deadline := s.clock().Add(s.wait)
	for {
		res, err := s.engine.Execute(ctx, fmt.Sprintf(
			`query libraryArtifactBySourceConceptRef(sourceConceptRef:%s)`, langparser.QuoteString(ref)))
		if err != nil {
			return "", fmt.Errorf("resolve the Library index row for %s: %w", fileId, err)
		}
		for _, row := range memqlengine.MaterializeRows(res) {
			if id, ok := row["id"].(string); ok && strings.TrimSpace(id) != "" {
				return memqlengine.BareShortId(strings.TrimSpace(id)), nil
			}
		}
		if !s.clock().Add(s.poll).Before(deadline) {
			return "", fmt.Errorf("the Library index row for %s did not appear within %s, so there is "+
				"no artifact id to hand the session", fileId, s.wait)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(s.poll):
		}
	}
}

// Release archives every staged input, which is the lifecycle half of the
// owner's decision: the image was an INPUT to one turn, not something the
// person keeps.
//
// BOTH WRITES, EXPLICITLY, and that is the part worth reading twice.
// `archiveArtifact` alone is enough in a running cluster: the
// `archiveFileOnArtifactArchive` automation rides `node.updated` on
// v1:library:artifact and archives the backing file, which is the promise
// v1:library:file.archived's own declaration makes. But that promise is kept
// by an AUTOMATION -- an event, a subscriber and a second write -- while this
// function runs in a deferred call at the end of a turn with nothing left to
// check the result. If the event does not reach a subscriber the file stays
// visible, forever, in the Files app of somebody who never asked for it.
//
// So the release does the pair itself rather than depending on the automation
// having fired. The automation still fires and writes the same value; both
// writes are idempotent, and no cycle is closed by either -- this is Go
// calling two mutations, not a second automation subscribing to the first
// (which is the shape that automation's own header refuses).
//
// Best-effort, and never an error the caller acts on: the turn has already
// happened, and failing a completed vision call because a cleanup write did
// not land would throw away the answer. Logged instead, loudly enough to find.
func (s *visionStager) Release(ctx context.Context, owner string, staged []stagedVisionInput) {
	if s == nil || s.engine == nil || len(staged) == 0 || strings.TrimSpace(owner) == "" {
		return
	}
	writeCtx := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, owner))
	for _, in := range staged {
		// THE INDEX ROW, when there is one. A stage that timed out waiting for
		// promotion has a file and no artifact, and the file below is then the
		// only thing to archive.
		if id := strings.TrimSpace(in.ArtifactId); id != "" {
			if _, err := s.engine.Execute(writeCtx, fmt.Sprintf(
				`mutation archiveArtifact(artifactId:%s)`, langparser.QuoteString(id))); err != nil {
				s.logger.Warn("vision input: could not archive the staged artifact; it will stay visible in the owner's Files app",
					"artifactId", id, "fileId", in.FileId, "error", err)
			}
		}
		// AND THE FILE, always. See the note above: not a fallback for the
		// artifact write, a write this function makes rather than waits for.
		if id := strings.TrimSpace(in.FileId); id != "" {
			if _, err := s.engine.Execute(writeCtx, fmt.Sprintf(
				`mutation archiveLibraryFile(fileId:%s)`, langparser.QuoteString(id))); err != nil {
				s.logger.Warn("vision input: could not archive the staged file",
					"fileId", id, "artifactId", in.ArtifactId, "error", err)
			}
		}
	}
}

// visionPromptWithInputs returns the prompt the app receives: the caller's
// text, followed by the files that actually landed, by name.
//
// THE NAMES COME FROM THE STAGE, never from a second derivation. That is the
// invariant this function exists to hold: the cockpit writes each input under
// the name Content-Disposition carries, which is the Library row's `name`,
// which is the name chosen in stageOne. A prompt composed from a separately
// derived filename would be correct until one of the two changed.
func visionPromptWithInputs(prompt string, staged []stagedVisionInput) string {
	if len(staged) == 0 {
		return prompt
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(prompt, "\n"))
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	if len(staged) == 1 {
		b.WriteString("The image is in your working directory as `" + staged[0].Name + "`. Read it to answer.")
		return b.String()
	}
	b.WriteString("The images are in your working directory, in order:\n")
	for i, in := range staged {
		fmt.Fprintf(&b, "%d. `%s`\n", i+1, in.Name)
	}
	b.WriteString("Read them to answer.")
	return b.String()
}

// visionInputExtensions is the extension each image type lands under.
//
// AN EXPLICIT TABLE, and the reason is a measurement rather than a preference.
// mime.ExtensionsByType("image/jpeg") answers with four spellings --
// ".jfif", ".jpe", ".jpeg", ".jpg" -- in no defined order, and the first
// sorted one is ".jfif". That is a legitimate JPEG extension almost nothing
// recognises: a harness decides how to read a file partly from its extension,
// so a jpeg landing as `.jfif` is an image the app quietly does not treat as
// one, and the turn comes back as though the picture said nothing.
//
// So the four types a vision call actually carries are named here, and the
// mime table is the fallback for anything else.
var visionInputExtensions = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// visionInputName is the filename one staged image lands under.
//
// Numbered from the position in the call, so a multi-image prompt can refer to
// "the first image" and mean a file the app can open.
func visionInputName(index int, mimeType string) string {
	m := visionMimeOrDefault(mimeType)
	ext, ok := visionInputExtensions[m]
	if !ok {
		if exts, err := mime.ExtensionsByType(m); err == nil && len(exts) > 0 {
			// ExtensionsByType is unordered; sorted-first keeps one MIME type
			// producing one extension across processes, which matters because
			// the name is written into a row.
			best := exts[0]
			for _, e := range exts {
				if e < best {
					best = e
				}
			}
			ext = best
		}
	}
	// A path separator or a traversal segment would escape the workspace on
	// the cockpit side. It cannot come out of the table, and it is checked
	// because the value is written into a filename on somebody else's machine.
	ext = path.Base(ext)
	if !strings.HasPrefix(ext, ".") || strings.ContainsAny(ext, `/\`) || ext == "." || ext == ".." {
		ext = ".png"
	}
	return fmt.Sprintf("vision-input-%02d%s", index+1, ext)
}

// visionMimeOrDefault answers a blank or unparseable MIME type with
// image/png.
//
// A vision call whose image carries no content type is a caller that did not
// say, not a caller that means "unknown bytes": the argument is typed as an
// image at every level above. Guessing png is what makes the extension and the
// stored mimeType agree, which is what a harness reads.
func visionMimeOrDefault(mimeType string) string {
	m := strings.TrimSpace(strings.ToLower(mimeType))
	if m == "" {
		return "image/png"
	}
	if parsed, _, err := mime.ParseMediaType(m); err == nil && strings.HasPrefix(parsed, "image/") {
		return parsed
	}
	if strings.HasPrefix(m, "image/") {
		return m
	}
	return "image/png"
}

// visionSessionSlug reduces a session id to the part that makes a file id
// legible and unique. The session id is `v1:worker:appSession:<shortId>`; the
// short id alone is what belongs in a filename-shaped key.
func visionSessionSlug(sessionId string) string {
	s := strings.TrimSpace(sessionId)
	if s == "" {
		return "unknown"
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		// The tail after the LAST colon, even when it is empty. Falling back
		// to the whole string on a trailing colon would slug the PREFIX --
		// `v1:worker:appSession:` became "v1workerappSession", which is the
		// same key for every such id, so two sessions would stage over each
		// other's files.
		s = s[i+1:]
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// appendStagedInputs adds each staged artifact id to the inputs the caller
// already named, keeping the caller's first.
//
// A COPY, not an append onto the caller's slice. req.Inputs belongs to the
// request the provider built and may be shared with the decision record; a
// mutating append would write a staged input into whatever else held that
// backing array.
func appendStagedInputs(inputs []string, staged []stagedVisionInput) []string {
	if len(staged) == 0 {
		return inputs
	}
	out := make([]string, 0, len(inputs)+len(staged))
	out = append(out, inputs...)
	for _, in := range staged {
		if strings.TrimSpace(in.ArtifactId) != "" {
			out = append(out, in.ArtifactId)
		}
	}
	return out
}
