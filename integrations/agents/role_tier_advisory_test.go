package agents

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// memql#3068: `v1:agents:agentRole.tier` documented safety behaviour that was
// never implemented -- a Tier-B disclaimer injected into the agent's system
// prompt at materialization time, and a Tier-C create-flow warning. Neither
// exists. The field's only consumer is roleCatalogForPrompt, which passes it to
// the agent-factory prompts as advisory text.
//
// # Why this is gated rather than just corrected
//
// A wrong description is cheap to fix and expensive to leave, because things
// read it as a contract. That is not hypothetical here: memql#3061 nearly kept
// `tier` writable on a locked predefined row ON THE STRENGTH OF THIS DOC,
// reasoning it was an operator-tunable knob. That was safe only because the
// documented behaviour did not exist -- had the disclaimer injection been real,
// the carve-out would have been a safety-control downgrade, editable by a
// non-system caller, guarded by no test.
//
// So correcting the sentence is necessary and not sufficient. The two ways it
// goes stale again are symmetrical, and this file pins both:
//
//   - someone IMPLEMENTS tier-keyed behaviour and leaves the description saying
//     it is advisory (TestAgentRoleTierIsPromptAdvisoryOnly), or
//   - someone reverts the description to the old claim while the code stays
//     advisory (TestAgentRoleTierDescriptionSaysAdvisory).
//
// Either way the failure names the other half, so whoever trips it knows what
// to change rather than deleting the assertion.

// TestAgentRoleTierIsPromptAdvisoryOnly pins that roleSnapshot.Tier is READ in
// exactly one place: the prompt catalog.
//
// It scans for `.Tier` field reads, which deliberately does NOT match the two
// non-read sites -- the struct field declaration (`Tier string`) and the
// composite-literal key that populates it (`Tier: stringField(...)`) -- because
// neither carries a selector dot. A new consumer is virtually always a read, so
// this is the shape that catches it.
func TestAgentRoleTierIsPromptAdvisoryOnly(t *testing.T) {
	// The paths a tier-keyed control would ACTUALLY land in, which is the point.
	//
	// The version this replaces scanned one directory, non-recursively -- the
	// package holding the field declaration. The documented-but-absent behaviour
	// was a disclaimer injected "at materialization time" and a create-flow
	// warning, and neither would be written here: the system prompt is assembled
	// in integrations/agent, and the server-side write validator that already
	// loads the role row lives in component/memql. Measured: a tier-keyed
	// create-flow REJECTION added to component/memql passed the old sweep, while
	// the concept description said no such thing existed. That is memql#3068
	// reappearing with its own guard green.
	roots := []string{".", "../agent", "../../component/memql"}

	// Known sites, as `<path-tail> | <trimmed line>`. Every entry is a read this
	// sweep has been shown NOT to be about agentRole.tier -- a different concept's
	// tier, a log field, or the two non-read sites on the field itself.
	//
	// A baseline rather than a cleverer pattern because the alternative is
	// guessing which `.Tier` belongs to which type from text, and a matcher that
	// guesses wrong either misfires (and gets deleted) or misses. An entry here
	// is a claim someone checked; a NEW line is a claim nobody has.
	known := map[string]string{
		"factory.go | Tier:                  stringField(payload, \"tier\"),":             "populates the field; not a read",
		"factory.go | \"tier\":              r.Tier,":                                     "THE consumer: roleCatalogForPrompt",
		"worker/dispatch.go | detail[\"tier\"] = cls.Tier.String()":                       "classifier tier, not agentRole",
		"nonstreaming.go | \"tier\", \"cheap\",":                                          "log field",
		"nonstreaming.go | \"tier\", \"escalation\",":                                     "log field",
		"healing_base_immutable_validation.go | merged := isBaseTier(payload[\"tier\"])":  "healing base tier",
		"executor_mutation.go | meta.priorBaseTier = isBaseTier(priorPayload[\"tier\"])":  "healing base tier",
		"skill_tier_validation.go | tier := readSeedStringField(def.Body, \"tier\")":      "skill/domain tier",
		"skill_tier_validation.go | skillTier := readSeedStringField(def.Body, \"tier\")": "skill tier",
	}
	// rowauthz_shadow.go and rowauthz_enforce.go carry a whole rowAuthz
	// decl.Tier surface -- `langparser.RowAuthzDecl.Tier`, the tier a CONCEPT
	// declares about who may see its rows, which has nothing to do with
	// agentRole.tier. Enumerating each line would make this baseline rot on
	// every edit to those files for no signal. (rowauthz_enforce.go joined the
	// list in memql#3172, which turned that surface from a measurement into
	// read-path enforcement: the reads multiplied, the concept behind them did
	// not change.)
	skipFiles := map[string]bool{
		"rowauthz_shadow.go":  true,
		"rowauthz_enforce.go": true,
	}

	var unknown []string
	var scanned int
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			base := filepath.Base(path)
			if skipFiles[base] {
				return nil
			}
			data, readErr := os.ReadFile(filepath.Clean(path))
			if readErr != nil {
				return readErr
			}
			scanned++
			for i, line := range strings.Split(string(data), "\n") {
				if !strings.Contains(line, ".Tier") && !strings.Contains(line, `"tier"`) {
					continue
				}
				trimmed := strings.TrimSpace(line)
				key := shortKeyFor(path) + " | " + trimmed
				if _, ok := known[key]; ok {
					continue
				}
				unknown = append(unknown, fmt.Sprintf("%s:%d  %s", path, i+1, trimmed))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	// A sweep that stops resolving files passes vacuously, which is the same
	// silent-disable shape the docs gates guard against.
	if scanned < 20 {
		t.Fatalf("only %d non-test .go file(s) scanned across %v -- the sweep is broken and would "+
			"pass vacuously", scanned, roots)
	}

	if len(unknown) > 0 {
		sort.Strings(unknown)
		t.Errorf("a tier read appeared that this sweep has not seen before:\n  %s\n\n"+
			"If it reads agentRole.tier, then dsl/agents/concepts.memql's description of that "+
			"field is now WRONG -- it says the field is advisory and that nothing branches on it. "+
			"Fix the code or fix the description; do not just add the line here. That description "+
			"once claimed a disclaimer injection that did not exist, and an authorization decision "+
			"was nearly taken on it (memql#3068, memql#3061).\n\n"+
			"If it reads some OTHER concept's tier, add it to `known` with the reason.",
			strings.Join(unknown, "\n  "))
	}
}

// shortKeyFor is the baseline key: enough path to disambiguate, not so much that
// moving a package rewrites every entry.
func shortKeyFor(path string) string {
	base := filepath.Base(path)
	if parent := filepath.Base(filepath.Dir(path)); parent == "worker" {
		return parent + "/" + base
	}
	return base
}

// TestAgentRoleTierDescriptionSaysAdvisory is the other direction: the concept
// description must keep stating the limit, so the sentence memql#3068 corrected
// cannot quietly come back while the code is still advisory.
//
// It asserts the load-bearing CLAIM rather than the whole paragraph, so the
// wording stays editable -- pinning prose verbatim makes a test that fails on
// every copy-edit and gets deleted.
func TestAgentRoleTierDescriptionSaysAdvisory(t *testing.T) {
	const rel = "../../dsl/agents/concepts.memql"
	data, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	src := string(data)

	if !strings.Contains(src, "ADVISORY ONLY on this concept (memql#3068)") {
		t.Errorf("the agentRole.tier description no longer states that the field is advisory " +
			"only. If tier-keyed behaviour has been implemented, say so there and widen " +
			"TestAgentRoleTierIsPromptAdvisoryOnly; if it has not, the limit has to stay written " +
			"down -- memql#3061 nearly took an authorization decision on the strength of the " +
			"version of this sentence that claimed behaviour which did not exist.")
	}

	// The specific false claims memql#3068 removed. They describe a control that
	// does not exist, and each is the sentence that misled a prior change.
	for _, gone := range []string{
		"disclaimer injected into their system prompt at materialization time",
		"the create-agent flow surfaces a stronger",
	} {
		if strings.Contains(src, gone) {
			t.Errorf("the agentRole.tier description has regained a claim memql#3068 removed as "+
				"untrue: %q\nNothing branches on agentRole.tier. The disclaimer machinery is keyed "+
				"on knowledgeDomain.tier (integrations/knowledge/seed_domain_content.go), not on "+
				"this field.", gone)
		}
	}
}

// itoa avoids pulling strconv in for one call site in a failure path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
