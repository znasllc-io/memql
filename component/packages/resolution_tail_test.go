package packages

import (
	"strings"
	"testing"
	"testing/fstest"
)

// tailManifest is validManifest's twin with a declared resolution tail on the
// storefront -- the shape the first real storefront needs: a multi-page
// prerendered bundle that 404s a mistyped path AND keeps its store binding.
const tailManifest = `formatVersion: 1
name: acme
deployables:
  - name: storefront
    path: clients/web
    kind: shopify_storefront
    resolutionTail: not_found
    binding:
      store: acme.myshopify.com
  - name: docs
    path: clients/docs
    kind: static
`

func tailPackage(manifest string) fstest.MapFS {
	p := validPackage()
	p[ManifestName] = file(manifest)
	return p
}

// A DECLARED TAIL REACHES THE REPORT, which is what EnsureSite reads. Without
// this the field is a yaml key the pipeline drops on the floor.
func TestADeclaredResolutionTailReachesTheReport(t *testing.T) {
	rep, err := Analyze(tailPackage(tailManifest), Options{SourceVersion: "abc123"})
	if err != nil {
		t.Fatalf("package refused: %v (problems: %+v)", err, rep.Problems)
	}
	if got := rep.Deployables[0].ResolutionTail; got != ResolutionTailNotFound {
		t.Errorf("storefront resolutionTail = %q, want %q", got, ResolutionTailNotFound)
	}
	// The reachable positive for the assertion below: the SAME analysis, on a
	// deployable that declares nothing, carries nothing.
	if got := rep.Deployables[1].ResolutionTail; got != "" {
		t.Errorf("a deployable declaring no tail carries %q, want empty", got)
	}
}

// AN UNRECOGNISED TAIL IS REFUSED AT READ TIME, naming both values.
//
// The edge reads an unrecognised tail on a ROW as absent, deliberately -- a
// typo must not take a live site's client-side routes dark. A manifest is
// read before anything is created, so the opposite rule applies: tell the
// author, rather than deploying a site that quietly falls back forever.
func TestAnUnrecognisedResolutionTailIsRefused(t *testing.T) {
	bad := strings.Replace(tailManifest, "resolutionTail: not_found", "resolutionTail: 404", 1)
	rep, err := Analyze(tailPackage(bad), Options{SourceVersion: "abc123"})
	if err == nil && rep.OK {
		t.Fatal("a manifest declaring resolutionTail: 404 was accepted")
	}
	msg := err.Error()
	for _, want := range []string{"404", ResolutionTailFallback, ResolutionTailNotFound} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not name %q: %s", want, msg)
		}
	}
}

// OMITTING IT IS NOT AN ERROR, on any kind. Every manifest in the world today
// omits it, and each must keep analyzing clean.
func TestOmittingTheResolutionTailIsValid(t *testing.T) {
	rep, err := Analyze(validPackage(), Options{SourceVersion: "abc123"})
	if err != nil || !rep.OK {
		t.Fatalf("the unchanged fixture stopped analyzing clean: %v (%+v)", err, rep.Problems)
	}
	for _, d := range rep.Deployables {
		if d.ResolutionTail != "" {
			t.Errorf("deployable %q invented a tail: %q", d.Name, d.ResolutionTail)
		}
	}
}

// ValidResolutionTail's own table, including the empty case that makes the
// field additive.
func TestValidResolutionTail(t *testing.T) {
	for tail, want := range map[string]bool{
		"":                     true,
		ResolutionTailFallback: true,
		ResolutionTailNotFound: true,
		"404":                  false,
		"index.html":           false,
		"Fallback":             false,
	} {
		if got := ValidResolutionTail(tail); got != want {
			t.Errorf("ValidResolutionTail(%q) = %v, want %v", tail, got, want)
		}
	}
}

// ResolutionTailIsSet answers a DIFFERENT question from ValidResolutionTail,
// and the difference is the empty string: valid, and not a choice. It is what
// decides whether createSite is passed the argument at all, and passing an
// explicit "" would write a value the row's enum refuses.
func TestResolutionTailIsSetTreatsEmptyAsNoChoice(t *testing.T) {
	if ResolutionTailIsSet("") {
		t.Error("the empty tail reads as a choice; createSite would be passed an empty enum value")
	}
	if !ResolutionTailIsSet(ResolutionTailNotFound) || !ResolutionTailIsSet(ResolutionTailFallback) {
		t.Error("a named tail does not read as a choice")
	}
}
