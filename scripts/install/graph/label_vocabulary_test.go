package graph

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// label_vocabulary_test.go -- the short-name gate beside the sentence gate.
//
// WHAT A LABEL IS FOR. A step's `label` is the status line a progress display
// shows while the step runs ("Creating the cluster") and the name the CLI
// prints as it starts. It is read at a glance, many times, by somebody waiting
// on a ten-minute operation -- so it has a tighter shape than a description:
// sentence case, at most MaxLabelWords words, no closing stop, and none of the
// vocabulary the description gate already refuses.
//
// The loader enforces presence and length, because a document that breaks those
// cannot be rendered. This gate enforces VOICE, which is a property of the
// shipped documents rather than of the format: a fixture labelled "Checking"
// is a fine fixture.
//
// Like the description gate, it rejects vocabulary and shape, and it cannot
// judge prose. A label in the right shape and the wrong words still needs a
// reviewer.

// Words a status line never uses, on top of everything the description gate
// refuses. `receipt` is here and not there because a description may one day
// need to explain what an uninstall reads; a five-word status line never does.
var bannedLabelWords = []string{
	"receipt",
	"wave",
	"exit code",
}

// properNouns are the capitalised words a sentence-case label may carry after
// its first word. Enumerated rather than inferred: "Creating The Cluster" and
// "Checking Docker" differ only in whether the capital belongs to a name, and
// only a list can know which.
var properNouns = map[string]bool{
	"MemQL":  true,
	"Docker": true,
	"AI":     true,
	"ArgoCD": true,
}

// toolNames may appear in a label only on a step that installs or removes that
// tool. Everywhere else they are the machinery showing through: "Seeding the
// k3d cluster" is a status line about an implementation, and the operator's
// cluster is simply "the cluster".
var toolNames = []string{"k3d", "kubectl", "mkcert"}

func isToolStep(id string) bool {
	return strings.HasPrefix(id, "tool") || strings.HasPrefix(id, "removeTool")
}

// labelProblems returns one sentence per rule the label breaks, nil when none.
// Pure, for the reason problemsWith is: the negative control asks it directly.
func labelProblems(stepID, label string) []string {
	if strings.TrimSpace(label) == "" {
		return []string{"the label is empty"}
	}
	// Everything the sentence gate refuses, a label refuses too.
	problems := problemsWith(label)

	words := strings.Fields(label)
	if len(words) > MaxLabelWords {
		problems = append(problems, "it has more than four words -- the sentence belongs in description")
	}
	first, _ := utf8.DecodeRuneInString(words[0])
	if !unicode.IsUpper(first) {
		problems = append(problems, "it does not start with a capital -- a label is sentence case")
	}
	for _, w := range words[1:] {
		r, _ := utf8.DecodeRuneInString(w)
		if unicode.IsUpper(r) && !properNouns[w] {
			problems = append(problems, "it capitalises "+w+" -- a label is sentence case, not title case")
		}
	}
	if last := label[len(label)-1]; strings.ContainsRune(".:;!,", rune(last)) {
		problems = append(problems, "it ends in punctuation -- a status line has no closing stop")
	}
	lower := strings.ToLower(label)
	for _, word := range bannedLabelWords {
		if strings.Contains(lower, word) {
			problems = append(problems, "it uses the internal word "+word)
		}
	}
	if !isToolStep(stepID) {
		for _, tool := range toolNames {
			for _, w := range strings.Fields(lower) {
				if w == tool {
					problems = append(problems, "it names the tool "+tool+
						" -- only the step that installs or removes a tool may name it")
				}
			}
		}
	}
	return problems
}

func TestShippedLabelsAreShortAndPlain(t *testing.T) {
	for _, doc := range shippedGraphs {
		t.Run(doc.name, func(t *testing.T) {
			g := mustLoadEmbedded(t, doc.load)
			for _, step := range g.Steps {
				for _, problem := range labelProblems(step.ID, step.Label) {
					t.Errorf("%s step %s: %s\n  label: %q", doc.name, step.ID, problem, step.Label)
				}
			}
		})
	}
}

// THE SAME STEP IS NAMED THE SAME WAY IN EVERY DOCUMENT. install-main.json and
// update-rebuild.json share step ids with install.json and rebuild.json; a
// clusterUp that read "Creating the cluster" on one lane and something else on
// the other would make one install look like two different products. The
// variant tests already pin the whole step; this states the label rule on its
// own, so a failure says what is wrong in the words that matter here.
func TestAStepIdHasOneLabelAcrossDocuments(t *testing.T) {
	seen := map[string]string{}
	where := map[string]string{}
	for _, doc := range shippedGraphs {
		g := mustLoadEmbedded(t, doc.load)
		for _, step := range g.Steps {
			if prev, ok := seen[step.ID]; ok && prev != step.Label {
				t.Errorf("step %s is labelled %q in %s and %q in %s", step.ID, prev, where[step.ID], step.Label, doc.name)
				continue
			}
			seen[step.ID] = step.Label
			where[step.ID] = doc.name
		}
	}
}

// The gate's negative control: every shape it exists to refuse, refused.
func TestTheLabelGateRefusesWhatItIsFor(t *testing.T) {
	for _, bad := range []struct{ id, label string }{
		{"clusterUp", "Creating The Cluster"},
		{"clusterUp", "creating the cluster"},
		{"clusterUp", "Creating the cluster and seeding it"},
		{"clusterUp", "Creating the cluster."},
		{"clusterUp", "Seeding the k3d cluster"},
		{"removeCluster", "Reading the receipt"},
		{"clusterUp", "Waiting for wave 3"},
		{"seedBootstrap", "Running seedBootstrap"},
		{"frontDoor", "Checking the capability"},
		{"frontDoor", "Reading the exit code"},
		{"clusterUp", ""},
	} {
		if labelProblems(bad.id, bad.label) == nil {
			t.Errorf("the gate accepted %q on %s", bad.label, bad.id)
		}
	}
	// And the two things it must NOT refuse: a proper noun mid-label, and a
	// tool named by the step that installs it.
	for _, good := range []struct{ id, label string }{
		{"dockerAccess", "Checking Docker"},
		{"stackCheckout", "Downloading MemQL"},
		{"toolK3d", "Installing k3d"},
		{"removeToolKubectl", "Removing kubectl"},
		{"magicLink", "Preparing sign-in"},
	} {
		if problems := labelProblems(good.id, good.label); problems != nil {
			t.Errorf("the gate refused %q on %s: %v", good.label, good.id, problems)
		}
	}
}
