package compose

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	pure "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	"github.com/znasllc-io/memql/component/memql"
)

// email_layout.go -- the layout an email template is written into.
//
// An email template is an email document in a layout (component/compose/
// emaildoc). The Materializer never writes one from nothing: the request names
// a layout (one of the built-ins, or one its organization saved), and the email
// is that layout, either filed as it is (a blank email) or with its content
// blocks written by the composer and merged back in by the server (a described
// one). The layout is resolved under the caller when the request is accepted
// and saved with it, so the replica that writes the email reads the same layout
// even if the organization changes or archives it meanwhile.
//
// THE PICTURES ARE THE CALLER'S OWN. The composer names a picture by a handle,
// the digest of bytes captured from a reference the person selected for
// inclusion, and nothing else becomes a picture in the email. A whole Library
// image file goes in as itself (artifact:<id>), which keeps the email's source
// editable and the picture reusable; anything else (a member of a ZIP, a file
// whose artifact the caller cannot read) goes in as a copy of the captured bytes.

// defaultEmailLayout is the layout an email is written into when the request
// names none.
const defaultEmailLayout = "builtin:article"

// bareIDPattern is the characters of a bare id that may follow "org:" in a
// layout id or "artifact:" in a picture's source (the email document's own
// rule for both).
var bareIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,160}$`)

// emailDraftWithoutComposer is why an email draft the caller supplied is
// refused: an email is only ever its layout, and on a node with no composer
// there is nothing to write the draft into it. A sentence for the person whose
// draft it is, recorded on the composition as it stands.
const emailDraftWithoutComposer = "An email is written into its layout. This node has no composer to write the draft you supplied into it."

// The refusals of a layout, each before anything is written.
var (
	errLayoutMalformed    = errors.New("compose: layoutId must be builtin:<name> or org:<id>")
	errLayoutUnknown      = errors.New("compose: choose one of the listed layouts")
	errLayoutUnreadable   = errors.New("compose: that layout is not readable by you")
	errLayoutArchived     = errors.New("compose: that layout is archived; restore it first")
	errLayoutOrganization = errors.New("compose: that layout belongs to another organization")
)

// resolveEmailLayout reads the layout a request names and returns the email
// document it starts from, as its canonical bytes, with Layout set to the id it
// was chosen by (its provenance).
//
// A built-in is Go data. An organization's layout is read under the caller's
// own actor, fresh: one they cannot read is the same answer as one that does
// not exist, one of another organization is refused for this one's email, and
// an archived one is refused until it is restored, as the picker no longer
// offers it.
func (i *Integration) resolveEmailLayout(ctx context.Context, a materializeArgs) (json.RawMessage, error) {
	id := a.layoutID()
	var doc emaildoc.Document
	switch name, org := strings.CutPrefix(id, "org:"); {
	case strings.HasPrefix(id, "builtin:"):
		builtin, ok := emaildoc.Layout(id)
		if !ok {
			return nil, errLayoutUnknown
		}
		doc = builtin
	case org && bareIDPattern.MatchString(name):
		row, err := i.store().emailLayoutById(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("compose: reading the layout: %w", err)
		}
		if row == nil {
			return nil, errLayoutUnreadable
		}
		organization := ""
		if len(a.AccountIds) == 1 {
			organization = memql.BareShortId(a.AccountIds[0])
		}
		if memql.BareShortId(stringOf(row["accountId"])) != organization {
			return nil, errLayoutOrganization
		}
		if stringOf(row["status"]) == "archived" {
			return nil, errLayoutArchived
		}
		saved, err := plainJSON(row["document"])
		if err != nil {
			return nil, fmt.Errorf("compose: that layout cannot be used: %w", err)
		}
		if doc, err = emaildoc.Parse(saved); err != nil {
			return nil, fmt.Errorf("compose: that layout cannot be used: %w", err)
		}
		if doc.Mode() != emaildoc.ModeLayout {
			return nil, errors.New("compose: that layout cannot be used: it has no blocks")
		}
	default:
		return nil, errLayoutMalformed
	}
	doc.Layout = id
	raw, err := doc.JSON()
	if err != nil {
		return nil, fmt.Errorf("compose: that layout cannot be used: %w", err)
	}
	// What is filed is this, laid out as a file is. A layout saved near the size
	// limit can be over it once it is (a saved layout is measured compact), and
	// that is better refused now than when the email is written.
	if _, err := emaildoc.Parse(raw); err != nil {
		return nil, fmt.Errorf("compose: that layout cannot be used: %w", err)
	}
	return raw, nil
}

// savedEmailLayout is the layout a saved request was accepted with.
func savedEmailLayout(request executionRequest) (emaildoc.Document, error) {
	if len(request.EmailLayout) == 0 {
		return emaildoc.Document{}, errors.New("the saved request names no email layout")
	}
	return emaildoc.Parse(request.EmailLayout)
}

// mergeEmailContent writes what the composer wrote into the layout and returns
// the email document's bytes. emaildoc.Merge says what is refused.
func mergeEmailContent(layout emaildoc.Document, content emaildoc.Content, images map[string]string) (string, error) {
	doc, err := emaildoc.Merge(layout, content, images)
	if err != nil {
		return "", err
	}
	body, err := doc.JSON()
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// EmailAssetHandle is the name a picture selected for inclusion is offered to
// the composer by, and the only name it can put a picture in an email by: the
// digest of its captured bytes, so a handle names those bytes and no others.
func EmailAssetHandle(image []byte) string {
	return "memql-asset:" + (pure.Result{Bytes: image}).SHA256()
}

// emailImageSources maps the handle of every picture selected for inclusion to
// the source it takes in the email (the plan's Rulings 12): a picture that is a
// whole Library file whose artifact the caller can read becomes that file,
// artifact:<id>; anything else, a member of a ZIP or a file without a readable
// artifact, becomes a data: copy of the captured bytes. A reference the person
// kept for inspiration offers no handle at all.
func (i *Integration) emailImageSources(ctx context.Context, resolved []Resolved) (map[string]string, error) {
	sources := map[string]string{}
	for _, source := range resolved {
		if !source.Ref.Content || !source.Ref.IncludeImages {
			continue
		}
		for _, file := range source.Files {
			if len(file.Image) == 0 {
				continue
			}
			handle := EmailAssetHandle(file.Image)
			if _, done := sources[handle]; done {
				continue
			}
			src := "data:" + file.MimeType + ";base64," + base64.StdEncoding.EncodeToString(file.Image)
			if wholeLibraryFile(source, file.Image) {
				artifact, err := i.store().libraryArtifactForFile(ctx, source.Ref.Ref)
				if err != nil {
					return nil, fmt.Errorf("finding a picture's Library entry: %w", err)
				}
				if id := memql.BareShortId(stringOf(artifact["id"])); bareIDPattern.MatchString(id) {
					src = "artifact:" + id
				}
			}
			sources[handle] = src
		}
	}
	return sources, nil
}

// wholeLibraryFile reports whether image is the whole of the Library file the
// source names, and not a member of an archive: the file's own recorded digest
// is the digest of these bytes. A file that records none is not known to be,
// and its picture is embedded.
func wholeLibraryFile(source Resolved, image []byte) bool {
	if source.Ref.Kind != KindLibraryFile || len(source.Rows) != 1 {
		return false
	}
	digest := stringOf(source.Rows[0]["sha256"])
	return digest != "" && strings.EqualFold(digest, (pure.Result{Bytes: image}).SHA256())
}

// plainJSON encodes v without escaping "<", ">" and "&", which a document's
// markup is full of: escaped, a document within the size limit can be over it.
func plainJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
