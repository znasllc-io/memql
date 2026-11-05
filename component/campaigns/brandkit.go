package campaigns

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// brandkit.go -- an organization's brand kit (concept v1:campaigns:brandKit):
// the look a colourless layout takes when an email is made from it, saved with
// campaignSaveBrandKit and tried with campaignBrandPreview.
//
// # One kit per organization
//
// The row's id is made from the organization (brandKitID), so two members who
// both create the kit meet in one row, and the kit is read by the organization
// (brandKitForAccount). Templates read it when they are made from a layout
// (brandKitFor); changing a kit does not change a template already made.
//
// # Saved the way a template is saved
//
// A kit is shared by an organization's members, so two people can save it at
// once, and a person can retry a save whose answer was lost. campaignSaveBrandKit
// therefore does what campaignSaveTemplate does: one cross-replica lock keyed by
// the organization, the stored kit read FRESH, the caller held to the exact
// revision they read (the row's createdAt), and a write stamped newer than it
// through a @serverOnly mutation. The concept is @serverWritten, so the gate
// cannot be walked around with a raw insert. A retry of a save that landed (the
// same kit again) answers with the revision it already has.
//
// # Pictures are kept inside the kit
//
// A Library picture can be read only by whoever uploaded it
// (Store.LibraryFileForArtifact), so a kit that pointed at the Library would
// show its logo to one member and break for every other. A picture may be sent
// as artifact:<id>: it is read as the CALLER reads it, through the same reader
// a render uses (artifactImages), and stored in the kit as a data: picture. A
// picture the caller cannot read is refused with the field it is in.
//
// # Warnings, never refusals
//
// emaildoc.ParseBrandKit refuses a kit that is not valid, with a sentence that
// names the field (a footer without an unsubscribe link is one). Contrast is not
// such a reason: a brand's own colours may be under WCAG AA, and Fylo's white
// headline on teal is. emaildoc.BrandWarnings says where, and the kit is saved.
//
// # What it may cost, and in which order
//
// What costs nothing comes first: the caller, the organization and the
// permission, then the kit checked with a stand-in for each Library picture, so
// a kit that would be refused for what it says reads no picture at all (and a
// kit of ten thousand social links is a sentence, not ten thousand reads). Then
// a render slot, the one a preview takes: the pictures are read and the kit is
// drawn on every built-in layout for its warnings, which is slow with large
// pictures, and none of it holds a lock. What runs under the lock is short, as
// for templates: the organization and the stored kit as they are now, the
// permission for what is to be done, the revision, the write.

// defaultBrandPreviewLayout is the layout a preview draws when none is named.
const defaultBrandPreviewLayout = "builtin:article"

var (
	errChooseBrandOrganization = errors.New("choose the organization this brand is for, while signed in")
	errBrandKitChanged         = errors.New("this brand changed; reload it before saving")
	errBrandNeedsWrite         = errors.New("saving the brand needs permission to write templates in this organization")
	errBrandLocked             = errors.New("Someone else is saving this brand. Try again in a moment.")
	errBrandNoRevision         = errors.New("this brand has no readable revision")
	errBrandPreviewAccess      = errors.New("You need read access to this organization's data to preview its brand.")
	errBrandPreviewLayout      = errors.New("choose one of the built-in layouts to preview the brand with")
	errBrandTooLarge           = fmt.Errorf("This brand kit is larger than %d MiB. Use fewer or smaller pictures.", emaildoc.MaxBytes>>20)
)

// brandKitID is the id of an organization's kit: the same for every caller and
// replica, so that two first saves of the kit are one row.
func brandKitID(accountID string) string { return sha256Hex("brand-kit:" + bare(accountID)) }

// handleSaveBrandKit implements integration.campaigns.saveBrandKit.
func (w *Worker) handleSaveBrandKit(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	accountID, expected := bare(argString(args, "accountId")), argString(args, "expectedRevision")
	if accountID == "" || callerUserID(ctx) == "" {
		return nil, errChooseBrandOrganization
	}
	if w.brandGate == nil {
		return nil, errors.New("brand write coordination is unavailable")
	}
	if err := w.requireOrganization(ctx, accountID); err != nil {
		return nil, err
	}
	if !w.mayWriteTemplates(ctx, accountID) {
		return nil, errBrandNeedsWrite
	}
	sent, err := checkSentKit(args["kit"])
	if err != nil {
		return nil, err
	}

	release, err := acquireRenderSlot(ctx)
	if err != nil {
		if errors.Is(err, errPreviewBusy) {
			err = errTemplateSaveBusy
		}
		return nil, err
	}
	kit, err := w.fillKitPictures(ctx, sent)
	var warnings []emaildoc.BrandWarning
	if err == nil {
		warnings = brandWarnings(ctx, kit)
	}
	release()
	if err != nil {
		return nil, err
	}

	revision, err := w.saveBrandKitUnderLock(ctx, accountID, expected, kit)
	if err != nil {
		return nil, err
	}
	return resultNode("campaignBrandKitSaved", map[string]any{
		"revision": revision.UTC().Format(time.RFC3339Nano), "warnings": brandWarningList(warnings),
	})
}

// saveBrandKitUnderLock decides a save under the organization's gate and
// answers the revision the kit has now.
func (w *Worker) saveBrandKitUnderLock(ctx context.Context, accountID, expected string, kit emaildoc.BrandKit) (time.Time, error) {
	release, err := w.lockBrand(ctx, accountID)
	if err != nil {
		return time.Time{}, err
	}
	defer release()
	// Looked at again: the pictures and the warnings took time.
	if err := w.requireOrganization(ctx, accountID); err != nil {
		return time.Time{}, err
	}
	row, found, err := w.store.BrandKitForAccount(ctx, accountID)
	if err != nil {
		return time.Time{}, err
	}
	verb := auth.VerbCreate
	if found {
		verb = auth.VerbUpdate
	}
	if !w.store.engine.OrganizationCapable(ctx, accountID, verb, auth.ResourceData) {
		return time.Time{}, errBrandNeedsWrite
	}
	canonical, err := kit.JSON()
	if err != nil {
		return time.Time{}, fmt.Errorf("brand kit: %w", err)
	}

	var prior time.Time
	mutation := "createBrandKit"
	if found {
		if prior = templateTime(row["createdAt"]); prior.IsZero() {
			return time.Time{}, errBrandNoRevision
		}
		// An uncertain response can be retried without writing another version:
		// the same kit already saved answers with the revision it has, whatever
		// revision the retry carries.
		if sameBrandKit(row["kit"], canonical) {
			return prior, nil
		}
		wanted, parseErr := time.Parse(time.RFC3339Nano, expected)
		if parseErr != nil || !prior.Equal(wanted) {
			return time.Time{}, errBrandKitChanged
		}
		mutation = "updateBrandKit"
	} else if expected != "" {
		// The person read a kit that is not there to read: the revision they
		// hold is of nothing this organization has.
		return time.Time{}, errBrandKitChanged
	}

	var object map[string]any
	if err := json.Unmarshal(canonical, &object); err != nil {
		return time.Time{}, fmt.Errorf("brand kit: %w", err)
	}
	revision := memql.VersionTimeAfter(prior, w.nowUTC())
	values := map[string]any{"brandKitId": brandKitID(accountID), "kit": object, "versionTime": revision.Format(time.RFC3339Nano)}
	if !found {
		// The organization is fixed when the kit is created.
		values["accountId"] = accountID
	}
	// The mutation name is a variable on purpose, as in template.go: both are
	// @serverOnly, and Store.execServerOnly is what stamps the internal origin
	// they need.
	rendered, err := langparser.RenderCall(mutation, values)
	if err != nil {
		return time.Time{}, fmt.Errorf("brand kit: %w", err)
	}
	if err := w.store.execServerOnly(ctx, "mutation "+rendered); err != nil {
		return time.Time{}, err
	}
	return revision, nil
}

// lockBrand takes the organization's brand gate. A wait for it that ended
// without the lock is a sentence that says what to do; the cause is logged. A
// gate that fails for any other reason comes back as the engine reported it.
func (w *Worker) lockBrand(ctx context.Context, accountID string) (func(), error) {
	release, err := w.brandGate(ctx, accountID)
	if err != nil {
		if !errors.Is(err, memql.ErrWriteGateBusy) {
			return nil, err
		}
		if w.logger != nil {
			w.logger.Warn("campaigns: a brand gate could not be taken", "account", accountID, "error", err)
		}
		return nil, errBrandLocked
	}
	return release, nil
}

// sameBrandKit reports whether the stored kit is the kit whose canonical JSON
// is given. Both are compared as ParseBrandKit and JSON write them, so the order
// the database keeps an object's keys in does not matter.
func sameBrandKit(stored any, canonical []byte) bool {
	raw, err := plainJSON(stored)
	if err != nil {
		return false
	}
	kit, err := emaildoc.ParseBrandKit(raw)
	if err != nil {
		return false
	}
	again, err := kit.JSON()
	return err == nil && bytes.Equal(again, canonical)
}

// handleBrandPreview implements integration.campaigns.brandPreview: a built-in
// layout with the kit applied, rendered as emailRender renders a document, and
// the kit's warnings. It saves nothing.
func (w *Worker) handleBrandPreview(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	accountID := bare(argString(args, "accountId"))
	if accountID == "" {
		return nil, errors.New("Choose the organization this brand is for.")
	}
	if callerUserID(ctx) == "" {
		return nil, errors.New("Sign in to preview a brand.")
	}
	layoutID := strings.TrimSpace(argString(args, "layoutId"))
	if layoutID == "" {
		layoutID = defaultBrandPreviewLayout
	}
	info, known := emaildoc.LayoutMeta(layoutID)
	layout, built := emaildoc.Layout(layoutID)
	if !known || !built {
		return nil, errBrandPreviewLayout
	}

	accounts, err := w.store.rows(ctx, call("query", "clientAccountById", arg{"accountId", accountID}))
	if err != nil {
		return nil, err
	}
	if len(accounts) != 1 || str(accounts[0], "status") != "active" {
		return nil, errOrganizationClosed
	}
	if !w.store.engine.OrganizationCapable(ctx, accountID, auth.VerbRead, auth.ResourceData) {
		return nil, errBrandPreviewAccess
	}
	sent, err := checkSentKit(args["kit"])
	if err != nil {
		return nil, err
	}

	release, err := acquireRenderSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	kit, err := w.fillKitPictures(ctx, sent)
	if err != nil {
		return nil, err
	}
	// The layout as a template made from it starts: its repeatable blocks at
	// their minimum.
	if layout, err = emaildoc.ExpandToMinimum(layout, info); err != nil {
		return nil, err
	}
	branded, err := emaildoc.ApplyBrand(layout, kit)
	if err != nil {
		return nil, err
	}
	// {{accountName}} is the organization's own name.
	sample := map[string]string{}
	if name := strings.TrimSpace(str(accounts[0], "name")); name != "" {
		sample["accountName"] = name
	}
	result, err := emaildoc.Render(ctx, branded, &pictureReads{inner: artifactImages{w}, left: maxPictureReads},
		emaildoc.RenderOptions{Preview: true, Sample: sample})
	if err != nil {
		if errors.Is(err, emaildoc.ErrTooLarge) {
			return nil, errPreviewTooLarge
		}
		return nil, err
	}
	return resultNode("campaignBrandPreview", map[string]any{
		"html": result.HTML, "warnings": brandWarningList(brandWarnings(ctx, kit)),
	})
}

// brandWarningList is the warnings as an answer carries them, {code, message}
// each: an empty list when there are none, never null, since a client reads
// warnings.length.
func brandWarningList(warnings []emaildoc.BrandWarning) []map[string]string {
	out := make([]map[string]string, 0, len(warnings))
	for _, w := range warnings {
		out = append(out, map[string]string{"code": w.Code, "message": w.Message})
	}
	return out
}

// brandKitFor reads an organization's brand kit as the caller sees it: the kit,
// its revision (the row's createdAt) and whether there is one. An organization
// the caller is not in answers as one with no kit (the concept's account tier).
// A stored kit that no longer reads is an error that says how to mend it.
func (w *Worker) brandKitFor(ctx context.Context, accountID string) (kit emaildoc.BrandKit, revision time.Time, ok bool, err error) {
	row, found, err := w.store.BrandKitForAccount(ctx, accountID)
	if err != nil || !found {
		return emaildoc.BrandKit{}, time.Time{}, false, err
	}
	raw, err := plainJSON(row["kit"])
	if err != nil {
		return emaildoc.BrandKit{}, time.Time{}, false, fmt.Errorf("brand kit: %w", err)
	}
	if kit, err = emaildoc.ParseBrandKit(raw); err != nil {
		return emaildoc.BrandKit{}, time.Time{}, false, fmt.Errorf("this organization's brand can't be read (%v); open Email, Settings, Brand and save it again", err)
	}
	return kit, templateTime(row["createdAt"]), true, nil
}

// BrandKitForAccount reads an organization's brand kit under the caller's
// actor, or reports false when there is none the caller can see. The concept's
// account tier answers an organization the caller is not in exactly as one
// with no kit. The row's createdAt is its revision.
func (s *Store) BrandKitForAccount(ctx context.Context, accountID string) (map[string]any, bool, error) {
	rows, err := s.rows(ctx, call("query", "brandKitForAccount", arg{"accountId", accountID}))
	if err != nil || len(rows) == 0 {
		return nil, false, err
	}
	return rows[0], true, nil
}

// kitPictureStandIn takes the place of a Library picture while a kit is
// checked before any picture is read: a one-pixel PNG, which every picture
// field of a kit accepts.
var kitPictureStandIn = func() string {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		panic("campaigns: encoding the picture stand-in: " + err.Error())
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}()

// kitLibraryPicture is one place a sent kit names a Library picture: what a
// refusal calls the field, the object and key it is under, and the artifact.
type kitLibraryPicture struct {
	field    string
	object   map[string]any
	key      string
	artifact string
}

// sentKit is a kit as a client sent it, decoded into an object of its own (the
// call's arguments are not changed), with the places it names a Library
// picture. kit is the parsed kit when it names none.
type sentKit struct {
	object   map[string]any
	pictures []kitLibraryPicture
	kit      *emaildoc.BrandKit
}

// checkSentKit reads the kit a client sent and refuses it, with ParseBrandKit's
// sentence, if it would be refused with every Library picture in it readable:
// each one is checked as a stand-in, and none is read.
func checkSentKit(raw any) (sentKit, error) {
	encoded, err := plainJSON(raw)
	if err != nil {
		return sentKit{}, fmt.Errorf("brand kit: %w", err)
	}
	if len(encoded) > emaildoc.MaxBytes {
		return sentKit{}, errBrandTooLarge
	}
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if decoder.Decode(&object) != nil || object == nil {
		// Not an object: ParseBrandKit says so in its words.
		_, err := parseKit(encoded)
		if err == nil {
			err = errors.New("a brand kit must be a JSON object")
		}
		return sentKit{}, err
	}
	sent := sentKit{object: object, pictures: kitLibraryPictures(object)}
	for _, p := range sent.pictures {
		p.object[p.key] = kitPictureStandIn
	}
	checked, err := plainJSON(object)
	if err != nil {
		return sentKit{}, fmt.Errorf("brand kit: %w", err)
	}
	kit, err := parseKit(checked)
	if err != nil {
		return sentKit{}, err
	}
	if len(sent.pictures) == 0 {
		sent.kit = &kit
	}
	return sent, nil
}

// kitLibraryPictures lists the places a kit names a Library picture
// (artifact:<id>), in the kit's order and with the names ParseBrandKit gives
// the fields. A part that is not the shape a kit has is passed over; the parse
// refuses it with its own sentence.
func kitLibraryPictures(kit map[string]any) []kitLibraryPicture {
	var out []kitLibraryPicture
	add := func(field string, object map[string]any, key string) {
		src, _ := object[key].(string)
		// layoutIDPattern is the set emaildoc reads an artifact id in; any other
		// artifact: source is left for the parse to refuse.
		id, ok := strings.CutPrefix(src, "artifact:")
		if ok && layoutIDPattern.MatchString(id) {
			out = append(out, kitLibraryPicture{field: field, object: object, key: key, artifact: id})
		}
	}
	if logo, ok := kit["logo"].(map[string]any); ok {
		add("logo", logo, "src")
		add("logo.onColour", logo, "onColour")
		add("logo.wide", logo, "wide")
	}
	if signature, ok := kit["signature"].(map[string]any); ok {
		add("signature photo", signature, "photo")
		if name, ok := signature["name"].(map[string]any); ok {
			add("signature name", name, "src")
		}
		add("signature picture", signature, "picture")
	}
	if social, ok := kit["social"].([]any); ok {
		for n, item := range social {
			if link, ok := item.(map[string]any); ok {
				add(fmt.Sprintf("social: link %d", n+1), link, "icon")
			}
		}
	}
	return out
}

// fillKitPictures reads each Library picture of a checked kit as the caller,
// puts it in as a data: picture, and parses the kit. A picture is read once
// however many places name it.
func (w *Worker) fillKitPictures(ctx context.Context, sent sentKit) (emaildoc.BrandKit, error) {
	if sent.kit != nil {
		return *sent.kit, nil
	}
	read := map[string]string{}
	for _, p := range sent.pictures {
		uri, done := read[p.artifact]
		if !done {
			picture, err := artifactImages{w}.ResolveImage(ctx, p.artifact)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return emaildoc.BrandKit{}, ctxErr
				}
				// Storage or the database not answering is no reason to choose
				// another picture; the cause is logged by ResolveImage.
				if errors.Is(err, errPictureUnreachable) {
					return emaildoc.BrandKit{}, fmt.Errorf("%s: the picture could not be read just now. Try again.", p.field)
				}
				return emaildoc.BrandKit{}, fmt.Errorf("%s: the picture could not be read. Choose a PNG, JPEG or GIF of up to 1 MiB that you uploaded", p.field)
			}
			uri = "data:" + picture.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(picture.Data)
			read[p.artifact] = uri
		}
		p.object[p.key] = uri
	}
	filled, err := plainJSON(sent.object)
	if err != nil {
		return emaildoc.BrandKit{}, fmt.Errorf("brand kit: %w", err)
	}
	return parseKit(filled)
}

// parseKit is emaildoc.ParseBrandKit, with a kit's own sentence for one too
// large to be a kit (the parser's speaks of an email file).
func parseKit(data []byte) (emaildoc.BrandKit, error) {
	kit, err := emaildoc.ParseBrandKit(data)
	if errors.Is(err, emaildoc.ErrFileTooLarge) {
		return emaildoc.BrandKit{}, errBrandTooLarge
	}
	return kit, err
}

// computeBrandWarnings works out a kit's warnings: every built-in layout
// rendered with the kit applied. A test counts it.
var computeBrandWarnings = emaildoc.BrandWarnings

// brandWarningMemory is the warnings of the kits worked out last, by the
// digest of the whole kit, for every caller on the node: they depend on the
// kit and nothing else.
var brandWarningMemory = &warningMemory{size: 64, kept: map[string][]emaildoc.BrandWarning{}}

// brandWarnings is emaildoc.BrandWarnings, worked out once for a kit and
// remembered: they render every built-in layout, in a render slot that
// previews and saves share, and the Brand page previews one kit on the layout
// that shows the part being edited, then saves it. They are remembered by the
// whole kit, pictures included, not by its theme alone: which parts the kit has
// (a reply, a follow line, a signature as text or as a picture) and the links
// and emphasis in its words decide which text is drawn on which colour. A
// computation whose ctx ended may have skipped layouts, and is not remembered.
func brandWarnings(ctx context.Context, kit emaildoc.BrandKit) []emaildoc.BrandWarning {
	raw, err := kit.JSON()
	if err != nil {
		return computeBrandWarnings(ctx, kit)
	}
	sum := sha256.Sum256(raw)
	key := hex.EncodeToString(sum[:])
	if warnings, ok := brandWarningMemory.get(key); ok {
		return warnings
	}
	warnings := computeBrandWarnings(ctx, kit)
	if ctx.Err() == nil {
		brandWarningMemory.put(key, warnings)
	}
	return slices.Clone(warnings)
}

// warningMemory keeps the warnings of up to size kits, forgetting the one kept
// longest first.
type warningMemory struct {
	mu    sync.Mutex
	size  int
	order []string
	kept  map[string][]emaildoc.BrandWarning
}

func (m *warningMemory) get(key string) ([]emaildoc.BrandWarning, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	warnings, ok := m.kept[key]
	return slices.Clone(warnings), ok
}

func (m *warningMemory) put(key string, warnings []emaildoc.BrandWarning) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.kept[key]; ok {
		return
	}
	if len(m.order) >= m.size {
		delete(m.kept, m.order[0])
		m.order = m.order[1:]
	}
	m.order = append(m.order, key)
	m.kept[key] = slices.Clone(warnings)
}
