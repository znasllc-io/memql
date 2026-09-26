package anchor

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// THE DEVELOPER-FLIPPABLE SET IS EXACTLY THE ANCHORED SET (Connect Shopify
// design, section 9, D4).
//
// A developer may turn a pack on or off only when it declared itself a
// storefront pack. Two drifts are both wrong: a pack anchored here that forgot
// to declare leaves the storefront a developer is taking live with a form
// nobody below owner can switch on, and a pack declaring itself without being
// anchored hands developers a switch over a pack no reviewer put on this list.
// This test holds the first; TestOnlyAnchoredPacksDeclareThemselvesStorefrontPacks
// holds the second.
func TestTheDeclaredStorefrontPacksAreTheAnchoredOnes(t *testing.T) {
	Storefront()

	want := append([]string(nil), Domains()...)
	sort.Strings(want)
	if got := memqldsl.StorefrontPacks(); !reflect.DeepEqual(got, want) {
		t.Fatalf("declared storefront packs = %v, anchored = %v. Call "+
			"dsl.RegisterStorefrontPack from the pack's Register, or take it off anchor.Domains()",
			got, want)
	}
}

// TestOnlyAnchoredPacksDeclareThemselvesStorefrontPacks is the direction the
// test above cannot see: its binary links only this package's imports, so a
// RegisterStorefrontPack call anywhere else -- a tag-gated example pack, a
// package wired from app/, a new pack -- never runs in it. So this reads the
// source instead.
func TestOnlyAnchoredPacksDeclareThemselvesStorefrontPacks(t *testing.T) {
	// The packages anchor.go imports. Anchoring a new storefront pack means
	// adding it in both places, on purpose.
	anchored := map[string]bool{"packs/reviewspack": true, "packs/wholesalepack": true}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(src), ".RegisterStorefrontPack(") {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		calls++
		if !anchored[filepath.ToSlash(rel)] {
			t.Errorf("%s calls RegisterStorefrontPack but is not an anchored storefront pack; "+
				"anchor it in packs/anchor or remove the call", filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls < len(anchored) {
		t.Fatalf("found %d RegisterStorefrontPack calls, fewer than the %d anchored packs -- "+
			"the walk is not reading the tree it should", calls, len(anchored))
	}
}
