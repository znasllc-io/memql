package wholesalepack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// store_gate_test.go -- A STORE ADMITS THE WHOLESALE SECTION'S ACTS ONLY ONCE IT
// IS SET UP, AND ONLY FOR ITS ORGANIZATION (ruling 55).
//
// The store gate is siteForStore, which every site-gated builtin goes through
// (directly, or through admitStore). A store with no settings row, or whose row
// names no organization, refuses every one of them with NotSetUpSentence before
// anything of the store beyond its settings rows is read, and before any write;
// wholesaleStoreSettings answers "not set up" with nothing of the store.

// storeGateReads are the only reads the gate makes of a store that is not set
// up: the site, and the store's settings rows.
var storeGateReads = map[string]bool{"siteById": true, "wholesaleSettingsById": true, "wholesaleSettingsForStore": true}

// siteGatedHandlers is every site-gated builtin's handler with a call that would
// otherwise go through, on settingsSite's store.
func siteGatedHandlers(p *Provider) map[string]struct {
	call func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error)
	args map[string]any
} {
	store := map[string]any{"siteId": "site-1", "storeId": "store-live"}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{"siteId": "site-1", "storeId": "store-live"}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	type entry = struct {
		call func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error)
		args map[string]any
	}
	return map[string]entry{
		"wholesaleList":          {p.list, store},
		"wholesaleDecisionLog":   {p.decisionLog, store},
		"wholesaleBuyers":        {p.buyers, store},
		"wholesaleStoreSettings": {p.storeSettings, store},
		"wholesaleUpdateSettings": {p.updateSettings, with(map[string]any{"applicationsOpen": true,
			"staffEmail": "desk@example.com"})},
		"wholesaleDocuments": {p.documents, store},
		"wholesalePublishDocument": {p.publishDocument, with(map[string]any{"kind": DocumentTerms, "label": "Terms",
			"body": "Net 30."})},
		"wholesaleAddBuyer": {p.addBuyer, with(map[string]any{"companyName": "Kestrel Cycles", "applicantName": "Ada Example",
			"applicantEmail": "ada@kestrel.example.com"})},
		"wholesaleExportStore": {p.exportStore, store},
	}
}

// EVERY SITE-GATED ACT ON A STORE THAT IS NOT SET UP -- no settings row, or a
// row tied to no organization -- is refused with the sentence, reads nothing of
// the store past its settings rows and writes nothing; wholesaleStoreSettings
// answers not set up, with every setting at its default and no document.
func TestEverySiteGatedActRefusesAStoreThatIsNotSetUp(t *testing.T) {
	registerShippedAdapters()
	untied := map[string]any{"storeId": "store-live", "ownerUserId": "merchant-1", "applicationsOpen": true,
		"termsRequired": true, "introText": "planted", "staffEmail": "desk@example.com",
		"createdAt": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	planted := []map[string]any{{"id": documentVersionConcept + ":dv-planted", "storeId": "store-live", "kind": DocumentTerms,
		"label": "Planted terms", "body": "planted", "sha256": "abc", "createdAt": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
	for state, settings := range map[string]map[string][]map[string]any{
		"no settings row":          nil,
		"a row of no organization": {"store-live": {untied}},
	} {
		for _, caller := range []struct {
			name string
			ctx  context.Context
		}{{"the store's person", staffCtx("staff-1")}, {"a cluster operator", operatorCtx("operator-1")}} {
			probe := &Provider{}
			for name := range siteGatedHandlers(probe) {
				reader := &fixtureReader{sites: []map[string]any{settingsSite()}, settings: settings, docs: planted}
				writer := &fakeWriter{}
				p := &Provider{reader: reader, writer: writer}
				h := siteGatedHandlers(p)[name]
				nodes, err := h.call(caller.ctx, h.args, 0)
				if name == "wholesaleStoreSettings" {
					if err != nil {
						t.Errorf("%s, %s: %s: %v, want the not-set-up reply", state, caller.name, name, err)
						continue
					}
					var got map[string]any
					_ = json.Unmarshal(nodes[0].Payload, &got)
					docs, _ := got["documents"].(map[string]any)
					may, _ := got["may"].(map[string]any)
					wantSetUp := caller.name == "a cluster operator"
					if got["exists"] != false || got["applicationsOpen"] != false || got["termsRequired"] != false ||
						got["introText"] != "" || got["staffEmail"] != "" || got["entitlementAdapter"] != "" ||
						len(docs) != len(DocumentKinds) || docs[DocumentTerms] != nil || docs[DocumentMap] != nil ||
						docs[DocumentPriceSheet] != nil || may["update"] != false || may["setUp"] != wantSetUp {
						t.Errorf("%s, %s: the settings reply tells something of the store: %+v", state, caller.name, got)
					}
				} else if name == "wholesaleAddBuyer" && caller.name == "a cluster operator" {
					// An operator adds nobody, refused before anything is read.
					if err == nil || !strings.Contains(err.Error(), "do not decide them") {
						t.Errorf("%s, %s: %s: %v, want the operator's refusal", state, caller.name, name, err)
					}
				} else if err == nil || err.Error() != NotSetUpSentence {
					t.Errorf("%s, %s: %s: %v, want %q", state, caller.name, name, err, NotSetUpSentence)
				}
				if len(writer.writes) != 0 {
					t.Errorf("%s, %s: %s wrote %+v", state, caller.name, name, writer.writes)
				}
				for _, q := range reader.asked {
					if !storeGateReads[q] {
						t.Errorf("%s, %s: %s read %s of a store that is not set up", state, caller.name, name, q)
					}
				}
			}
		}
	}
}

// A SET-UP NAMING THE SITE TIES A ROW THAT IS NOT TIED, keeping its owner and
// what it holds; after it the store's organization is admitted.
func TestASetUpNamingTheSiteTiesAnUntiedRow(t *testing.T) {
	registerShippedAdapters()
	row := map[string]any{"storeId": "store-live", "ownerUserId": "merchant-1", "applicationsOpen": true,
		"staffEmail": "desk@example.com", "createdAt": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	elsewhere := map[string]any{"id": "site-2", "ownerUserId": "merchant-2", "accountId": "acct-south",
		"binding": map[string]any{"storeId": "store-live"}}
	reader := &fixtureReader{sites: []map[string]any{settingsSite(), elsewhere},
		settings: map[string][]map[string]any{"store-live": {row}}}
	writer := &fakeWriter{}
	p := &Provider{reader: reader, writer: writer}
	// TWO SITES BOUND AND NONE NAMED: which organization's program it is cannot
	// be told.
	if _, err := p.setWholesaleSettings(operatorCtx("operator-1"), map[string]any{"storeId": "store-live",
		"applicationsOpen": true}, 0); err == nil || err.Error() != NameTheSiteSentence {
		t.Fatalf("a set-up of an untied row with two sites bound: %v, want %q", err, NameTheSiteSentence)
	}
	if len(writer.writes) != 0 {
		t.Fatalf("the refused set-up wrote %+v", writer.writes)
	}
	if _, err := p.setWholesaleSettings(operatorCtx("operator-1"), map[string]any{"storeId": "store-live", "siteId": "site-1",
		"applicationsOpen": true}, 0); err != nil {
		t.Fatalf("a set-up naming the site: %v", err)
	}
	got := writer.only(t, "setWholesaleSettingsRow")
	if got["accountId"] != "acct-north" || got["siteId"] != "site-1" || got["ownerUserId"] != "v1:identity:user:merchant-1" ||
		got["staffEmail"] != "desk@example.com" {
		t.Fatalf("the set-up wrote %+v, want the row tied to acct-north and still merchant-1's", got)
	}
	// THE ROW AS WRITTEN: the store's organization is admitted, the other refused.
	row["accountId"], row["siteId"] = "acct-north", "site-1"
	if _, err := p.list(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live"}, 0); err != nil {
		t.Fatalf("the store's own organization after the set-up: %v", err)
	}
	if _, err := p.list(staffCtx("staff-2"), map[string]any{"siteId": "site-2", "storeId": "store-live"}, 0); err == nil ||
		!strings.Contains(err.Error(), "another organization") {
		t.Fatalf("another organization's site after the set-up: %v", err)
	}
}
