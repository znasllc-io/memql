package wholesalepack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// links_test.go -- the private terms link, the acceptance and the price-sheet
// link, as decisions over rows behind the reader and writer seams, with a
// fake clock.

const (
	linkStore    = "store-links-1"
	linkSite     = "site-links-1"
	linkMerchant = "v1:identity:user:merchant-links"
	linkApp      = "app-links-1"
	linkEmail    = "ada@kestrel.example.com"
)

var linkClock = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

// linkWorld is one store's rows as the engine would answer them: a link is
// read by its owner and the system only (the row tier), and a query keyed by an
// application id matches the id as written.
type linkWorld struct {
	clock       time.Time
	apps        []map[string]any
	decisions   []map[string]any
	ents        []map[string]any
	settings    map[string]any
	docs        []map[string]any
	links       map[string]map[string]any // row id -> current version
	acceptances map[string]map[string]any // row id -> the row
	reads       []string                  // query names, in order
	readArgs    []map[string]string
	writes      []writeCall
	// afterAcceptanceRead runs once an acceptance read has answered.
	afterAcceptanceRead func()
	log                 bytes.Buffer
}

func newLinkWorld() *linkWorld {
	w := &linkWorld{
		clock: linkClock,
		apps: []map[string]any{{"id": applicationConcept + ":" + linkApp, "storeId": linkStore, "siteId": linkSite,
			"ownerUserId": linkMerchant, "accountId": "acct-links", "companyName": "Kestrel Cycles",
			"applicantName": "Ada Example", "applicantEmail": linkEmail, "createdAt": linkClock.Add(-time.Hour)}},
		settings: map[string]any{"id": SettingsRowID(linkStore), "storeId": linkStore, "applicationsOpen": true,
			"termsRequired": true, "linkLifetimeDays": 30},
		links:       map[string]map[string]any{},
		acceptances: map[string]map[string]any{},
	}
	w.publish(DocumentTerms, "Terms 2026", "Net 30. Kestrel Cycles trade terms.")
	w.publish(DocumentMap, "MAP 2026", "Advertise at list price.")
	w.publish(DocumentPriceSheet, "Spring list", "Frame 410.00")
	return w
}

// publish appends a version of kind, newer than every version before it.
func (w *linkWorld) publish(kind, label, body string) map[string]any {
	v := map[string]any{"id": documentVersionConcept + ":" + kind + "-" + fmt.Sprint(len(w.docs)), "storeId": linkStore,
		"kind": kind, "label": label, "body": body, "sha256": sha256Hex(body), "ownerUserId": linkMerchant,
		"publishedAt": w.clock.Add(time.Duration(len(w.docs)) * time.Minute).Format(time.RFC3339Nano),
		"createdAt":   w.clock.Add(time.Duration(len(w.docs)) * time.Minute)}
	w.docs = append(w.docs, v)
	return v
}

func (w *linkWorld) provider() *Provider {
	return &Provider{reader: w, writer: w, now: func() time.Time { return w.clock },
		logger: slog.New(slog.NewTextHandler(&w.log, nil))}
}

func (w *linkWorld) app() map[string]any { return w.apps[0] }

func linkVisible(ctx context.Context, row map[string]any) bool {
	return isSystem(ctx) || actorOf(ctx) == trimmed(row["ownerUserId"])
}

func copyRow(row map[string]any) map[string]any {
	out := make(map[string]any, len(row))
	for k, v := range row {
		out[k] = v
	}
	return out
}

func (w *linkWorld) Rows(ctx context.Context, query string, args map[string]string) ([]map[string]any, error) {
	w.reads = append(w.reads, query)
	w.readArgs = append(w.readArgs, args)
	switch query {
	case "privateLinkByTokenHash":
		var out []map[string]any
		for _, l := range w.links {
			if trimmed(l["storeId"]) == args["storeId"] && trimmed(l["tokenHash"]) == args["tokenHash"] && linkVisible(ctx, l) {
				row := copyRow(l)
				delete(row, "tokenHash") // privateLinkPublic
				out = append(out, row)
			}
		}
		return out, nil
	case "privateLinkForApplication":
		var out []map[string]any
		for _, l := range w.links {
			if trimmed(l["applicationId"]) == args["applicationId"] && trimmed(l["purpose"]) == args["purpose"] && linkVisible(ctx, l) {
				out = append(out, copyRow(l))
			}
		}
		return out, nil
	case "acceptanceForApplication":
		var out []map[string]any
		for _, a := range w.acceptances {
			if trimmed(a["applicationId"]) == args["applicationId"] && linkVisible(ctx, a) {
				out = append(out, copyRow(a))
			}
		}
		if w.afterAcceptanceRead != nil {
			w.afterAcceptanceRead()
		}
		return out, nil
	case "applicationById":
		for _, app := range w.apps {
			if bareID(trimmed(app["id"])) == bareID(args["applicationId"]) {
				return []map[string]any{app}, nil
			}
		}
	case "decisionsForApplication":
		return newestFirst(matchingApplication(w.decisions, args["applicationId"])), nil
	case "entitlementForApplication":
		return newestFirst(matchingApplication(w.ents, args["applicationId"])), nil
	case "wholesaleSettingsById":
		if w.settings != nil && args["settingsId"] == SettingsRowID(linkStore) {
			return []map[string]any{w.settings}, nil
		}
	case "documentVersionsForStore":
		return docsMatching(w.docs, args["storeId"], args["kind"]), nil
	case "documentVersionBriefsForStore":
		return briefsOf(docsMatching(w.docs, args["storeId"], args["kind"])), nil
	case "wholesaleDocumentVersionById":
		return docByID(w.docs, args["documentVersionId"]), nil
	case "wholesaleDocumentVersionBriefById":
		return briefsOf(docByID(w.docs, args["documentVersionId"])), nil
	}
	return nil, nil
}

func matchingApplication(rows []map[string]any, applicationID string) []map[string]any {
	var out []map[string]any
	for _, row := range rows {
		if trimmed(row["applicationId"]) == applicationID {
			out = append(out, row)
		}
	}
	return out
}

func (w *linkWorld) RowsWithList(ctx context.Context, query string, args map[string]string, _ string, _ []string) ([]map[string]any, error) {
	return w.Rows(ctx, query, args)
}

func (w *linkWorld) RowsPage(ctx context.Context, query string, args map[string]string, _ string) ([]map[string]any, string, error) {
	rows, err := w.Rows(ctx, query, args)
	return rows, "", err
}

func (w *linkWorld) RowsPageWithList(ctx context.Context, query string, args map[string]string, _ string, list []string, _ string) ([]map[string]any, string, error) {
	switch query {
	case "acceptancesForApplicationIds", "decisionsForApplicationIds", "entitlementsForApplicationIds":
		w.reads = append(w.reads, query)
		src := map[string][]map[string]any{"decisionsForApplicationIds": w.decisions, "entitlementsForApplicationIds": w.ents}[query]
		if query == "acceptancesForApplicationIds" {
			for _, a := range w.acceptances {
				src = append(src, a)
			}
		}
		var out []map[string]any
		for _, id := range list {
			out = append(out, matchingApplication(src, id)...)
		}
		return newestFirst(out), "", nil
	}
	rows, err := w.Rows(ctx, query, args)
	return rows, "", err
}

// Write applies a pack mutation to the world, honouring a fence on "row absent".
func (w *linkWorld) Write(ctx context.Context, mutation string, args map[string]any) error {
	switch mutation {
	case "putPrivateLinkRow":
		id := asString(args["linkId"])
		row := copyRow(args)
		delete(row, "linkId")
		row["id"] = "v1:wholesale:privateLink:" + id
		row["createdAt"] = w.clock
		w.links[id] = row
	case "putAcceptanceRow":
		id := asString(args["acceptanceId"])
		if memql.HasRowVersionFence(ctx, "v1:wholesale:acceptance", id) && w.acceptances[id] != nil {
			return fmt.Errorf("wholesale: putAcceptanceRow: %w", memql.ErrRowVersionChanged)
		}
		row := copyRow(args)
		delete(row, "acceptanceId")
		row["id"] = "v1:wholesale:acceptance:" + id
		row["createdAt"] = w.clock
		w.acceptances[id] = row
	}
	w.writes = append(w.writes, writeCall{mutation: mutation, args: args, actor: actorOf(ctx), ctx: ctx})
	return nil
}

func (w *linkWorld) Insert(ctx context.Context, mutation string, args map[string]any) (string, error) {
	return "generated", w.Write(ctx, mutation, args)
}

func (w *linkWorld) count(mutation string) int {
	n := 0
	for _, c := range w.writes {
		if c.mutation == mutation {
			n++
		}
	}
	return n
}

// shopper is the borrowed authority the shopper handler runs under: the site
// owner, who is the application's merchant.
func shopper() context.Context { return auth.ContextWithUserActor(context.Background(), linkMerchant) }

// mint makes a link for the world's application and answers its token.
func (w *linkWorld) mint(t *testing.T, p *Provider, purpose string) string {
	t.Helper()
	token, _, err := p.mintLink(context.Background(), w.app(), purpose, w.clock)
	if err != nil {
		t.Fatalf("mintLink: %v", err)
	}
	return token
}

// refusalCode is the named refusal err carries, or "".
func refusalCode(err error) string {
	var r *memql.ShopperRefusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

func termsArgs(token string) map[string]any {
	return map[string]any{"requestToken": token, "storeId": linkStore, "siteId": linkSite}
}

// acceptArgs is a good post: both boxes on, a name, and the fingerprints of the
// current terms and MAP policy.
func (w *linkWorld) acceptArgs(token string) map[string]any {
	args := termsArgs(token)
	args["acceptTerms"], args["acceptMap"], args["signedName"] = "on", "on", "  Ada Example  "
	args["statementSha256"] = sha256Hex(EffectiveSettings(w.settings).AcceptanceText)
	for field, kind := range map[string]string{"termsSha256": DocumentTerms, "mapSha256": DocumentMap} {
		// A kind with no version is posted a well-formed fingerprint of nothing.
		args[field] = strings.Repeat("0", 64)
		if current := docsMatching(w.docs, linkStore, kind); len(current) > 0 {
			args[field] = trimmed(current[0]["sha256"])
		}
	}
	return args
}

func (w *linkWorld) onlyAcceptance(t *testing.T) map[string]any {
	t.Helper()
	if len(w.acceptances) != 1 {
		t.Fatalf("%d acceptances, want 1", len(w.acceptances))
	}
	for _, a := range w.acceptances {
		return a
	}
	return nil
}

func (w *linkWorld) termsLink() map[string]any {
	return w.links[PrivateLinkRowID(LinkPurposeTerms, linkApp)]
}

// ---------------------------------------------------------------------------
// The token
// ---------------------------------------------------------------------------

// THE TOKEN IS NEVER STORED: no write carries it, and the link holds its hex
// SHA-256.
func TestTheTokenIsNeverStored(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	token := w.mint(t, p, LinkPurposeTerms)
	if len(token) < 40 || strings.ContainsAny(token, "+/=") {
		t.Fatalf("the token %q is not 32 bytes of base64url without padding", token)
	}
	for _, c := range w.writes {
		for k, v := range c.args {
			if strings.Contains(asString(v), token) {
				t.Fatalf("%s wrote the token in %s", c.mutation, k)
			}
		}
	}
	link := w.termsLink()
	if link == nil || link["tokenHash"] != sha256Hex(token) {
		t.Fatalf("the link is %+v, want tokenHash the hex sha256 of the token", link)
	}
	if link["ownerUserId"] != linkMerchant || link["accountId"] != "acct-links" || link["storeId"] != linkStore ||
		link["applicationId"] != applicationConcept+":"+linkApp || link["purpose"] != LinkPurposeTerms ||
		link["siteId"] != linkSite || link["usedAt"] != "" {
		t.Fatalf("the link is %+v", link)
	}
	if got, want := asString(link["expiresAt"]), linkClock.Add(30*24*time.Hour).Format(time.RFC3339Nano); got != want {
		t.Fatalf("expiresAt = %q, want %q (issuedAt plus the store's thirty days)", got, want)
	}
	if w.writes[0].actor != linkMerchant {
		t.Fatalf("the link was written as %q, want the application's merchant", w.writes[0].actor)
	}
	sheet := w.mint(t, p, LinkPurposePriceSheet)
	if s := w.links[PrivateLinkRowID(LinkPurposePriceSheet, linkApp)]; s == nil || s["expiresAt"] != "" || s["tokenHash"] != sha256Hex(sheet) {
		t.Fatalf("the price-sheet link is %+v, want no expiry", s)
	}
}

// THE TWO SPELLINGS OF AN APPLICATION ID DERIVE ONE ROW ID.
func TestTheDerivedIdsTakeOneSpellingOfTheApplication(t *testing.T) {
	if PrivateLinkRowID(LinkPurposeTerms, linkApp) != PrivateLinkRowID(LinkPurposeTerms, applicationConcept+":"+linkApp) ||
		PrivateLinkRowID(LinkPurposeTerms, linkApp) != "wholesale-link-terms-"+linkApp {
		t.Fatalf("PrivateLinkRowID = %q", PrivateLinkRowID(LinkPurposeTerms, applicationConcept+":"+linkApp))
	}
	if AcceptanceRowID(linkApp) != AcceptanceRowID(applicationConcept+":"+linkApp) || AcceptanceRowID(linkApp) != "wholesale-acceptance-"+linkApp {
		t.Fatalf("AcceptanceRowID = %q", AcceptanceRowID(applicationConcept+":"+linkApp))
	}
}

// ---------------------------------------------------------------------------
// The terms link
// ---------------------------------------------------------------------------

// A TERMS LINK EXPIRES at issuedAt plus the store's lifetime: at the instant,
// and after it.
func TestAnExpiredTermsLinkIsRefusedAsLinkExpired(t *testing.T) {
	for _, after := range []time.Duration{30 * 24 * time.Hour, 31 * 24 * time.Hour} {
		w := newLinkWorld()
		p := w.provider()
		token := w.mint(t, p, LinkPurposeTerms)
		w.clock = linkClock.Add(after)
		if _, err := p.termsForLink(shopper(), termsArgs(token), 0); refusalCode(err) != RefusalLinkExpired {
			t.Fatalf("%v after issue: %v, want %s", after, err, RefusalLinkExpired)
		}
		if _, err := p.acceptTerms(shopper(), w.acceptArgs(token), 0); refusalCode(err) != RefusalLinkExpired {
			t.Fatalf("an accept %v after issue: %v, want %s", after, err, RefusalLinkExpired)
		}
	}
}

func TestAUsedTermsLinkIsRefusedAsLinkUsed(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	token := w.mint(t, p, LinkPurposeTerms)
	w.termsLink()["usedAt"] = linkClock.Format(time.RFC3339Nano)
	if _, err := p.termsForLink(shopper(), termsArgs(token), 0); refusalCode(err) != RefusalLinkUsed {
		t.Fatalf("a used link: %v, want %s", err, RefusalLinkUsed)
	}
}

func TestATermsLinkOfARejectedApplicationIsRefusedAsLinkUsed(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	token := w.mint(t, p, LinkPurposeTerms)
	w.decisions = append(w.decisions, map[string]any{"id": "dec-1", "applicationId": applicationConcept + ":" + linkApp,
		"transition": TransitionReject, "createdAt": linkClock})
	if _, err := p.termsForLink(shopper(), termsArgs(token), 0); refusalCode(err) != RefusalLinkUsed {
		t.Fatalf("a rejected application's link: %v, want %s", err, RefusalLinkUsed)
	}
}

// A LINK SENT AGAIN REPLACES THE TOKEN: the first one finds nothing.
func TestAReplacedTokenIsRefusedAsLinkInvalid(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	first := w.mint(t, p, LinkPurposeTerms)
	second := w.mint(t, p, LinkPurposeTerms)
	if _, err := p.termsForLink(shopper(), termsArgs(first), 0); refusalCode(err) != RefusalLinkInvalid {
		t.Fatalf("the replaced token: %v, want %s", err, RefusalLinkInvalid)
	}
	if _, err := p.termsForLink(shopper(), termsArgs(second), 0); err != nil {
		t.Fatalf("the new token: %v", err)
	}
}

func TestATokenOfAnotherStoreIsRefusedAsLinkInvalid(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	token := w.mint(t, p, LinkPurposeTerms)
	args := termsArgs(token)
	args["storeId"] = "store-links-other"
	if _, err := p.termsForLink(shopper(), args, 0); refusalCode(err) != RefusalLinkInvalid {
		t.Fatalf("a token of another store: %v, want %s", err, RefusalLinkInvalid)
	}
}

// THE TERMS READ SHOWS THE DOCUMENTS AND NOTHING THAT IDENTIFIES: no email, no
// id, no hash of the token.
func TestTheTermsReadShowsNoEmailNoIdAndNoHash(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	token := w.mint(t, p, LinkPurposeTerms)
	nodes, err := p.termsForLink(shopper(), termsArgs(token), 0)
	got := decodeOne(t, nodes, err)
	raw, _ := json.Marshal(got)
	for _, secret := range []string{linkEmail, linkApp, sha256Hex(token), token, "acct-links", linkMerchant, "documentVersion"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the terms read carries %q: %s", secret, raw)
		}
	}
	sheet := trimmed(docsMatching(w.docs, linkStore, DocumentPriceSheet)[0]["id"])
	for i, q := range w.reads {
		if q == "wholesaleDocumentVersionById" && bareID(w.readArgs[i]["documentVersionId"]) == bareID(sheet) {
			t.Fatal("the terms page read the price sheet's body")
		}
	}
	docs := listOfMaps(got["documents"])
	if got["companyName"] != "Kestrel Cycles" || len(docs) != 2 || docs[0]["kind"] != DocumentTerms || docs[1]["kind"] != DocumentMap ||
		docs[0]["label"] != "Terms 2026" || docs[0]["body"] != "Net 30. Kestrel Cycles trade terms." ||
		docs[0]["sha256"] != sha256Hex("Net 30. Kestrel Cycles trade terms.") || docs[1]["sha256"] != sha256Hex("Advertise at list price.") {
		t.Fatalf("the terms read is %s", raw)
	}
	if got["statement"] != DefaultAcceptanceText || got["statementSha256"] != sha256Hex(DefaultAcceptanceText) ||
		got["expiresAt"] != linkClock.Add(30*24*time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("the terms read is %s", raw)
	}
	for k := range got {
		if k != "companyName" && k != "documents" && k != "statement" && k != "statementSha256" && k != "expiresAt" {
			t.Fatalf("the terms read answers %q: %s", k, raw)
		}
	}
}

// A STORE WITH NO CURRENT TERMS OR MAP VERSION, or an application that cannot
// be read, is link_invalid to the shopper and one line for the operator naming
// the application -- never the token.
func TestALinkWithNothingToShowIsLinkInvalidAndLogged(t *testing.T) {
	for name, break_ := range map[string]func(w *linkWorld){
		"no terms": func(w *linkWorld) { w.docs = docsMatching(w.docs, linkStore, DocumentMap) },
		"no map":   func(w *linkWorld) { w.docs = docsMatching(w.docs, linkStore, DocumentTerms) },
		"no application": func(w *linkWorld) {
			w.apps = nil
		},
	} {
		w := newLinkWorld()
		p := w.provider()
		token := w.mint(t, p, LinkPurposeTerms)
		break_(w)
		if _, err := p.termsForLink(shopper(), termsArgs(token), 0); refusalCode(err) != RefusalLinkInvalid {
			t.Fatalf("%s: %v, want %s", name, err, RefusalLinkInvalid)
		}
		if _, err := p.acceptTerms(shopper(), w.acceptArgs(token), 0); refusalCode(err) != RefusalLinkInvalid {
			t.Fatalf("%s, accepting: %v, want %s", name, err, RefusalLinkInvalid)
		}
		if !strings.Contains(w.log.String(), linkApp) || strings.Contains(w.log.String(), token) {
			t.Fatalf("%s: the operator's line is %q", name, w.log.String())
		}
		if w.count("putAcceptanceRow") != 0 {
			t.Fatalf("%s: an acceptance was written", name)
		}
	}
}

// ---------------------------------------------------------------------------
// The acceptance
// ---------------------------------------------------------------------------

func TestAnAcceptanceHoldsEveryField(t *testing.T) {
	w := newLinkWorld()
	w.settings["acceptanceText"] = "I accept the Kestrel Cycles trade terms."
	p := w.provider()
	token := w.mint(t, p, LinkPurposeTerms)
	w.clock = linkClock.Add(2 * time.Hour)
	ctx := memql.ContextWithShopperEvidence(shopper(), memql.ShopperEvidence{ClientAddress: "198.51.100.7", UserAgent: "Example Browser/1.0"})
	if _, err := p.acceptTerms(ctx, w.acceptArgs(token), 0); err != nil {
		t.Fatalf("accept: %v", err)
	}
	a := w.onlyAcceptance(t)
	terms := docsMatching(w.docs, linkStore, DocumentTerms)[0]
	mapv := docsMatching(w.docs, linkStore, DocumentMap)[0]
	want := map[string]any{
		"ownerUserId": linkMerchant, "storeId": linkStore, "accountId": "acct-links",
		"applicationId": applicationConcept + ":" + linkApp, "applicantEmail": linkEmail, "signedName": "Ada Example",
		"acceptedAt":     linkClock.Add(2 * time.Hour).Format(time.RFC3339Nano),
		"termsVersionId": terms["id"], "termsSha256": terms["sha256"], "mapVersionId": mapv["id"], "mapSha256": mapv["sha256"],
		"statement": "I accept the Kestrel Cycles trade terms.", "clientAddress": "198.51.100.7", "userAgent": "Example Browser/1.0",
		"linkIssuedAt": linkClock.Format(time.RFC3339Nano),
	}
	for k, v := range want {
		if a[k] != v {
			t.Errorf("acceptance %s = %v, want %v", k, a[k], v)
		}
	}
	if a["id"] != "v1:wholesale:acceptance:"+AcceptanceRowID(linkApp) {
		t.Fatalf("the acceptance is at %v", a["id"])
	}
	var fenced bool
	for _, c := range w.writes {
		if c.mutation == "putAcceptanceRow" {
			fenced = memql.HasRowVersionFence(c.ctx, "v1:wholesale:acceptance", AcceptanceRowID(linkApp))
			if c.actor != linkMerchant {
				t.Fatalf("the acceptance was written as %q", c.actor)
			}
		}
	}
	if !fenced {
		t.Fatal("the acceptance was not fenced on its row being absent")
	}
	if used := w.termsLink()["usedAt"]; used != linkClock.Add(2*time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("the link's usedAt = %v", used)
	}
	if w.termsLink()["tokenHash"] != sha256Hex(token) || w.termsLink()["expiresAt"] != linkClock.Add(30*24*time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("marking the link used changed it: %+v", w.termsLink())
	}
	if _, err := p.termsForLink(shopper(), termsArgs(token), 0); refusalCode(err) != RefusalLinkUsed {
		t.Fatalf("the link after the acceptance: %v, want %s", err, RefusalLinkUsed)
	}
}

func TestADirectCallRecordsNoAddressAndNoBrowser(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	token := w.mint(t, p, LinkPurposeTerms)
	args := w.acceptArgs(token)
	args["clientAddress"], args["userAgent"] = "203.0.113.9", "Forged/1.0"
	if _, err := p.acceptTerms(shopper(), args, 0); err != nil {
		t.Fatalf("accept: %v", err)
	}
	a := w.onlyAcceptance(t)
	if a["clientAddress"] != "" || a["userAgent"] != "" {
		t.Fatalf("a call with no evidence recorded %q and %q", a["clientAddress"], a["userAgent"])
	}
}

// EVIDENCE IS CUT TO ITS BOUNDS, on a character boundary.
func TestEvidenceIsCutToItsBounds(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	token := w.mint(t, p, LinkPurposeTerms)
	ctx := memql.ContextWithShopperEvidence(shopper(), memql.ShopperEvidence{
		ClientAddress: strings.Repeat("1", 70), UserAgent: strings.Repeat("a", 511) + "é"})
	if _, err := p.acceptTerms(ctx, w.acceptArgs(token), 0); err != nil {
		t.Fatalf("accept: %v", err)
	}
	a := w.onlyAcceptance(t)
	if a["clientAddress"] != strings.Repeat("1", 64) || a["userAgent"] != strings.Repeat("a", 511) {
		t.Fatalf("the evidence was kept as %d and %d bytes", len(asString(a["clientAddress"])), len(asString(a["userAgent"])))
	}
}

func TestBothBoxesMustArriveAsOn(t *testing.T) {
	for name, change := range map[string]map[string]any{
		"terms off":   {"acceptTerms": "off"},
		"map missing": {"acceptMap": ""},
		"terms true":  {"acceptTerms": "true"},
		"no name":     {"signedName": "   "},
	} {
		w := newLinkWorld()
		p := w.provider()
		token := w.mint(t, p, LinkPurposeTerms)
		args := w.acceptArgs(token)
		for k, v := range change {
			args[k] = v
		}
		if _, err := p.acceptTerms(shopper(), args, 0); refusalCode(err) != "invalid" {
			t.Fatalf("%s: %v, want invalid", name, err)
		}
		if w.count("putAcceptanceRow") != 0 || w.termsLink()["usedAt"] != "" {
			t.Fatalf("%s: something was written", name)
		}
	}
}

func TestASecondAcceptanceAnswersLinkUsed(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	token := w.mint(t, p, LinkPurposeTerms)
	if _, err := p.acceptTerms(shopper(), w.acceptArgs(token), 0); err != nil {
		t.Fatalf("accept: %v", err)
	}
	// A new link for the same application, whose acceptance exists.
	again := w.mint(t, p, LinkPurposeTerms)
	before := len(w.writes)
	if _, err := p.acceptTerms(shopper(), w.acceptArgs(again), 0); refusalCode(err) != RefusalLinkUsed {
		t.Fatalf("a second acceptance: %v, want %s", err, RefusalLinkUsed)
	}
	if len(w.writes) != before {
		t.Fatalf("a second acceptance wrote %+v", w.writes[before:])
	}
}

// A FENCE THAT LOSES IS link_used: the other post was recorded.
func TestAnAcceptanceThatLosesItsFenceAnswersLinkUsed(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	token := w.mint(t, p, LinkPurposeTerms)
	fired := false
	w.afterAcceptanceRead = func() {
		if fired {
			return
		}
		fired = true
		w.acceptances[AcceptanceRowID(linkApp)] = map[string]any{"id": "v1:wholesale:acceptance:" + AcceptanceRowID(linkApp),
			"applicationId": "elsewhere", "ownerUserId": linkMerchant}
	}
	if _, err := p.acceptTerms(shopper(), w.acceptArgs(token), 0); refusalCode(err) != RefusalLinkUsed {
		t.Fatalf("the losing post: %v, want %s", err, RefusalLinkUsed)
	}
	if w.termsLink()["usedAt"] != "" {
		t.Fatal("the losing post marked the link used")
	}
}

// TWO SPELLINGS OF ONE APPLICATION ID ARE ONE ACCEPTANCE: a second link whose
// row names the bare id finds the acceptance written under the canonical one,
// and every acceptance write is fenced on the one derived id.
func TestTwoAcceptancesPostedUnderTheTwoSpellingsOfOneApplicationWriteOneRow(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	token := w.mint(t, p, LinkPurposeTerms)
	if _, err := p.acceptTerms(shopper(), w.acceptArgs(token), 0); err != nil {
		t.Fatalf("accept: %v", err)
	}
	legacy, hash, _ := mintToken()
	w.links["wholesale-link-terms-legacy"] = map[string]any{"id": "v1:wholesale:privateLink:wholesale-link-terms-legacy",
		"ownerUserId": linkMerchant, "storeId": linkStore, "applicationId": linkApp, "purpose": LinkPurposeTerms,
		"tokenHash": hash, "issuedAt": linkClock.Format(time.RFC3339Nano),
		"expiresAt": linkClock.Add(24 * time.Hour).Format(time.RFC3339Nano), "usedAt": ""}
	if _, err := p.acceptTerms(shopper(), w.acceptArgs(legacy), 0); refusalCode(err) != RefusalLinkUsed {
		t.Fatalf("the acceptance under the bare spelling: %v, want %s", err, RefusalLinkUsed)
	}
	if w.count("putAcceptanceRow") != 1 || len(w.acceptances) != 1 {
		t.Fatalf("%d acceptance writes, %d rows", w.count("putAcceptanceRow"), len(w.acceptances))
	}
	// And a first acceptance through a link naming the bare id is written at the
	// same derived id as one naming the canonical id.
	w2 := newLinkWorld()
	p2 := w2.provider()
	w2.links["wholesale-link-terms-legacy"] = copyRow(w.links["wholesale-link-terms-legacy"])
	if _, err := p2.acceptTerms(shopper(), w2.acceptArgs(legacy), 0); err != nil {
		t.Fatalf("accept through the bare spelling: %v", err)
	}
	if _, at := w2.acceptances[AcceptanceRowID(linkApp)]; !at {
		t.Fatalf("the acceptance through the bare spelling is at %v", w2.acceptances)
	}
	if got := w2.onlyAcceptance(t)["applicationId"]; got != applicationConcept+":"+linkApp {
		t.Fatalf("the acceptance names %v, want the canonical id", got)
	}
}

// ---------------------------------------------------------------------------
// Ruling 49: an acceptance records what the person was shown
// ---------------------------------------------------------------------------

// shownFingerprints is what the terms read answered, as the page posts it.
func shownFingerprints(t *testing.T, p *Provider, token string) (terms, mapv string) {
	t.Helper()
	got := decodedBy(t)(p.termsForLink(shopper(), termsArgs(token), 0))
	docs := listOfMaps(got["documents"])
	return asString(docs[0]["sha256"]), asString(docs[1]["sha256"])
}

func TestAPostCarryingTheFingerprintsTheReadAnsweredWritesTheAcceptance(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	token := w.mint(t, p, LinkPurposeTerms)
	terms, mapv := shownFingerprints(t, p, token)
	args := w.acceptArgs(token)
	// Upper-case hex is the same fingerprint.
	args["termsSha256"], args["mapSha256"] = strings.ToUpper(terms), mapv
	if _, err := p.acceptTerms(shopper(), args, 0); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if a := w.onlyAcceptance(t); a["termsSha256"] != terms || a["mapSha256"] != mapv {
		t.Fatalf("the acceptance records %v and %v", a["termsSha256"], a["mapSha256"])
	}
}

func TestANewVersionBetweenTheReadAndThePostIsTermsChanged(t *testing.T) {
	for _, kind := range []string{DocumentTerms, DocumentMap} {
		w := newLinkWorld()
		p := w.provider()
		token := w.mint(t, p, LinkPurposeTerms)
		terms, mapv := shownFingerprints(t, p, token)
		w.clock = linkClock.Add(time.Hour)
		fresh := w.publish(kind, "Revised "+kind, "A revised "+kind+".")
		args := w.acceptArgs(token)
		args["termsSha256"], args["mapSha256"] = terms, mapv
		if _, err := p.acceptTerms(shopper(), args, 0); refusalCode(err) != RefusalTermsChanged {
			t.Fatalf("%s revised: %v, want %s", kind, err, RefusalTermsChanged)
		}
		if len(w.acceptances) != 0 || w.count("putAcceptanceRow") != 0 || w.termsLink()["usedAt"] != "" {
			t.Fatalf("%s revised: something was written", kind)
		}
		got := decodedBy(t)(p.termsForLink(shopper(), termsArgs(token), 0))
		shown := map[string]string{}
		for _, d := range listOfMaps(got["documents"]) {
			shown[asString(d["kind"])] = asString(d["sha256"])
		}
		if shown[kind] != fresh["sha256"] {
			t.Fatalf("%s revised: the read again shows %v, want the new version", kind, shown)
		}
	}
}

func TestAFingerprintThatIsNotSixtyFourHexIsInvalid(t *testing.T) {
	for name, value := range map[string]string{
		"63 characters": strings.Repeat("a", 63),
		"65 characters": strings.Repeat("a", 65),
		"not hex":       strings.Repeat("g", 64),
	} {
		for _, field := range []string{"termsSha256", "mapSha256", "statementSha256"} {
			w := newLinkWorld()
			p := w.provider()
			token := w.mint(t, p, LinkPurposeTerms)
			args := w.acceptArgs(token)
			args[field] = value
			if _, err := p.acceptTerms(shopper(), args, 0); refusalCode(err) != "invalid" {
				t.Fatalf("%s %s: %v, want invalid", field, name, err)
			}
			if w.count("putAcceptanceRow") != 0 {
				t.Fatalf("%s %s: an acceptance was written", field, name)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Approval waits for the acceptance
// ---------------------------------------------------------------------------

func TestApproveIsRefusedWithoutAnAcceptanceWhenTermsAreRequired(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	_, err := p.recordDecision(staffCtx("v1:identity:user:staff-links"),
		map[string]any{"applicationId": linkApp, "transition": TransitionApprove}, 0)
	if err == nil || err.Error() != "This application has not accepted the terms yet." {
		t.Fatalf("approve without an acceptance: %v", err)
	}
	if w.count("appendApplicationDecision") != 0 {
		t.Fatal("a refused approve was written")
	}
	// A reject needs none.
	if _, err := p.recordDecision(staffCtx("v1:identity:user:staff-links"),
		map[string]any{"applicationId": linkApp, "transition": TransitionReject}, 0); err != nil {
		t.Fatalf("a reject: %v", err)
	}
}

func TestApproveNeedsNoAcceptanceWhenTermsAreNotRequired(t *testing.T) {
	w := newLinkWorld()
	w.settings["termsRequired"] = false
	p := w.provider()
	if _, err := p.recordDecision(staffCtx("v1:identity:user:staff-links"),
		map[string]any{"applicationId": linkApp, "transition": TransitionApprove}, 0); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// And with the terms required, an accepted application is approved.
	w2 := newLinkWorld()
	p2 := w2.provider()
	token := w2.mint(t, p2, LinkPurposeTerms)
	if _, err := p2.acceptTerms(shopper(), w2.acceptArgs(token), 0); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := p2.recordDecision(staffCtx("v1:identity:user:staff-links"),
		map[string]any{"applicationId": applicationConcept + ":" + linkApp, "transition": TransitionApprove}, 0); err != nil {
		t.Fatalf("approve an accepted application: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The price-sheet link
// ---------------------------------------------------------------------------

func (w *linkWorld) approve(granted string) {
	w.decisions = append(w.decisions, map[string]any{"id": "dec-approve", "applicationId": applicationConcept + ":" + linkApp,
		"transition": TransitionApprove, "createdAt": linkClock})
	if granted != "" {
		w.ents = []map[string]any{{"id": EntitlementRowID(linkApp), "applicationId": applicationConcept + ":" + linkApp,
			"state": granted, "createdAt": linkClock}}
	}
}

func TestAPriceSheetLinkOpensOnlyWhileApprovedAndGranted(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(w *linkWorld)
		want  string
	}{
		"submitted":            {func(w *linkWorld) {}, RefusalLinkUsed},
		"approved, no access":  {func(w *linkWorld) { w.approve("") }, RefusalLinkUsed},
		"approved, pending":    {func(w *linkWorld) { w.approve(EntitlementPending) }, RefusalLinkUsed},
		"approved and granted": {func(w *linkWorld) { w.approve(EntitlementGranted) }, ""},
		"revoked": {func(w *linkWorld) {
			w.approve(EntitlementGranted)
			w.decisions = append(w.decisions, map[string]any{"id": "dec-revoke", "applicationId": applicationConcept + ":" + linkApp,
				"transition": TransitionRevoke, "createdAt": linkClock.Add(time.Minute)})
		}, RefusalLinkUsed},
	} {
		w := newLinkWorld()
		p := w.provider()
		token := w.mint(t, p, LinkPurposePriceSheet)
		c.setup(w)
		// Long after issue: a price-sheet link has no expiry.
		w.clock = linkClock.Add(400 * 24 * time.Hour)
		_, err := p.priceSheetForLink(shopper(), termsArgs(token), 0)
		if refusalCode(err) != c.want || (c.want == "" && err != nil) {
			t.Fatalf("%s: %v, want %q", name, err, c.want)
		}
	}
	// A terms token does not open the price sheet, nor the reverse.
	w := newLinkWorld()
	p := w.provider()
	w.approve(EntitlementGranted)
	terms := w.mint(t, p, LinkPurposeTerms)
	sheet := w.mint(t, p, LinkPurposePriceSheet)
	if _, err := p.priceSheetForLink(shopper(), termsArgs(terms), 0); refusalCode(err) != RefusalLinkInvalid {
		t.Fatalf("a terms token on the price sheet: %v", err)
	}
	if _, err := p.termsForLink(shopper(), termsArgs(sheet), 0); refusalCode(err) != RefusalLinkInvalid {
		t.Fatalf("a price-sheet token on the terms: %v", err)
	}
}

func TestAPriceSheetLinkShowsTheCurrentVersion(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	w.approve(EntitlementGranted)
	token := w.mint(t, p, LinkPurposePriceSheet)
	w.clock = linkClock.Add(time.Hour)
	fresh := w.publish(DocumentPriceSheet, "Summer list", "Frame 395.00")
	got := decodedBy(t)(p.priceSheetForLink(shopper(), termsArgs(token), 0))
	raw, _ := json.Marshal(got)
	doc, _ := got["document"].(map[string]any)
	if got["companyName"] != "Kestrel Cycles" || doc["label"] != "Summer list" || doc["body"] != "Frame 395.00" ||
		doc["sha256"] != fresh["sha256"] || doc["publishedAt"] != fresh["publishedAt"] {
		t.Fatalf("the price sheet reads %s", raw)
	}
	for _, secret := range []string{linkEmail, linkApp, sha256Hex(token), "documentVersion"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the price sheet carries %q: %s", secret, raw)
		}
	}
	if len(got) != 2 || len(doc) != 4 {
		t.Fatalf("the price sheet answers more than it says: %s", raw)
	}
}

// ---------------------------------------------------------------------------
// The declarations
// ---------------------------------------------------------------------------

func TestTheTermsAndPriceSheetReadsArePrivate(t *testing.T) {
	for _, r := range []memql.ShopperRead{termsRead(), priceSheetRead()} {
		if !r.Private || r.Kind != memql.ShopperReadKindBuiltin || strings.Join(r.Refusals, ",") != strings.Join(LinkRefusals, ",") {
			t.Fatalf("%s: %+v", r.Name, r)
		}
		if len(r.Fields) != 1 || r.Fields[0].Name != "requestToken" || !r.Fields[0].Required {
			t.Fatalf("%s fields: %+v", r.Name, r.Fields)
		}
	}
	if termsRead().Name != "terms" || termsRead().Construct != "wholesaleTermsForLink" ||
		priceSheetRead().Name != "priceSheet" || priceSheetRead().Construct != "wholesalePriceSheetForLink" {
		t.Fatalf("the reads are %+v and %+v", termsRead(), priceSheetRead())
	}
	f := acceptanceForm()
	if f.Name != "acceptance" || f.Construct != "wholesaleAcceptTerms" || f.Kind != memql.ShopperReadKindBuiltin || !f.Evidence ||
		f.RedirectOK != "/wholesale/accepted" || f.RedirectError != "/wholesale/link-problem" ||
		strings.Join(f.Refusals, ",") != strings.Join(AcceptanceRefusals, ",") {
		t.Fatalf("the acceptance form is %+v", f)
	}
	want := map[string]int{"requestToken": 200, "acceptTerms": 8, "acceptMap": 8, "signedName": 120, "termsSha256": 64, "mapSha256": 64,
		"statementSha256": 64}
	for _, field := range f.Fields {
		if want[field.Name] != field.MaxLength || !field.Required {
			t.Fatalf("the acceptance field %+v", field)
		}
		switch field.Name {
		case "acceptTerms", "acceptMap":
			if strings.Join(field.Enum, ",") != "on" {
				t.Fatalf("the box %s accepts %v, want on only", field.Name, field.Enum)
			}
		case "termsSha256", "mapSha256", "statementSha256":
			if field.Format != memql.ShopperFormatSHA256 {
				t.Fatalf("the fingerprint %s has the format %q", field.Name, field.Format)
			}
		}
		delete(want, field.Name)
	}
	if desc := tokenField().Description; !strings.Contains(desc, "without one as invalid") || strings.Contains(desc, " t parameter") {
		t.Fatalf("the token's description is not what the surface does: %q", desc)
	}
	if len(want) != 0 {
		t.Fatalf("the acceptance form lacks %v", want)
	}
	if strings.Join(AcceptanceRefusals, ",") != strings.Join(append(append([]string{}, LinkRefusals...), RefusalTermsChanged), ",") {
		t.Fatalf("AcceptanceRefusals = %v", AcceptanceRefusals)
	}
}

// ---------------------------------------------------------------------------
// The stage, the application and the export
// ---------------------------------------------------------------------------

func TestAnAcceptedApplicationIsReadyToReview(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	staff := staffCtx(linkMerchant)
	got := decodedBy(t)(p.get(staff, map[string]any{"applicationId": linkApp}, 0))
	if got["stage"] != StageWaitingForTerms || got["acceptance"] != nil || got["termsLink"] != nil {
		t.Fatalf("an application that has not accepted: stage %v, acceptance %v, termsLink %v", got["stage"], got["acceptance"], got["termsLink"])
	}
	if strings.Join(asStrings(got["acts"]), ",") != ActReject {
		t.Fatalf("acts while waiting: %v", got["acts"])
	}
	token := w.mint(t, p, LinkPurposeTerms)
	ctx := memql.ContextWithShopperEvidence(shopper(), memql.ShopperEvidence{ClientAddress: "198.51.100.7", UserAgent: "Example Browser/1.0"})
	if _, err := p.acceptTerms(ctx, w.acceptArgs(token), 0); err != nil {
		t.Fatalf("accept: %v", err)
	}
	got = decodedBy(t)(p.get(staff, map[string]any{"applicationId": linkApp}, 0))
	if got["stage"] != StageReadyToReview || strings.Join(asStrings(got["acts"]), ",") != ActReject+","+ActApprove {
		t.Fatalf("an accepted application: stage %v, acts %v", got["stage"], got["acts"])
	}
	w.reads = nil
	got = decodedBy(t)(p.get(staff, map[string]any{"applicationId": linkApp}, 0))
	for _, q := range w.reads {
		if q == "wholesaleDocumentVersionById" {
			t.Fatal("the application read a document body to show a label")
		}
	}
	acc, _ := got["acceptance"].(map[string]any)
	want := map[string]any{"signedName": "Ada Example", "acceptedAt": linkClock.Format(time.RFC3339Nano),
		"termsLabel": "Terms 2026", "termsSha256": sha256Hex("Net 30. Kestrel Cycles trade terms."),
		"mapLabel": "MAP 2026", "mapSha256": sha256Hex("Advertise at list price."),
		"clientAddress": "198.51.100.7", "userAgent": "Example Browser/1.0"}
	if len(acc) != len(want) {
		t.Fatalf("the acceptance shows %+v", acc)
	}
	for k, v := range want {
		if acc[k] != v {
			t.Errorf("acceptance %s = %v, want %v", k, acc[k], v)
		}
	}
	link, _ := got["termsLink"].(map[string]any)
	if len(link) != 2 || link["expiresAt"] != linkClock.Add(30*24*time.Hour).Format(time.RFC3339Nano) || link["expired"] != false {
		t.Fatalf("the terms link shows %+v", link)
	}
	w.clock = linkClock.Add(31 * 24 * time.Hour)
	got = decodedBy(t)(p.get(staff, map[string]any{"applicationId": linkApp}, 0))
	if link, _ := got["termsLink"].(map[string]any); link["expired"] != true {
		t.Fatalf("the terms link past its expiry shows %+v", link)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), sha256Hex(token)) || strings.Contains(string(raw), token) {
		t.Fatalf("the application carries the token or its hash: %s", raw)
	}
	// The state builtin's stage reads the acceptance too.
	state := decodedBy(t)(p.applicationState(staff, map[string]any{"applicationId": linkApp}, 0))
	if state["stage"] != StageReadyToReview {
		t.Fatalf("wholesaleApplicationState's stage = %v", state["stage"])
	}
}

func asStrings(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, asString(i))
	}
	return out
}

// THE LIST READS ACCEPTANCES IN A BATCH, as it reads decisions: one read for
// the page of applications, none per application, and the stage follows.
func TestTheListReadsAcceptancesInABatch(t *testing.T) {
	f := newStoreFake()
	f.settings["termsRequired"] = true
	a := f.app(1, "Kestrel Cycles", "Ada Example", "ada@kestrel.example.com", 1)
	f.app(2, "Bluebird Bikes", "Sam Rivers", "sam@bluebird.example.com", 2)
	f.app(3, "Harbor Wheelworks", "Lee Park", "lee@harbor.example.com", 3)
	f.acceptances = append(f.acceptances, map[string]any{"id": "v1:wholesale:acceptance:" + AcceptanceRowID(a),
		"applicationId": applicationConcept + ":" + a, "ownerUserId": "v1:identity:user:merchant-1", "createdAt": readsClock})
	got := listOf(t, f.provider(), map[string]any{"stage": stageAll})
	stages := map[string]string{}
	for _, r := range rowsOf(t, got) {
		stages[asString(r["companyName"])] = asString(r["stage"])
	}
	if stages["Kestrel Cycles"] != StageReadyToReview || stages["Bluebird Bikes"] != StageWaitingForTerms ||
		stages["Harbor Wheelworks"] != StageWaitingForTerms {
		t.Fatalf("stages = %v", stages)
	}
	if f.pages["acceptancesForApplicationIds"] != 1 || f.single["acceptanceForApplication"] != 0 {
		t.Fatalf("the list read acceptances %d times in a batch and %d one by one", f.pages["acceptancesForApplicationIds"],
			f.single["acceptanceForApplication"])
	}
	counts, _ := got["counts"].(map[string]any)
	if counts[StageReadyToReview] != float64(1) || counts[StageWaitingForTerms] != float64(2) {
		t.Fatalf("counts = %v", counts)
	}
}

func TestTheExportCarriesTheAcceptances(t *testing.T) {
	f := newStoreFake()
	f.settings["termsRequired"] = true
	a := f.app(1, "Kestrel Cycles", "Ada Example", "ada@kestrel.example.com", 1)
	f.acceptances = append(f.acceptances, map[string]any{"id": "v1:wholesale:acceptance:" + AcceptanceRowID(a),
		"applicationId": a, "ownerUserId": "v1:identity:user:merchant-1", "signedName": "Ada Example",
		"tokenHash": "never-exported", "createdAt": readsClock})
	got := decodedBy(t)(f.provider().exportStore(staffReads(), map[string]any{"siteId": readsSite, "storeId": readsStore}, 0))
	var doc map[string]any
	if err := json.Unmarshal([]byte(asString(got["json"])), &doc); err != nil {
		t.Fatalf("export json: %v", err)
	}
	accs := listOfMaps(doc["acceptances"])
	if len(accs) != 1 || accs[0]["signedName"] != "Ada Example" || accs[0]["applicationId"] != applicationConcept+":"+a {
		t.Fatalf("the export's acceptances are %+v", doc["acceptances"])
	}
	if strings.Contains(asString(got["json"]), "never-exported") {
		t.Fatal("the export carries a token hash")
	}
	if apps := listOfMaps(doc["applications"]); len(apps) != 1 || apps[0]["stage"] != StageReadyToReview {
		t.Fatalf("the exported application is %+v", apps)
	}
}

// ---------------------------------------------------------------------------
// The token reaches nothing, and another store's token tells nothing
// ---------------------------------------------------------------------------

// A LINK BUILTIN CALLED DIRECTLY by a signed-in person with a token of another
// store answers exactly what an unknown token answers, and reads nothing past
// the token lookup.
func TestALinkBuiltinCalledWithATokenOfAnotherStoreAnswersAsAnUnknownToken(t *testing.T) {
	stranger := staffCtx("v1:identity:user:stranger")
	calls := map[string]func(p *Provider, args map[string]any) ([]memorynodes.MemoryNode, error){
		"wholesaleTermsForLink": func(p *Provider, args map[string]any) ([]memorynodes.MemoryNode, error) {
			return p.termsForLink(stranger, args, 0)
		},
		"wholesaleAcceptTerms": func(p *Provider, args map[string]any) ([]memorynodes.MemoryNode, error) {
			return p.acceptTerms(stranger, args, 0)
		},
		"wholesalePriceSheetForLink": func(p *Provider, args map[string]any) ([]memorynodes.MemoryNode, error) {
			return p.priceSheetForLink(stranger, args, 0)
		},
	}
	for name, call := range calls {
		answer := func(token, store string) (string, []string) {
			w := newLinkWorld()
			w.approve(EntitlementGranted)
			p := w.provider()
			w.mint(t, p, LinkPurposeTerms)
			w.mint(t, p, LinkPurposePriceSheet)
			w.reads, w.writes = nil, nil
			args := w.acceptArgs(token)
			args["storeId"] = store
			_, err := call(p, args)
			if err == nil {
				t.Fatalf("%s answered a stranger", name)
			}
			if len(w.writes) != 0 {
				t.Fatalf("%s wrote for a stranger: %+v", name, w.writes)
			}
			return err.Error() + "|" + refusalCode(err), w.reads
		}
		// A token of linkStore, as a stranger whose store is another one.
		w := newLinkWorld()
		w.approve(EntitlementGranted)
		p := w.provider()
		termsToken := w.mint(t, p, LinkPurposeTerms)
		sheetToken := w.mint(t, p, LinkPurposePriceSheet)
		unknown, unknownReads := answer("an-unknown-token", "store-of-the-stranger")
		for _, token := range []string{termsToken, sheetToken} {
			w.reads, w.writes = nil, nil
			args := w.acceptArgs(token)
			args["storeId"] = "store-of-the-stranger"
			_, err := call(p, args)
			if err == nil || err.Error()+"|"+refusalCode(err) != unknown {
				t.Fatalf("%s with another store's token: %v, want what an unknown token answers: %s", name, err, unknown)
			}
			if strings.Join(w.reads, ",") != strings.Join(unknownReads, ",") || len(w.reads) != 1 || w.reads[0] != "privateLinkByTokenHash" {
				t.Fatalf("%s read %v for another store's token, %v for an unknown one", name, w.reads, unknownReads)
			}
			// And naming the other store itself, the stranger reads no link: the
			// token is looked up under their own authority.
			w.reads = nil
			args["storeId"] = linkStore
			if _, err := call(p, args); err == nil || err.Error()+"|"+refusalCode(err) != unknown {
				t.Fatalf("%s naming the token's own store as a stranger: %v", name, err)
			}
			if len(w.reads) != 1 {
				t.Fatalf("%s read %v as a stranger", name, w.reads)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("%s's refusal carries the token", name)
			}
		}
	}
}

// NO LOG LINE, ERROR OR REPLY IN THE ACCEPT PATH CARRIES THE TOKEN, whatever
// the path's outcome.
func TestNoLogLineErrorOrReplyInTheAcceptPathCarriesTheToken(t *testing.T) {
	outcomes := map[string]func(w *linkWorld){
		"accepted":       func(w *linkWorld) {},
		"expired":        func(w *linkWorld) { w.clock = linkClock.Add(40 * 24 * time.Hour) },
		"used":           func(w *linkWorld) { w.termsLink()["usedAt"] = linkClock.Format(time.RFC3339Nano) },
		"decided":        func(w *linkWorld) { w.approve(EntitlementGranted) },
		"no terms":       func(w *linkWorld) { w.docs = docsMatching(w.docs, linkStore, DocumentMap) },
		"no application": func(w *linkWorld) { w.apps = nil },
		"terms changed":  func(w *linkWorld) { w.publish(DocumentTerms, "Revised", "Revised terms.") },
		"box off":        func(w *linkWorld) {},
		"lost the fence": func(w *linkWorld) {
			w.afterAcceptanceRead = func() {
				w.acceptances[AcceptanceRowID(linkApp)] = map[string]any{"id": "x", "ownerUserId": linkMerchant}
			}
		},
		"write fails": func(w *linkWorld) {},
	}
	for name, setup := range outcomes {
		w := newLinkWorld()
		p := w.provider()
		token := w.mint(t, p, LinkPurposeTerms)
		args := w.acceptArgs(token)
		setup(w)
		if name == "box off" {
			args["acceptMap"] = "off"
		}
		if name == "write fails" {
			p.writer = failingWriter{inner: w}
		}
		nodes, err := p.acceptTerms(shopper(), args, 0)
		var seen []string
		if err != nil {
			seen = append(seen, err.Error(), fmt.Sprintf("%+v", err))
		}
		for _, n := range nodes {
			seen = append(seen, string(n.Payload), n.ID)
		}
		seen = append(seen, w.log.String())
		for _, c := range w.writes {
			for _, v := range c.args {
				seen = append(seen, asString(v))
			}
		}
		for _, s := range seen {
			if strings.Contains(s, token) {
				t.Fatalf("%s: the token reached %q", name, s)
			}
		}
		if name == "accepted" {
			raw := string(nodes[0].Payload)
			for _, secret := range []string{linkEmail, linkApp, sha256Hex(token)} {
				if strings.Contains(raw, secret) {
					t.Fatalf("the acceptance's reply carries %q: %s", secret, raw)
				}
			}
		}
	}
}

// failingWriter refuses the acceptance write with an error that names nothing.
type failingWriter struct{ inner *linkWorld }

func (f failingWriter) Write(ctx context.Context, mutation string, args map[string]any) error {
	if mutation == "putAcceptanceRow" {
		return errors.New("wholesale: putAcceptanceRow: the database is unavailable")
	}
	return f.inner.Write(ctx, mutation, args)
}

func (f failingWriter) Insert(ctx context.Context, mutation string, args map[string]any) (string, error) {
	return f.inner.Insert(ctx, mutation, args)
}

// A STORE WITH NO CURRENT PRICE SHEET is link_invalid on a price-sheet link,
// and one line for the operator.
func TestAPriceSheetLinkWithNoPriceSheetIsLinkInvalidAndLogged(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	w.approve(EntitlementGranted)
	token := w.mint(t, p, LinkPurposePriceSheet)
	w.docs = append(docsMatching(w.docs, linkStore, DocumentTerms), docsMatching(w.docs, linkStore, DocumentMap)...)
	if _, err := p.priceSheetForLink(shopper(), termsArgs(token), 0); refusalCode(err) != RefusalLinkInvalid {
		t.Fatalf("no price sheet: %v, want %s", err, RefusalLinkInvalid)
	}
	if !strings.Contains(w.log.String(), linkApp) || strings.Contains(w.log.String(), token) {
		t.Fatalf("the operator's line is %q", w.log.String())
	}
}

// ruling 52: THE STATEMENT IS TIED TO WHAT WAS SHOWN. A staff edit of the
// sentence between the read and the post refuses the post as terms_changed,
// with nothing written and the link usable; a second read answers the new
// sentence and its fingerprint.
func TestAnEditOfTheSentenceBetweenTheReadAndThePostIsTermsChanged(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	token := w.mint(t, p, LinkPurposeTerms)
	shown := decodedBy(t)(p.termsForLink(shopper(), termsArgs(token), 0))
	args := w.acceptArgs(token)
	args["statementSha256"] = asString(shown["statementSha256"])
	w.settings["acceptanceText"] = "I accept the revised Kestrel Cycles terms."
	if _, err := p.acceptTerms(shopper(), args, 0); refusalCode(err) != RefusalTermsChanged {
		t.Fatalf("a post of the old sentence: %v, want %s", err, RefusalTermsChanged)
	}
	if len(w.acceptances) != 0 || w.termsLink()["usedAt"] != "" {
		t.Fatal("a post of the old sentence wrote something")
	}
	again := decodedBy(t)(p.termsForLink(shopper(), termsArgs(token), 0))
	if again["statement"] != "I accept the revised Kestrel Cycles terms." ||
		again["statementSha256"] != sha256Hex("I accept the revised Kestrel Cycles terms.") {
		t.Fatalf("the read again answers %v / %v", again["statement"], again["statementSha256"])
	}
	args["statementSha256"] = again["statementSha256"]
	if _, err := p.acceptTerms(shopper(), args, 0); err != nil {
		t.Fatalf("a post of the new sentence: %v", err)
	}
	if a := w.onlyAcceptance(t); a["statement"] != "I accept the revised Kestrel Cycles terms." {
		t.Fatalf("the acceptance records %v", a["statement"])
	}
}

// A TERMS LINK IS link_used WHENEVER AN ACCEPTANCE EXISTS for its application,
// whatever usedAt says: here the usedAt write failed after the acceptance.
func TestATermsLinkIsUsedOnceAnAcceptanceExistsWhateverUsedAtSays(t *testing.T) {
	w := newLinkWorld()
	p := w.provider()
	token := w.mint(t, p, LinkPurposeTerms)
	p.writer = usedAtFails{inner: w}
	if _, err := p.acceptTerms(shopper(), w.acceptArgs(token), 0); err == nil {
		t.Fatal("the accept answered no error although its usedAt write failed")
	}
	if len(w.acceptances) != 1 || w.termsLink()["usedAt"] != "" {
		t.Fatalf("the acceptance or usedAt is not as the failure left it: %d, %v", len(w.acceptances), w.termsLink()["usedAt"])
	}
	if _, err := p.termsForLink(shopper(), termsArgs(token), 0); refusalCode(err) != RefusalLinkUsed {
		t.Fatalf("the terms read after the acceptance: %v, want %s", err, RefusalLinkUsed)
	}
	if _, err := p.acceptTerms(shopper(), w.acceptArgs(token), 0); refusalCode(err) != RefusalLinkUsed {
		t.Fatalf("a retry: %v, want %s", err, RefusalLinkUsed)
	}
}

// usedAtFails refuses the link's usedAt write and passes every other.
type usedAtFails struct{ inner *linkWorld }

func (u usedAtFails) Write(ctx context.Context, mutation string, args map[string]any) error {
	if mutation == "putPrivateLinkRow" && asString(args["usedAt"]) != "" {
		return errors.New("wholesale: putPrivateLinkRow: the database is unavailable")
	}
	return u.inner.Write(ctx, mutation, args)
}

func (u usedAtFails) Insert(ctx context.Context, mutation string, args map[string]any) (string, error) {
	return u.inner.Insert(ctx, mutation, args)
}
