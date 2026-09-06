package memql

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// osTargetsPath is the MemQL OS target registry, relative to this package.
// The path is written out rather than derived so a failure names the file a
// reader has to go and open.
const osTargetsPath = "../../clients/os/src/apps/deployables/targets.ts"

// osOfferedKindsPattern matches the registry's offered-kinds literal and
// captures the bracketed list. It accepts exactly the one-line form
//
//	export const OFFERED_KINDS = ["spa", "static", "shopify_storefront"] as const;
//
// and nothing computed: the gate compares NAMES, and a list assembled at
// runtime or broken across lines is one it cannot read. It says so rather
// than passing.
var osOfferedKindsPattern = regexp.MustCompile(
	`(?m)^\s*export\s+const\s+OFFERED_KINDS\s*=\s*\[([^\]]*)\]\s*as\s+const\s*;?\s*$`,
)

// TestSiteKindEnumMatchesOsOfferedKinds holds the OS's offered kinds equal to
// v1:platform:site.kind, in both directions.
//
// THERE ARE DELIBERATELY TWO LISTS. The enum is what the engine accepts; the
// registry is what the OS offers a control for (epic memql#4885, design
// section B), and the OS cannot ask the engine per keystroke which kinds
// exist. So the risk is not duplication, it is DRIFT, and drift here is
// invisible in the worst way: a kind the enum grew and the OS does not offer
// is a deployable nobody can create from the shell, and a kind the OS offers
// that the enum lacks is a picker entry whose every write is refused. Both
// sides keep working. This reads the list out of the TypeScript, the way
// TestFleetOnlineWindowMatchesTheClients reads the online window, and fails when
// it stops matching the LOADED concept.
//
// A MISSING FILE IS A FAILURE, NOT A SKIP. A skip would make this gate
// silently vacuous the moment the registry moved, which is exactly when the
// two lists are most likely to have diverged.
func TestSiteKindEnumMatchesOsOfferedKinds(t *testing.T) {
	siteDecl(t)
	c, ok := memorynodes.All()[conceptPlatformSite]
	if !ok || c == nil {
		t.Fatalf("%s is not in the concept registry", conceptPlatformSite)
	}
	enum := siteKindEnumValues(t, c)

	raw, err := os.ReadFile(osTargetsPath)
	if err != nil {
		t.Fatalf("the OS target registry is unreadable at %s: %v\n"+
			"This gate is the only thing keeping v1:platform:site.kind and the kinds the OS\n"+
			"offers in step. If the file moved, update osTargetsPath; if it was deleted, the OS\n"+
			"is offering kinds some other way and that needs a decision, not a skip.",
			osTargetsPath, err)
	}
	m := osOfferedKindsPattern.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s does not export OFFERED_KINDS as a one-line `[...] as const` literal.\n"+
			"It must, and as a literal rather than a computed expression: this gate compares the\n"+
			"NAMES, and a list assembled at runtime is one it cannot read.",
			osTargetsPath)
	}
	offered := parseStringList(string(m[1]))
	if len(offered) == 0 {
		t.Fatalf("OFFERED_KINDS in %s is empty; the web target offers nothing", osTargetsPath)
	}

	enumSet := map[string]bool{}
	for _, k := range enum {
		enumSet[k] = true
	}
	offeredSet := map[string]bool{}
	for _, k := range offered {
		offeredSet[k] = true
	}

	for _, k := range offered {
		if !enumSet[k] {
			t.Fatalf("%s offers kind %q, which v1:platform:site.kind does not declare.\n"+
				"  OS OFFERED_KINDS       = %v\n"+
				"  v1:platform:site.kind  = %v\n"+
				"A kind offered in the shell and refused by every write is a picker entry that can\n"+
				"only fail. Either add the value to the enum (a DESIGN change: each one costs a\n"+
				"resolution tail in component/edge) or take it out of the registry.",
				osTargetsPath, k, offered, enum)
		}
	}
	for _, k := range enum {
		if !offeredSet[k] {
			t.Fatalf("v1:platform:site.kind declares %q, which %s does not offer.\n"+
				"  v1:platform:site.kind  = %v\n"+
				"  OS OFFERED_KINDS       = %v\n"+
				"A kind the engine accepts and the shell offers no control for is a deployable\n"+
				"nobody can create from the OS. Add it to OFFERED_KINDS and its picker entry, or\n"+
				"take it out of the enum.",
				k, osTargetsPath, enum, offered)
		}
	}
}

// parseStringList reads the comma-separated string literals inside a
// TypeScript array literal, sorted. Anything that is not a quoted string --
// an identifier, a spread -- is left out, so a computed member fails the
// comparison rather than being guessed at.
func parseStringList(inner string) []string {
	var out []string
	for _, part := range strings.Split(inner, ",") {
		s := strings.TrimSpace(part)
		if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
			out = append(out, s[1:len(s)-1])
		}
	}
	sort.Strings(out)
	return out
}
