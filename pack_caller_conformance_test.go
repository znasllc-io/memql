package main

import (
	"strings"
	"testing"
)

// TestOnlyAllowlistedPackagesMarkAPackCaller is component/auth's pack_caller.go
// rule made enforceable, as TestOnlyAllowlistedPackagesStampInternalOrigin is
// for the origin (ruling 57).
//
// The mark names the pack whose Go is making one call. A builtin that trusts it
// (beside internal origin) admits that pack's call and nothing else -- a
// registered automation's step carries internal origin, which the executor
// stamps on every package's steps, but never the mark, which DSL cannot set.
// That is only true while the set of packages that can SET the mark is small
// and reviewed, and while the set of places that READ it as authority is the
// one gate that also requires internal origin. Both sets are pinned here, by
// the same parsed walk the origin gate uses (authReferences: references, not
// greps, aliases resolved, test files excluded).
//
// THE MARK DOES NOT TRAVEL AS AUTHORITY. It is set inline on one Execute, and
// it rides that call's context into whatever the builtin then does; the only
// guard that matters is that nothing there reads it. So the reader list is
// exactly one file, and a second reader -- however well meant -- fails here
// and needs its own review.
func TestOnlyAllowlistedPackagesMarkAPackCaller(t *testing.T) {
	setters := map[string]string{
		"packs/wholesalepack": "the wholesale pack's engineCaller (entitlement.go) marks its one Execute -- the call of " +
			"the one builtin an entitlement adapter names -- as the wholesale pack's, INLINE beside its internal-origin " +
			"stamp, after the pack's own checks (the same call the origin gate's packs/wholesalepack reason describes); " +
			"the Shopify wholesale builtins admit internal origin only beside it (entitlement_test.go " +
			"TestTheCallerStampsInternalOriginOnItsOneCall)",
	}
	readers := map[string]string{
		"integrations/shopify/wholesale_capabilities.go": "requireWholesaleCaller, the first statement of the five " +
			"Shopify wholesale builtins, which admits a call only with internal origin AND the wholesale pack's mark",
	}
	wirePath := []string{"component/grpc", "component/node"}

	set := authReferences(t, "ContextWithPackCaller")
	if len(set) == 0 {
		t.Fatal("no ContextWithPackCaller references found anywhere -- this gate has stopped resolving them and would now pass vacuously")
	}
	for _, dir := range sortedKeys(set) {
		for _, banned := range wirePath {
			if dir == banned || strings.HasPrefix(dir, banned+"/") {
				t.Errorf("%s marks a pack caller on a context derived from an inbound request:\n  %s", dir, strings.Join(set[dir], "\n  "))
			}
		}
		if _, ok := setters[dir]; !ok {
			t.Errorf("%s marks a pack caller but is not in the allowlist:\n  %s\nA mark is what a pack-only builtin admits; "+
				"setting one is a deliberate act a reviewer sees, with the reason it is that pack's own call.",
				dir, strings.Join(set[dir], "\n  "))
		}
	}
	for _, dir := range sortedKeys(setters) {
		if _, ok := set[dir]; !ok {
			t.Errorf("stale allowlist entry %q -- it no longer marks a pack caller, so remove it", dir)
		}
	}
	// One site, in the caller, so the mark cannot drift onto the writer's or a
	// reader's context in the same package.
	if sites := set["packs/wholesalepack"]; len(sites) != 1 || !strings.HasPrefix(sites[0], "packs/wholesalepack/entitlement.go:") {
		t.Errorf("the wholesale pack must mark exactly one call, engineCaller's in entitlement.go: %v", sites)
	}

	read := authReferences(t, "PackCallerFromContext")
	got := map[string][]string{}
	for _, sites := range read {
		for _, site := range sites {
			file := site[:strings.Index(site, ":")]
			got[file] = append(got[file], site)
		}
	}
	if len(got) == 0 {
		t.Fatal("no PackCallerFromContext references found anywhere -- the gate that trusts the mark is gone, or this has stopped resolving it")
	}
	for file, sites := range got {
		if _, ok := readers[file]; !ok {
			t.Errorf("%s reads the pack caller mark:\n  %s\nThe mark rides the marked call's context into everything the "+
				"builtin does; only the one gate that also requires internal origin may read it as authority.",
				file, strings.Join(sites, "\n  "))
		}
		if len(sites) != 1 {
			t.Errorf("%s reads the mark %d times, want once (requireWholesaleCaller): %v", file, len(sites), sites)
		}
	}
	for file := range readers {
		if _, ok := got[file]; !ok {
			t.Errorf("stale reader entry %q -- it no longer reads the mark, so remove it", file)
		}
	}
}
