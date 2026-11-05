package campaigns

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// template_from_layout.go -- campaignCreateTemplateFromLayout: a draft template
// made at once from a built-in layout. It is the "Start from the layout" button
// and the first step of "Write it": the layout in its smallest form (a repeatable
// block at its minimum, so a Steps email starts with two steps), with the
// organization's brand kit applied when it has one, saved through the same
// create path as any template. The subject is the layout's example subject.

var (
	errUnknownLayout           = errors.New("choose one of the built-in layouts")
	errTemplateGateUnavailable = errors.New("template write coordination is unavailable")
)

// handleCreateTemplateFromLayout implements
// integration.campaigns.createTemplateFromLayout.
func (w *Worker) handleCreateTemplateFromLayout(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	templateID, revision, issues, err := w.createTemplateFromLayout(ctx, bare(argString(args, "accountId")),
		strings.TrimSpace(argString(args, "name")), strings.TrimSpace(argString(args, "layoutId")), nil)
	if err != nil {
		return nil, err
	}
	return templateReceipt(templateID, revision, "draft", issues)
}

// createTemplateFromLayout saves a draft made from the layout, with c as its
// composition when given, and answers the new template's id and revision.
func (w *Worker) createTemplateFromLayout(ctx context.Context, accountID, name, layoutID string, c *composition) (string, time.Time, []emaildoc.Issue, error) {
	badName := name == "" || utf8.RuneCountInString(name) > maxTemplateNameChars || strings.IndexFunc(name, unicode.IsControl) >= 0
	if accountID == "" || callerUserID(ctx) == "" || badName {
		return "", time.Time{}, nil, errChooseTemplate
	}
	info, known := emaildoc.LayoutMeta(layoutID)
	layout, built := emaildoc.Layout(layoutID)
	if !known || !built {
		return "", time.Time{}, nil, errUnknownLayout
	}
	if w.templateGate == nil {
		return "", time.Time{}, nil, errTemplateGateUnavailable
	}
	if err := w.requireOrganization(ctx, accountID); err != nil {
		return "", time.Time{}, nil, err
	}
	if !w.mayWriteTemplates(ctx, accountID) {
		return "", time.Time{}, nil, errTemplateNeedsWrite
	}

	doc, err := emaildoc.ExpandToMinimum(layout, info)
	if err != nil {
		return "", time.Time{}, nil, err
	}
	kit, _, hasKit, err := w.brandKitFor(ctx, accountID)
	if err != nil {
		return "", time.Time{}, nil, err
	}
	if hasKit {
		if doc, err = emaildoc.ApplyBrand(doc, kit); err != nil {
			return "", time.Time{}, nil, err
		}
	}
	id, err := newTemplateID()
	if err != nil {
		return "", time.Time{}, nil, err
	}
	req := templateRequest{templateID: id, accountID: accountID, name: name, action: "save", document: doc}
	rendered, err := w.renderTemplate(ctx, req)
	if err != nil {
		return "", time.Time{}, nil, err
	}
	if c != nil {
		if rendered.content.composition, err = c.object(); err != nil {
			return "", time.Time{}, nil, err
		}
	}

	release, err := w.lockTemplate(ctx, id)
	if err != nil {
		return "", time.Time{}, nil, err
	}
	defer release()
	if err := w.requireOrganization(ctx, accountID); err != nil {
		return "", time.Time{}, nil, err
	}
	if !w.store.engine.OrganizationCapable(ctx, accountID, auth.VerbCreate, auth.ResourceData) {
		return "", time.Time{}, nil, errTemplateNeedsWrite
	}
	revision, err := w.insertTemplate(ctx, req, rendered)
	if err != nil {
		return "", time.Time{}, nil, err
	}
	return id, revision, rendered.issues, nil
}

// newTemplateID is the id of a template nobody has named: 128 random bits as
// 32 hex characters, the shape of the id a Library file's template gets.
func newTemplateID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
