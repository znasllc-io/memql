package wholesalepack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// documents.go -- THE DOCUMENTS A STORE PUBLISHES: its terms, its MAP policy and
// its price sheet, each a series of append-only versions (U8).
//
// # A version is written once and never again
//
// What an applicant accepted must still be readable after the store publishes
// the next version, so a publish is a NEW ROW at an id the engine generates,
// and there is no write that changes one. The current version of a kind is its
// newest. The body is Markdown text kept exactly as given, and sha256 is the
// digest of exactly those bytes, so an acceptance can name what was shown.
//
// # Who is admitted, and what is read as whom
//
// Both builtins are gated by the wholesale part, judged by the organization
// gate against the SITE's organization, and the site is then checked to serve
// the store (siteForStore) before anything keyed by the store is read. After
// that the versions are read as the system: the caller has been established.
// A version is the store's merchant's (the site's owner), tied to the site's
// organization, published by the caller, and written as the merchant -- at
// internal origin, which the row gate does not owner-check, so the gate and
// the site check are what authorize it.
//
// # A store id has two spellings
//
// A version is written under the bare store id, the form the pack writes its
// new rows in, and every read asks every form (storeIDForms), so a version
// written under the canonical form is never missed.

// The three kinds of document a store publishes, in the order a reader meets them.
const (
	DocumentTerms      = "terms"
	DocumentMap        = "map"
	DocumentPriceSheet = "priceSheet"
)

// DocumentKinds is every kind, in order.
var DocumentKinds = []string{DocumentTerms, DocumentMap, DocumentPriceSheet}

// DefaultLinkLifetimeDays is how many days a link to an applicant stays usable
// when the store names none.
const DefaultLinkLifetimeDays = 30

// DefaultAcceptanceText is the sentence an applicant accepts the terms with
// when the store names none.
const DefaultAcceptanceText = "I have read and accept the Wholesale Terms and Conditions and the MAP Policy shown above."

// TermsFirstSentence refuses opening applications while the terms are required
// and a document has no version. It is for the store's people, shown as
// written; a shopper is refused as a closed store refuses them.
const TermsFirstSentence = "Publish the terms, the MAP policy and the price sheet first."

// errTermsFirst is termsRuleRefusal's refusal.
var errTermsFirst = errors.New(TermsFirstSentence)

// maxDocumentBytes is the largest body a version may hold.
const maxDocumentBytes = 200 * 1024

// maxDocumentLabel is the longest label, in characters, once trimmed.
const maxDocumentLabel = 120

// The link lifetime a store may name, in days.
const (
	minLinkLifetimeDays = 1
	maxLinkLifetimeDays = 365
)

// documentVersionConcept is the document version's concept id.
const documentVersionConcept = "v1:wholesale:documentVersion"

// documentsActor is the system actor that reads a store's document versions
// once the caller has been admitted.
const documentsActor = "wholesale-documents"

// sha256Hex is the lowercase hex SHA-256 digest of text's UTF-8 bytes.
func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// validDocumentKind refuses a kind outside the three, naming them.
func validDocumentKind(kind string) error {
	for _, k := range DocumentKinds {
		if kind == k {
			return nil
		}
	}
	return fmt.Errorf("wholesale: %q is not a document kind; a document is terms, map or priceSheet", kind)
}

// publishDocument answers wholesalePublishDocument: one new version.
//
// EVERYTHING THE CALL NAMES IS CHECKED BEFORE ANYTHING IS READ: the kind, the
// label, the body and the person publishing. Then the site is checked to serve
// the store, and the version is written as the site's owner.
func (p *Provider) publishDocument(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := trimmed(args["storeId"])
	if storeID == "" {
		return nil, fmt.Errorf("wholesale: storeId is required; documents are per store")
	}
	kind := trimmed(args["kind"])
	if err := validDocumentKind(kind); err != nil {
		return nil, err
	}
	label := trimmed(args["label"])
	if n := utf8.RuneCountInString(label); n < 1 || n > maxDocumentLabel {
		return nil, fmt.Errorf("wholesale: a document's label is 1 to %d characters; this one is %d", maxDocumentLabel, n)
	}
	// THE BODY AS GIVEN: its digest is over exactly these bytes.
	body := asString(args["body"])
	if len(body) == 0 || len(body) > maxDocumentBytes {
		return nil, fmt.Errorf("wholesale: a document's body is 1 to %d bytes; this one is %d", maxDocumentBytes, len(body))
	}
	publisher := ""
	if ac, ok := auth.AccessFromContext(ctx); ok {
		publisher = userRef(ac.UserId)
	}
	if publisher == "" {
		return nil, errors.New("wholesale: a document is published by a signed-in person")
	}
	site, err := p.siteForStore(ctx, trimmed(args["siteId"]), storeID)
	if err != nil {
		return nil, err
	}
	owner := userRef(trimmed(site["ownerUserId"]))
	if owner == "" {
		return nil, fmt.Errorf("wholesale: site %q names no owner, so there is no merchant to own store %q's documents",
			trimmed(args["siteId"]), storeID)
	}
	digest := sha256Hex(body)
	// EVERY ARGUMENT NAMED, nothing left for the mutation to stamp.
	id, err := p.insert(auth.ContextWithUserActor(ctx, owner), "appendWholesaleDocumentVersion", map[string]any{
		"ownerUserId": owner,
		"storeId":     bareID(storeID),
		"accountId":   trimmed(site["accountId"]),
		"kind":        kind,
		"label":       label,
		"body":        body,
		"sha256":      digest,
		"publishedBy": publisher,
		"publishedAt": p.clock().Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, err
	}
	return confirm("wholesale:document:published", map[string]any{"documentVersionId": id, "sha256": digest})
}

// documents answers wholesaleDocuments: every kind with its current version and
// its history, or, with versionId, that one version of this store.
func (p *Provider) documents(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID, _, err := p.admitStore(ctx, args)
	if err != nil {
		return nil, err
	}
	people := p.people(ctx)
	if versionID := trimmed(args["versionId"]); versionID != "" {
		version, err := p.storeDocumentVersion(ctx, storeID, versionID)
		if err != nil {
			return nil, err
		}
		out := versionView(version, people)
		out["kind"] = trimmed(version["kind"])
		out["body"] = asString(version["body"])
		return confirm("wholesale:document", map[string]any{"version": out})
	}
	kinds := make([]map[string]any, 0, len(DocumentKinds))
	for _, kind := range DocumentKinds {
		versions, err := p.documentVersionsOf(ctx, storeID, kind)
		if err != nil {
			return nil, err
		}
		history := make([]map[string]any, 0, len(versions))
		for _, v := range versions {
			history = append(history, versionView(v, people))
		}
		var current any
		if len(versions) > 0 {
			view := versionView(versions[0], people)
			view["body"] = asString(versions[0]["body"])
			current = view
		}
		kinds = append(kinds, map[string]any{"kind": kind, "current": current, "history": history})
	}
	return confirm("wholesale:documents", map[string]any{"kinds": kinds})
}

// versionView is one version as a reply shows it, without its body.
func versionView(v map[string]any, people *peopleNames) map[string]any {
	publisher := trimmed(v["publishedBy"])
	return map[string]any{
		"id":              trimmed(v["id"]),
		"label":           asString(v["label"]),
		"publishedAt":     timeText(v["publishedAt"]),
		"publishedBy":     publisher,
		"publishedByName": people.name(publisher),
		"sha256":          trimmed(v["sha256"]),
	}
}

// storeDocumentVersion is one of the store's versions by its id, in either
// form. A version of another store is answered exactly as an unknown one.
func (p *Provider) storeDocumentVersion(ctx context.Context, storeID, versionID string) (map[string]any, error) {
	for _, kind := range DocumentKinds {
		versions, err := p.documentVersionsOf(ctx, storeID, kind)
		if err != nil {
			return nil, err
		}
		for _, v := range versions {
			if bareID(trimmed(v["id"])) == bareID(versionID) {
				return v, nil
			}
		}
	}
	return nil, fmt.Errorf("wholesale: there is no document version %q of this store", versionID)
}

// documentVersionsOf is every version of one of the store's documents, newest
// first, read as the system under every form of the store id and paged to the
// end, each version once. Its caller has admitted the caller.
func (p *Provider) documentVersionsOf(ctx context.Context, storeID, kind string) ([]map[string]any, error) {
	sys := auth.ContextWithSystemActor(ctx, documentsActor)
	seen := map[string]bool{}
	var out []map[string]any
	for _, form := range storeIDForms(storeID) {
		err := p.eachPage(sys, "documentVersionsForStore", map[string]string{"storeId": form, "kind": kind}, "", nil,
			func(row map[string]any) {
				if id := bareID(trimmed(row["id"])); id != "" {
					if seen[id] {
						return
					}
					seen[id] = true
				}
				out = append(out, row)
			})
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return versionTime(out[i]).After(versionTime(out[j])) })
	return out, nil
}

// versionTime is when a version was written: its row's createdAt, or, on a row
// read without one, its publishedAt.
func versionTime(v map[string]any) time.Time {
	if at := rowTime(v["createdAt"]); !at.IsZero() {
		return at
	}
	return rowTime(v["publishedAt"])
}

// currentDocuments is the store's current version of each kind that has one,
// by kind; a kind with no version is absent. Its caller has admitted the caller.
func (p *Provider) currentDocuments(ctx context.Context, storeID string) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	for _, kind := range DocumentKinds {
		versions, err := p.documentVersionsOf(ctx, storeID, kind)
		if err != nil {
			return nil, err
		}
		if len(versions) > 0 {
			out[kind] = versions[0]
		}
	}
	return out, nil
}

// termsRuleRefusal refuses a store that would take applications (open) while
// requiring its terms (termsRequired) with a kind that has no version. Either
// one false needs no document and reads nothing.
func (p *Provider) termsRuleRefusal(ctx context.Context, storeID string, open, termsRequired bool) error {
	if !open || !termsRequired {
		return nil
	}
	current, err := p.currentDocuments(ctx, storeID)
	if err != nil {
		return err
	}
	for _, kind := range DocumentKinds {
		if current[kind] == nil {
			return errTermsFirst
		}
	}
	return nil
}

// documentsSummary is the current version of each kind as the settings answer
// it: {label, sha256}, or nil for a kind with none.
func (p *Provider) documentsSummary(ctx context.Context, storeID string) (map[string]any, error) {
	current, err := p.currentDocuments(ctx, storeID)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, kind := range DocumentKinds {
		var summary any
		if v := current[kind]; v != nil {
			summary = map[string]any{"label": asString(v["label"]), "sha256": trimmed(v["sha256"])}
		}
		out[kind] = summary
	}
	return out, nil
}

// storeDocumentVersions is every version of every kind of the store, kind by
// kind, each newest first: the export's.
func (p *Provider) storeDocumentVersions(ctx context.Context, storeID string) ([]map[string]any, error) {
	out := []map[string]any{}
	for _, kind := range DocumentKinds {
		versions, err := p.documentVersionsOf(ctx, storeID, kind)
		if err != nil {
			return nil, err
		}
		out = append(out, versions...)
	}
	return out, nil
}

// clock is the engine's time, or the test seam's.
func (p *Provider) clock() time.Time {
	if p.now != nil {
		return p.now().UTC()
	}
	return time.Now().UTC()
}

// intArg narrows an argument to a whole number, or reports it is not one.
func intArg(v any) (int, bool) {
	var n float64
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		n = float64(t)
	case float64:
		n = t
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, false
		}
		n = f
	default:
		return 0, false
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || n > math.MaxInt32 || n < math.MinInt32 {
		return 0, false
	}
	return int(n), true
}

// validLinkLifetime refuses a link lifetime outside 1 to 365 days.
func validLinkLifetime(v any) (int, error) {
	days, ok := intArg(v)
	if !ok || days < minLinkLifetimeDays || days > maxLinkLifetimeDays {
		return 0, fmt.Errorf("wholesale: a link stays usable for %d to %d days; %v is not", minLinkLifetimeDays,
			maxLinkLifetimeDays, v)
	}
	return days, nil
}
