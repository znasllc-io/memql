package agents

import (
	"os"
	"path/filepath"
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
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	type site struct {
		file string
		line int
		text string
	}
	var reads []site
	var scanned int

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Clean(name))
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		scanned++
		for i, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, ".Tier") {
				continue
			}
			reads = append(reads, site{file: name, line: i + 1, text: strings.TrimSpace(line)})
		}
	}

	// A sweep that stops resolving files passes vacuously, which is the same
	// silent-disable shape the docs gates guard against.
	if scanned < 2 {
		t.Fatalf("only %d non-test .go file(s) scanned in this package -- the sweep is broken, "+
			"not the code", scanned)
	}

	const wantRead = `"tier":            r.Tier,`
	if len(reads) != 1 {
		var got []string
		for _, r := range reads {
			got = append(got, r.file+":"+itoa(r.line)+"  "+r.text)
		}
		t.Fatalf("agentRole.tier is read in %d place(s); it must be read in exactly ONE, the "+
			"prompt catalog in roleCatalogForPrompt.\n  %s\n\n"+
			"If you have just implemented tier-keyed behaviour (a Tier-B disclaimer, a Tier-C "+
			"create-flow warning), that is a real feature and this test is not the obstacle -- but "+
			"the `tier` @description in dsl/agents/concepts.memql currently states, in as many "+
			"words, that NOTHING branches on this field. Update that description in the same "+
			"change and widen this test to match. memql#3068 exists because those two drifted "+
			"apart once already, and memql#3061 nearly took an authorization decision on the "+
			"strength of the wrong half.",
			len(reads), strings.Join(got, "\n  "))
	}
	if !strings.Contains(reads[0].text, "r.Tier") {
		t.Errorf("the single read of agentRole.tier is not the prompt-catalog one.\n"+
			"  got:  %s:%d  %s\n  want a line of the form: %s",
			reads[0].file, reads[0].line, reads[0].text, wantRead)
	}
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
