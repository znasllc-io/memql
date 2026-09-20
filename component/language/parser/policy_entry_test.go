package parser

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/airoute"
)

// TestValidatePolicyEntry_AcceptsEveryFormOfTheClosedGrammar walks the whole
// accepted surface. The grammar is CLOSED, so a form missing from this table is
// a form the loader will refuse, and the table is what says which is which.
func TestValidatePolicyEntry_AcceptsEveryFormOfTheClosedGrammar(t *testing.T) {
	for _, tc := range []struct {
		entry string
		why   string
	}{
		{"streamClaudeSonnet", "a bare name is a provider registry entry"},
		{"chat54Mini", "digits inside an identifier are legal"},

		{"fleet:strongest", "the strongest model on the person's own machines"},
		{"fleet:fastest", "the quickest one"},
		{"fleet:qwen3.8:27b", "a model id may itself contain a colon, which is why the split takes the FIRST one"},
		{"fleet:llama3.3-70b", "a model id carries dots and dashes; they are somebody else's naming convention"},

		{"app:*", "any signed-in local app that can run the call"},
		{"app:claude-code", "one named app"},
		{"app:codex", "the other one"},

		{"federation:cheapest", "the cheapest qualifying federated record"},
		{"federation:strongest", "the strongest one"},
		{"federation:streamClaudeSonnet", "one named federated provider"},

		{"policy:localFirst", "another policy, expanded at load"},
	} {
		if err := ValidatePolicyEntry(tc.entry); err != nil {
			t.Errorf("ValidatePolicyEntry(%q) refused it (%s): %v", tc.entry, tc.why, err)
		}
	}
}

// TestValidatePolicyEntry_RefusesEverythingElse is the other half: the forms
// that must NOT load.
//
// The failure this prevents is quiet. An unrecognised entry that passed would
// reach the router as a provider name, miss the registry, and be reported as
// "no such provider" -- which reads as a missing model rather than as the
// spelling mistake it is.
func TestValidatePolicyEntry_RefusesEverythingElse(t *testing.T) {
	for _, tc := range []struct {
		entry string
		why   string
	}{
		{"", "empty"},
		{"   ", "whitespace only"},

		{"fleet:", "a scheme with nothing after it names no model"},
		{"app:", "a scheme with nothing after it names no app"},
		{"federation:", "a scheme with nothing after it names no provider"},
		{"policy:", "a scheme with nothing after it names no policy"},

		{"cloud:cheapest", "there is no cloud door; the paid one is federation"},
		{"local:strongest", "the local door is spelled fleet"},

		// A model id may contain a colon, so `fleet:strongest:extra` would read
		// as a model literally named "strongest:extra" and fail at resolution
		// as "no such model". A selector word is reserved, so a colon after one
		// is a malformed selector rather than an id.
		{"fleet:strongest:extra", "a selector takes nothing after it"},
		{"fleet:fastest:7b", "the same, for the other selector"},

		{"9provider", "an identifier does not start with a digit"},
		{"provider name", "an identifier carries no space"},
		{"policy:local first", "a policy name is an identifier"},
		{"federation:local first", "a federated provider name is an identifier"},
		{"fleet:qwen 27b", "a model id carries no whitespace"},
	} {
		if err := ValidatePolicyEntry(tc.entry); err == nil {
			t.Errorf("ValidatePolicyEntry(%q) was accepted (%s); the grammar is closed", tc.entry, tc.why)
		}
	}
}

// TestValidatePolicyEntry_FleetStarIsRetiredAndSaysWhatToWrite pins the one
// refusal whose MESSAGE is load-bearing.
//
// An author who wrote fleet:* meant "the best local model". The spelling for
// that is fleet:strongest, and a refusal saying only "invalid entry" sends them
// to the engine source to work out what changed. The wildcard was retired
// because it could not say WHICH of a person's machines it meant -- the choice
// fell to whichever registered first.
func TestValidatePolicyEntry_FleetStarIsRetiredAndSaysWhatToWrite(t *testing.T) {
	err := ValidatePolicyEntry("fleet:*")
	if err == nil {
		t.Fatal("ValidatePolicyEntry(\"fleet:*\") was accepted; it is retired")
	}
	if !strings.Contains(err.Error(), "fleet:strongest") {
		t.Fatalf("the refusal does not name the replacement: %v", err)
	}
}

// TestValidatePolicyEntry_UnknownSchemeNamesTheAcceptedForms checks the other
// message a person reads. An error that says an entry is wrong without saying
// what right looks like costs a trip to the source every time.
func TestValidatePolicyEntry_UnknownSchemeNamesTheAcceptedForms(t *testing.T) {
	err := ValidatePolicyEntry("cloud:cheapest")
	if err == nil {
		t.Fatal("ValidatePolicyEntry(\"cloud:cheapest\") was accepted")
	}
	for _, want := range []string{"fleet:strongest", "app:*", "federation:cheapest", "policy:<name>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// TestIsSelectorEntry_DecomposesDoorsAndOnlyDoors pins the split between the
// two helper predicates.
//
// A `policy:` entry is deliberately NOT a selector: it is expanded away at
// load and the router never walks one. A caller that treated it as a door
// would try to resolve a provider literally named after the policy.
func TestIsSelectorEntry_DecomposesDoorsAndOnlyDoors(t *testing.T) {
	for _, tc := range []struct {
		entry        string
		wantScheme   string
		wantSelector string
	}{
		{"fleet:strongest", "fleet", "strongest"},
		{"fleet:qwen3.8:27b", "fleet", "qwen3.8:27b"},
		{"app:*", "app", "*"},
		{"federation:cheapest", "federation", "cheapest"},
	} {
		scheme, selector, ok := IsSelectorEntry(tc.entry)
		if !ok {
			t.Errorf("IsSelectorEntry(%q) reported not-a-selector", tc.entry)
			continue
		}
		if scheme != tc.wantScheme || selector != tc.wantSelector {
			t.Errorf("IsSelectorEntry(%q) = (%q, %q), want (%q, %q)",
				tc.entry, scheme, selector, tc.wantScheme, tc.wantSelector)
		}
	}

	for _, entry := range []string{"streamClaudeSonnet", "policy:localFirst", ""} {
		if _, _, ok := IsSelectorEntry(entry); ok {
			t.Errorf("IsSelectorEntry(%q) reported a selector; it is not a door", entry)
		}
	}
}

// TestIsPolicyEntry_NamesTheReferencedPolicy pins the composition half.
func TestIsPolicyEntry_NamesTheReferencedPolicy(t *testing.T) {
	name, ok := IsPolicyEntry("policy:localFirst")
	if !ok || name != "localFirst" {
		t.Fatalf("IsPolicyEntry(\"policy:localFirst\") = (%q, %v), want (localFirst, true)", name, ok)
	}

	for _, entry := range []string{"fleet:strongest", "app:*", "streamClaudeSonnet", "policy:", ""} {
		if _, ok := IsPolicyEntry(entry); ok {
			t.Errorf("IsPolicyEntry(%q) reported a policy reference", entry)
		}
	}
}

// TestValidatePolicyEntry_EmbedderSchemeIsClosedAtOne covers the scheme epic
// memql#5137 fills. The grammar half ships here so that epic never has to
// touch this parser; the RESOLVER is its own, and until it exists the router
// refuses the entry by name.
func TestValidatePolicyEntry_EmbedderSchemeIsClosedAtOne(t *testing.T) {
	if err := ValidatePolicyEntry("embedder:active"); err != nil {
		t.Fatalf("ValidatePolicyEntry(%q): %v", "embedder:active", err)
	}
	for _, bad := range []string{"embedder:", "embedder:*", "embedder:strongest", "embedder:qwen3-embedding"} {
		err := ValidatePolicyEntry(bad)
		if err == nil {
			t.Fatalf("ValidatePolicyEntry(%q) was accepted; the embedder scheme takes one selector", bad)
		}
		// Naming a concrete embedder already has two spellings, and the
		// message must send an author to them rather than leaving them to
		// invent a third.
		if !strings.Contains(err.Error(), "fleet:<modelId>") {
			t.Fatalf("the refusal %q does not say how to name a concrete embedder", err)
		}
	}
}

// TestEntryFormsMessageListsEveryScheme keeps the unknown-scheme message
// honest. A scheme added to the closed set and left out of the message is one
// an author is told does not exist while the parser accepts it.
func TestEntryFormsMessageListsEveryScheme(t *testing.T) {
	joined := strings.Join(entryFormsForMessage(), " ")
	for _, scheme := range entrySchemes {
		if !strings.Contains(joined, scheme+":") {
			t.Fatalf("the entry-forms message %q omits the %q scheme", joined, scheme)
		}
	}
}

// TestAppEntryTakesAModelPin covers the one form the grammar gains in epic
// memql#5391 (design D8): `app:<id>:<model>` pins an app model the way
// `fleet:<modelId>` pins a fleet model.
func TestAppEntryTakesAModelPin(t *testing.T) {
	for _, entry := range []string{
		"app:*",
		"app:claude-code",
		"app:codex",
		"app:claude-code:claude-sonnet-4-6",
		"app:codex:gpt-5.4",
	} {
		if err := ValidatePolicyEntry(entry); err != nil {
			t.Fatalf("ValidatePolicyEntry(%q) = %v, want nil", entry, err)
		}
	}
}

// An app id outside the closed runnable set is refused AT LOAD, and the message
// names the set. The engine has no protocol for another app, so an entry naming
// one is a chain step that could only ever be passed over -- which reads,
// months later, as a door that is shut rather than as a policy that is wrong.
func TestAppEntryRefusesAnAppOutsideTheClosedSet(t *testing.T) {
	for _, entry := range []string{"app:gemini-cli", "app:codexx", "app:gemini-cli:gemini-3"} {
		err := ValidatePolicyEntry(entry)
		if err == nil {
			t.Fatalf("ValidatePolicyEntry(%q) = nil, want a refusal", entry)
		}
		for _, known := range airoute.RunnableApps() {
			if !strings.Contains(err.Error(), known) {
				t.Fatalf("ValidatePolicyEntry(%q) = %q, which does not name %q -- the message must name the set", entry, err, known)
			}
		}
	}
}

// The WILDCARD CANNOT PIN A MODEL. `app:*` asks for any signed-in app, and a
// model name belongs to one app -- `app:*:claude-sonnet-4-6` would ask Codex
// for a Claude model. Refused rather than silently ignored on the apps it
// cannot apply to.
func TestAppWildcardTakesNoModelPin(t *testing.T) {
	err := ValidatePolicyEntry("app:*:claude-sonnet-4-6")
	if err == nil {
		t.Fatalf(`ValidatePolicyEntry("app:*:claude-sonnet-4-6") = nil, want a refusal`)
	}
	if !strings.Contains(err.Error(), "app:<id>:<model>") {
		t.Fatalf("the refusal must name the form that does pin a model, got %q", err)
	}
}

// A model pin with nothing after the second colon is a typo, not "no pin".
func TestAppEntryRefusesAnEmptyModelPin(t *testing.T) {
	for _, entry := range []string{"app:claude-code:", "app:claude-code: ", "app:claude-code:a b"} {
		if err := ValidatePolicyEntry(entry); err == nil {
			t.Fatalf("ValidatePolicyEntry(%q) = nil, want a refusal", entry)
		}
	}
}

// Nothing may follow the model. A third colon is a malformed entry rather than
// a model id containing one: an app's model names are flags on a command line,
// not Ollama tags.
func TestAppEntryRefusesATrailingSegment(t *testing.T) {
	if err := ValidatePolicyEntry("app:claude-code:sonnet:extra"); err == nil {
		t.Fatalf(`ValidatePolicyEntry("app:claude-code:sonnet:extra") = nil, want a refusal`)
	}
}
