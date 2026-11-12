package wholesalepack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// reader.go -- THE NARROW ROW READ THE PACK'S GO HALF DECIDES OVER.
//
// AN INTERFACE RATHER THAN THE ENGINE, and the reason is testability of the
// parts that matter: the state fold, the transition legality check and the
// applications-open gate are decisions over ROWS, and a test that has to
// build an *ExecuteResult to exercise them is testing the engine's envelope
// instead. Behind this seam they are functions over values.
//
// This is packs/reviewspack/published.go's seam, kept deliberately
// identical rather than factored into a shared helper. A shared one would
// have to be in component/memql, which would make a convenience for two
// packs into a part of the engine's plugin contract -- and the contract is
// the thing a third pack has to live with for ever. Two forty-line
// implementations that can diverge are the cheaper mistake.

// rowReader is the read the pack's Go half needs.
type rowReader interface {
	Rows(ctx context.Context, query string, args map[string]string) ([]map[string]any, error)
	// RowsWithList is Rows plus ONE list-valued argument, which the state
	// fold over a page of applications needs and no other read here does.
	RowsWithList(ctx context.Context, query string, args map[string]string,
		listName string, list []string) ([]map[string]any, error)
	// RowsPage is one page of a paginated query from cursor ("" is the first
	// page), with the cursor of the next ("" when this page was the last).
	RowsPage(ctx context.Context, query string, args map[string]string, cursor string) (rows []map[string]any, next string, err error)
	// RowsPageWithList is RowsPage plus ONE list-valued argument.
	RowsPageWithList(ctx context.Context, query string, args map[string]string, listName string, list []string,
		cursor string) (rows []map[string]any, next string, err error)
}

// engineRowReader is the production implementation.
type engineRowReader struct {
	engine memql.IntegrationEngineAccess
}

func (e *engineRowReader) Rows(ctx context.Context, query string, args map[string]string) ([]map[string]any, error) {
	return e.rows(ctx, query, args, "", nil)
}

func (e *engineRowReader) RowsWithList(ctx context.Context, query string, args map[string]string,
	listName string, list []string) ([]map[string]any, error) {
	return e.rows(ctx, query, args, listName, list)
}

func (e *engineRowReader) RowsPage(ctx context.Context, query string, args map[string]string, cursor string) ([]map[string]any, string, error) {
	return e.RowsPageWithList(ctx, query, args, "", nil, cursor)
}

func (e *engineRowReader) RowsPageWithList(ctx context.Context, query string, args map[string]string,
	listName string, list []string, cursor string) ([]map[string]any, string, error) {
	result, err := e.execute(memql.ContextWithCursor(ctx, cursor), query, args, listName, list)
	if err != nil {
		return nil, "", err
	}
	// A page that says there is more and names no cursor is refused, never
	// read as the last one: that would turn a bounded read into a whole one.
	next, err := memql.ResultPageCursor(result)
	if err != nil {
		return nil, "", fmt.Errorf("wholesale: %s: %w", query, err)
	}
	return memql.MaterializeRows(result), next, nil
}

func (e *engineRowReader) rows(ctx context.Context, query string, args map[string]string,
	listName string, list []string) ([]map[string]any, error) {
	result, err := e.execute(ctx, query, args, listName, list)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(result), nil
}

func (e *engineRowReader) execute(ctx context.Context, query string, args map[string]string,
	listName string, list []string) (*memql.ExecuteResult, error) {
	if e == nil || e.engine == nil {
		return nil, fmt.Errorf("wholesale: the pack has no engine handle")
	}
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("query ")
	b.WriteString(query)
	b.WriteByte('(')
	for i, k := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		// QuoteString, never Go quoting: it is the engine's own literal
		// escaping, and a company name is caller-supplied text.
		b.WriteString(langparser.QuoteString(args[k]))
	}
	if listName != "" {
		if len(names) > 0 {
			b.WriteString(", ")
		}
		b.WriteString(listName)
		b.WriteString(": [")
		for i, v := range list {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(langparser.QuoteString(v))
		}
		b.WriteString("]")
	}
	b.WriteByte(')')

	result, err := e.engine.Execute(ctx, b.String())
	if err != nil {
		return nil, fmt.Errorf("wholesale: %s: %w", query, err)
	}
	return result, nil
}

// rowsFor runs one of the pack's own queries through the seam.
func (p *Provider) rowsFor(ctx context.Context, query string, args map[string]string) ([]map[string]any, error) {
	if p.reader == nil {
		return nil, fmt.Errorf("wholesale: the pack has no reader; this capability cannot run")
	}
	return p.reader.Rows(ctx, query, args)
}

// rowsPage runs one page of one of the pack's own paginated queries.
func (p *Provider) rowsPage(ctx context.Context, query string, args map[string]string, cursor string) ([]map[string]any, string, error) {
	if p.reader == nil {
		return nil, "", fmt.Errorf("wholesale: the pack has no reader; this capability cannot run")
	}
	return p.reader.RowsPage(ctx, query, args, cursor)
}

// asString narrows a payload value to text.
func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// trimmed is asString plus the trim every caller here wants.
func trimmed(v any) string { return strings.TrimSpace(asString(v)) }

// Writer is how this pack PERSISTS. It renders one of the pack's own
// mutations and runs it under the caller's own actor.
//
// THE MUTATIONS ARE @serverOnly. Each is reached only from the builtin that
// judges it, so a client calling one directly -- a decision appended around
// the legality fold -- is refused by the engine. engineWriter stamps internal
// origin for exactly its one call.
//
// THE STAMP IS THE ONLY WAY IN. Every concept the pack declares is
// @serverWritten (ruling 50): a write of any of its rows without internal
// origin -- the public insert() included -- is refused, so this writer is the
// one path that writes them.
//
// THE STAMP ESCAPES THE ROW GATE. A write at internal origin is not
// owner-checked (component/memql/rowauthz_write_guard.go), so a row's tier
// does not protect it from this writer: whoever reaches a builtin that
// writes could otherwise write over any row at an id they can name. So each
// builtin authorizes its caller ITSELF, before it calls this:
//
//   - wholesaleSubmitApplication: the store's applicationsOpen gate, an
//     accountId refused unless it is the named site's, then the id
//     established absent (read as the system) and the write fenced on "row
//     absent", so no application is ever written over;
//   - wholesaleRecordDecision: the wholesale part, judged by the organization
//     gate against the APPLICATION's organization; then a signed-in person
//     who is not one of the cluster's operators (deciderOf -- the decider is
//     always the caller); then the application read under the CALLER, so a
//     caller who cannot read it cannot decide it; then the legality fold over the
//     WHOLE log, read as the system (X27);
//   - wholesaleRetryAccess: the part against the application's organization,
//     the application read under the caller, then the decision log and the
//     entitlement row decide whether there is access to retry;
//   - wholesaleUpdateSettings: the part against the SITE's organization, then
//     the store gate (siteForStore): the site serves the store, the store is
//     set up -- its settings row is tied to an organization -- and the site is
//     of it; it merges into the row the operators set up and keeps the row's
//     owner, organization and site;
//   - wholesaleSetSettings: internal origin or a cluster operator only,
//     never the part. The row is always tied to an organization: the named
//     site's, or the one bound site's, else refused (NameTheSiteSentence). An
//     existing row keeps its owner, and is tied only to a site of that owner
//     (AnotherOwnerSentence); a NEW ROW'S OWNER is the site's owner;
//   - wholesaleProvisionEntitlement / wholesaleRevokeEntitlement: internal
//     origin only (the store's automation), then the decision log.
//   - wholesaleBackfillAccounts: internal origin or a cluster operator
//     only, refusing everyone else before it reads; a row already tied is
//     never rewritten;
//   - wholesaleAcceptTerms: no capability -- the token is the authority. The
//     token's hash must match a link of the store the call names, read under
//     the caller, unused, unexpired and of a submitted application, before
//     anything else is read; an existing acceptance refuses; the acceptance is
//     fenced on row-absent at its derived id, so none is ever written over, and
//     only then is the link the token named written again with its usedAt;
//   - the private link itself: written by mintLink, which only the resolver
//     that mails the link calls, as the application's merchant.
//
// SEPARATE FROM Caller BECAUSE THE CALL FORMS DIFFER AND THE DIFFERENCE IS
// EASY TO LOSE: a mutation is `name(args)`, a builtin is `builtin
// name(args)`. Separate from rowReader because a write is not a read and a
// test that fakes one should not silently fake the other.
type rowWriter interface {
	Write(ctx context.Context, mutation string, args map[string]any) error
	// Insert is Write for a mutation whose row id the engine generates, and
	// answers that id: an append-only row the pack must name back to its caller.
	Insert(ctx context.Context, mutation string, args map[string]any) (string, error)
}

// engineWriter is the production implementation.
type engineWriter struct {
	engine memql.IntegrationEngineAccess
}

func (w *engineWriter) Write(ctx context.Context, mutation string, args map[string]any) error {
	_, err := w.execute(ctx, mutation, args)
	return err
}

func (w *engineWriter) Insert(ctx context.Context, mutation string, args map[string]any) (string, error) {
	result, err := w.execute(ctx, mutation, args)
	if err != nil {
		return "", err
	}
	// The engine answers the row it wrote, at the id it generated.
	rows := memql.MaterializeRows(result)
	if len(rows) == 0 || trimmed(rows[0]["id"]) == "" {
		return "", fmt.Errorf("wholesale: %s wrote a row the engine did not name", mutation)
	}
	return trimmed(rows[0]["id"]), nil
}

// execute runs one of the pack's mutations: the writer's ONE stamp.
func (w *engineWriter) execute(ctx context.Context, mutation string, args map[string]any) (*memql.ExecuteResult, error) {
	if w == nil || w.engine == nil {
		return nil, fmt.Errorf("wholesale: the pack has no engine handle, so %q cannot be written", mutation)
	}
	// INLINE, so the internal stamp dies with this one call (auth.ContextWithInternalOrigin).
	result, err := w.engine.Execute(auth.ContextWithInternalOrigin(ctx), renderMutationCall(mutation, args))
	if err != nil {
		return nil, fmt.Errorf("wholesale: %s: %w", mutation, err)
	}
	return result, nil
}

// renderMutationCall writes the invocation a pack mutation becomes.
//
// NO KEYWORD, which is the mutation form -- `builtin ` and `query ` are the
// other two and both would be wrong here. Named and asserted for the same
// reason renderBuiltinCall is: the engine is the only thing that can tell
// these apart, and a fake writer cannot.
func renderMutationCall(mutation string, args map[string]any) string {
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(mutation)
	b.WriteByte('(')
	for i, k := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(renderLiteral(args[k]))
	}
	b.WriteByte(')')
	return b.String()
}

// renderLiteral writes one argument as MemQL source.
//
// QuoteString FOR EVERY STRING, never Go quoting: it is the engine's own
// literal escaping, and a company name is caller-supplied text that reaches
// this from a public form.
func renderLiteral(v any) string {
	switch t := v.(type) {
	case string:
		return langparser.QuoteString(t)
	case int:
		return strconv.Itoa(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return langparser.QuoteString(asString(v))
	}
}

// slugOf renders any identifier as a BARE SLUG a derived row id may carry.
//
// THE ENGINE REFUSES A ROW ID WITH COLONS IN IT: a shortId "must be a bare
// slug/UUID (no colons) or the concept-prefixed form" (see
// docs/public/concepts/identifiers.md). A store id may itself be a
// v1:-prefixed row id, so composing one into a derived id without this
// produces an id every write refuses -- which the live e2e suite found and
// no fixture could, because a fixture never validates an id.
func slugOf(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// userIDPrefix is the canonical form's prefix for a person's id.
const userIDPrefix = "v1:identity:user:"

// userRef is a person's id in the form the read predicate matches.
//
// THE ROW TIER'S READ ARM COMPARES ownerUserId WITH actor.userId AS WRITTEN
// (the filter canonicalizes @relationship fields only), and a real actor's
// userId is canonical: the identity resolver admits only a v1:identity:user:
// subject and keeps it whole (component/auth/identity_resolver.go), and the
// owner the shopper handler borrows is the site row's stored owner, which the
// engine keeps canonical. So every owner this pack copies from another row --
// a site's, an application's, a settings row's -- is written in that form,
// whatever form the source row used, or its merchant could not read it.
//
// ONLY A PERSON'S ID IS CANONICALIZED. A bare id is a person's, and gains the
// prefix; a canonical one is kept; an id already prefixed for another kind --
// `system:x`, or another concept's canonical id -- is left exactly as it is,
// because turning it into a user id would name a person who does not exist.
func userRef(id string) string {
	id = strings.TrimSpace(id)
	switch {
	case id == "", strings.HasPrefix(id, userIDPrefix), strings.Contains(id, ":"):
		return id
	}
	return userIDPrefix + id
}

// applicationRef is an application's id in canonical form.
func applicationRef(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, applicationConcept+":") {
		return id
	}
	return applicationConcept + ":" + bareID(id)
}

// rowsForApplication runs a query keyed by applicationId under BOTH forms of
// the id, bare and canonical, and answers the rows of both, newest first.
//
// THE ENGINE DOES NOT JOIN THE TWO FORMS: a decision or an entitlement written
// naming the bare id is not found by a read naming the canonical one, and the
// reverse (measured in access_db_test.go). Rows were written in whichever form
// each caller used before decisions were written canonical, so a read that
// asked one form would miss part of a log -- and a fold over part of a log is
// an application shown as submitted after somebody rejected it.
func (p *Provider) rowsForApplication(ctx context.Context, query, applicationID string) ([]map[string]any, error) {
	seen := map[string]bool{}
	var out []map[string]any
	for _, form := range uniqueForms(bareID(strings.TrimSpace(applicationID)), applicationRef(applicationID)) {
		rows, err := p.rowsFor(ctx, query, map[string]string{"applicationId": form})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if id := trimmed(row["id"]); id != "" {
				if seen[id] {
					continue
				}
				seen[id] = true
			}
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return rowTime(out[i]["createdAt"]).After(rowTime(out[j]["createdAt"])) })
	return out, nil
}

// rowsForApplications is rowsForApplication for many applications at once:
// query (decisionsForApplicationIds or entitlementsForApplicationIds) asked
// with BOTH forms of every id and PAGED TO ITS END -- a fold over a short
// answer would show an application as submitted after somebody decided it --
// its rows without repeats, grouped by the application's bare id, each group
// newest first. Fifty applications per read, so a store's list is a read per
// fifty applications rather than two per application.
func (p *Provider) rowsForApplications(ctx context.Context, query string, applicationIDs []string) (map[string][]map[string]any, error) {
	out := map[string][]map[string]any{}
	seen := map[string]bool{}
	for from := 0; from < len(applicationIDs); from += batchSize {
		to := from + batchSize
		if to > len(applicationIDs) {
			to = len(applicationIDs)
		}
		var forms []string
		for _, id := range applicationIDs[from:to] {
			forms = append(forms, uniqueForms(bareID(strings.TrimSpace(id)), applicationRef(id))...)
		}
		err := p.eachPage(ctx, query, map[string]string{}, "applicationIds", forms, func(row map[string]any) {
			if id := trimmed(row["id"]); id != "" {
				if seen[id] {
					return
				}
				seen[id] = true
			}
			key := bareID(trimmed(row["applicationId"]))
			out[key] = append(out[key], row)
		})
		if err != nil {
			return nil, err
		}
	}
	for key := range out {
		group := out[key]
		sort.SliceStable(group, func(i, j int) bool { return rowTime(group[i]["createdAt"]).After(rowTime(group[j]["createdAt"])) })
	}
	return out, nil
}

// batchSize is how many applications one batch read names.
const batchSize = 50

// eachPage reads every page of a paginated query with one list argument, in
// order, handing each row to visit.
func (p *Provider) eachPage(ctx context.Context, query string, args map[string]string, listName string, list []string,
	visit func(map[string]any)) error {
	cursor := ""
	for {
		rows, next, err := p.rowsPageWithList(ctx, query, args, listName, list, cursor)
		if err != nil {
			return err
		}
		for _, row := range rows {
			visit(row)
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

// rowsPageWithList runs one page of a paginated query with one list argument.
func (p *Provider) rowsPageWithList(ctx context.Context, query string, args map[string]string, listName string,
	list []string, cursor string) ([]map[string]any, string, error) {
	if p.reader == nil {
		return nil, "", fmt.Errorf("wholesale: the pack has no reader; this capability cannot run")
	}
	return p.reader.RowsPageWithList(ctx, query, args, listName, list, cursor)
}

// uniqueForms drops an empty or repeated form.
func uniqueForms(forms ...string) []string {
	var out []string
	for _, f := range forms {
		if f != "" && (len(out) == 0 || out[len(out)-1] != f) {
			out = append(out, f)
		}
	}
	return out
}

// write runs one of the pack's own mutations through the seam.
func (p *Provider) write(ctx context.Context, mutation string, args map[string]any) error {
	if p.writer == nil {
		return fmt.Errorf("wholesale: the pack has no writer; %q cannot be persisted", mutation)
	}
	return p.writer.Write(ctx, mutation, args)
}

// insert runs one of the pack's own mutations whose row id the engine
// generates, and answers that id.
func (p *Provider) insert(ctx context.Context, mutation string, args map[string]any) (string, error) {
	if p.writer == nil {
		return "", fmt.Errorf("wholesale: the pack has no writer; %q cannot be persisted", mutation)
	}
	return p.writer.Insert(ctx, mutation, args)
}

// confirm is the ONE node a write capability replies with.
//
// A RECEIPT, NOT THE ROW. The row is the mutation's, and echoing a copy of
// it here would be a second, unversioned answer to "what was written" that
// nothing keeps in step. What a caller needs back is that it happened and
// against what, so that is what this carries.
func confirm(kind string, fields map[string]any) ([]memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("wholesale: marshal %s: %w", kind, err)
	}
	now := time.Now().UTC()
	return []memorynodes.MemoryNode{{
		// NOT A ROW ID AND NOT A CONCEPT ID. A receipt is never stored --
		// the engine returns a builtin's nodes as the expression's value and
		// persists nothing -- so these two fields exist to identify the
		// ANSWER, not a row. They deliberately do not take the "v1:" form a
		// stored id carries, because a reader who saw one would reasonably
		// go looking for the row it names.
		ID:        kind + "@" + strconv.FormatInt(now.UnixNano(), 36),
		Concept:   kind,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: now,
		Payload:   payload,
	}}, nil
}
