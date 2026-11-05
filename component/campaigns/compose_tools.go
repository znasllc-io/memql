package campaigns

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/compose"
)

// compose_tools.go -- the three tools of an email composition run, as the
// builtins campaignComposeContext, campaignComposeRender and
// campaignComposeSubmit (the emailCompose* tools of dsl/campaigns/tools.memql).
//
// A composition runs whole in Claude Code. The session never sends a document:
// it reads the context, writes a COMPOSITION (subject, preheader, what to write
// in the content blocks, which optional blocks to drop and how often to repeat
// the repeatable ones), renders it to read the issues, and submits it. The
// server rebuilds the email each time from the raw built-in layout with the
// organization's current brand kit applied (the neutral layout when it has
// none) and the material's pictures resolved from their handles
// (emaildoc.Compose), so the brand's pictures never pass through the model and
// the structure check is Compose's own.
//
// # Which template, and who may
//
// Every tool takes the template from templateId and the run from goalId, both
// of which the tool declares @autoInjected: the run stamps them, and the
// central validator drops a value the model supplies. The template is read
// under the caller, and the caller must be able to use its organization
// (requireOrganization) and write templates in it (mayWriteTemplates). Then the
// template must carry a composition that is writing, and the call's goal must
// be that composition's: a session that was cancelled, and is still talking
// after the composition was retried under a new goal, acts on nothing. The
// row's ownerUserId is NOT consulted: every save restamps it as the last
// writer, so a colleague's save would otherwise lock the composer out.
//
// # What a call may cost
//
// A call holds one render slot (acquireComposeSlot) from the moment it reads
// the material until it has rendered: the material and the render are the
// heavy part, and the slots are the ones previews and saves render in, so a node
// holds no more of them at once than it renders emails. Submit gives its slot
// back before it takes the template's lock.
//
// # Submit
//
// The composed email and the finished composition (done, with its notes) are
// written in ONE row version under the template's lock, as a draft: two writes
// would show a revision between them. The template is held to the revision the
// composition started from (its baseRevision). When somebody changed it
// meanwhile, nothing of theirs is overwritten: the email is saved as a new
// template beside it, "<name> (MemQL's version)", and the original's
// composition is failed with a sentence that says where the email went. A run
// never publishes.

const (
	compositionWriting = "writing"
	compositionDone    = "done"
	compositionFailed  = "failed"

	// conflictCopySuffix ends the name of the template a submit saves beside
	// one that changed while it was written.
	conflictCopySuffix = " (MemQL's version)"
)

var (
	errCompositionNotWriting  = errors.New("This composition is no longer running.")
	errComposeNoTemplate      = errors.New("These tools work only inside an email composition.")
	errComposeTemplateMissing = errors.New("The template this composition writes can't be found.")
	errNotComposing           = errors.New("MemQL is not writing this template.")
	errCompositionShape       = errors.New("Send the composition as an object: {subject, preheader, content, drop, repeat}.")
)

// composingTemplate is the template a tool call works on, as it was read when
// the call began.
type composingTemplate struct {
	id, accountID, name string
	composition         composition
}

// composingTemplate reads the template a call names and checks that the caller
// may compose it, and that this call's run is the composition that is writing:
// see the header.
func (w *Worker) composingTemplate(ctx context.Context, args map[string]any) (composingTemplate, error) {
	id := bare(strings.TrimSpace(argString(args, "templateId")))
	goalID := strings.TrimSpace(argString(args, "goalId"))
	if id == "" || !templateIDPattern.MatchString(id) || goalID == "" || callerUserID(ctx) == "" {
		return composingTemplate{}, errComposeNoTemplate
	}
	rows, err := w.store.rows(ctx, call("query", "templateById", arg{"templateId", id}))
	if err != nil {
		return composingTemplate{}, err
	}
	if len(rows) == 0 {
		return composingTemplate{}, errComposeTemplateMissing
	}
	accountID := bare(str(rows[0], "accountId"))
	if accountID == "" {
		return composingTemplate{}, errComposeTemplateMissing
	}
	if err := w.requireOrganization(ctx, accountID); err != nil {
		return composingTemplate{}, err
	}
	if !w.mayWriteTemplates(ctx, accountID) {
		return composingTemplate{}, errTemplateNeedsWrite
	}
	c, ok := compositionFromRow(rows[0])
	if !ok {
		return composingTemplate{}, errNotComposing
	}
	// Finished, cancelled, or started again under another goal since this
	// session's run began.
	if c.State != compositionWriting || !sameGoal(c.GoalID, goalID) {
		return composingTemplate{}, errCompositionNotWriting
	}
	return composingTemplate{id: id, accountID: accountID, name: str(rows[0], "name"), composition: c}, nil
}

// sameGoal reports whether a composition's goal and a call's are one goal,
// whichever of the two is written bare.
func sameGoal(composed, called string) bool {
	return composed != "" && bare(composed) == bare(called)
}

// handleComposeContext implements integration.campaigns.composeContext.
func (w *Worker) handleComposeContext(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	t, err := w.composingTemplate(ctx, args)
	if err != nil {
		return nil, err
	}
	release, err := acquireComposeSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	m, err := w.readMaterial(ctx, t.composition.Request)
	if err != nil {
		return nil, err
	}
	answer, err := w.composeContext(ctx, t, m)
	if err != nil {
		return nil, err
	}
	return resultNode("campaignComposeContext", answer)
}

// handleComposeRender implements integration.campaigns.composeRender: the
// composition in its layout, rendered as a preview. It saves nothing.
func (w *Worker) handleComposeRender(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	t, err := w.composingTemplate(ctx, args)
	if err != nil {
		return nil, err
	}
	c, err := compositionArg(args["composition"])
	if err != nil {
		return nil, err
	}
	release, err := acquireComposeSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	m, err := w.readMaterial(ctx, t.composition.Request)
	if err != nil {
		return nil, err
	}
	doc, slots, err := w.composeDocument(ctx, t, c, m)
	if err != nil {
		return nil, err
	}
	// In the slot the material was read in. The composed email holds data:
	// pictures only (the kit's and the material's), so the render reads nothing
	// from the Library.
	result, err := emaildoc.Render(ctx, doc, nil, emaildoc.RenderOptions{Preview: true})
	if err != nil {
		if errors.Is(err, emaildoc.ErrTooLarge) {
			return nil, errPreviewTooLarge
		}
		return nil, err
	}
	return resultNode("campaignComposeRendered", map[string]any{
		"subject": result.Subject, "html": withHandles(result.HTML), "text": result.Text,
		"issues": previewIssues(result.Issues), "slots": slots,
	})
}

// handleComposeSubmit implements integration.campaigns.composeSubmit: the
// composition saved as the template's draft, and the composition done. It never
// publishes.
func (w *Worker) handleComposeSubmit(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	t, err := w.composingTemplate(ctx, args)
	if err != nil {
		return nil, err
	}
	if w.templateGate == nil {
		return nil, errTemplateGateUnavailable
	}
	c, err := compositionArg(args["composition"])
	if err != nil {
		return nil, err
	}
	rendered, notes, err := w.renderComposed(ctx, t, c)
	if err != nil {
		return nil, err
	}
	templateID, revision, err := w.saveComposed(ctx, t, rendered, notes)
	if err != nil {
		return nil, err
	}
	return resultNode("campaignComposeSubmitted", map[string]any{
		"templateId": templateID, "revision": revision.UTC().Format(time.RFC3339Nano), "issues": rendered.issues,
	})
}

// renderComposed reads the material, composes the email and renders it as any
// template is saved, in ONE render slot that it gives back before the template
// is locked (template.go, "Render first, lock after"). It answers the rendering
// and the notes on what was left out; the material itself is not kept.
func (w *Worker) renderComposed(ctx context.Context, t composingTemplate, c emaildoc.Composition) (*renderedTemplate, []string, error) {
	release, err := acquireComposeSlot(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	m, err := w.readMaterial(ctx, t.composition.Request)
	if err != nil {
		return nil, nil, err
	}
	doc, _, err := w.composeDocument(ctx, t, c, m)
	if err != nil {
		return nil, nil, err
	}
	rendered, err := w.renderInSlot(ctx, templateRequest{templateID: t.id, accountID: t.accountID, name: t.name, action: "save", document: doc})
	if err != nil {
		return nil, nil, err
	}
	return rendered, m.notes(), nil
}

// saveComposed writes the composed email over the template and the composition
// done, in one row version under the template's lock, and answers the template
// that holds the email and its revision. When the template changed since the
// composition began, the email goes to a copy instead (saveComposedCopy).
func (w *Worker) saveComposed(ctx context.Context, t composingTemplate, rendered *renderedTemplate, notes []string) (string, time.Time, error) {
	release, err := w.lockTemplate(ctx, t.id)
	if err != nil {
		return "", time.Time{}, err
	}
	defer release()
	// Looked at again: the material and the render took time.
	if err := w.requireOrganization(ctx, t.accountID); err != nil {
		return "", time.Time{}, err
	}
	rows, err := w.store.rows(ctx, call("query", "templateById", arg{"templateId", t.id}))
	if err != nil {
		return "", time.Time{}, err
	}
	if len(rows) == 0 {
		return "", time.Time{}, errComposeTemplateMissing
	}
	row := rows[0]
	if !sameOrganization(t.accountID, str(row, "accountId")) {
		return "", time.Time{}, errTemplateOrgMoved
	}
	if !w.store.engine.OrganizationCapable(ctx, t.accountID, auth.VerbUpdate, auth.ResourceData) {
		return "", time.Time{}, errTemplateNeedsWrite
	}
	// Cancelled, finished or started again since the call began: this run's
	// email is not wanted.
	stored, ok := compositionFromRow(row)
	if !ok || stored.State != compositionWriting || !sameRun(stored, t.composition) {
		return "", time.Time{}, errCompositionNotWriting
	}
	prior := templateTime(row["createdAt"])
	if prior.IsZero() {
		return "", time.Time{}, errTemplateNoRevision
	}
	finished := w.nowUTC()
	if !prior.Equal(stored.BaseRevision) {
		return w.saveComposedCopy(ctx, t, row, stored, rendered, notes, finished)
	}

	done := stored
	done.State, done.Error, done.FinishedAt, done.Notes = compositionDone, "", finished, notes
	object, err := done.object()
	if err != nil {
		return "", time.Time{}, err
	}
	content := rendered.content
	content.name = str(row, "name")
	if content.sourceArtifactID == "" {
		content.sourceArtifactID = bare(str(row, "sourceArtifactId"))
	}
	content.composition = object
	revision := w.templateRevision(prior)
	values := content.values(t.id, revision)
	values["status"] = templateStatus["save"] // a draft, whatever its issues
	if err := w.writeTemplate(ctx, "updateTemplate", values); err != nil {
		return "", time.Time{}, err
	}
	return t.id, revision, nil
}

// saveComposedCopy saves the email as a new draft template beside one that
// changed while it was written, and fails the original's composition with
// where the email went. It runs under the original's lock, read as row.
func (w *Worker) saveComposedCopy(ctx context.Context, t composingTemplate, row map[string]any, stored composition, rendered *renderedTemplate, notes []string, finished time.Time) (string, time.Time, error) {
	name := conflictCopyName(str(row, "name"))
	copyID, err := newTemplateID()
	if err != nil {
		return "", time.Time{}, err
	}
	release, err := w.lockTemplate(ctx, copyID)
	if err != nil {
		return "", time.Time{}, err
	}
	defer release()
	if !w.store.engine.OrganizationCapable(ctx, t.accountID, auth.VerbCreate, auth.ResourceData) {
		return "", time.Time{}, errTemplateNeedsWrite
	}

	// The copy is MemQL's version, finished: what it was asked and what was
	// left out. It started from no revision of its own.
	done := stored
	done.State, done.Error, done.FinishedAt, done.Notes, done.BaseRevision = compositionDone, "", finished, notes, time.Time{}
	object, err := done.object()
	if err != nil {
		return "", time.Time{}, err
	}
	copied := renderedTemplate{content: rendered.content, issues: rendered.issues}
	// A copy is no Library file's template: one file has one template.
	copied.content.name, copied.content.sourceArtifactID, copied.content.composition = name, "", object
	revision, err := w.insertTemplate(ctx, templateRequest{templateID: copyID, accountID: t.accountID, name: name, action: "save"}, &copied)
	if err != nil {
		return "", time.Time{}, err
	}

	failed := stored
	failed.State, failed.FinishedAt, failed.Notes = compositionFailed, finished, notes
	failed.Error = "The template changed, so MemQL saved a copy."
	if _, err := w.rewriteComposition(ctx, t.id, row, failed); err != nil {
		return "", time.Time{}, err
	}
	return copyID, revision, nil
}

// sameRun reports whether two readings of a template's composition are the same
// run: a retry starts another goal at another time.
func sameRun(a, b composition) bool {
	return a.GoalID == b.GoalID && a.StartedAt.Equal(b.StartedAt)
}

// conflictCopyName is the name of the template a submit saves beside one that
// changed: "<name> (MemQL's version)", the name cut so that the whole is at
// most maxTemplateNameChars runes.
func conflictCopyName(name string) string {
	room := maxTemplateNameChars - utf8.RuneCountInString(conflictCopySuffix)
	if utf8.RuneCountInString(name) > room {
		name = strings.TrimRightFunc(string([]rune(name)[:room]), unicode.IsSpace)
	}
	return name + conflictCopySuffix
}

// compositionKeys are the fields of a composition (emaildoc.Composition).
var compositionKeys = []string{"subject", "preheader", "content", "drop", "repeat"}

// compositionArg reads the composition a tool call carries. A field it does not
// have is refused by name rather than ignored: a draft that sends a whole
// document would otherwise save its subject and nothing else.
func compositionArg(raw any) (emaildoc.Composition, error) {
	if text, ok := raw.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(text), &decoded) != nil {
			return emaildoc.Composition{}, errCompositionShape
		}
		raw = decoded
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return emaildoc.Composition{}, errCompositionShape
	}
	var unknown []string
	for key := range object {
		if !slices.Contains(compositionKeys, key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return emaildoc.Composition{}, fmt.Errorf("A composition has no %s. Send only subject, preheader, content, drop and repeat, never a whole document.", strings.Join(unknown, ", "))
	}
	encoded, err := plainJSON(object)
	if err != nil {
		return emaildoc.Composition{}, err
	}
	var c emaildoc.Composition
	if err := json.Unmarshal(encoded, &c); err != nil {
		return emaildoc.Composition{}, fmt.Errorf("The composition could not be read: %v.", err)
	}
	return c, nil
}

// brandedLayout is the raw built-in layout a composition names with the
// organization's current brand kit applied, or the neutral layout when it has
// none, with its catalogue entry and whether a kit was applied. Never the
// template's stored document: that is the layout at its minimum, and composing
// over it would repeat its copies again.
func (w *Worker) brandedLayout(ctx context.Context, accountID, layoutID string) (emaildoc.Document, emaildoc.LayoutInfo, bool, error) {
	info, known := emaildoc.LayoutMeta(layoutID)
	layout, built := emaildoc.Layout(layoutID)
	if !known || !built {
		return emaildoc.Document{}, emaildoc.LayoutInfo{}, false, errUnknownLayout
	}
	kit, _, hasKit, err := w.brandKitFor(ctx, accountID)
	if err != nil {
		return emaildoc.Document{}, emaildoc.LayoutInfo{}, false, err
	}
	if hasKit {
		if layout, err = emaildoc.ApplyBrand(layout, kit); err != nil {
			return emaildoc.Document{}, emaildoc.LayoutInfo{}, false, err
		}
	}
	return layout, info, hasKit, nil
}

// composeDocument is the email a composition makes, and the content slots it
// writes to once its drops and repeats are applied. A composition the layout
// refuses is Compose's sentence.
func (w *Worker) composeDocument(ctx context.Context, t composingTemplate, c emaildoc.Composition, m material) (emaildoc.Document, []emaildoc.Slot, error) {
	layout, info, _, err := w.brandedLayout(ctx, t.accountID, t.composition.Request.LayoutID)
	if err != nil {
		return emaildoc.Document{}, nil, err
	}
	doc, err := emaildoc.Compose(layout, info, c, m.images(c))
	if err != nil {
		return emaildoc.Document{}, nil, err
	}
	slots, err := emaildoc.ComposeSlots(layout, info, c.Drop, c.Repeat)
	if err != nil {
		return emaildoc.Document{}, nil, err
	}
	if slots == nil {
		slots = []emaildoc.Slot{}
	}
	return doc, slots, nil
}

// outlineBlock is a block of the layout as the composer is shown it: what it
// is, whose it is (brand or content, from its container when it says neither),
// how large it is drawn and the example words it holds. A picture's source is
// never shown.
type outlineBlock struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Role     string          `json:"role"`
	Size     float64         `json:"size,omitempty"`
	Width    int             `json:"width,omitempty"`
	Height   float64         `json:"height,omitempty"`
	Text     string          `json:"text,omitempty"`
	Label    string          `json:"label,omitempty"`
	Alt      string          `json:"alt,omitempty"`
	Href     string          `json:"href,omitempty"`
	Optional bool            `json:"optional,omitempty"`
	Repeat   *repeatRange    `json:"repeat,omitempty"`
	Blocks   []outlineBlock  `json:"blocks,omitempty"`
	Columns  []outlineColumn `json:"columns,omitempty"`
}

type outlineColumn struct {
	Width  float64        `json:"width"`
	Blocks []outlineBlock `json:"blocks"`
}

type repeatRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

func layoutOutlineBlocks(blocks []emaildoc.Block, container string, info emaildoc.LayoutInfo) []outlineBlock {
	out := make([]outlineBlock, 0, len(blocks))
	for _, b := range blocks {
		role := b.Role
		if role == "" {
			role = container
		}
		o := outlineBlock{ID: b.ID, Type: b.Type, Role: role, Size: b.Size, Width: b.Width, Height: b.Height,
			Text: b.Text, Label: b.Label, Alt: b.Alt, Href: b.Href, Optional: slices.Contains(info.Optional, b.ID)}
		if r, ok := info.Repeat[b.ID]; ok {
			o.Repeat = &repeatRange{Min: r.Min, Max: r.Max}
		}
		if len(b.Blocks) > 0 {
			o.Blocks = layoutOutlineBlocks(b.Blocks, role, info)
		}
		for _, column := range b.Columns {
			o.Columns = append(o.Columns, outlineColumn{Width: column.Width, Blocks: layoutOutlineBlocks(column.Blocks, role, info)})
		}
		out = append(out, o)
	}
	return out
}

// contextPicture is a picture of the material as the composer is shown it.
// Handle is empty for the picture of the finished email when an email cannot
// carry it.
type contextPicture struct {
	Handle    string `json:"handle"`
	File      string `json:"file"`
	MIME      string `json:"mime"`
	Bytes     int    `json:"bytes"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Reference bool   `json:"reference"`
}

type contextFile struct {
	File  string `json:"file"`
	MIME  string `json:"mime"`
	Bytes int    `json:"bytes"`
}

// composeContext is what a composition starts from: the template and its
// organization, the request's description, the layout (an outline of its
// blocks, the optional ids, the repeat ranges and the content slots at their
// minimum), the brand slots the email keeps (the ones the kit fills; with no
// kit, brandKit is false and they are the neutral layout's), the material
// (every file by its workspace name, every picture with its handle, the picture
// of the finished email, what was left out) and the picture budget the brand
// leaves. It holds no picture's bytes. A composition run hands it to its
// composer as it starts.
func (w *Worker) composeContext(ctx context.Context, t composingTemplate, m material) (map[string]any, error) {
	layout, info, hasKit, err := w.brandedLayout(ctx, t.accountID, t.composition.Request.LayoutID)
	if err != nil {
		return nil, err
	}
	slots, err := emaildoc.ComposeSlots(layout, info, nil, nil)
	if err != nil {
		return nil, err
	}
	if slots == nil {
		slots = []emaildoc.Slot{}
	}
	accountName, err := w.organizationName(ctx, t.accountID)
	if err != nil {
		return nil, err
	}

	repeat := map[string]repeatRange{}
	for id, r := range info.Repeat {
		repeat[id] = repeatRange{Min: r.Min, Max: r.Max}
	}
	brandSlots := []string{}
	for _, slot := range emaildoc.BrandSlots() {
		if hasLayoutBlock(layout.Blocks, slot) {
			brandSlots = append(brandSlots, slot)
		}
	}
	files, pictures := []contextFile{}, []contextPicture{}
	for _, f := range m.Files {
		files = append(files, contextFile{File: f.Workspace, MIME: f.MIME, Bytes: len(f.Data)})
		if !strings.HasPrefix(f.MIME, "image/") {
			continue
		}
		picture := contextPicture{File: f.Workspace, MIME: f.MIME, Bytes: len(f.Data), Reference: f.Workspace == m.Reference}
		if config, _, err := image.DecodeConfig(bytes.NewReader(f.Data)); err == nil {
			picture.Width, picture.Height = config.Width, config.Height
		}
		if handle := compose.EmailAssetHandle(f.Data); m.Handles[handle].mime != "" {
			picture.Handle = handle
		}
		pictures = append(pictures, picture)
	}
	brandPictures, brandBytes := brandPictureShare(layout)

	return map[string]any{
		"templateId": t.id, "name": t.name, "accountName": accountName,
		"description": t.composition.Request.Description,
		"layout": map[string]any{
			"id": info.ID, "name": info.Name, "description": info.Description,
			"subject": layout.Subject, "preheader": layout.Preheader,
			"blocks": layoutOutlineBlocks(layout.Blocks, "content", info),
		},
		"optional": orEmpty(info.Optional), "repeat": repeat, "slots": slots,
		"brandKit": hasKit, "brandSlots": brandSlots,
		"pictures": pictures, "files": files, "reference": m.Reference, "leftOut": m.notes(),
		"budget": map[string]any{
			"maxPictures": emaildoc.MaxImages, "maxBytes": emaildoc.MaxImageBytes,
			"brandPictures": brandPictures, "brandBytes": brandBytes,
			"pictures": max(0, emaildoc.MaxImages-brandPictures), "bytes": max(0, emaildoc.MaxImageBytes-brandBytes),
		},
	}, nil
}

// organizationName is the organization's own name, or "".
func (w *Worker) organizationName(ctx context.Context, accountID string) (string, error) {
	rows, err := w.store.rows(ctx, call("query", "clientAccountById", arg{"accountId", accountID}))
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return strings.TrimSpace(str(rows[0], "name")), nil
}

// eachLayoutBlock visits every block of blocks, depth first, columns included.
func eachLayoutBlock(blocks []emaildoc.Block, visit func(b emaildoc.Block)) {
	for _, b := range blocks {
		visit(b)
		eachLayoutBlock(b.Blocks, visit)
		for _, column := range b.Columns {
			eachLayoutBlock(column.Blocks, visit)
		}
	}
}

func hasLayoutBlock(blocks []emaildoc.Block, id string) bool {
	found := false
	eachLayoutBlock(blocks, func(b emaildoc.Block) { found = found || b.ID == id })
	return found
}

// brandPictureShare is how many distinct pictures a brand-applied layout
// already carries, and their bytes: every picture in it is the kit's, since a
// layout's own are empty placeholders. They count against the email's budget
// the way a render counts them, once each however often they are used.
func brandPictureShare(layout emaildoc.Document) (count, size int) {
	seen := map[string]bool{}
	add := func(src string) {
		src = strings.TrimSpace(src)
		_, payload, ok := strings.Cut(src, ";base64,")
		if !ok || !strings.HasPrefix(src, "data:image/") || seen[src] {
			return
		}
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return
		}
		seen[src] = true
		count++
		size += len(data)
	}
	eachLayoutBlock(layout.Blocks, func(b emaildoc.Block) {
		if b.Type == "image" {
			add(b.Src)
		}
		for _, link := range b.Links {
			add(link.Icon)
		}
	})
	return count, size
}

// embeddedPicture is a picture a render embeds, as its data URI.
var embeddedPicture = regexp.MustCompile(`data:image/(?:png|jpeg|gif);base64,[A-Za-z0-9+/]*=*`)

// withHandles is rendered HTML with every embedded picture written as its handle
// (compose.EmailAssetHandle of its bytes): a composer reads which picture went
// where, and no picture's bytes pass through the model.
func withHandles(markup string) string {
	return embeddedPicture.ReplaceAllStringFunc(markup, func(uri string) string {
		_, payload, _ := strings.Cut(uri, ",")
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return ""
		}
		return compose.EmailAssetHandle(data)
	})
}
