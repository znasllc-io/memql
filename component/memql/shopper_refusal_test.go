package memql

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// shopper_refusal_test.go -- A PACK MAY NAME ITS OWN REFUSALS, AND ONLY THOSE.
//
// The surface answers a closed set of four codes, and any refusal from a
// construct used to become "failed". A pack whose construct knows WHY it
// refused (a review link that expired, a link already used) declares those
// codes on its form or read; the handler then tells the shopper by name, and
// still answers "failed" for anything the pack did not declare.

func TestARefusalSurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("reviews: submit: %w", NewShopperRefusal("link_expired", "the link expired on 2026-11-01"))
	code, ok := ShopperRefusalCode(err, []string{"link_expired", "link_used"})
	if !ok || code != "link_expired" {
		t.Fatalf("ShopperRefusalCode = %q, %v; want link_expired through a %%w wrap", code, ok)
	}
	if !strings.Contains(err.Error(), "the link expired") {
		t.Fatalf("Error() = %q, want the message kept for the log", err.Error())
	}
}

func TestAnUndeclaredRefusalIsNotForwarded(t *testing.T) {
	if code, ok := ShopperRefusalCode(NewShopperRefusal("link_bogus", "x"), []string{"link_expired"}); ok {
		t.Fatalf("an undeclared code %q was forwarded; only a declared code reaches a shopper", code)
	}
	if _, ok := ShopperRefusalCode(errors.New("plain"), []string{"link_expired"}); ok {
		t.Fatal("a plain error was read as a refusal")
	}
}

func registerPanics(t *testing.T, register func()) (msg string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	register()
	return ""
}

func TestADeclaredRefusalMustBeACodeOfItsOwn(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	for _, bad := range []string{"failed", "invalid", "too_large", "unavailable", "Link-Expired", "", "link expired"} {
		ResetShopperSurfaceForTest()
		msg := registerPanics(t, func() {
			RegisterShopperForm(ShopperForm{
				Pack: "reviews", Name: "review", Construct: "submitReview",
				Fields:     []ShopperField{{Name: "body", Required: true}},
				RedirectOK: "/ok", RedirectError: "/problem",
				Refusals:   []string{bad},
			})
		})
		if msg == "" {
			t.Errorf("refusal %q was accepted; a declared code must be a lower_snake code the surface does not already answer", bad)
		}
	}
	ResetShopperSurfaceForTest()
	if msg := registerPanics(t, func() {
		RegisterShopperRead(ShopperRead{
			Pack: "reviews", Name: "draft", Construct: "reviewsDraftForLink", Kind: ShopperReadKindBuiltin,
			Fields:   []ShopperField{{Name: "requestToken", Required: true}},
			Refusals: []string{"link_expired", "link_used", "link_invalid"},
		})
	}); msg != "" {
		t.Fatalf("a read declaring three refusals was refused: %s", msg)
	}
	if got := ShopperReadFor("reviews", "draft").Refusals; len(got) != 3 {
		t.Fatalf("Refusals = %v, want the three declared", got)
	}
}
