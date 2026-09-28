package main

import (
	"regexp"
	"testing"
)

// The escape hatch added in memql#4125 had no users in the tree when it
// landed, so TestRetiredVocabulary's own sweep could not demonstrate it
// works -- a zero-hit exemption path is indistinguishable from a broken one.
// It has users now (memql#5721's portal triage), but this still pins the
// behaviour directly rather than trusting that those lines keep existing.
func TestLineIsVocabExempt(t *testing.T) {
	banned := "run `make release VERSION=0.19.1` to inspect the image locally"

	cases := []struct {
		name string
		line string
		want bool
	}{
		{"no marker", banned, false},
		{"marker with reason", banned + " <!-- retired-vocabulary-ok: local inspection, memql#4116 -->", true},
		{"marker with no reason", banned + " <!-- retired-vocabulary-ok: -->", false},
		{"marker unterminated", banned + " <!-- retired-vocabulary-ok: local inspection", false},
		{"marker name alone is not enough", banned + " retired-vocabulary-ok", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lineIsVocabExempt(tc.line); got != tc.want {
				t.Errorf("lineIsVocabExempt(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// TestRetiredVocabularyCatchesTheRetiredShapes pins what the memql#5721
// patterns are FOR. Their sweep over the real tree finds nothing once the
// sweep's fixes land, and a pattern that matches nothing is indistinguishable
// from one that can never match -- so each retired shape is written down here
// as the sentence it would be, beside the current sentences it must leave
// alone. The "caught" lines are phrasings the tree really carried; the
// "allowed" lines are current pages the patterns were probed against.
func TestRetiredVocabularyCatchesTheRetiredShapes(t *testing.T) {
	compiled := make([]*regexp.Regexp, len(retiredVocabulary))
	for i, rv := range retiredVocabulary {
		compiled[i] = regexp.MustCompile(rv.pattern)
	}
	matches := func(line string) bool {
		for _, re := range compiled {
			if re.MatchString(line) {
				return true
			}
		}
		return false
	}

	caught := []string{
		"| `si.completion.started` | `SI_COMPLETION_STARTED` | Emitted when an AI request begins |",
		"EVENT_KIND_SI_COMPLETION_ERROR = 503;",
		"2. **Drive** it from the **Cockpit** (terminal-native ops) or **MemQL OS**",
		"| **Cockpit** | the terminal IDE | a developer's machine |",
		"memql-cockpit -- terminal-native IDE and operations console",
		"3. **Run** it as one binary locally or as the node mesh for scale; same",
		"The MemQL portal's Deployments view was it (memql#3319 + memql#3380)",
		"Rollouts reference: the product carrier repo's `deploy/rollouts/README.md`",
		"see the product pack repo's docs/operate/deployment-strategy.md",
		"go build -tags voice -o bin/memql-voice .",
		"make dev NODE=cognition",
		"Node types (bff, voice, cognition, agent, planner) share one database.",
		"The **BFF** / **Voice** / **Cognition** nodes",
		"PK: (partition, id, createdAt)",
	}
	allowed := []string{
		"| `ai.completion.started` | `si_completion_started` | Emitted when an AI request begins |",
		"> The Cockpit Editor was the second consumer until the Cockpit's TUI was",
		"| **MemQL OS** | the graphical ops console | one instance |",
		"So it is one binary per cluster.",
		"linux/amd64, **before the cognition and voice node types were removed** -- so",
		"The cognition node (a node type since removed), agent and planner all",
		"`make secrets` seeds the `memql-voice` secret idempotently, preserving existing",
		"The cluster's agent node runs the voice controller in Go: speech detection,",
		"product DSL is delivered at runtime via the dsl-bundle component, not carrier-built",
		"column at all and its primary key is `(id, \"createdAt\")` -- read it in",
	}

	for _, line := range caught {
		if !matches(line) {
			t.Errorf("no retiredVocabulary pattern catches a retired shape it exists for:\n  %s", line)
		}
	}
	for _, line := range allowed {
		if matches(line) {
			t.Errorf("a retiredVocabulary pattern flags a current, correct sentence:\n  %s", line)
		}
	}
}
