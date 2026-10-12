package campaigns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// template.go -- saving, publishing, archiving and restoring an organization's
// email templates (the campaignSaveTemplate builtin).
//
// # The email is a document
//
// What a client sends as content is the email document: a layout-mode document
// (blocks and a theme) or a custom-mode one (HTML and its plain-text body). The
// server parses it with emaildoc.Parse and renders it as it will be sent, with
// emaildoc.Render in send mode, and stores what the render made (the subject,
// the plain-text part and the HTML part, which is what a send uses) next to what
// it was made from (the document, the layout it names, the preheader). A custom
// template that is saved as {subject, textBody, htmlBody}, as every template was
// before documents, is a custom document, and its HTML is stored as written.
//
// # Issues
//
// A render finds what keeps an email from being sent as it is: a picture to
// choose, a picture that cannot be read, a link without an address, no
// unsubscribe link, an email too large to send. Saving never refuses for them:
// a draft is a draft, and the answer lists them (issues) so an editor can show
// where. Publishing refuses while any remains, with their sentences, so a
// template that is ready is one that can go out. Their number is bounded the way
// a preview's is (previewIssues), since a document can make a hundred thousand.
// Only an email too large to be written out at all (past emaildoc.MaxRenderBytes)
// is refused when it is saved, with a sentence of its own.
//
// # Whose pictures
//
// A document names a picture as artifact:<id>, a Library file. The render reads
// it as the CALLER does, through artifactImages, and reads no more of them than
// a preview does (pictureReads): the artifact and its file are looked up under
// the caller's own actor, so a picture that belongs to somebody else, even a
// colleague in the same organization, is an image_unavailable issue and storage
// is not asked for it. The template keeps the document as it was written, with
// artifact: pictures, and its HTML with the pictures embedded; whoever publishes
// it embeds their own readable pictures, not another person's.
//
// # One template per organization and file
//
// With sourceArtifactId (the Library file the email is written in) the template
// already published from that file is the one saved, whatever templateId says,
// and the first save creates it under an id made from the organization and the
// file (sourceTemplateID), so two editors who both create it from the same file
// meet in the same row. The write gate is the file's. The template's own gate is
// taken as well once it is known, so that a save from the file and a save from
// the Campaigns app, which names only the template, cannot both pass the same
// revision.
//
// # Render first, lock after
//
// A render reads nothing a lock protects: only the document and the caller's own
// pictures. It can also take a long time (a slot to wait for, then up to nine
// reads of storage, each allowed ten seconds), and a lock is a transaction left
// open on the request pool, whose backends end a transaction that has been idle
// for a minute (a holder past that loses its lock to the next caller: a lost
// update) and cancel a statement that waits eight seconds for one. So a save
// renders BEFORE it takes its locks, and what runs under them is short: read the
// template fresh, compare, check the revision, write. The organization and the
// permission are looked at before the render, so somebody who may not save costs
// no render, and again under the locks, since the render took time. A lock that
// cannot be taken is a sentence for a person, not the database's words.
//
// # Revisions
//
// As for every shared write here: one cross-replica lock, the stored row read
// FRESH, the caller held to the exact revision they read (the row's createdAt),
// and a write stamped newer than it through a @serverOnly mutation, which the
// concept refuses every other writer for (executor_mutation.go).
//
// Archive and restore take the revision and nothing else: putting a template
// away is not an edit, and a client that holds only the card has no document to
// send. The content in the call is not even read, and the stored content is
// written back as it is. Publishing takes the revision AND the exact content the
// person reviewed, so the bytes that go live are the bytes they saw.

var templateIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,160}$`)

const (
	maxTemplateNameChars = 200

	// maxRefusalIssues is how many problems the refusal to publish spells out.
	// The answer to a save lists every issue (bounded as a preview's is); a
	// sentence is for a person to read, and fifty pictures that cannot be read
	// are one problem said fifty times.
	maxRefusalIssues = 10
)

var (
	errChooseTemplate     = errors.New("choose an organization and a template name (up to 200 characters)")
	errUnknownAction      = errors.New("unknown template action")
	errNotALibraryFile    = errors.New("The source file isn't a Library file. Choose a file from the Library, or leave the source empty.")
	errTemplateSaveBusy   = errors.New("Saving is busy right now. Try again in a moment.")
	errTemplateLocked     = errors.New("Someone else is saving this template. Try again in a moment.")
	errTemplateNeedsWrite = errors.New("template write permission is required in this organization")
	errOrganizationClosed = errors.New("organization is not active or accessible")
	errTemplateNotFound   = errors.New("template not found; create and save it before publishing")
	errTemplateChanged    = errors.New("this template changed; compare the latest revision before saving or publishing")
	errUnsavedTemplate    = errors.New("save and review this revision before publishing")
	errTemplateOrgMoved   = errors.New("a template cannot move between organizations; create a copy instead")
	errTemplateNoRevision = errors.New("template has no readable revision")
)

// templateStatus is the status each action leaves a template in: editing always
// returns it to draft, and so does restoring it.
var templateStatus = map[string]string{"save": "draft", "publish": "ready", "archive": "archived", "restore": "draft"}

// templateRequest is a call to campaignSaveTemplate once its arguments have been
// read: the ids are bare and the action is one of the four.
type templateRequest struct {
	templateID string // the target; empty until a file has said which template it is
	accountID  string
	sourceID   string // the Library file, or empty
	name       string
	expected   string // the revision the caller read, or empty when creating
	action     string
	document   emaildoc.Document // the parsed content; read only for save and publish
}

// templateContent is what a template holds and a write replaces: the rendering
// a send uses and the document it was made from.
type templateContent struct {
	name, subject, textBody, htmlBody, preheader, layoutID, sourceArtifactID string
	// document is the email document as a JSON object, or nil for a template
	// saved before documents existed, which is written back without one.
	document map[string]any
}

// handleSaveTemplate implements integration.campaigns.saveTemplate.
func (w *Worker) handleSaveTemplate(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	req := templateRequest{
		templateID: bare(argString(args, "templateId")),
		accountID:  bare(argString(args, "accountId")),
		sourceID:   bare(argString(args, "sourceArtifactId")),
		name:       strings.TrimSpace(argString(args, "name")),
		expected:   argString(args, "expectedRevision"),
		action:     argString(args, "action"),
	}
	if req.action == "" {
		req.action = "save"
	}
	// Only a save or a publish brings content. Archive and restore ignore
	// whatever else the call carries.
	writing := req.action == "save" || req.action == "publish"
	badName := req.name == "" || utf8.RuneCountInString(req.name) > maxTemplateNameChars || strings.IndexFunc(req.name, unicode.IsControl) >= 0
	switch {
	case req.templateID == "" && req.sourceID == "",
		req.templateID != "" && !templateIDPattern.MatchString(req.templateID),
		req.accountID == "", callerUserID(ctx) == "", writing && badName:
		return nil, errChooseTemplate
	}
	if req.sourceID != "" && !templateIDPattern.MatchString(req.sourceID) {
		return nil, errNotALibraryFile
	}
	if _, known := templateStatus[req.action]; !known {
		return nil, errUnknownAction
	}
	if writing {
		// Measured as the file it is, as a preview measures it: an email that is
		// previewed is not refused here as larger than a file may be.
		encoded, err := encodeDocument(args["content"])
		if err != nil {
			return nil, fmt.Errorf("template: %w", err)
		}
		if req.document, err = emaildoc.Parse(encoded); err != nil {
			return nil, fmt.Errorf("template: %w", err)
		}
	}

	if w.templateGate == nil {
		return nil, errors.New("template write coordination is unavailable")
	}

	// What takes time comes before any lock. The caller must be able to save in
	// the organization (so that somebody who may not costs no render), and then the
	// document is rendered as the caller reads pictures; archive and restore render
	// nothing.
	var rendered *renderedTemplate
	if writing {
		if err := w.requireOrganization(ctx, req.accountID); err != nil {
			return nil, err
		}
		if !w.mayWriteTemplates(ctx, req.accountID) {
			return nil, errTemplateNeedsWrite
		}
		var err error
		if rendered, err = w.renderTemplate(ctx, req); err != nil {
			return nil, err
		}
	}
	return w.saveUnderLocks(ctx, req, rendered)
}

// saveUnderLocks is what decides a save, and all of it is short: the locks, the
// organization and the template as they are now, the permission for what is to
// be done, the comparison with what is stored, the revision, the write.
// rendered is nil for archive and restore.
func (w *Worker) saveUnderLocks(ctx context.Context, req templateRequest, rendered *renderedTemplate) ([]memorynodes.MemoryNode, error) {
	gateKey := req.templateID
	if req.sourceID != "" {
		gateKey = "source:" + req.accountID + ":" + req.sourceID
	}
	release, err := w.lockTemplate(ctx, gateKey)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := w.requireOrganization(ctx, req.accountID); err != nil {
		return nil, err
	}

	if req.sourceID != "" {
		// The file says which template this is. Without one the template is the
		// one the file would get, unless the caller named an id for it.
		found, ok, err := w.store.TemplateForSource(ctx, req.accountID, req.sourceID)
		if err != nil {
			return nil, err
		}
		switch {
		case ok:
			req.templateID = bare(str(found, "id"))
		case req.templateID == "":
			req.templateID = sourceTemplateID(req.accountID, req.sourceID)
		}
		releaseTemplate, err := w.lockTemplate(ctx, req.templateID)
		if err != nil {
			return nil, err
		}
		defer releaseTemplate()
	}

	rows, err := w.store.rows(ctx, call("query", "templateById", arg{"templateId", req.templateID}))
	if err != nil {
		return nil, err
	}
	verb := auth.VerbCreate
	if len(rows) > 0 {
		verb = auth.VerbUpdate
	}
	if !w.store.engine.OrganizationCapable(ctx, req.accountID, verb, auth.ResourceData) {
		return nil, errTemplateNeedsWrite
	}
	if len(rows) == 0 {
		return w.createTemplate(ctx, req, rendered)
	}
	return w.changeTemplate(ctx, req, rows[0], rendered)
}

// lockTemplate takes one of the template gates. A gate that cannot be taken
// (the pool cancels a statement that has waited eight seconds for it, or the
// caller stops waiting) is a sentence that says what to do; the cause is logged.
func (w *Worker) lockTemplate(ctx context.Context, key string) (func(), error) {
	release, err := w.templateGate(ctx, key)
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("campaigns: a template gate could not be taken", "gate", key, "error", err)
		}
		return nil, errTemplateLocked
	}
	return release, nil
}

// requireOrganization says whether the organization is one the caller may use at
// all: it is active, and the engine shows it to them (it answers nothing about
// one they are not in).
func (w *Worker) requireOrganization(ctx context.Context, accountID string) error {
	accounts, err := w.store.rows(ctx, call("query", "clientAccountById", arg{"accountId", accountID}))
	if err != nil {
		return err
	}
	if len(accounts) != 1 || str(accounts[0], "status") != "active" {
		return errOrganizationClosed
	}
	return nil
}

// mayWriteTemplates is the check made before the render, when it is not known
// whether the template is new: somebody who can neither create nor update on the
// organization's data cannot save any template in it. Which of the two it is
// decides under the locks.
func (w *Worker) mayWriteTemplates(ctx context.Context, accountID string) bool {
	return w.store.engine.OrganizationCapable(ctx, accountID, auth.VerbCreate, auth.ResourceData) ||
		w.store.engine.OrganizationCapable(ctx, accountID, auth.VerbUpdate, auth.ResourceData)
}

// createTemplate saves a template nobody has saved yet, as a draft.
func (w *Worker) createTemplate(ctx context.Context, req templateRequest, rendered *renderedTemplate) ([]memorynodes.MemoryNode, error) {
	if req.expected != "" || req.action != "save" {
		return nil, errTemplateNotFound
	}
	revision := w.templateRevision(time.Time{})
	values := rendered.content.values(req.templateID, revision)
	values["accountId"] = req.accountID
	if err := w.writeTemplate(ctx, "createTemplate", values); err != nil {
		return nil, err
	}
	return templateReceipt(req.templateID, revision, "draft", rendered.issues)
}

// changeTemplate saves, publishes, archives or restores a template that is
// stored. row is the stored version, read fresh under the template's gate.
func (w *Worker) changeTemplate(ctx context.Context, req templateRequest, row map[string]any, rendered *renderedTemplate) ([]memorynodes.MemoryNode, error) {
	if !sameOrganization(req.accountID, str(row, "accountId")) {
		return nil, errTemplateOrgMoved
	}
	prior := templateTime(row["createdAt"])
	if prior.IsZero() {
		return nil, errTemplateNoRevision
	}
	current, target := str(row, "status"), templateStatus[req.action]

	var (
		content templateContent
		issues  = []emaildoc.Issue{}
		same    bool
	)
	switch req.action {
	case "archive", "restore":
		// An uncertain response can be retried without writing another version:
		// what was asked is already so, whatever revision the retry carries. A
		// template that is not archived is as restored as it can be.
		if req.action == "archive" && current == "archived" || req.action == "restore" && current != "archived" {
			return templateReceipt(req.templateID, prior, current, issues)
		}
		content = storedTemplate(row)
	default:
		content, issues = rendered.content, rendered.issues
		if req.action == "publish" && len(issues) > 0 {
			return nil, publishRefusal(issues)
		}
		// A call that names no file keeps the one the template has.
		if content.sourceArtifactID == "" {
			content.sourceArtifactID = bare(str(row, "sourceArtifactId"))
		}
		same = sameTemplate(row, content)
		// The same content already saved, or published, answers with the receipt
		// it already has. This also makes an autosave of unchanged bytes inert.
		if same && current == target {
			return templateReceipt(req.templateID, prior, target, issues)
		}
	}

	wanted, parseErr := time.Parse(time.RFC3339Nano, req.expected)
	if parseErr != nil || !prior.Equal(wanted) {
		return nil, errTemplateChanged
	}
	if req.action == "publish" && !same {
		return nil, errUnsavedTemplate
	}
	revision := w.templateRevision(prior)
	values := content.values(req.templateID, revision)
	values["status"] = target
	if err := w.writeTemplate(ctx, "updateTemplate", values); err != nil {
		return nil, err
	}
	return templateReceipt(req.templateID, revision, target, issues)
}

// renderedTemplate is a document rendered as it will be sent: what a template
// made from it holds, and the issues that keep it from being sent as it is
// (never nil).
type renderedTemplate struct {
	content templateContent
	issues  []emaildoc.Issue
}

// renderTemplate renders the document as it will be sent. It holds no lock, and
// it is slow when pictures are (see the header).
func (w *Worker) renderTemplate(ctx context.Context, req templateRequest) (*renderedTemplate, error) {
	// A render can cost a node hundreds of MiB (see email_render.go), so saving
	// takes one of the same slots a preview does, here, after the organization
	// has been looked at and the document is known to be admissible.
	releaseSlot, err := acquireRenderSlot(ctx)
	if err != nil {
		if errors.Is(err, errPreviewBusy) {
			err = errTemplateSaveBusy
		}
		return nil, err
	}
	defer releaseSlot()
	result, err := emaildoc.Render(ctx, req.document, &pictureReads{inner: artifactImages{w}, left: maxPictureReads}, emaildoc.RenderOptions{})
	if err != nil {
		if errors.Is(err, emaildoc.ErrTooLarge) {
			return nil, errPreviewTooLarge
		}
		return nil, err
	}
	document, err := documentObject(req.document)
	if err != nil {
		return nil, err
	}
	content := templateContent{
		name: req.name, subject: req.document.Subject, textBody: result.Text, htmlBody: result.HTML,
		preheader: req.document.Preheader, layoutID: req.document.Layout, sourceArtifactID: req.sourceID,
		document: document,
	}
	return &renderedTemplate{content: content, issues: previewIssues(result.Issues)}, nil
}

// storedTemplate is the content of a stored version, to write back as it is. Its
// source file is the one it has: an archive does not change where it came from.
func storedTemplate(row map[string]any) templateContent {
	content := templateContent{
		name: str(row, "name"), subject: str(row, "subject"), textBody: str(row, "textBody"), htmlBody: str(row, "htmlBody"),
		preheader: str(row, "preheader"), layoutID: str(row, "layoutId"), sourceArtifactID: bare(str(row, "sourceArtifactId")),
	}
	if document, ok := row["document"].(map[string]any); ok && len(document) > 0 {
		content.document = document
	}
	return content
}

// values is the arguments of the mutation that writes the content. All of them
// are always named, so a save that leaves out the layout or the preheader clears
// it instead of keeping the stale one. A template saved before documents
// existed has no document to write back.
func (c templateContent) values(templateID string, revision time.Time) map[string]any {
	values := map[string]any{
		"templateId": templateID, "name": c.name, "subject": c.subject, "textBody": c.textBody, "htmlBody": c.htmlBody,
		"preheader": c.preheader, "layoutId": c.layoutID, "sourceArtifactId": c.sourceArtifactID,
		"versionTime": revision.Format(time.RFC3339Nano),
	}
	if c.document != nil {
		values["document"] = c.document
	}
	return values
}

// sameTemplate reports whether saving the content would write what is already
// stored: the name, the three parts, the preheader, the layout, the file and the
// document. The documents are compared as the JSON they are, which sorts object
// keys and reads numbers the same whichever way they came (from a client, or
// back out of the database). The file counts, so that saving a template from
// another file moves it there instead of answering that nothing changed.
//
// A template saved before documents existed has no document. It is a custom
// template by construction, so its document is the one its own parts make:
// publishing it unchanged is the same content, as it was before documents, and
// the document is stored with the publication.
func sameTemplate(row map[string]any, c templateContent) bool {
	if c.name != str(row, "name") || c.subject != str(row, "subject") || c.textBody != str(row, "textBody") ||
		c.htmlBody != str(row, "htmlBody") || c.preheader != str(row, "preheader") || c.layoutID != str(row, "layoutId") {
		return false
	}
	if c.sourceArtifactID != bare(str(row, "sourceArtifactId")) {
		return false
	}
	stored := storedTemplate(row).document
	if stored == nil {
		var err error
		stored, err = documentObject(emaildoc.Document{
			Subject: str(row, "subject"), Preheader: str(row, "preheader"), TextBody: str(row, "textBody"), HTMLBody: str(row, "htmlBody"),
		})
		if err != nil {
			return false
		}
	}
	incoming, err := plainJSON(c.document)
	if err != nil {
		return false
	}
	have, err := plainJSON(stored)
	return err == nil && string(incoming) == string(have)
}

// writeTemplate runs one of the two @serverOnly mutations. The call is rendered
// with the parser's own renderer because the document is an object, which the
// package's call helper does not carry. The mutation name is a variable on
// purpose: Store.execServerOnly is what stamps the internal origin both need.
func (w *Worker) writeTemplate(ctx context.Context, mutation string, values map[string]any) error {
	rendered, err := langparser.RenderCall(mutation, values)
	if err != nil {
		return fmt.Errorf("template: %w", err)
	}
	return w.store.execServerOnly(ctx, "mutation "+rendered)
}

// templateRevision is the revision a write is stamped with: newer than the one
// it read, even if this replica's clock lags.
func (w *Worker) templateRevision(prior time.Time) time.Time {
	now := time.Now()
	if w.now != nil {
		now = w.now()
	}
	return memql.VersionTimeAfter(prior, now)
}

// publishRefusal is the refusal to publish a template that has issues: their
// sentences, so a person sees what to fix. It names the first few; the answer to
// the save they just made lists them all.
func publishRefusal(issues []emaildoc.Issue) error {
	shown := issues
	if len(shown) > maxRefusalIssues {
		shown = shown[:maxRefusalIssues]
	}
	messages := make([]string, 0, len(shown))
	for _, issue := range shown {
		messages = append(messages, issue.Message)
	}
	text := "fix these before publishing: " + strings.Join(messages, " ")
	if more := len(issues) - len(shown); more > 0 {
		text += fmt.Sprintf(" And %d more.", more)
	}
	return errors.New(text)
}

// sourceTemplateID is the id of the template a Library file gets in an
// organization when nobody named one: the same for every caller and replica, so
// that two first saves of one file are one template.
func sourceTemplateID(accountID, sourceArtifactID string) string {
	sum := sha256.Sum256([]byte("memql/campaigns/template-source\x00" + accountID + "\x00" + sourceArtifactID))
	return hex.EncodeToString(sum[:])[:32]
}

// TemplateForSource reads the template an organization published from a Library
// file, or reports false when there is none the caller can see. The concept's
// account tier answers a template outside the caller's organizations exactly as
// it answers one that does not exist, so the file id is not a way into other
// organizations' templates. The row's createdAt is its revision.
func (s *Store) TemplateForSource(ctx context.Context, accountID, sourceArtifactID string) (map[string]any, bool, error) {
	rows, err := s.rows(ctx, call("query", "templateForSource", arg{"accountId", accountID}, arg{"sourceArtifactId", sourceArtifactID}))
	if err != nil || len(rows) == 0 {
		return nil, false, err
	}
	return rows[0], true, nil
}

func templateTime(value any) time.Time {
	if instant, ok := value.(time.Time); ok {
		return instant
	}
	instant, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(value))
	return instant
}

// templateReceipt is the answer to a save: the template, its revision and
// status, and the issues of the email (an empty list when there are none, never
// null, since a client reads issues.length).
func templateReceipt(templateID string, revision time.Time, status string, issues []emaildoc.Issue) ([]memorynodes.MemoryNode, error) {
	if issues == nil {
		issues = []emaildoc.Issue{}
	}
	return resultNode("campaignTemplateSaved", map[string]any{
		"templateId": templateID, "revision": revision.UTC().Format(time.RFC3339Nano), "status": status, "saved": true, "issues": issues,
	})
}
