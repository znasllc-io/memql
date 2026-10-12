package campaigns

import (
	"context"
	"encoding/json"
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

// layout.go -- an organization's own email layouts: a finished email kept as the
// starting point for the next one (concept v1:campaigns:emailLayout).
//
// # Saved the way a template is saved
//
// A layout is shared by an organization's members, so two people can save it at
// once, and a person can retry a save whose answer was lost. campaignSaveLayout
// therefore does what campaignSaveTemplate does: it takes one cross-replica lock
// keyed by the layout, reads the stored row FRESH, holds the caller to the exact
// revision they read (the row's createdAt), and writes through a @serverOnly
// mutation stamped with a revision that is newer than the one it read. The
// concept refuses every other writer (executor_mutation.go), so the gate cannot
// be walked around with a raw insert.
//
// Archive and restore take the revision and nothing else: putting a layout away
// is not an edit, and a client that holds only the card has no document to send.
// An archived layout stays readable (emails made from it name it) and is not
// offered by the picker; it is restored before it is edited.
//
// # Built-in layouts are not rows
//
// The four built-in layouts are Go data that ships with the renderer, so a
// picker row for one is made up on the way out and an organization cannot save,
// archive or change one. Their ids ("builtin:<name>") and an organization
// layout's ("org:<bareId>") are opaque to a client: it passes the one it picked
// to composeMaterialize and never builds or splits one. Saving takes the BARE
// layoutId, which is the one thing a client composes itself, and the opaque id
// of a picker row is refused there on purpose.

// layoutIDPattern is the characters of a layout id: the same set as a template
// id, and the set the document format allows after "org:" in a layout name.
var layoutIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,160}$`)

const (
	maxLayoutNameChars        = 200
	maxLayoutDescriptionChars = 500
	// layoutListPages bounds the walk over an organization's layouts at a hundred
	// rows a page. Reaching it is a refusal and not a truncation: a picker that
	// silently dropped layouts would be worse than one that said it could not list.
	layoutListPages = 50
)

func (w *Worker) handleSaveLayout(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	layoutID, accountID := bare(argString(args, "layoutId")), bare(argString(args, "accountId"))
	name, description := strings.TrimSpace(argString(args, "name")), strings.TrimSpace(argString(args, "description"))
	sourceArtifactID := bare(argString(args, "sourceArtifactId"))
	expected, action := argString(args, "expectedRevision"), argString(args, "action")
	if action == "" {
		action = "save"
	}
	// The content (name, description, source file, document) is looked at only
	// when saving. Archive and restore ignore whatever else the call carries.
	saving := action == "save"
	badName := name == "" || utf8.RuneCountInString(name) > maxLayoutNameChars || strings.IndexFunc(name, unicode.IsControl) >= 0
	if !layoutIDPattern.MatchString(layoutID) || accountID == "" || callerUserID(ctx) == "" || (saving && badName) {
		return nil, errors.New("choose an organization and a layout name (up to 200 characters)")
	}
	if saving && (utf8.RuneCountInString(description) > maxLayoutDescriptionChars || strings.IndexFunc(description, unicode.IsControl) >= 0) {
		return nil, errors.New("a layout description is one line of up to 500 characters")
	}
	if saving && sourceArtifactID != "" && !layoutIDPattern.MatchString(sourceArtifactID) {
		return nil, errors.New("The source file isn't a Library file. Choose a file from the Library, or leave the source empty.")
	}
	if action != "save" && action != "archive" && action != "restore" {
		return nil, errors.New("unknown layout action")
	}
	if saving {
		encoded, err := json.Marshal(args["document"])
		if err != nil {
			return nil, fmt.Errorf("layout: %w", err)
		}
		doc, err := emaildoc.Parse(encoded)
		if err != nil {
			return nil, fmt.Errorf("layout: %w", err)
		}
		if doc.Mode() != emaildoc.ModeLayout {
			return nil, errors.New("a layout needs blocks; save custom HTML as a template instead")
		}
	}

	if w.layoutGate == nil {
		return nil, errors.New("layout write coordination is unavailable")
	}
	release, err := w.layoutGate(ctx, layoutID)
	if err != nil {
		return nil, err
	}
	defer release()
	accounts, err := w.store.rows(ctx, call("query", "clientAccountById", arg{"accountId", accountID}))
	if err != nil {
		return nil, err
	}
	if len(accounts) != 1 || str(accounts[0], "status") != "active" {
		return nil, errors.New("organization is not active or accessible")
	}
	row, found, err := w.store.EmailLayoutByID(ctx, layoutID)
	if err != nil {
		return nil, err
	}
	verb := auth.VerbCreate
	if found {
		verb = auth.VerbUpdate
	}
	if !w.store.engine.OrganizationCapable(ctx, accountID, verb, auth.ResourceData) {
		return nil, errors.New("layout write permission is required in this organization")
	}

	var prior time.Time
	status, mutation := "active", "createEmailLayout"
	if found {
		if !sameOrganization(accountID, str(row, "accountId")) {
			return nil, errors.New("a layout cannot move between organizations; save a copy instead")
		}
		prior = templateTime(row["createdAt"])
		if prior.IsZero() {
			return nil, errors.New("layout has no readable revision")
		}
		current := str(row, "status")
		target := map[string]string{"save": "active", "archive": "archived", "restore": "active"}[action]
		if saving && current == "archived" {
			return nil, errors.New("restore this layout before editing it")
		}
		// An uncertain response can be retried without writing another version:
		// the same content already saved, or the archive or restore already
		// applied, answers with the receipt it already has whatever revision the
		// retry carries. This also makes an autosave of unchanged content inert.
		if saving && sameLayoutContent(row, name, description, sourceArtifactID, args["document"]) || !saving && current == target {
			return layoutReceipt(layoutID, prior, current)
		}
		wanted, parseErr := time.Parse(time.RFC3339Nano, expected)
		if parseErr != nil || !prior.Equal(wanted) {
			return nil, errors.New("this layout changed; compare the latest revision before saving")
		}
		status, mutation = target, "updateEmailLayout"
	} else if action != "save" {
		return nil, errors.New("layout not found; save it before archiving or restoring")
	} else if expected != "" {
		// An edit names the revision it read. No layout is visible under this id,
		// so what the person read is not available to them any more: it was never
		// saved, or it belongs to an organization they are not in (the engine
		// answers both alike). Archiving and restoring are not what they were
		// doing, so they are not what they are told.
		return nil, errors.New("That layout isn't available. Choose another layout or save a new one.")
	}

	now := time.Now()
	if w.now != nil {
		now = w.now()
	}
	revision := memql.VersionTimeAfter(prior, now)
	values := map[string]any{"layoutId": layoutID, "versionTime": revision.Format(time.RFC3339Nano)}
	switch {
	case !found:
		// The mutation stamps the owner and the status of a new layout.
		values["accountId"] = accountID
		setLayoutContent(values, name, description, sourceArtifactID, args["document"])
	case saving:
		setLayoutContent(values, name, description, sourceArtifactID, args["document"])
		values["status"] = status
	default:
		values["status"] = status
	}
	// The mutation name is a variable on purpose, as in template.go: both layout
	// mutations are @serverOnly and Store.execServerOnly is what stamps the
	// internal origin they need.
	rendered, err := langparser.RenderCall(mutation, values)
	if err != nil {
		return nil, fmt.Errorf("layout: %w", err)
	}
	if err := w.store.execServerOnly(ctx, "mutation "+rendered); err != nil {
		return nil, err
	}
	return layoutReceipt(layoutID, revision, status)
}

// setLayoutContent puts the four things a save replaces into a mutation call.
// All four are always named, so a save that leaves out the description or the
// source file clears it instead of keeping the stale one.
func setLayoutContent(values map[string]any, name, description, sourceArtifactID string, document any) {
	values["name"], values["description"], values["sourceArtifactId"], values["document"] = name, description, sourceArtifactID, document
}

// sameLayoutContent reports whether a save would write what is already stored.
// The documents are compared as the JSON they are, which sorts object keys and
// reads numbers the same whichever way they came (from a client, or back out of
// the database).
func sameLayoutContent(row map[string]any, name, description, sourceArtifactID string, document any) bool {
	if name != str(row, "name") || description != str(row, "description") || sourceArtifactID != bare(str(row, "sourceArtifactId")) {
		return false
	}
	incoming, err := json.Marshal(document)
	if err != nil {
		return false
	}
	stored, err := json.Marshal(row["document"])
	return err == nil && string(incoming) == string(stored)
}

func layoutReceipt(layoutID string, revision time.Time, status string) ([]memorynodes.MemoryNode, error) {
	return resultNode("campaignLayoutSaved", map[string]any{"layoutId": layoutID, "revision": revision.UTC().Format(time.RFC3339Nano), "status": status, "saved": true})
}

// handleEmailLayouts lists what an organization can start an email from: the
// four built-in layouts, then its own, newest first. The rows are the picker's
// and carry the whole document, so a client can preview a layout and the
// Materializer's front door can show what it will fill.
func (w *Worker) handleEmailLayouts(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	accountID := bare(argString(args, "accountId"))
	if accountID == "" {
		return nil, errors.New("choose an organization")
	}
	accounts, err := w.store.rows(ctx, call("query", "clientAccountById", arg{"accountId", accountID}))
	if err != nil {
		return nil, err
	}
	if len(accounts) != 1 || str(accounts[0], "status") != "active" {
		return nil, errors.New("organization is not active or accessible")
	}
	if !w.store.engine.OrganizationCapable(ctx, accountID, auth.VerbRead, auth.ResourceData) {
		return nil, errors.New("layout read permission is required in this organization")
	}

	// Layouts re-reads every built-in document on each call, by design, so it is
	// called once here.
	builtins := emaildoc.Layouts()
	rows := make([]map[string]any, 0, len(builtins)+4)
	for _, info := range builtins {
		document, err := documentObject(info.Document)
		if err != nil {
			return nil, err
		}
		rows = append(rows, map[string]any{
			"id": info.ID, "kind": "builtin", "name": info.Name, "description": info.Description, "document": document, "status": "active",
		})
	}
	stored, err := w.store.EmailLayoutsForAccount(ctx, accountID, boolean(args, "includeArchived"))
	if err != nil {
		return nil, err
	}
	for _, row := range stored {
		layoutID := bare(str(row, "id"))
		rows = append(rows, map[string]any{
			"id": "org:" + layoutID, "layoutId": layoutID, "kind": "organization", "name": str(row, "name"), "description": str(row, "description"),
			"document": row["document"], "status": str(row, "status"), "sourceArtifactId": bare(str(row, "sourceArtifactId")),
			"revision": templateTime(row["createdAt"]).UTC().Format(time.RFC3339Nano),
		})
	}
	return resultRows("emailLayout", rows)
}

// documentObject is a document as the JSON object its file holds.
func documentObject(doc emaildoc.Document) (map[string]any, error) {
	raw, err := doc.JSON()
	if err != nil {
		return nil, fmt.Errorf("layout: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("layout: %w", err)
	}
	return out, nil
}

// EmailLayoutByID reads one organization layout under the caller's actor, or
// reports false when there is none the caller can see. The concept's account
// tier answers a layout outside the caller's organizations exactly as it
// answers one that does not exist, so the id is not a capability. The row's
// createdAt is its revision.
func (s *Store) EmailLayoutByID(ctx context.Context, layoutID string) (map[string]any, bool, error) {
	rows, err := s.rows(ctx, call("query", "emailLayoutById", arg{"layoutId", layoutID}))
	if err != nil || len(rows) == 0 {
		return nil, false, err
	}
	return rows[0], true, nil
}

// EmailLayoutsForAccount reads an organization's layouts, newest first: the
// active ones, or all of them with includeArchived. It walks every page, since
// a list that stopped at the first hundred would drop layouts without saying so.
func (s *Store) EmailLayoutsForAccount(ctx context.Context, accountID string, includeArchived bool) ([]map[string]any, error) {
	args := []arg{{"accountId", accountID}}
	if !includeArchived {
		args = append(args, arg{"status", "active"})
	}
	q := call("query", "emailLayoutsForAccount", args...)
	var out []map[string]any
	cursor := ""
	for page := 0; page < layoutListPages; page++ {
		rows, next, err := s.rowsPage(ctx, cursor, q)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if next == "" {
			return out, nil
		}
		cursor = next
	}
	return nil, errors.New("campaigns: emailLayoutsForAccount: this organization has more layouts than can be listed")
}
