package dslconformance

// EVERY PROMPT DECLARES A LEVEL, AND THE ASSIGNMENT IS ASSERTED BY NAME
// (epic memql#5127, design D3).
//
// # Why the assignment and not only the presence
//
// "Every prompt carries @level" is the load gate's job and the loader refuses
// boot without it. What that cannot catch is a level that is PRESENT and wrong:
// `authoringEmit` at `fast` loads perfectly and quietly emits constructs from
// the cheapest model in the tree, and the only symptom is worse output that
// nobody attributes to a routing change.
//
// So the table below is the assignment itself, spelled out. Changing a prompt's
// level is then an edit in two places -- a deliberate act a reviewer sees --
// rather than a one-word change in a file of forty annotations.
//
// # The three the record did not name
//
// The design record's D3 lists fifteen prompts by name. Three in this corpus
// appear nowhere in it: `consolidateMemory`, `plannerAgent` and
// `forgeMentoringExplanation`. Each is assigned here deliberately, with its
// reason, rather than defaulted -- an unlisted prompt taking whatever level the
// author of the sweep happened to type is exactly the silent-wrong-level case
// above.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// levels is the closed set. A fifth would be an abstraction a person cannot
// hold in their head, which is the whole argument for four.
var levels = map[string]bool{"fast": true, "strong": true, "reasoning": true, "embeddings": true}

// promptLevels is the assignment, by prompt name.
//
// fast: triage, intake, classification, summaries and suggestions -- work whose
// answer is short, bounded and cheap to redo.
// strong: an agent's reply, a conductor turn, an authoring design pass -- work a
// person reads and judges.
// reasoning: emitting or repairing a construct, and re-planning -- work where a
// confident wrong answer is worse than no answer, which is why the shipped
// reasoningParks rule parks rather than degrading it.
var promptLevels = map[string]string{
	// Named in D3.
	"goalComplexityTriage": "fast",
	"responsibilityIntake": "fast",
	"classifySymptom":      "fast",
	"docSummary":           "fast",
	"askSpecialist":        "fast",
	"seedDomainBridge":     "fast",
	"seedDomainContent":    "fast",
	"trainerAgent":         "strong",
	"agentFactoryAnalyze":  "strong",
	"reactiveConductor":    "strong",
	"authoringDesign":      "strong",
	"authoringEmit":        "reasoning",
	"authoringRepair":      "reasoning",
	"replanGap":            "reasoning",
	"compileGoal":          "reasoning",

	// NOT named in D3, assigned here with the reason.
	//
	// plannerAgent: an orchestrator persona that decomposes goals and decides
	// dispatch. The loop that invoked it is retired (memql#5052) and the prompt
	// still loads, so it takes the level the work would need if anything
	// invoked it again -- which is the same band as agentReply, not the
	// reasoning band, because it chooses among options rather than authoring.
	"plannerAgent": "strong",
	// workAgentReply: executes one owned work step with tools and reports its
	// result. The agent chooses actions and writes a user-facing reply; it does
	// not emit executable DSL, so the agent reply band is sufficient.
	"workAgentReply": "strong",
	// deriveProcedureHole: proposes how ONE argument of a learned procedure is
	// derived from an earlier step's result (epic memql#5402, D6's single
	// bounded call). Reasoning rather than fast, and the reason is the shape
	// of the mistake rather than the difficulty: the answer is an EXPRESSION
	// in a closed grammar that must reproduce a value in every recorded
	// instance, and a confidently wrong one that happens to hold on the two
	// instances it was shown is exactly what the engine then checks and
	// rejects -- so a cheaper band buys nothing but rejected calls. It sits
	// beside authoringEmit for the same reason: it writes something
	// executable, and reasoningParks means an unavailable door parks rather
	// than degrading it, which here costs one free parameter and never a
	// wrong derivation.
	"deriveProcedureHole": "reasoning",
	// consolidateMemory: distils at most ONE durable belief from a clustered
	// set of episodic rows. A short bounded answer over material already
	// narrowed by similarity, which is the fast band's shape exactly.
	"consolidateMemory": "fast",
	// forgeMentoringExplanation: three to six sentences of feedback to a
	// non-owner submitter. Prose, short, and re-runnable.
	"forgeMentoringExplanation": "fast",

	// The router's own two (epic memql#5137, D7 and D9).
	//
	// compileRule: turns one sentence into a rule. It reads as authoring, and
	// the reasoning band is where authoring lives -- but it is deliberately
	// STRONG, and the difference is who checks the answer. authoringEmit and
	// authoringRepair produce a construct that goes on to run, so a confident
	// wrong answer becomes cluster behaviour; this one produces a rule a person
	// reads beside a restatement and a simulation of what it would have done to
	// their last hundred calls, and confirms or discards. A human gate that
	// SHOWS THE CONSEQUENCE is what makes strong sufficient here.
	//
	// It is also the one prompt whose subject is where work may run, which is
	// why the shipped compilerLocalOnly rule pins it to the fleet at precedence
	// 120 and parks rather than degrading. The level says how capable a model
	// it needs; the rule says whose hardware -- and reasoning would have made
	// those two fight, since reasoningParks routes to federationStrongest.
	"compileRule": "strong",
	// composeRoutingPolicy proposes an owner-reviewed draft, like compileRule.
	// Strong is sufficient because the person reviews it before routing changes;
	// policyCompilerLocalOnly keeps the authoring request on the fleet.
	"composeRoutingPolicy": "strong",
	// classifyRequest: one word plus a confidence, over a request the
	// deterministic table already declined to settle. Short, bounded, and cheap
	// to redo -- the fast band's shape exactly, and it must stay there: this is
	// a classifier that runs to AVOID spending, so a classifier that spends is
	// the failure it exists to prevent.
	"classifyRequest": "fast",
}

var (
	promptDeclRe = regexp.MustCompile(`(?m)^prompt\s+([A-Za-z0-9_]+)\s*\{`)
	levelAnnRe   = regexp.MustCompile(`^@level\("([^"]*)"\)`)
)

// corpusPromptLevels walks every dsl/**/prompts.memql and returns the level
// each prompt declares, plus the file it was found in.
func corpusPromptLevels(t *testing.T) (map[string]string, map[string]string) {
	t.Helper()
	root := repoRoot(t)
	levelOf := map[string]string{}
	fileOf := map[string]string{}

	matches, err := filepath.Glob(filepath.Join(root, "dsl", "*", "prompts.memql"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range matches {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("reading %s: %v", path, readErr)
		}
		lines := strings.Split(string(raw), "\n")
		rel, _ := filepath.Rel(root, path)
		for i, line := range lines {
			m := promptDeclRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			name := m[1]
			fileOf[name] = rel
			levelOf[name] = ""
			for _, ann := range annotationsAbove(lines, i) {
				if a := levelAnnRe.FindStringSubmatch(ann); a != nil {
					levelOf[name] = a[1]
				}
			}
		}
	}
	if len(levelOf) == 0 {
		t.Fatal("no prompts found under dsl/*/prompts.memql -- a gate over nothing passes for the wrong reason")
	}
	return levelOf, fileOf
}

// TestEveryPromptCarriesALevel. The loader refuses boot without one; this says
// so in a test that names the prompt, so a corpus edit fails here rather than
// at somebody's next `make up`.
func TestEveryPromptCarriesALevel(t *testing.T) {
	levelOf, fileOf := corpusPromptLevels(t)
	var missing []string
	for name, level := range levelOf {
		switch {
		case level == "":
			missing = append(missing, name+" ("+fileOf[name]+"): no @level")
		case !levels[level]:
			missing = append(missing, name+" ("+fileOf[name]+"): @level("+level+") is not one of fast, strong, reasoning, embeddings")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%d prompt(s) without a usable level:\n  %s\n\n"+
			"A prompt declares how much intelligence its call needs, never a model. Without one the\n"+
			"router has nothing to match a rule on, and the loader refuses boot rather than guessing.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestTheD3LevelAssignmentHoldsByName is the half that catches a level that is
// present and wrong.
func TestTheD3LevelAssignmentHoldsByName(t *testing.T) {
	levelOf, fileOf := corpusPromptLevels(t)
	for name, want := range promptLevels {
		got, ok := levelOf[name]
		if !ok {
			t.Errorf("prompt %q is in the assignment table and not in the corpus. If it was deleted,\n"+
				"delete its row here in the same change; a table naming prompts that do not exist\n"+
				"stops being read.", name)
			continue
		}
		if got != want {
			t.Errorf("prompt %q (%s) declares @level(%q), and the assignment says %q.\n"+
				"Changing a prompt's level is a routing change: it decides which rule matches and,\n"+
				"through reasoningParks and embeddingsPark, whether an exhausted chain degrades or\n"+
				"parks. Change both, or neither.", name, fileOf[name], got, want)
		}
	}
}

// TestEveryCorpusPromptIsInTheAssignment keeps the table from falling behind
// the tree. A prompt added to the corpus and not here would carry whatever
// level its author typed, unreviewed -- which is the case this file exists for.
func TestEveryCorpusPromptIsInTheAssignment(t *testing.T) {
	levelOf, fileOf := corpusPromptLevels(t)
	var unlisted []string
	for name := range levelOf {
		if _, ok := promptLevels[name]; !ok {
			unlisted = append(unlisted, name+" ("+fileOf[name]+")")
		}
	}
	sort.Strings(unlisted)
	if len(unlisted) > 0 {
		t.Fatalf("prompt(s) in the corpus and not in the assignment table: %s\n\n"+
			"Add each with the level it needs AND the reason, the way the three the design record\n"+
			"did not name are recorded above. A level nobody argued for is one nobody will notice\n"+
			"is wrong.", strings.Join(unlisted, ", "))
	}
}

// TestAgentReplyIsNotInThisCorpusAndSaysWhy.
//
// D3 assigns `agentReply` the `strong` level and it is not in this repository:
// it lives in the product pack's runtime DSL, mounted at MEMQL_DSL_PATH. The
// pack loader holds it to the same rule, so the pack repo needs the same sweep
// and no change here can make it.
//
// The test exists so that reading the table above and finding agentReply absent
// is answered here rather than read as an omission -- and so that if the prompt
// ever DOES move into this tree, the note stops being true loudly.
func TestAgentReplyIsNotInThisCorpusAndSaysWhy(t *testing.T) {
	levelOf, _ := corpusPromptLevels(t)
	if _, ok := levelOf["agentReply"]; ok {
		t.Fatal("agentReply is now declared in this repository's corpus. It used to live only in the " +
			"product pack's runtime DSL, which is why the assignment table above does not carry it. " +
			"Add it to promptLevels with @level(\"strong\") and delete this test.")
	}
}
