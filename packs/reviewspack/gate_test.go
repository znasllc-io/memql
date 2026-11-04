package reviewspack

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/parser"
)

// gate_test.go -- EVERY ACT A MODERATOR TAKES PASSES THE ORGANIZATION GATE.
//
// `execute app:deployables/reviews` is a Deployables part: a person granted it
// in one organization must not act on another's reviews with it. The shared
// gate (component/memql/organization_action_capabilities.go) can only check
// the right organization when it knows which row a call is about, so each
// acting builtin declares the part AND carries the argument the gate resolves
// from: the review (whose accountId was copied off its site) or the site.
// Pinned against the shipped DSL, so a rename on either side fails here.
//
// The shopper's builtins carry no part: a shopper is nobody to MemQL and
// holds no capability; their reach is the declared shopper surface.

func parsedBuiltins(t *testing.T) map[string]*ast.BuiltinDecl {
	t.Helper()
	raw, err := fs.ReadFile(Tree(), "builtins.memql")
	if err != nil {
		t.Fatal(err)
	}
	src, err := parser.NormaliseAll(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(src)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*ast.BuiltinDecl{}
	for _, def := range file.Definitions {
		if b, ok := def.(*ast.BuiltinDecl); ok {
			out[b.Name] = b
		}
	}
	return out
}

func declaresReviewsPart(b *ast.BuiltinDecl) bool {
	for _, a := range b.Attributes {
		if a.Name == "requiresCapability" && strings.Contains(fmt.Sprint(a.Value, a.Args), "app:deployables/reviews") &&
			strings.Contains(fmt.Sprint(a.Value, a.Args), "execute") {
			return true
		}
	}
	return false
}

func TestEveryActingBuiltinDeclaresTheReviewsPartAndItsTarget(t *testing.T) {
	builtins := parsedBuiltins(t)
	acting := map[string]string{
		"reviewsList":             "siteId",
		"reviewsUpdateSettings":   "siteId",
		"reviewsSetPublicDisplay": "siteId",
		"reviewsIssueRequest":     "siteId",
		"reviewsDecide":           "reviewId",
		"reviewsEraseReviewer":    "reviewId",
		"reviewsExportStore":      "siteId",
	}
	for name, target := range acting {
		b, ok := builtins[name]
		if !ok {
			t.Errorf("%s is not declared", name)
			continue
		}
		if !declaresReviewsPart(b) {
			t.Errorf("%s does not declare @requiresCapability(\"execute\", \"app:deployables/reviews\")", name)
		}
		found := false
		for _, f := range b.Fields {
			if f.Name == target {
				found = f.Required
			}
		}
		if !found {
			t.Errorf("%s does not require %q, the argument the organization gate resolves its target from", name, target)
		}
	}
	for _, name := range []string{"reviewsSubmitFromLink", "reviewsDraftForLink", "reviewsPublishedForProduct"} {
		if b, ok := builtins[name]; ok && declaresReviewsPart(b) {
			t.Errorf("%s is on the shopper surface and must carry no part: a shopper holds no capability", name)
		}
	}
	// The moderator notice is the store's automation's, which holds no part
	// and calls with internal origin; notify.go refuses anything else.
	if b, ok := builtins["reviewsNotifyModerator"]; !ok || declaresReviewsPart(b) {
		t.Error("reviewsNotifyModerator must be declared, and carry no part: its caller is an automation")
	}
}

func TestTheStoreWideActsRefuseASiteNotBoundToTheStore(t *testing.T) {
	op := operatorCtxFor("staff-1")
	elsewhere := map[string]any{"id": "site-x", "ownerUserId": "merchant-1", "binding": map[string]any{"storeId": "store-other"}}
	// settings
	if _, err := (&Provider{reader: &decideReader{settings: existingSettings(), site: elsewhere}, writer: &fakeWriter{}}).updateSettings(op,
		map[string]any{"storeId": "store-live", "siteId": "site-x", "notifyEmail": "a@example.com"}, 0); err == nil {
		t.Error("settings were changed through a site that does not serve the store")
	}
	// list
	if _, err := (&Provider{reader: &listReader{site: elsewhere}}).list(op,
		map[string]any{"storeId": "store-live", "siteId": "site-x"}, 0); err == nil {
		t.Error("the list answered through a site that does not serve the store")
	}
	// no site at all
	if _, err := (&Provider{reader: &listReader{}}).list(op, map[string]any{"storeId": "store-live"}, 0); err == nil {
		t.Error("the list answered with no site to judge the organization by")
	}
}
