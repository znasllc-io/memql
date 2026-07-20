package parser

// Grammar-version stamp (S6, memql#2361). Nothing previously recorded which
// grammar a .memql file, pack, or authored-bundle row was written against, so
// a construct authored under an older grammar degraded to a boot Warn/Debug
// instead of a detectable, actionable mismatch (the product pack rotted
// exactly this way, bff#153).
//
// GrammarVersion is bumped ON EVERY GRAMMAR EPIC -- a change that retires or
// reshapes an authored form (new invocation syntax, retired annotation,
// payload-binding change). Each bump MUST ship a `memqlmigrate
// --rewrite=<epic>` mode: the codemod-per-epic pattern is the published
// migration channel (docs/public/language/authoring-rules.md).

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// GrammarVersion names the current grammar epoch. Format: <year>.<month>-<epic-slug>.
const GrammarVersion = "2026.07-null-coalescing-operator"

// GrammarFingerprint is a drift detector over the author-facing keyword
// surface: when the invocation-kind keyword set changes, the pinned test
// fails and forces a CONSCIOUS GrammarVersion bump (plus the migration mode
// that must accompany it).
func GrammarFingerprint() string {
	kws := InvocationKindKeywords()
	sort.Strings(kws)
	sum := sha256.Sum256([]byte(strings.Join(kws, "|")))
	return hex.EncodeToString(sum[:8])
}
