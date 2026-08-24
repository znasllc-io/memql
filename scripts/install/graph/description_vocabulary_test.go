package graph

import (
	"regexp"
	"strings"
	"testing"
)

// description_vocabulary_test.go -- znasllc-io/memql#4456.
//
// WHAT THIS GATE IS FOR. A step's `description` is not a comment: it is the
// sentence an operator reads. The wizard renders the running step's
// description under the progress bar as the whole of what it says about a
// ten-minute operation (memql#4454), and the CLI prints the same string. One
// source, two surfaces, and no other narration on either -- so a description
// written in the vocabulary of the thing that implements it is the product
// describing itself as a build system.
//
// They all were. "Inventory the machine: OS/arch support, docker daemon, port
// availability, free disk." is a commit message. "Write the identity bootstrap
// values ... so it creates its own owner instead of waiting on a /setup visit
// nobody was told about" is a design record. Each was true, and each was
// addressed to whoever was writing the installer rather than to whoever is
// running it.
//
// So the sweep is not the deliverable -- this is. A rewrite decays back the
// moment the next step is added by someone reading the file for examples, and
// what they would find beside a gate is fifteen product sentences and a test
// that says why.
//
// WHAT IT CANNOT DO is judge prose. It rejects vocabulary, which is the part
// that mechanises: internal nouns, identifiers, script names and issue
// references. A grammatical sentence in the wrong voice still gets through and
// still needs review. A gate that claimed more than that would be trusted for
// more than it does.

// camelCase catches every identifier that leaked into a sentence: a step id
// (`seedBootstrap`), an envelope field (`linkState`), a result key
// (`allPassed`). It is deliberately shaped so ordinary words and product names
// pass -- `MemQL`, `ArgoCD` and `k3d` all start with a capital or carry no
// internal one -- and so that nothing has to be enumerated. A name nobody has
// invented yet is caught on the day it is written.
var camelCase = regexp.MustCompile(`\b[a-z]+[A-Z][A-Za-z]*\b`)

// The nouns that describe HOW MemQL installs rather than WHAT is happening.
// Every one of these is a real word this repository uses correctly in its own
// documentation; what makes them wrong here is the audience.
var bannedWords = []string{
	"capability",
	"envelope",
	"graph",
	"wave",
	"overlay",
	"reconcile",
	"idempotent",
	"stderr",
	"stdout",
	"exit code",
	"predicate",
	"kustomize",
	"manifest",
	// Go's GOOS/GOARCH shorthand. An operator has a machine, not an os/arch
	// pair -- this one was found by the negative control below rather than by
	// reading the list, which is the whole reason that control exists.
	"os/arch",
}

// Shapes rather than words: a script filename, a result path, a command-line
// flag, an issue reference. Each is a thing the operator cannot act on and
// three of the four are things they cannot even see.
var bannedShapes = []struct {
	name string
	re   *regexp.Regexp
}{
	{"a script filename", regexp.MustCompile(`\.sh\b`)},
	{"an envelope result path", regexp.MustCompile(`\bresult\.`)},
	{"a command-line flag", regexp.MustCompile(`(^|\s)--[a-z]`)},
	{"an issue reference", regexp.MustCompile(`(?i)memql#\d+`)},
}

// problemsWith returns one sentence per rule the text breaks, and nil when it
// breaks none.
//
// PURE, AND SHARED BY THE GATE AND ITS NEGATIVE CONTROL. Written as a
// `checkDescription(t, ...)` first, which meant the control had to fabricate a
// `testing.T` to ask "would this have failed" -- a probe that leans on the
// zero value of another package's struct, and one that would go quietly
// useless if that ever stopped reporting. A function that returns its findings
// can simply be asked.
func problemsWith(text string) []string {
	var problems []string
	if strings.TrimSpace(text) == "" {
		return []string{"the description is empty"}
	}
	if found := camelCase.FindString(text); found != "" {
		problems = append(problems, "it names the identifier "+found+
			" -- a description is what the operator READS, so say what is happening in their terms")
	}
	lower := strings.ToLower(text)
	for _, word := range bannedWords {
		if strings.Contains(lower, word) {
			problems = append(problems, "it uses the internal word "+word+
				" -- that describes how MemQL installs, not what is happening to this machine")
		}
	}
	for _, shape := range bannedShapes {
		if shape.re.MatchString(text) {
			problems = append(problems, "it contains "+shape.name+", which the operator cannot act on")
		}
	}
	return problems
}

func checkDescription(t *testing.T, where, text string) {
	t.Helper()
	for _, problem := range problemsWith(text) {
		t.Errorf("%s: %s\n  in: %s\n  See memql#4456 for the sweep this replaced.", where, problem, text)
	}
}

func TestShippedDescriptionsAreWrittenForOperators(t *testing.T) {
	for _, doc := range []struct {
		name string
		load func() (*Graph, error)
	}{
		{"install.json", Install},
		{"install-main.json", InstallFromMain},
		{"uninstall.json", Uninstall},
		{"rebuild.json", Rebuild},
	} {
		t.Run(doc.name, func(t *testing.T) {
			// THE EMBEDDED DOCUMENT, not the file on disk, so this gate covers
			// exactly the bytes that ship rather than a copy beside them.
			g := mustLoadEmbedded(t, doc.load)
			checkDescription(t, doc.name, g.Description)
			for _, step := range g.Steps {
				checkDescription(t, doc.name+" step "+step.ID, step.Description)
			}
		})
	}
}

// The gate's own negative control: it must FAIL on the sentences it replaced.
//
// Without this the test above passes on a corpus that was already clean and
// says nothing about whether the detector works -- the shape of green that
// proves only that the instrument is switched off.
func TestTheVocabularyGateWouldHaveCaughtTheOldWording(t *testing.T) {
	for _, old := range []string{
		"Inventory the machine: OS/arch support, docker daemon, port availability, free disk.",
		"Create the k3d cluster, install ArgoCD, seed secrets and reconcile the local overlay.",
		"Recover the cluster owner's magic link from the identity pod, as a fallback sign-in " +
			"route. Identity logs a link only when one is REQUESTED, so a restarted pod carries " +
			"none -- an empty window is reported as linkState=none rather than failing the install.",
		"Take back exactly what the install graph put on this machine.",
		"Claim the cluster's owner recovery key and show it once -- on the terminal for a CLI " +
			"run, on the install done screen in the editor (memql#4079).",
	} {
		if problemsWith(old) == nil {
			t.Errorf("the gate accepted wording it exists to reject:\n  %s", old)
		}
	}
}
