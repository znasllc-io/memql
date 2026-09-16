package dslconformance

// THE SHIPPED POLICIES TRY THE DOORS IN COST ORDER, AND THE SHIPPED RULES NAME
// ONLY SHIPPED POLICIES (epic memql#5127, design D4 / D6).
//
// This gate is the third version of itself, and the property has changed twice
// with the design rather than being relaxed:
//
//   - memql#4676 said a seeded local-first policy must author NO cloud
//     fallback, because a chain reaching the cloud starts billing the moment a
//     laptop closes -- silently, since nothing about a working reply says which
//     vendor produced it.
//   - memql#5096 added a middle step that costs nothing extra (a Claude Code or
//     Codex subscription the person already pays for) and made the federation
//     hop ask the cost ceiling before it is taken, so the default falls through
//     and what the gate protects is the ORDER: local, then an app, then
//     anybody's money.
//   - memql#5127 split WHERE to look (a policy) from WHICH CALLS it is for (a
//     rule). Six policies became three, and the ordering property moved onto
//     rules as well: a shipped rule that named an unshipped policy would be a
//     routing decision with no chain behind it.
//
// IT IS A CORPUS SCAN rather than an engine assertion, for the reason the first
// version gave about itself: the property is about what the .memql files SAY,
// and the edit worth catching is an author moving a vendor entry to the front
// of a shipped chain "so it does not park". The failure messages therefore name
// the alternative -- an explicitly authored policy, or a custom rule at a
// higher precedence -- rather than only saying no.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// shippedPolicies is every policy this repository seeds. A policy added here
// opts into the ordering gate; a policy added to the .memql file and NOT here
// is caught by TestEveryShippedPolicyIsCovered below, so the list cannot
// silently fall behind the tree.
var shippedPolicies = []string{
	"localFirst",
	"fastLocalFirst",
	"localOnly",
	"federationStrongest",
	"embeddingsBinding",
}

// shippedRules is every rule this repository seeds, with the policy it names.
// The pair is spelled out rather than derived so that changing one without the
// other is a failure rather than a silent re-route.
var shippedRules = map[string]string{
	"default":                 "localFirst",
	"fastLane":                "fastLocalFirst",
	"backgroundLane":          "localFirst",
	"backgroundEscalation":    "localFirst",
	"operatorReasoning":       "localFirst",
	"reasoningParks":          "federationStrongest",
	"embeddingsBound":         "embeddingsBinding",
	"compilerLocalOnly":       "localOnly",
	"policyCompilerLocalOnly": "localOnly",
}

// retiredPolicies were deleted by memql#5127. A policy nothing can name is a
// decoration, and these six were named by call sites that now declare a level
// instead. They are listed so their return is a failure rather than a merge.
var retiredPolicies = []string{
	"balancedChat",
	"cheapestCapable",
	"fastCoding",
	"strongReasoning",
	"backgroundExecution",
	"backgroundEscalation",
}

const (
	embedderActiveRef = "embedder:active"
	fleetWildcardRef  = "fleet:*"
	fleetStrongestRef = "fleet:strongest"
	appWildcardRef    = "app:*"
	fleetRefPrefix    = "fleet:"
	appRefPrefix      = "app:"
	federationPrefix  = "federation:"
)

var (
	policyDeclRe = regexp.MustCompile(`(?m)^policy\s+([A-Za-z0-9_]+)\s*\{`)
	ruleDeclRe   = regexp.MustCompile(`(?m)^rule\s+([A-Za-z0-9_]+)\s*\{`)
	annotationRe = regexp.MustCompile(`^@(primary|fallback)\("([^"]*)"\)`)
	rulePolicyRe = regexp.MustCompile(`^@policy\("([^"]*)"\)`)
	precedenceRe = regexp.MustCompile(`^@precedence\((-?\d+)\)`)
	lockedRe     = regexp.MustCompile(`^@locked\s*$`)
)

// annotationsAbove walks BACKWARD from a declaration line collecting its
// annotation block, then REVERSES it.
//
// The reverse is load-bearing: a backward walk yields the block bottom-up, and
// a chain read in the wrong order would make every ordering assertion below
// check the opposite of what it says. Stopping at the first line that is
// neither an annotation nor a comment is what keeps one construct's
// annotations from being read as another's.
func annotationsAbove(lines []string, decl int) []string {
	var reversed []string
	for j := decl - 1; j >= 0; j-- {
		trimmed := strings.TrimSpace(lines[j])
		if strings.HasPrefix(trimmed, "@") {
			reversed = append(reversed, trimmed)
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "///") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		break
	}
	out := make([]string, 0, len(reversed))
	for k := len(reversed) - 1; k >= 0; k-- {
		out = append(out, reversed[k])
	}
	return out
}

func corpusLines(t *testing.T, parts ...string) []string {
	t.Helper()
	path := filepath.Join(append([]string{repoRoot(t)}, parts...)...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.Split(string(raw), "\n")
}

// policyChains returns each policy's provider chain in try order: the @primary
// first, then each @fallback.
func policyChains(t *testing.T) map[string][]string {
	t.Helper()
	lines := corpusLines(t, "dsl", "policies", "policies.memql")
	out := map[string][]string{}
	for i, line := range lines {
		m := policyDeclRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var chain []string
		for _, ann := range annotationsAbove(lines, i) {
			if a := annotationRe.FindStringSubmatch(ann); a != nil {
				chain = append(chain, a[2])
			}
		}
		out[m[1]] = chain
	}
	return out
}

type shippedRule struct {
	policy     string
	precedence int
	locked     bool
	hasPrec    bool
}

func ruleRecords(t *testing.T) map[string]shippedRule {
	t.Helper()
	lines := corpusLines(t, "dsl", "rules", "rules.memql")
	out := map[string]shippedRule{}
	for i, line := range lines {
		m := ruleDeclRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		rec := shippedRule{}
		for _, ann := range annotationsAbove(lines, i) {
			if a := rulePolicyRe.FindStringSubmatch(ann); a != nil {
				rec.policy = a[1]
			}
			if a := precedenceRe.FindStringSubmatch(ann); a != nil {
				rec.hasPrec = true
				rec.precedence = atoiOrFail(t, a[1])
			}
			if lockedRe.MatchString(ann) {
				rec.locked = true
			}
		}
		out[m[1]] = rec
	}
	return out
}

func atoiOrFail(t *testing.T, s string) int {
	t.Helper()
	n := 0
	neg := false
	for i, c := range s {
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			t.Fatalf("precedence %q is not an integer", s)
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		return -n
	}
	return n
}

// TestEveryShippedPolicyStartsAtTheCheapestDoor is the ordering property. Every
// shipped chain reaches the fleet before an app and an app before a vendor, so
// paid inference is last BY CONSTRUCTION rather than by care.
func TestEveryShippedPolicyStartsAtTheCheapestDoor(t *testing.T) {
	chains := policyChains(t)
	if len(chains) == 0 {
		t.Fatal("no policies parsed -- a gate over nothing passes for the wrong reason")
	}
	for _, name := range shippedPolicies {
		chain, ok := chains[name]
		if !ok {
			t.Errorf("shipped policy %q is not in dsl/policies/policies.memql", name)
			continue
		}
		if len(chain) == 0 {
			t.Errorf("policy %q has an empty chain; a policy with no entries resolves nothing", name)
			continue
		}
		// THE EMBEDDER DOOR HAS NO STATIC SIDE, so the door-ordering question
		// this gate asks does not have an answer for it (epic memql#5137).
		//
		// `embedder:active` resolves to whatever the operator BOUND, which may
		// be a model on their own machine or a vendor's. That is not this file
		// choosing a vendor first: it is the operator's own act, recorded on a
		// row, which is exactly the "explicit and lands on every decision
		// record" escape the message below names -- expressed as a binding
		// rather than as a custom policy, because an index is built with one
		// embedder and cannot be handed a second.
		//
		// The exemption is NARROW BY CONSTRUCTION rather than by list
		// membership: it applies to a one-entry chain naming exactly this
		// reference. A second entry after it would be a FALLBACK to a different
		// vector space, which is the defect the policy exists to prevent, and
		// the check below still catches it.
		if len(chain) == 1 && chain[0] == embedderActiveRef {
			continue
		}
		wantLocal := fleetStrongestRef
		if name == "fastLocalFirst" {
			wantLocal = "fleet:fastest"
		}
		if chain[0] != wantLocal {
			t.Errorf("policy %q starts at %q, not %q.\n"+
				"Every shipped chain reaches the person's own hardware first -- that is what makes\n"+
				"paid-last structural rather than a rule somebody remembers. If a chain genuinely\n"+
				"needs a vendor first, that is a CUSTOM policy named by a custom rule at a higher\n"+
				"precedence, where it is explicit and lands on every decision record.",
				name, chain[0], wantLocal)
		}
		seenApp, seenFederation := false, false
		for _, entry := range chain {
			switch {
			case strings.HasPrefix(entry, fleetRefPrefix):
				if seenApp || seenFederation {
					t.Errorf("policy %q reaches the fleet at %q AFTER a more expensive door; the order is local, app, vendor", name, entry)
				}
			case strings.HasPrefix(entry, appRefPrefix):
				if seenFederation {
					t.Errorf("policy %q reaches an app at %q after a vendor entry", name, entry)
				}
				seenApp = true
			default:
				seenFederation = true
			}
		}
	}
}

// TestNoShippedChainWritesTheRetiredFleetWildcard. `fleet:*` said "any", which
// is not what it did: it resolved to the STRONGEST eligible local model. The
// loader refuses it with the replacement in the message, and this catches an
// author reintroducing it in the corpus before boot does.
func TestNoShippedChainWritesTheRetiredFleetWildcard(t *testing.T) {
	for _, file := range [][]string{
		{"dsl", "policies", "policies.memql"},
		{"dsl", "rules", "rules.memql"},
	} {
		for i, line := range corpusLines(t, file...) {
			if strings.Contains(line, `"`+fleetWildcardRef+`"`) {
				t.Errorf("%s:%d writes the retired %q; write %q, which is what it always meant",
					filepath.Join(file...), i+1, fleetWildcardRef, fleetStrongestRef)
			}
		}
	}
}

// TestEveryShippedPolicyIsCovered keeps the list above from falling behind the
// file. A policy added to the corpus and not to shippedPolicies would be
// exempt from every assertion here while looking covered.
func TestEveryShippedPolicyIsCovered(t *testing.T) {
	chains := policyChains(t)
	covered := map[string]bool{}
	for _, n := range shippedPolicies {
		covered[n] = true
	}
	var uncovered []string
	for name := range chains {
		if !covered[name] {
			uncovered = append(uncovered, name)
		}
	}
	sort.Strings(uncovered)
	if len(uncovered) > 0 {
		t.Fatalf("policies in the corpus and not in shippedPolicies: %v.\n"+
			"Add them there so they are held to the ordering gate -- or delete them: this file's\n"+
			"own standing rule is that a policy nothing can name is a decoration.", uncovered)
	}
}

// TestTheRetiredPoliciesAreGone. Six policies were deleted because every call
// site that named them now declares a LEVEL instead, and a policy nothing can
// name is a decoration that reads like a default.
func TestTheRetiredPoliciesAreGone(t *testing.T) {
	chains := policyChains(t)
	for _, name := range retiredPolicies {
		if _, ok := chains[name]; ok {
			t.Errorf("policy %q is back in the corpus. It was deleted with epic memql#5127 because\n"+
				"the call sites that named it now declare a level, and a rule chooses the chain.\n"+
				"If this chain is wanted again it needs a RULE naming it, or it is a decoration.", name)
		}
	}
}

// TestEveryShippedRuleNamesAShippedPolicy is the rules half of the same
// property. A rule naming a policy that is not there is a routing decision with
// no chain behind it, and the failure mode is a call that resolves nothing.
func TestEveryShippedRuleNamesAShippedPolicy(t *testing.T) {
	rules := ruleRecords(t)
	if len(rules) == 0 {
		t.Fatal("no rules parsed from dsl/rules/rules.memql -- a gate over nothing passes for the wrong reason")
	}
	chains := policyChains(t)
	for name, rec := range rules {
		if rec.policy == "" {
			t.Errorf("rule %q names no policy; @policy is required", name)
			continue
		}
		if _, ok := chains[rec.policy]; !ok {
			t.Errorf("rule %q names policy %q, which is not shipped", name, rec.policy)
		}
	}
}

// TestTheShippedRuleSetMatchesTheDeclaredPolicies pins the set BY NAME and BY POLICY. A
// rule added to the corpus is a routing decision every cluster inherits, so it
// is a deliberate edit here as well as there.
func TestTheShippedRuleSetMatchesTheDeclaredPolicies(t *testing.T) {
	rules := ruleRecords(t)
	for name, wantPolicy := range shippedRules {
		rec, ok := rules[name]
		if !ok {
			t.Errorf("shipped rule %q is missing from dsl/rules/rules.memql", name)
			continue
		}
		if rec.policy != wantPolicy {
			t.Errorf("rule %q names policy %q, want %q", name, rec.policy, wantPolicy)
		}
	}
	for name := range rules {
		if _, ok := shippedRules[name]; !ok {
			t.Errorf("rule %q is in the corpus and not in shippedRules. A shipped rule is a routing\n"+
				"decision every cluster inherits; add it here with the policy it names, or make it a\n"+
				"custom rule an owner opts into.", name)
		}
	}
}

// TestEveryShippedRuleIsLocked. Locked means three things and all three are
// wanted for a shipped rule: it evaluates before every unlocked one, it is
// re-read from the embedded tree on every boot so nothing done at runtime
// survives a restart, and no runtime-authored rule may take its name.
func TestEveryShippedRuleIsLocked(t *testing.T) {
	for name, rec := range ruleRecords(t) {
		if !rec.locked {
			t.Errorf("shipped rule %q is not @locked. Without it an owner can redefine it at runtime and\n"+
				"the redefinition survives until a restart silently reverts it.", name)
		}
	}
}

// TestShippedRulePrecedencesAreDistinct. A tie between two rules of the same
// locked-ness is a LOAD ERROR, so a duplicate here would refuse boot -- this
// catches it in a test that names both rules instead.
func TestShippedRulePrecedencesAreDistinct(t *testing.T) {
	seen := map[int]string{}
	for name, rec := range ruleRecords(t) {
		if !rec.hasPrec {
			t.Errorf("shipped rule %q declares no @precedence; its position would depend on nothing", name)
			continue
		}
		if other, clash := seen[rec.precedence]; clash {
			t.Errorf("rules %q and %q both declare @precedence(%d). A tie is resolved by nothing, so the\n"+
				"rule that wins would differ between replicas -- and the loader refuses boot on it.",
				other, name, rec.precedence)
			continue
		}
		seen[rec.precedence] = name
	}
}

// TestTheDefaultRuleStatesNoConditions is what makes "a call that matches no
// rule is impossible" true by construction rather than by care. Every reader
// downstream assumes it, and none of them handles the other case.
func TestTheDefaultRuleStatesNoConditions(t *testing.T) {
	lines := corpusLines(t, "dsl", "rules", "rules.memql")
	found := false
	for i, line := range lines {
		m := ruleDeclRe.FindStringSubmatch(line)
		if m == nil || m[1] != "default" {
			continue
		}
		found = true
		for _, ann := range annotationsAbove(lines, i) {
			if strings.HasPrefix(ann, "@when(") && ann != "@when()" {
				t.Errorf("the default rule states a condition (%s). It is the FLOOR: the moment it can fail\n"+
					"to match, a call can match nothing, and nothing downstream handles that case.", ann)
			}
		}
	}
	if !found {
		t.Fatal("no rule named `default` in dsl/rules/rules.memql; it is the floor and cannot be removed")
	}
}

// retiredPolicyLiterals is retiredPolicies minus one.
//
// `backgroundEscalation` is excluded because the WORD survived the policy: it
// is now a call TAG (core/airoute.TagBackgroundEscalation) that the shipped
// rule of the same name matches on. A tag is exactly what replaced the policy,
// so banning the string would ban the replacement.
var retiredPolicyLiterals = []string{
	"balancedChat",
	"cheapestCapable",
	"fastCoding",
	"strongReasoning",
	"backgroundExecution",
}

// TestNoGoSourceNamesAPolicyByStringLiteral is the gate that keeps the deleted
// Go literals from growing back.
//
// Before this epic the role-to-policy mapping lived TWICE -- once in
// dsl/policies as @preferredRole and once in integrations/agent/replier.go as a
// pair of string literals -- and the two happened to agree with nothing
// enforcing it. The DSL half is gone; this stops the Go half from returning,
// and it caught one that had already grown: integrations/agents/factory.go
// stamped `policyName: "balancedChat"` on every new agent row, defaulted from a
// role-catalog field, long after the replier stopped reading it.
//
// It walks the AST rather than the text, so a policy named in a DOC COMMENT --
// which component/language/ast/ast.go legitimately does, showing what a policy
// declaration looks like -- is not a finding. A textual scan flags those, and a
// gate that flags its own documentation is one somebody switches off.
func TestNoGoSourceNamesAPolicyByStringLiteral(t *testing.T) {
	root := repoRoot(t)
	names := map[string]bool{}
	for _, n := range append(append([]string{}, shippedPolicies...), retiredPolicyLiterals...) {
		names[n] = true
	}
	fset := token.NewFileSet()
	for _, path := range trackedGoFilesForPolicyGate(t, root) {
		if strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, "dsl/") {
			continue
		}
		// component/memql and component/router legitimately name policies: one
		// loads them and the other walks them.
		if strings.HasPrefix(path, "component/memql/") || strings.HasPrefix(path, "component/router/") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(root, path), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value := strings.Trim(lit.Value, "`\"")
			if !names[value] {
				return true
			}
			t.Errorf("%s:%d names policy %q as a Go string literal.\n"+
				"A call site declares a LEVEL; a rule chooses the policy. A policy name in Go is a\n"+
				"routing decision that no rule can see and no decision record explains.",
				path, fset.Position(lit.Pos()).Line, value)
			return true
		})
	}
}

// trackedGoFilesForPolicyGate reads the git INDEX rather than walking the
// filesystem, so an untracked scratch file cannot red the gate and a deleted
// one cannot green it -- the reason every repo-walking gate in this tree does
// the same.
func trackedGoFilesForPolicyGate(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, p := range strings.Split(string(out), "\x00") {
		if strings.TrimSpace(p) != "" {
			files = append(files, p)
		}
	}
	if len(files) < 200 {
		t.Fatalf("the index listed %d .go files, which is too few for this tree", len(files))
	}
	return files
}
