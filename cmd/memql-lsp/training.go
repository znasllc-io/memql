package main

// training.go serves `memql/trainingState`: for each construct in an
// open document, what state it is in against the connected cluster
// (memql#3759, construct-training design §3 and §4).
//
//	untrained  the cluster has no record of it
//	drifted    the cluster has it, promoted, and its source no longer matches
//	trained    the cluster has it, promoted, and its source matches
//	seeded     the cluster has it from disk -- never promoted, and not
//	           promotable without a rollout
//	unknown    nothing can be said
//
// It is the sibling of runnable.go and is built the same way, for the same
// stated reason: THE LANGUAGE SERVER OWNS ALL PARSING. The extension renders
// state; it does not compute it. The document half of the comparison is
// sense.ConstructHashes -- one span scan, one hash, shared with the engine and
// pinned to it by the corpus parity gate memql#3758 landed -- and this file adds
// no second notion of where a construct begins or what its source hashes to.
//
// # Where the cluster's half comes from
//
// The catalog arrives IN THE REQUEST. This server has no cluster connection and
// should not grow one: it is an offline analysis process built over a workspace
// directory, while the extension already holds the authenticated stream that
// serves `ListConstructs` (memql#3749). So the client says what the cluster
// knows and the server says what that means, which keeps the one thing that must
// not be duplicated -- the reading of a .memql buffer -- on the side that has the
// parser. It is the same division `memql/imports` settled on when it let the
// client supply the bytes: the client says which inputs to look at, the server
// owns what they mean.
//
// It is sent WITH EVERY REQUEST rather than pushed once and cached here, and the
// cost of that is the reason catalogConstruct is four fields instead of
// ConstructInfo's eleven: the whole catalog is roughly a thousand entries, which
// is a manageable payload at four small strings each and a wholly unmanageable
// one once `args` and `source` are along for the ride. The alternative -- a
// notification that sets a cached catalog on the server -- buys that payload
// back and pays for it in exactly the wrong currency. A cache has to distinguish
// "never pushed" from "pushed empty" from "the cluster went away since", every
// one of those transitions is a chance to get the rule below wrong, and none of
// them is visible in the request that gets the answer wrong. Sending it makes
// the state a pure function of the call.
//
// # The rule this file is mostly about
//
// NOT CONNECTED MEANS `unknown`, NEVER `untrained`. An absent cluster must not
// read as "the cluster does not have this": every disconnection would become a
// screen of false alarms, each offering a Promote against nothing.
//
// That is not enforced here by remembering to check a flag. It is structural, in
// three places that each fail safe on their own:
//
//   - the request carries `catalog` as a POINTER, so an absent catalog and an
//     empty one are different JSON values rather than the same nil slice;
//   - clusterCatalog's ZERO VALUE is disconnected, so a catalog nobody
//     constructed answers `unknown` rather than "absent from an empty map";
//   - catalogAnswer's zero value is catalogSilent, so the three-valued lookup
//     degrades to "no answer" rather than to "not there".
//
// A nil map and an empty map are indistinguishable at the point of lookup, which
// is exactly why the distinction cannot live in the map.
//
// The same trap has a second, quieter form: a construct kind the catalog
// STRUCTURALLY does not carry. `action` and `capability` are authored kinds
// loaded by component/actions, not by any registry the engine holds, so
// ListConstructs omits them by design -- and their absence is evidence of
// nothing. They report `unknown` too. memql.ConstructKindForKeyword is what says
// so, and it says so in the engine, next to the kind vocabulary itself.
//
// The wire shape below is a FIXED contract shared with the TypeScript consumer.
// Field names, the five-value `state` set, and the "empty array, never null"
// rule for `constructs` are load-bearing; changing any of them means changing
// both sides in the same commit.

import (
	"path"

	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/cmd/memql-lsp/internal/position"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// methodTrainingState is the custom request's method name, and
// capabilityTrainingState is the key the server advertises under
// `capabilities.experimental` so a client can feature-detect the request instead
// of calling it blind and handling MethodNotFound.
//
// Both names, and the five `state` values below, are the extension's -- posted
// on memql#3761 before this side existed, precisely so the two could be checked
// against each other while a rename was still one constant. That side has since
// SHIPPED the rendering against them, so these are no longer this file's to
// choose: `state` is a closed vocabulary crossing the wire as a bare string, an
// unrecognised value degrades to `unknown` there, and `unknown` renders as
// nothing at all -- so a divergence in any of them is invisible on both sides
// and looks exactly like the feature working.
const methodTrainingState = "memql/trainingState"
const capabilityTrainingState = "memqlTrainingState"

// The five states. Fixed vocabulary, defined in the design's §2 and named here
// exactly as the wire carries them.
const (
	// trainingStateUnknown is the state of a construct nothing can be said
	// about: no cluster is connected, or its kind is one the catalog does not
	// carry. It is FIRST in this list because it is the safe answer -- every
	// other state is a claim about a cluster, and this is the absence of one.
	trainingStateUnknown = "unknown"
	// trainingStateUntrained: the connected cluster has no record of it.
	trainingStateUntrained = "untrained"
	// trainingStateDrifted: promoted into the cluster, and the local source no
	// longer matches what was promoted.
	trainingStateDrifted = "drifted"
	// trainingStateTrained: promoted, persisted, live, and matching.
	trainingStateTrained = "trained"
	// trainingStateSeeded: loaded from disk at boot. Known to the cluster and
	// never promoted; changing what the cluster runs needs a rollout, not a
	// promote.
	trainingStateSeeded = "seeded"
)

type trainingStateParams struct {
	TextDocument protocol.TextDocumentIdentifier `json:"textDocument"`
	// Catalog is what the connected cluster has loaded -- the `constructs` list
	// from ListConstructs, projected to the fields the state rules read.
	//
	// A POINTER, and this is the single most important declaration in the file.
	// `null` (or absent) is NO CONNECTED CLUSTER; `[]` is a connected cluster
	// that has loaded nothing. Those two are worlds apart -- the first means
	// every state is unknown, the second means every state is untrained -- and a
	// plain `[]catalogConstruct` would decode both to the same nil slice, at
	// which point no code downstream could tell them apart no matter how
	// carefully it was written.
	//
	// `memql/imports` takes the same shape for the same reason on a smaller
	// stake: `Text *string`, so an absent buffer and an empty one stay
	// distinguishable.
	Catalog *[]catalogConstruct `json:"catalog"`
}

// catalogConstruct is one entry of the cluster's construct catalog, projected to
// what the state rules actually read.
//
// Deliberately a SUBSET of ListConstructs' ConstructInfo rather than a mirror of
// it. Every field a client copies across is a field the two sides can disagree
// about, and the ones omitted here -- description, args, runnable, origin_path,
// source -- have no bearing on training state. `namespace` is omitted too, and
// that one is worth naming: a concept's domain is already inside its canonical
// `name`, and memql.CatalogConstructKey is what reads it out, so accepting a
// separate namespace would create a second, contradictable answer to "which
// domain is this concept in".
type catalogConstruct struct {
	// Name is the registry key: a concept's canonical id
	// ("v1:cognition:space"), or the declared name for every other kind.
	Name string `json:"name"`
	// Kind is the catalog's kind, not the authored keyword -- "mutation" for
	// what a file spells `mutate`. Passed through untranslated; the translation
	// happens once, here, via memql.ConstructKindForKeyword.
	Kind string `json:"kind"`
	// Origin is "core" | "bundle" | "promoted", derived server-side by the
	// engine in one place and passed through unexamined. Nothing re-derives it,
	// because a second derivation is a second definition.
	Origin string `json:"origin"`
	// SourceHash is the canonical content hash of the construct's authored
	// source. EMPTY means "not available", never "hashes to nothing" -- see
	// hashesAgree.
	SourceHash string `json:"sourceHash"`
}

type trainingStateResult struct {
	// Connected reports whether a catalog was supplied at all. It carries no
	// information the per-construct states do not, and exists so the client can
	// render "no cluster" once instead of inferring it from a screenful of
	// `unknown` -- and so the disconnected case stays legible on the wire, not
	// only in this file.
	Connected bool `json:"connected"`
	// Constructs is every top-level construct the document declares, in
	// declaration order. Always non-nil.
	Constructs []trainingConstruct `json:"constructs"`
}

type trainingConstruct struct {
	// Kind is the AUTHORED keyword, as written in the file (`mutate`), matching
	// what `memql/runnableConstructs` reports for the same construct. The
	// catalog's kind is an engine-side key and does not belong in a result about
	// a document.
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Concept is the signature-bound concept short name for a two-identifier
	// signature (`query <Concept> <name>`), empty otherwise.
	Concept string `json:"concept,omitempty"`
	// SignatureRange is the same range `memql/runnableConstructs` carries, so a
	// training decoration and a Run lens anchor to the same place.
	SignatureRange protocol.Range `json:"signatureRange"`
	// State is one of the five constants above.
	State string `json:"state"`
	// Origin is the matched catalog entry's origin, passed straight through.
	// Absent unless a catalog entry was matched, which is what makes it usable:
	// it distinguishes a `seeded` construct sealed in the engine image (core)
	// from one delivered by a product bundle, and those two want different words
	// in the tooltip even though neither has an action.
	Origin string `json:"origin,omitempty"`
}

// trainingState answers the request for one document.
//
// It never fails, for the reasons runnable.go states: the editor issues this on
// every keystroke and every CodeLens refresh, so an unknown URI, an unopened
// document and a half-typed buffer are ordinary conditions rather than errors,
// and an error would surface as a popup on a document somebody is still typing.
//
// Note it does NOT consult the Sense service. Unlike the runnable projection,
// nothing here needs the workspace registry -- a construct's span and hash are
// purely lexical. So training state keeps working on a workspace whose offline
// engine build failed, which is precisely the workspace an author is most likely
// to be looking at while iterating.
func (s *server) trainingState(params trainingStateParams) trainingStateResult {
	catalog := newClusterCatalog(params.Catalog)

	// Allocated up front so the field marshals as [] and never as null: the
	// TypeScript consumer iterates it directly.
	out := trainingStateResult{
		Connected:  catalog.connected,
		Constructs: []trainingConstruct{},
	}

	text, ok := s.docs.get(params.TextDocument.URI)
	if !ok {
		return out
	}
	domain := documentDomain(params.TextDocument.URI)

	for _, c := range sense.ConstructHashes(text) {
		entry, answer := catalog.find(c, domain)
		out.Constructs = append(out.Constructs, trainingConstruct{
			Kind:    c.Kind,
			Name:    c.Name,
			Concept: c.Concept,
			SignatureRange: protocol.Range{
				Start: position.ToLSPPosition(text, c.SignatureRange.Start.Line, c.SignatureRange.Start.Column),
				End:   position.ToLSPPosition(text, c.SignatureRange.End.Line, c.SignatureRange.End.Column),
			},
			State:  trainingStateFor(c, entry, answer),
			Origin: entry.Origin,
		})
	}
	return out
}

// --- the model ----------------------------------------------------------

// catalogAnswer is the THREE-valued result of asking the catalog about one
// construct. Three rather than two because two is exactly one too few: collapsing
// "there is no catalog to ask" into "the catalog does not have it" is the bug the
// whole issue is about, and a bool cannot hold the difference.
type catalogAnswer int

const (
	// catalogSilent -- no answer exists. THE ZERO VALUE, deliberately: the
	// state machine's safe outcome is `unknown`, so a caller that forgets to set
	// this lands on the harmless answer instead of asserting something false
	// about a cluster.
	catalogSilent catalogAnswer = iota
	// catalogAbsent -- the catalog could answer and does not have it.
	catalogAbsent
	// catalogPresent -- the catalog has it.
	catalogPresent
)

// String names the value. Worth the four lines: the failure this whole file
// guards against is silence being read as absence, and a test that reports it as
// "answered 1, want 0" makes the reader decode the very distinction the message
// is about.
func (a catalogAnswer) String() string {
	switch a {
	case catalogAbsent:
		return "catalogAbsent"
	case catalogPresent:
		return "catalogPresent"
	default:
		return "catalogSilent"
	}
}

// clusterCatalog is what the connected cluster knows, OR the fact that there is
// no connected cluster. Its ZERO VALUE IS THE SECOND: an unset clusterCatalog is
// a disconnected one, and answers `unknown` for everything.
//
// The `connected` field is not redundant with a nil map, and that is the entire
// point of the type. Go cannot distinguish a nil map from an empty one at a
// lookup -- both miss, silently -- so a model that carried only the map would
// have made "no cluster" and "a cluster with nothing loaded" the same value, and
// no amount of care at the call sites could have recovered the difference.
type clusterCatalog struct {
	connected bool
	entries   map[string]catalogConstruct
}

// newClusterCatalog converts the request's catalog into the model. A nil pointer
// -- an absent or `null` field -- yields the disconnected zero value; anything
// else, INCLUDING an empty list, yields a connected catalog.
func newClusterCatalog(supplied *[]catalogConstruct) clusterCatalog {
	if supplied == nil {
		return clusterCatalog{}
	}
	entries := make(map[string]catalogConstruct, len(*supplied))
	for _, e := range *supplied {
		// First-wins, mirroring buildConstructSourceIndex: a duplicate key is
		// the cluster's problem, not this map's, and picking the second would
		// differ from what the engine's own index does with the same input.
		key := memql.CatalogConstructKey(e.Kind, e.Name)
		if _, exists := entries[key]; exists {
			continue
		}
		entries[key] = e
	}
	return clusterCatalog{connected: true, entries: entries}
}

// find asks the catalog about one construct the document declares.
//
// The keyword-to-kind translation happens here and nowhere else, and its FALSE
// return is not a failure: `action` and `capability` are authored kinds the
// catalog structurally does not carry, so a miss on one of them is silence, not
// absence. Reporting them as untrained would be the disconnected bug in
// miniature -- a claim about the cluster derived from a question the cluster was
// never asked.
func (c clusterCatalog) find(declared sense.ConstructHash, domain string) (catalogConstruct, catalogAnswer) {
	if !c.connected {
		return catalogConstruct{}, catalogSilent
	}
	kind, cataloged := memql.ConstructKindForKeyword(declared.Kind)
	if !cataloged {
		return catalogConstruct{}, catalogSilent
	}
	entry, found := c.entries[memql.DocumentConstructKey(kind, declared.Name, domain)]
	if !found {
		return catalogConstruct{}, catalogAbsent
	}
	return entry, catalogPresent
}

// trainingStateFor is the state machine, in the order the rules resolve.
//
// ORIGIN DECIDES THE TIER BEFORE THE HASH DECIDES ANYTHING, and that ordering is
// the one judgement in this file worth arguing about. A `seeded` construct --
// core or bundle -- stays seeded even when its local source differs from what
// the cluster loaded. Three reasons, and they agree:
//
//   - Drift is DEFINED against a promotion (design §2: "a construct the cluster
//     knows, whose local source no longer matches what was promoted"). A seeded
//     construct has no promoted version, so there is nothing for it to have
//     drifted from.
//   - The states exist to pick an ACTION SET (design §4), and `seeded` has none
//     -- "no action; needs a rollout". Rendering an edited core construct as
//     `drifted` would put a Promote lens on it, and the engine refuses to let a
//     promoted construct shadow a core name, so the affordance could only ever
//     fail.
//   - Nothing is lost by it. `seeded` already means "not live without a
//     rollout", which is exactly what an edited seeded construct needs to be
//     told.
func trainingStateFor(declared sense.ConstructHash, entry catalogConstruct, answer catalogAnswer) string {
	switch answer {
	case catalogSilent:
		return trainingStateUnknown
	case catalogAbsent:
		return trainingStateUntrained
	}
	if entry.Origin != memql.ConstructOriginPromoted {
		// core, bundle -- and anything a client sends that is neither, which
		// degrades here on purpose: an unrecognised origin is a client bug, and
		// `seeded` is the one state that offers no action, so a wrong guess
		// costs a missing affordance rather than a refused one.
		return trainingStateSeeded
	}
	if !hashesAgree(entry.SourceHash, declared.SourceHash) {
		return trainingStateDrifted
	}
	return trainingStateTrained
}

// hashesAgree reports whether two construct source hashes are the same KNOWN
// hash.
//
// The emptiness check is not defensive noise. sense.ConstructSourceHash returns
// the empty string for source it does not have, deliberately, "so that a missing
// answer must not compare equal to another missing answer as though both were
// known" -- and both sides can produce one: the catalog stamps an empty hash for
// a construct with no readable source, and a promotion marker that is a name
// RESERVATION rather than a registration carries no source at all. A plain `==`
// would call that pair `trained`, which is the most confident possible way to be
// wrong about a construct nobody can see.
func hashesAgree(a, b string) bool {
	return a != "" && a == b
}

// documentDomain is the DSL domain a document's constructs are declared in: the
// name of the directory containing the file.
//
// That is the tree's layout in every form it takes -- `dsl/<domain>/*.memql`
// embedded, `$MEMQL_DSL_PATH/<domain>/*.memql` for a mounted bundle -- and it is
// the same derivation the engine's own source index makes (treeDomainOf over a
// tree-relative path). It is needed for exactly one kind: a concept's registry
// key is its canonical id, and two domains may each declare a `state`.
//
// A file outside that layout -- a scratch buffer somewhere in the workspace,
// which design §5 calls the training path -- yields a domain that matches no
// concept in the catalog, so a concept declared there reads as untrained. That
// is the correct answer, not a degradation.
//
// `path` rather than `filepath`: uriToPath always returns a slash-separated
// path, including for a Windows drive path (C:/w/x.memql), and running it
// through the OS-specific splitter would only introduce a way for the two to
// disagree.
func documentDomain(uri string) string {
	return path.Base(path.Dir(uriToPath(uri)))
}
