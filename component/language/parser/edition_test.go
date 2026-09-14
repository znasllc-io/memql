package parser

import (
	"strings"
	"testing"
)

// The current edition is always registered, and its front end is the core
// grammar itself: Prepare hands the author's text to the pipeline unchanged.
func TestCurrentEditionFrontEndIsTheCore(t *testing.T) {
	fe, err := FrontEndFor(Edition)
	if err != nil {
		t.Fatalf("FrontEndFor(%q): %v", Edition, err)
	}
	if fe.GrammarVersion != GrammarVersion {
		t.Errorf("current edition's grammar = %q, want GrammarVersion %q", fe.GrammarVersion, GrammarVersion)
	}
	src := "concept probe {\n  a string\n}\n"
	got, err := fe.Prepare(src)
	if err != nil || got != src {
		t.Errorf("current edition's Prepare changed the source (err=%v):\n%s", err, got)
	}
}

// An edition the engine does not have is refused by name, listing the ones it
// does -- the author's next move is to pick one of them.
func TestUnknownEditionNamesTheKnownOnes(t *testing.T) {
	_, err := FrontEndFor("1999")
	if err == nil {
		t.Fatal("FrontEndFor(1999) accepted an edition this engine does not have")
	}
	for _, want := range []string{`"1999"`, Edition} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}
}

// A registered edition is readable until its remover runs, and the table is
// left as it was found. The current edition cannot be removed through a
// remover it did not hand out.
func TestRegisterEditionIsScopedByItsRemover(t *testing.T) {
	remove := RegisterEdition(FrontEnd{
		Edition:        "2099",
		GrammarVersion: "2099.01-probe-00000000",
		Prepare:        func(src string) (string, error) { return strings.ReplaceAll(src, "predicate ", "trait "), nil },
	})
	fe, err := FrontEndFor("2099")
	if err != nil {
		t.Fatalf("registered edition not readable: %v", err)
	}
	out, _ := fe.Prepare("predicate isProbe {\n  return a == true\n}\n")
	if !strings.HasPrefix(out, "trait isProbe") {
		t.Errorf("registered front end did not run: %q", out)
	}
	if got := Editions(); len(got) != 2 || got[0] != Edition || got[1] != "2099" {
		t.Errorf("Editions() = %v, want [%s 2099] oldest first", got, Edition)
	}
	remove()
	if _, err := FrontEndFor("2099"); err == nil {
		t.Error("edition still readable after its remover ran")
	}
	if _, err := FrontEndFor(Edition); err != nil {
		t.Errorf("removing a synthetic edition removed the current one: %v", err)
	}
}

func TestRegisterEditionRefusesADuplicate(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("registering the current edition a second time did not panic")
		}
	}()
	RegisterEdition(FrontEnd{Edition: Edition, Prepare: func(s string) (string, error) { return s, nil }})
}

// The language line is a major.minor pair: it is what a tree's memql.toml
// compares against, so a malformed constant would make every tree unreadable.
func TestLanguageVersionIsMajorMinor(t *testing.T) {
	parts := strings.Split(LanguageVersion, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("LanguageVersion = %q, want <major>.<minor>", LanguageVersion)
	}
	for _, p := range parts {
		for _, r := range p {
			if r < '0' || r > '9' {
				t.Fatalf("LanguageVersion = %q, want digits only", LanguageVersion)
			}
		}
	}
}
