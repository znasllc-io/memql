package memql

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// Which bundle a site row may name.
//
// A site's bundleRef and candidateRef are what the edge follows to the bytes it
// serves on that site's hostname, and a row names one of its own versions or
// nothing:
//
//   - blob://sites/<its id>/<version>/ -- what the publisher writes. It composes
//     the prefix from whichever spelling of the id its caller holds, so the bare
//     spelling and the canonical one are both the site's own.
//   - file://<directory> -- the directory the image ships a platform site in
//     (frontdoor.PlatformSiteBundleDir), for that platform site.
//   - empty -- nothing published yet, which a draft may hold.
//
// # Why the rule lives in the engine
//
// For the reason the store's own-token rule does (shopify_store_guard.go): it is
// the one place every writer converges, and the edge imports this package, so
// the write guard and the serving path ask the same function. A row the guard
// would refuse is not served, whichever binary wrote it.

// CodeSiteVersionNotItsOwn is the refusal a site row's reference gets when it
// names anything but a version of that site.
const CodeSiteVersionNotItsOwn = "site_version_not_its_own"

// CodeSiteIDNotOneSegment is the refusal a new site gets when its id cannot name
// one storage prefix.
const CodeSiteIDNotOneSegment = "site_id_not_one_segment"

// siteSegmentForm is one path segment of a version's prefix: a site's bare id, or
// the version beneath it, which is the publisher's content hash or a placeholder
// such as the OS's `pending`. Comparing ids decides nothing if two spellings can
// name one object, and Azure's naming rules say a path segment should not end in a
// dot and that reserved URL characters are escaped on the way in, so the form
// admits only the plain ones. Every id the engine generates fits it.
var siteSegmentForm = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9_-])?$`)

// siteBundleFields are the two fields of v1:platform:site the edge opens.
var siteBundleFields = []string{"bundleRef", "candidateRef"}

// SiteIDNamesOnePrefix reports whether id, bare or canonical, names exactly one
// prefix under sites/. A site's versions are what lies beneath its prefix, so the
// id has to name one.
func SiteIDNamesOnePrefix(id string) bool {
	return siteSegmentForm.MatchString(BareShortId(id))
}

// SiteNamesItsOwnBundle reports whether ref is a bundle the site may serve.
// siteID is the row's id, bare or canonical.
func SiteNamesItsOwnBundle(siteID, ref string) bool {
	if ref == "" {
		return true
	}
	if !SiteIDNamesOnePrefix(siteID) {
		return false
	}
	site := BareShortId(siteID)
	if dir, ok := strings.CutPrefix(ref, "file://"); ok {
		return dir != "" && dir == frontdoor.PlatformSiteBundleDir(site)
	}
	key, ok := strings.CutPrefix(ref, "blob://")
	if !ok {
		return false
	}
	// The opener adds a missing trailing slash, so its absence is not a different prefix.
	segments := strings.Split(strings.TrimSuffix(key, "/"), "/")
	if len(segments) != 3 || segments[0] != "sites" {
		return false
	}
	if segments[1] != site && segments[1] != conceptPlatformSite+":"+site {
		return false
	}
	return siteSegmentForm.MatchString(segments[2])
}

// validateSiteBundleRefs refuses a site row naming a bundle the edge would not
// serve, and a new site whose id cannot name one storage prefix.
//
// It judges the FINAL row against the stored one, beside the organization
// boundary and for its reason: a before-write hook may set a field the delta
// never named. prior is nil on a create.
//
// ONLY A NEW OR CHANGED REFERENCE IS JUDGED. next is the merged row, so every
// write carries the stored references, and a row written before this rule must
// stay writable, because the repair is an ordinary write to it. The edge serves
// nothing for such a reference meanwhile. Clearing one is always allowed.
//
// No actor is exempt. The publisher, a rollback and the seeds all name the site's
// own version, so nothing legitimate needs an exemption.
func validateSiteBundleRefs(id string, prior, next map[string]any) error {
	if prior == nil && id != "" && !SiteIDNamesOnePrefix(id) {
		return fmt.Errorf("v1:platform:site: %s: %q cannot be a site id. A site's id names its storage prefix, so it is one path segment of letters, digits, '.', '_' and '-'",
			CodeSiteIDNotOneSegment, BareShortId(id))
	}
	for _, field := range siteBundleFields {
		ref := stringFromAny(next[field])
		if ref == stringFromAny(prior[field]) || SiteNamesItsOwnBundle(id, ref) {
			continue
		}
		return fmt.Errorf("v1:platform:site: %s: %s %q is not a version of this deployable. A deployable names only versions published for it: publish a bundle to it, or point it at a version it has already served",
			CodeSiteVersionNotItsOwn, field, ref)
	}
	return nil
}
