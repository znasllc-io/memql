// Package vendorname holds the operator-specific names this repository must not
// contain, in ONE place.
//
// WHY A PACKAGE FOR A LIST OF STRINGS. Two tests ask the same question from
// different packages -- the repo-wide sweep in the root package, and the
// rendered-overlay assertion in deploy/k8s/overlays/local -- and Go gives them
// no other way to share an answer. They previously each spelled the domain
// themselves, which is a second copy of a fact, and a second copy is one that
// can be updated alone. It also means this file is now the ONLY file in the
// repository allowed to contain any of these strings, so the sweep needs
// exactly one exemption instead of one per consumer.
//
// WHAT BELONGS HERE. A name that identifies ONE operator's thing: a domain
// they own, a resource group, a cluster, a storage account, a directory object
// id. MemQL is company-agnostic (memql#3593, memql#4217) -- a hostname is the
// operator's input and an Azure resource is the operator's property, so neither
// is the engine's to name. Both live in that operator's own instance repository.
//
// WHAT DOES NOT BELONG HERE, and the distinction is easy to get wrong:
//
//   - `acrmemql` is the MemQL PROJECT's own container registry, where this
//     engine's official images are published. It is the product's, not a
//     client's, and banning it would be a category error.
//   - `znasllc-io` is the GitHub ORGANISATION this repository lives in. Source
//     URLs, module paths and image references legitimately name it, and no
//     entry below matches it -- the domain entries end in a dot, the org has a
//     hyphen.
//   - `id-memql-db` / `id-memql-mail` are a NAMING CONVENTION the entry-install
//     runbook itself suggests (`id-<product>-<purpose>`), not an identity. The
//     managed identity's client id IS below, because a GUID identifies exactly
//     one directory object and can mean nothing else.
//
// THE CAVEAT, stated where the list is rather than only where it is consumed:
// this is a banned-NAMES list. It catches these names and not the next one. A
// passing sweep is evidence that these strings are absent, and evidence of
// nothing else.
package vendorname

import "strings"

// Name is one banned literal together with what it names, so a failure can say
// WHAT was found rather than only that something was.
type Name struct {
	// Text is the literal to look for, lowercase. Matching is
	// case-insensitive; callers lowercase the haystack, not this.
	Text string
	// What names the thing, for the failure message.
	What string
}

// banned is the closed list. Order is stable so failure output is stable.
var banned = []Name{
	{"znas.io", "an operator's domain -- a hostname is a value, supplied as --domain"},
	{"znasllc.io", "the same operator's second domain"},
	{"rg-znas-memql", "an operator's Azure resource group (also matches its -backup sibling)"},
	{"aks-znas-memql", "an operator's AKS cluster"},
	{"stznasmemqlbackup", "an operator's Azure storage account"},
	{"e946f97f-f9b1-47f5-8b73-d732169d449b", "an operator's managed-identity client id"},
}

// Banned returns the names, as a copy so a caller cannot edit the list every
// other caller reads.
func Banned() []Name {
	out := make([]Name, len(banned))
	copy(out, banned)
	return out
}

// FirstIn reports the first banned name appearing anywhere in s, matched
// case-insensitively. The bool is false when s is clean.
//
// First rather than all, because the caller reports per line or per file and
// naming one hit is enough to send a reader to the right place.
func FirstIn(s string) (Name, bool) {
	lower := strings.ToLower(s)
	for _, n := range banned {
		if strings.Contains(lower, n.Text) {
			return n, true
		}
	}
	return Name{}, false
}
