package conformance

// refusal_wording_test.go -- every refusal the corpus names carries its
// WORDING, not just its code (memql#5387; D23 and D24 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// D24 is the requirement these pins exist for: "every refusal names the
// construct, the position, the rule id and the replacement, and when the fix
// is mechanical, the rewritten line; the corpus pins the wording". A refusal
// CODE is a contract with a machine. The MESSAGE is the contract with the
// author, and it is the half that rots: a refactor that keeps the code and
// loses the sentence naming `memqlmigrate --rewrite=expressions` turns a
// two-minute fix into an afternoon, and no test notices, because the code
// still matches.
//
// TWO PINS, AND THEY WATCH DIFFERENT THINGS.
//
//   - TestEveryNamedRefusalIsPinned reads the corpus DATA -- every
//     expect.json, through the one decoder corpus_test.go uses -- and requires
//     every refusal case to pin a message. corpusValidateCase refuses the same
//     thing at discovery, but it does so with a t.Fatalf on the FIRST offender
//     inside TestCorpusVerdicts, which stops the whole runner and names one
//     file. This pin reads the data without running the engine, so it lists
//     every unpinned case at once and says why the message matters; and it
//     fails on the two shapes the discovery check cannot see at all -- a
//     corpus the reader has stopped reading, and a corpus with no refusal
//     cases left in it. Both of those make every gate over the corpus pass by
//     matching nothing.
//   - TestRefusalsNameTheirReplacement reads the RETIREMENT TABLES rather
//     than the corpus: a refusal for a retired form must say what to write
//     instead. That is a claim about the tree's own data, not about English.

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// refusalVerdicts are the verdicts that carry a message contract.
var refusalVerdicts = map[string]bool{verdictRefuseParse: true, verdictRefuseLoad: true}

// corpusCaseRef is one case as the wording pins read it: where it is and what
// it claims, with nothing of the engine attached.
type corpusCaseRef struct {
	Path    string // "<edition>/cells/query/actor/with-true.memql"
	Verdict string
	Code    string
	Message string
}

// allCorpusCases reads every case of every edition directory, through the same
// walk and the same expect.json decoder the verdict runner uses
// (corpusEditions / corpusExpectDirsIn / readCorpusExpect in corpus_test.go).
// It deliberately does NOT apply corpusValidateCase: the point of this reader
// is to see the corpus as it is written, so a case the runner would fatal on
// is reported here as a listed finding rather than hiding the rest.
func allCorpusCases(t *testing.T) []corpusCaseRef {
	t.Helper()
	root := os.DirFS(".")
	editions, err := corpusEditions(root)
	if err != nil {
		t.Fatalf("read test/conformance: %v", err)
	}
	var out []corpusCaseRef
	for _, edition := range editions {
		dirs, err := corpusExpectDirsIn(root, edition)
		if err != nil {
			t.Fatalf("walk %s: %v", edition, err)
		}
		for _, dir := range dirs {
			ef, err := readCorpusExpect(root, dir)
			if err != nil {
				t.Fatalf("%v", err)
			}
			for _, c := range ef.Cases {
				out = append(out, corpusCaseRef{
					Path:    dir + "/" + c.File,
					Verdict: c.Verdict,
					Code:    c.Code,
					Message: c.Message,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func TestEveryNamedRefusalIsPinned(t *testing.T) {
	cases := allCorpusCases(t)
	if len(cases) == 0 {
		t.Fatal("no corpus cases were read at all: the pin would pass over an empty set, which is the " +
			"fail-open shape a completeness gate exists to prevent")
	}
	var refusals, pinned int
	var unpinned []string
	for _, c := range cases {
		if !refusalVerdicts[c.Verdict] {
			continue
		}
		refusals++
		if strings.TrimSpace(c.Message) == "" {
			unpinned = append(unpinned, fmt.Sprintf("%s (%s)", c.Path, c.Verdict))
			continue
		}
		pinned++
	}
	if refusals == 0 {
		t.Fatal("the corpus holds no refusal cases at all -- either the reader stopped reading or every " +
			"negative case was deleted; both make this pin vacuous")
	}
	if len(unpinned) > 0 {
		sort.Strings(unpinned)
		t.Errorf("%d of %d refusal cases pin no message.\n\n"+
			"A refusal's CODE is the contract with a machine; its MESSAGE is the contract with the author, "+
			"and it is the half that rots silently -- a refactor keeps the code and drops the sentence naming "+
			"the replacement, and nothing notices.\n\n"+
			"Add a \"message\" to each case below: a distinctive substring of what the engine actually says, "+
			"including the replacement it names. Run the engine against the fixture and copy what it says; "+
			"never pin a guess.\n\n  %s",
			len(unpinned), refusals, strings.Join(unpinned, "\n  "))
	}
	if !t.Failed() {
		t.Logf("refusal-wording pins: %d refusal cases of %d cases, all pinned", pinned, len(cases))
	}
}

// TestRefusalsNameTheirReplacement is the D24 half that matters most and is
// scoped to where it is checkable: a refusal for a RETIRED form must name what
// to write instead. The retirement tables carry the replacement text
// (annotations.Retirements()), so this is a claim about the tree's own data,
// not about English.
func TestRefusalsNameTheirReplacement(t *testing.T) {
	// A retirement's whole value to an author is the sentence that says what
	// to write instead. annotations.Retirements() carries that text as data,
	// so this is checkable: every retirement's hint must be non-empty and must
	// be a sentence rather than a bare token.
	rs := annotations.Retirements()
	if len(rs) == 0 {
		t.Fatal("annotations.Retirements() is empty: either the table moved or the accessor stopped " +
			"reading it, and this pin passes over nothing either way")
	}
	for _, r := range rs {
		hint := strings.TrimSpace(r.Hint)
		if hint == "" {
			t.Errorf("retirement %s names no replacement: an author who writes it is told only that it is "+
				"gone, which is the half of the message that does not help", retirementLabel(r))
			continue
		}
		if !strings.ContainsAny(hint, " ") {
			t.Errorf("retirement %s's hint is a bare token (%q) rather than a sentence naming the fix",
				retirementLabel(r), hint)
		}
	}
	if !t.Failed() {
		t.Logf("replacement pins: %d retirements, every one naming what to write instead", len(rs))
	}
}

// retirementLabel names a retirement the way an author meets it: the
// annotation, and the receiver it is retired on when it is not retired
// everywhere. A family entry says so, since its Name is a prefix rather than
// a name anybody writes.
func retirementLabel(r annotations.Retirement) string {
	name := "@" + r.Name
	if r.Prefix {
		name = "@" + r.Name + "* (family)"
	}
	if r.Receiver == "" {
		return name
	}
	return fmt.Sprintf("%s on %s", name, r.Receiver)
}
