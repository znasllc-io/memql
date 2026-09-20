package parser

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/core/airoute"
)

// policy_entry.go holds the CLOSED grammar for one entry of a policy chain --
// the strings that appear in `@primary(...)`, `@fallback(...)` and a rule's
// `@exclude(...)`.
//
// It lives in the parser rather than beside the registries because both the
// policy loader and the rule loader hold their entries to it, and a second copy
// of a grammar is a copy that drifts. The whole point of the grammar is that a
// call site never names a model: an entry says which DOOR to try and how to
// pick behind it, so an entry the grammar does not recognise is a routing
// instruction the router will silently do nothing with.
//
// The accepted forms, and nothing else:
//
//	<providerName>              a registry entry, by name
//	fleet:strongest             the strongest model on the person's own machines
//	fleet:fastest               the fastest one
//	fleet:<modelId>             one named local model
//	app:*                       any signed-in local app that can run the call
//	app:<id>                    one named local app
//	app:<id>:<model>            one named local app, with its model pinned
//	federation:cheapest         the cheapest qualifying federated record
//	federation:strongest        the strongest one
//	federation:<providerName>   one named federated provider
//	policy:<name>               another policy, expanded at load

// Entry schemes. The set is closed: a scheme outside it is refused rather than
// passed through, because an unrecognised scheme reads as a provider name that
// happens to contain a colon and fails much later, at resolution, as "no such
// provider".
const (
	// EntrySchemeFleet reaches the person's own machines.
	EntrySchemeFleet = "fleet"
	// EntrySchemeApp reaches a signed-in local app (Claude Code, Codex).
	EntrySchemeApp = "app"
	// EntrySchemeFederation reaches a paid vendor through workload identity
	// federation.
	EntrySchemeFederation = "federation"
	// EntrySchemeEmbedder names the cluster's ACTIVE EMBEDDER BINDING rather
	// than a door (epic memql#5137). It is its own scheme because the binding
	// is not a door: whichever model is bound may live on the fleet or at a
	// vendor, and the DOOR is derived from what the binding resolves to. A
	// spelling under `fleet:` would have been wrong for every federated
	// embedder, and one under `federation:` wrong for every local one.
	EntrySchemeEmbedder = "embedder"
	// EntrySchemePolicy names another policy, expanded at load so the router
	// never walks one.
	EntrySchemePolicy = "policy"
)

// AppWildcardSelector is what follows `app:` to mean ANY signed-in app, in the
// owner's own order. It is spelled once here rather than as a bare literal so
// the two places that special-case it cannot disagree.
const AppWildcardSelector = "*"

// entrySchemes is the closed set, in the order an error message lists them:
// the three doors in cost order, then the composition form.
var entrySchemes = []string{EntrySchemeFleet, EntrySchemeApp, EntrySchemeFederation, EntrySchemeEmbedder, EntrySchemePolicy}

// fleetSelectors and federationSelectors are the reserved words behind their
// scheme. Anything else after the colon is a concrete id -- which is why a
// misspelled selector cannot be caught by "is it in this set": it is
// indistinguishable from a model nobody has installed. What IS catchable, and
// is caught below, is a selector word carrying extra colon-separated junk.
var (
	fleetSelectors      = map[string]bool{"strongest": true, "fastest": true}
	federationSelectors = map[string]bool{"cheapest": true, "strongest": true}
)

// ValidatePolicyEntry reports whether entry is one of the accepted forms.
//
// It is called from the policy loader (over every `@primary` / `@fallback`) and
// from the rule parser (over every `@exclude`), so a bundle mounted at
// MEMQL_DSL_PATH is held to the same grammar as the embedded tree.
func ValidatePolicyEntry(entry string) error {
	trimmed := strings.TrimSpace(entry)
	if trimmed == "" {
		return fmt.Errorf("policy entry is empty: an entry is a provider name, or one of %s",
			strings.Join(entryFormsForMessage(), ", "))
	}

	// fleet:* is REFUSED BY NAME, before the generic path, so its message can
	// carry the replacement. An author who wrote fleet:* meant "the best local
	// model"; the spelling for that is fleet:strongest, and a refusal that says
	// only "invalid" sends them to the source to work out what changed.
	if trimmed == EntrySchemeFleet+":*" {
		return fmt.Errorf("policy entry %q is retired: write fleet:strongest for the best local model, "+
			"or fleet:fastest for the quickest one -- the wildcard could not say which of a person's "+
			"machines it meant, so the choice was made by whichever registered first", trimmed)
	}

	scheme, rest, hasColon := strings.Cut(trimmed, ":")
	if !hasColon {
		// A bare name is a provider registry entry.
		if err := validateEntryIdentifier(trimmed, "provider name"); err != nil {
			return fmt.Errorf("policy entry %q: %w", trimmed, err)
		}
		return nil
	}

	switch scheme {
	case EntrySchemeFleet:
		if rest == "" {
			return fmt.Errorf("policy entry %q names no local model: write fleet:strongest, fleet:fastest, "+
				"or fleet:<modelId>", trimmed)
		}
		// A MODEL ID MAY ITSELF CONTAIN A COLON (`qwen3.8:27b`), which is why
		// the split above takes the FIRST colon only. That makes one mistake
		// invisible: `fleet:strongest:extra` would read as a model literally
		// named "strongest:extra" and fail at resolution as "no such model".
		// A selector word is reserved, so a colon after one is a malformed
		// selector rather than an id.
		if head, tail, more := strings.Cut(rest, ":"); more && fleetSelectors[head] {
			return fmt.Errorf("policy entry %q: %q is a selector, not a model id, so nothing may follow it "+
				"(the trailing %q looks like a typo)", trimmed, head, tail)
		}
		if fleetSelectors[rest] {
			return nil
		}
		return validateEntryId(trimmed, rest, "model id")

	case EntrySchemeApp:
		if rest == "" {
			return fmt.Errorf("policy entry %q names no app: write app:* for any signed-in app, "+
				"app:<id> for one of them, or app:<id>:<model> to pin that app's model", trimmed)
		}
		// THE WILDCARD TAKES NO MODEL. `app:*` asks for any signed-in app, and
		// a model name belongs to ONE app -- `app:*:claude-sonnet-4-6` would
		// ask Codex for a Claude model. Refusing is the only honest answer;
		// ignoring the pin on the apps it cannot apply to would make the
		// decision record say a model was asked for that never was.
		if rest == AppWildcardSelector {
			return nil
		}
		appId, model, pinned := strings.Cut(rest, ":")
		if appId == AppWildcardSelector {
			return fmt.Errorf("policy entry %q: app:* names any signed-in app and cannot pin a model, "+
				"because a model name belongs to one app -- write app:<id>:<model> to pin one", trimmed)
		}
		// THE APP ID IS HELD TO THE CLOSED SET, at LOAD. The engine drives the
		// apps core/airoute names and has no protocol for another one, so an
		// entry naming anything else is a chain step that could only ever be
		// passed over -- which reads, months later, as a door that is shut
		// rather than as a policy that is wrong.
		if !airoute.IsRunnableApp(appId) {
			return fmt.Errorf("policy entry %q: %q is not an app this engine drives -- the runnable set is %s",
				trimmed, appId, strings.Join(airoute.RunnableApps(), ", "))
		}
		if !pinned {
			return nil
		}
		// NOTHING FOLLOWS THE MODEL. An app's model names are flags on a
		// command line rather than Ollama tags, so a third colon is a
		// malformed entry rather than a model id that contains one.
		if head, tail, more := strings.Cut(model, ":"); more {
			return fmt.Errorf("policy entry %q: nothing follows the model in app:<id>:<model> "+
				"(the model reads as %q and the trailing %q looks like a typo)", trimmed, head, tail)
		}
		return validateEntryId(trimmed, model, "app model")

	case EntrySchemeFederation:
		if rest == "" {
			return fmt.Errorf("policy entry %q names no federated provider: write federation:cheapest, "+
				"federation:strongest, or federation:<providerName>", trimmed)
		}
		if federationSelectors[rest] {
			return nil
		}
		if err := validateEntryIdentifier(rest, "provider name"); err != nil {
			return fmt.Errorf("policy entry %q: %w", trimmed, err)
		}
		return nil

	case EntrySchemeEmbedder:
		// A CLOSED SET OF ONE. There is exactly one question worth asking of
		// the binding -- which model is bound right now -- and a second
		// selector here would be a second answer to it. An id-shaped entry is
		// refused rather than passed through: naming a concrete embedder is
		// what `fleet:<modelId>` and `federation:<providerName>` already do,
		// and admitting it here would give one thing two spellings whose
		// decision records read differently.
		if !embedderSelectors[rest] {
			return fmt.Errorf("policy entry %q: the embedder scheme takes %s and nothing else -- "+
				"to name a concrete embedder write fleet:<modelId> or federation:<providerName>",
				trimmed, strings.Join(sortedKeysOf(embedderSelectors), ", "))
		}
		return nil

	case EntrySchemePolicy:
		if rest == "" {
			return fmt.Errorf("policy entry %q names no policy: write policy:<name>", trimmed)
		}
		if err := validateEntryIdentifier(rest, "policy name"); err != nil {
			return fmt.Errorf("policy entry %q: %w", trimmed, err)
		}
		return nil
	}

	return fmt.Errorf("policy entry %q: unknown scheme %q -- an entry is a bare provider name, or one of %s",
		trimmed, scheme, strings.Join(entryFormsForMessage(), ", "))
}

// IsSelectorEntry decomposes a DOOR entry into its scheme and what follows the
// colon, reporting ok only for fleet / app / federation.
//
// A `policy:` entry is deliberately NOT a selector -- it is expanded away at
// load and the router never walks one -- so it answers through IsPolicyEntry
// instead. The two are disjoint on purpose: a caller that treated `policy:x` as
// a door would try to resolve a provider named "x".
//
// It reports the SHAPE and does not validate: run ValidatePolicyEntry for that.
func IsSelectorEntry(entry string) (scheme, selector string, ok bool) {
	scheme, selector, hasColon := strings.Cut(strings.TrimSpace(entry), ":")
	if !hasColon {
		return "", "", false
	}
	switch scheme {
	case EntrySchemeFleet, EntrySchemeApp, EntrySchemeFederation:
		return scheme, selector, true
	}
	return "", "", false
}

// IsPolicyEntry reports whether entry names another policy, and which.
func IsPolicyEntry(entry string) (name string, ok bool) {
	scheme, rest, hasColon := strings.Cut(strings.TrimSpace(entry), ":")
	if !hasColon || scheme != EntrySchemePolicy || rest == "" {
		return "", false
	}
	return rest, true
}

// entryFormsForMessage is the accepted forms as an error message lists them.
// Built from the scheme constants so a scheme cannot be added without the
// message learning it.
func entryFormsForMessage() []string {
	out := make([]string, 0, len(entrySchemes))
	for _, scheme := range entrySchemes {
		switch scheme {
		case EntrySchemeFleet:
			out = append(out, "fleet:strongest, fleet:fastest, fleet:<modelId>")
		case EntrySchemeApp:
			out = append(out, "app:*, app:<id>, app:<id>:<model>")
		case EntrySchemeFederation:
			out = append(out, "federation:cheapest, federation:strongest, federation:<providerName>")
		case EntrySchemeEmbedder:
			out = append(out, "embedder:active")
		case EntrySchemePolicy:
			out = append(out, "policy:<name>")
		}
	}
	return out
}

// validateEntryIdentifier holds a NAME to the DSL's identifier shape: it starts
// with a letter and continues in letters and digits.
//
// Provider names, policy names and federated provider names are all registry
// keys authored in this DSL, so they are held to the identifier shape. Model
// ids and app ids are NOT -- those are somebody else's names and carry dots,
// dashes and colons -- which is what validateEntryId is for.
func validateEntryIdentifier(name, what string) error {
	if name == "" {
		return fmt.Errorf("%s is empty", what)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return fmt.Errorf("%s %q starts with a digit -- a %s is an identifier", what, name, what)
			}
		default:
			return fmt.Errorf("%s %q contains %q -- a %s is letters and digits, starting with a letter", what, name, string(r), what)
		}
	}
	return nil
}

// validateEntryId holds an EXTERNAL id -- a model id, an app id -- to the one
// thing that is genuinely an error rather than a naming convention somebody
// else chose: it must be non-empty and carry no whitespace.
//
// Deliberately permissive. `qwen3.8:27b` and `claude-code` are both real, and a
// tighter rule here would refuse a model that exists on the operator's machine
// in the name of a house style that is not ours to impose.
func validateEntryId(entry, id, what string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("policy entry %q names an empty %s", entry, what)
	}
	if strings.ContainsAny(id, " \t\n\r") {
		return fmt.Errorf("policy entry %q: the %s %q contains whitespace", entry, what, id)
	}
	return nil
}

// embedderSelectors is the closed set the `embedder` scheme accepts.
//
// `active` is the only member and is likely to stay the only one: the binding
// answers one question. It resolves through a seam epic memql#5137 installs;
// until then the router refuses it by name rather than treating it as a door
// that happens to be shut, because those need different fixes.
var embedderSelectors = map[string]bool{"active": true}

// sortedKeysOf renders a selector set in a stable order for an error message,
// so two authors reading the same refusal see the same sentence.
func sortedKeysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
