package memql

// construct_catalog.go answers "what has this cluster actually loaded" at
// REGISTRY grain (memql#3749, constructs-view design §4).
//
// It is deliberately not a file walk. The pack browser
// (ListPackDomains / ListPackFiles / ReadPackFile) is the file-grain read and
// stays exactly as it is; it answers "show me this file". This answers "what do
// you have", and the difference is load-bearing in both directions:
//
//   - a PROMOTED construct lives in v1:authoring:construct and in no file at
//     all, so a file walk is structurally blind to it;
//   - a construct the loader SKIPPED -- @disabled, or one that failed to parse
//     -- is in a file and not in the engine, so a file walk would report a
//     construct the cluster cannot run.
//
// So the registries decide WHICH constructs exist. The tree is consulted only
// to answer two questions about a construct already known to be loaded: which
// file it came from, and what its authored source hashes to.
//
// # The three fields worth reading the code for
//
// `origin` is DERIVED HERE AND NOWHERE ELSE. core when the construct's file
// resolves inside the embedded tree, bundle when it resolves under a
// runtime-mounted product domain (what MEMQL_DSL_PATH registers), promoted when
// the engine holds an authored-promotion marker for it. No client re-derives
// it, because a second derivation is a second definition.
//
// `runnable` is NOT a new judgement. It is the five kinds the runtime-panel
// design already fixed -- query, mutation, logic, tool, automation -- with the
// reason recorded there: spec / trait / prompt / seed / concept / shape /
// provider / builtin each need an execution semantic decided (which row does a
// spec evaluate against; who pays for a prompt's provider call) that the design
// defers. A client reads this field rather than re-deriving from `kind`, which
// also spares it the keyword-vs-kind mismatch: the authored keyword is `mutate`
// and the kind reported here is `mutation`.
//
// `args` comes from Sense, over the construct's authored source, via the same
// AnalyzeRunnable the language server serves `memql/runnableConstructs` from.
// Not from Function.ArgsSchema. That is the whole of §2.3 of the design: ONE
// parser reports the argument form, so a generated argument form cannot
// silently disagree with the compiler about what a construct accepts. Deriving
// it a second way here would reintroduce exactly the divergence the rule exists
// to prevent -- and the shape is literally sense.RunnableArg, so the two cannot
// drift structurally either.

import (
	"sort"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/component/memql/sense"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// Construct kinds reported by the catalog. The set is closed and matches the
// constructs-view design's table, plus `seed`: seeds are a live engine registry
// (e.seeds) and omitting them would break the one property the catalog
// promises, that its counts match what the engine loaded.
//
// `action` and `capability` are authored kinds too, and are deliberately ABSENT
// -- they are loaded by component/actions, not by any registry the engine
// holds, so this catalog has nothing to report about them rather than an empty
// answer to give.
const (
	ConstructKindConcept    = "concept"
	ConstructKindQuery      = "query"
	ConstructKindMutation   = "mutation"
	ConstructKindLogic      = "logic"
	ConstructKindTool       = "tool"
	ConstructKindAutomation = "automation"
	ConstructKindSpec       = "spec"
	ConstructKindTrait      = "trait"
	ConstructKindShape      = "shape"
	ConstructKindPrompt     = "prompt"
	ConstructKindProvider   = "provider"
	ConstructKindBuiltin    = "builtin"
	ConstructKindPolicy     = "policy"
	ConstructKindSeed       = "seed"
)

// Construct origins. Exactly three, derived by deriveConstructOrigin.
const (
	ConstructOriginCore     = "core"
	ConstructOriginBundle   = "bundle"
	ConstructOriginPromoted = "promoted"
)

// runnableConstructKinds is the five-kind runnable set, keyed by the kind this
// catalog reports (so `mutation`, not the authored keyword `mutate`).
var runnableConstructKinds = map[string]bool{
	ConstructKindQuery:      true,
	ConstructKindMutation:   true,
	ConstructKindLogic:      true,
	ConstructKindTool:       true,
	ConstructKindAutomation: true,
}

// constructKeyword maps a reported kind to the keyword it is AUTHORED under, so
// the source index slices on the right token. Only `mutation` differs.
var constructKeyword = map[string]string{
	ConstructKindConcept:    "concept",
	ConstructKindQuery:      "query",
	ConstructKindMutation:   "mutate",
	ConstructKindLogic:      "logic",
	ConstructKindTool:       "tool",
	ConstructKindAutomation: "automation",
	ConstructKindSpec:       "spec",
	ConstructKindTrait:      "trait",
	ConstructKindShape:      "shape",
	ConstructKindPrompt:     "prompt",
	ConstructKindProvider:   "provider",
	ConstructKindBuiltin:    "builtin",
	ConstructKindPolicy:     "policy",
	ConstructKindSeed:       "seed",
}

// kindForConstructKeyword inverts constructKeyword once, at init. The map is
// injective -- `mutation`/`mutate` is the only pair whose two halves differ at
// all -- so the inverse is total and unambiguous.
var kindForConstructKeyword = func() map[string]string {
	out := make(map[string]string, len(constructKeyword))
	for kind, keyword := range constructKeyword {
		out[keyword] = kind
	}
	return out
}()

// ConstructKindForKeyword maps an AUTHORED construct keyword to the kind this
// catalog reports it under, and reports whether the catalog carries that kind at
// all.
//
// Exported for the language server, which holds the other half of drift
// detection (memql#3759): the document side of that comparison sees the keyword
// an author wrote (`mutate`) while the catalog side sees the kind the registries
// key on (`mutation`), so one of the two has to be translated before anything
// can be matched at all. Reading the same map both sides are built from is the
// only way that translation cannot rot -- a kind added to constructKeyword
// extends the language server for free, where a hand-written "map the one name
// that differs" in the client would silently mis-key the next one.
//
// THE FALSE RETURN IS LOAD-BEARING, and it is not an error case. `action` and
// `capability` are authored kinds this catalog deliberately does not carry (see
// this file's header): they are loaded by component/actions, not by any registry
// the engine holds, so the catalog has nothing to say about them rather than an
// empty answer to give. A caller must render that as "the catalog cannot speak
// to this", never as "the cluster does not have this" -- the same distinction
// the disconnected case turns on, at a smaller scale and with the identical
// failure if the two are collapsed.
func ConstructKindForKeyword(keyword string) (string, bool) {
	kind, ok := kindForConstructKeyword[keyword]
	return kind, ok
}

// CatalogConstructKey and DocumentConstructKey are the ONE identity under which
// a construct known to a REGISTRY and a construct declared in a FILE are matched
// to each other. Two functions rather than one because the two sides hold
// different halves of the same key and neither can be asked for the other's:
//
//   - a registry entry names a concept by its canonical id (v1:cognition:space),
//     which carries the domain inside it;
//   - a file declares the same concept as a bare short name (`space`), with the
//     domain supplied by the directory the file sits in.
//
// Concepts are the only kind that needs the domain, and they need it because a
// concept's registry key is that canonical id and two domains may each declare a
// `state`. Every other kind lives in a flat, process-wide registry, so its name
// is already unique and the domain would only add a way to miss a match.
//
// Both are exported for the language server (memql#3759), which computes the
// document half of drift detection and must land on exactly the key the catalog
// half landed on. A second implementation of this rule is the failure mode
// memql#3758's parity gate exists to catch -- a keying disagreement makes every
// construct read as drifted at once, which looks like a broken cluster.
func CatalogConstructKey(kind, name string) string {
	if kind == ConstructKindConcept {
		return DocumentConstructKey(kind, conceptShortName(name), conceptRootDomain(name))
	}
	return DocumentConstructKey(kind, name, "")
}

// DocumentConstructKey is CatalogConstructKey's file-side half: see there.
func DocumentConstructKey(kind, name, domain string) string {
	if kind == ConstructKindConcept {
		return kind + "\x00" + domain + "/" + name
	}
	return kind + "\x00" + name
}

// ConstructCatalogEntry is one construct the engine has loaded.
type ConstructCatalogEntry struct {
	// Name is the construct's registry key. For a concept that is its
	// canonical id (v1:cognition:space); for every other kind it is the
	// declared name.
	Name string
	// Kind is one of the ConstructKind* constants.
	Kind string
	// Namespace is the DSL domain the construct was authored in. Empty for a
	// promoted construct, which has no file and whose target namespace the
	// shared registries do not carry.
	Namespace string
	// Origin is core | bundle | promoted. See deriveConstructOrigin.
	Origin string
	// OriginPath is the construct's file, relative to the DSL tree root
	// ("cognition/queries.memql"). Empty for a promoted construct and for the
	// handful of constructs registered from Go rather than from a file.
	OriginPath string
	// Description is resolved server-side: the construct's own description as
	// the loader recorded it, which is the `///` doc comment for the kinds that
	// take one and @description for those that do.
	Description string
	// Runnable reports membership of the five-kind runnable set.
	Runnable bool
	// Args is the construct's declared input schema, in authored order, in the
	// SAME shape the language server reports. Always non-nil; empty for a
	// construct with no inputs AND for every view-only kind, which has no
	// argument form because it has no run.
	Args []sense.RunnableArg
	// BoundConcept is the signature binding for the kinds that have one --
	// query / mutation / shape / spec / seed. Empty otherwise.
	BoundConcept string
	// SourceHash is sense.ConstructSourceHash over the construct's authored
	// source. Empty when the source is not available (a construct registered
	// from Go). This is what drift detection diffs against (memql#3758).
	SourceHash string
	// Source is the construct's authored text, carried ONLY when there is no
	// file to read it out of -- which is to say, only when OriginPath is empty.
	//
	// A promoted construct is that case, and it is why the field exists: its
	// detail page has to render a source and its jump-to-source has to become a
	// read-only untitled document, and nothing else can serve either. A
	// file-backed construct leaves it empty deliberately: the pack browser
	// already serves that file, and shipping every construct body on every
	// catalog read would cost megabytes to duplicate a better path.
	Source string
}

// promotedMarker is the value stored in MemQLEngine.promotedAuthored.
//
// The PRESENCE of the key is what every core-first check reads, and that is
// unchanged: a promoted construct may be re-promoted or demoted, a sealed core
// name may not. The marker adds the one thing the shared registries otherwise
// lose -- the AUTHORED SOURCE of what was promoted. A promoted construct lives
// in no file, so without it the catalog can neither hash the construct (leaving
// drift detection, memql#3758, nothing to compare against) nor report its
// argument form (leaving a Run affordance on a promoted query with no
// arguments to fill in).
type promotedMarker struct {
	// Source is the construct's authored text, as promoted. Empty when the
	// marker is a name RESERVATION rather than a registration.
	Source string
	// SourceHash is sense.ConstructSourceHash over Source, computed once at
	// promotion rather than per catalog read.
	SourceHash string
}

// newPromotedMarker builds the marker for a construct being registered.
func newPromotedMarker(source string) promotedMarker {
	return promotedMarker{Source: source, SourceHash: sense.ConstructSourceHash(source)}
}

// AutomationCatalogEntry is one automation, as reported to the catalog by the
// component that loaded it.
type AutomationCatalogEntry struct {
	Name        string
	Description string
	// Origin is the file path the automation was loaded from, in the same form
	// Function.Origin carries.
	Origin string
}

// AutomationCataloger reports the automations a node has loaded.
//
// Automations are the one runnable kind the engine does not itself hold a
// registry for -- component/automations owns the scheduler and its automation
// map. The alternative to this seam is for the catalog to walk the tree and
// report the automations a file DECLARES, which is exactly the file-grain
// answer this whole message exists to stop giving: it would list an automation
// this node skipped and miss one promoted into it.
//
// Wired via SetAutomationCataloger at bootstrap, the same shape SetLogicRunner
// uses. Unset -- a binary that loads no scheduler -- reports no automations,
// which is the truthful answer for that node.
type AutomationCataloger interface {
	CatalogAutomations() []AutomationCatalogEntry
}

// SetAutomationCataloger wires the automation source for the construct catalog.
func (e *MemQLEngine) SetAutomationCataloger(c AutomationCataloger) {
	e.automationCataloger = c
}

// ConstructCatalog returns every construct the engine has loaded, sorted by
// (kind, namespace, name) so two calls against an unchanged engine return an
// identical list.
//
// The result is always non-nil.
func (e *MemQLEngine) ConstructCatalog() []ConstructCatalogEntry {
	out := []ConstructCatalogEntry{}
	if e == nil {
		return out
	}

	index := buildConstructSourceIndex()
	domains := packDomainOrigins()

	add := func(entry ConstructCatalogEntry) {
		promoted, marker := e.promotedConstruct(entry.Kind, entry.Name)
		// The authored source, from whichever of the two places holds it. It is
		// what both the hash and the argument form are computed from, so a
		// construct whose source cannot be found gets neither rather than a
		// half-answer assembled from somewhere else.
		source := ""
		if promoted {
			// A promoted construct lives in no file: no path, and no domain
			// directory to take a namespace from. Everything the catalog can
			// say about its source comes off the promotion marker, which is
			// the only copy the shared registries keep.
			//
			// It does NOT clear entry.Namespace. That line used to be here and
			// was a pure no-op: every cataloger except catalogConcepts leaves
			// Namespace empty for the else-branch to fill from the file path,
			// and catalogConcepts sets it from the concept's own CANONICAL ID,
			// which owes nothing to a directory. So its only reachable effect
			// was to erase a correct answer -- invisible until memql#3746 made
			// a concept promotable, at which point every promoted concept would
			// have reported an empty namespace and sorted ahead of the tree.
			source = marker.Source
			entry.OriginPath = ""
			entry.SourceHash = marker.SourceHash
			// Carried only here: with no file, this is the only copy a client
			// can reach.
			entry.Source = marker.Source
		} else if src, found := index.lookup(entry.Kind, entry.Name); found {
			source = src.source
			entry.OriginPath = src.path
			entry.SourceHash = sense.ConstructSourceHash(src.source)
			if entry.Namespace == "" {
				entry.Namespace = treeDomainOf(src.path)
			}
		}
		entry.Origin = deriveConstructOrigin(entry.OriginPath, promoted, domains)
		entry.Runnable = runnableConstructKinds[entry.Kind]
		entry.Args = catalogArgs(entry.Kind, entry.Name, entry.Runnable, source)
		out = append(out, entry)
	}

	e.catalogConcepts(add)
	e.catalogFunctions(add)
	e.catalogSpecs(add)
	e.catalogShapes(add)
	e.catalogTools(add)
	e.catalogPrompts(add)
	e.catalogProviders(add)
	e.catalogPolicies(add)
	e.catalogSeeds(add)
	e.catalogAutomations(add)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// deriveConstructOrigin is THE derivation of a construct's origin. Every caller
// reads its answer; none computes its own.
//
// A construct with no file and no promotion marker is core: the engine registers
// a handful of constructs from Go rather than from the tree, and they ship
// inside the engine binary, which is what core means.
func deriveConstructOrigin(originPath string, promoted bool, domains map[string]string) string {
	if promoted {
		return ConstructOriginPromoted
	}
	if originPath == "" {
		return ConstructOriginCore
	}
	if strings.HasPrefix(domains[treeDomainOf(originPath)], "pack:") {
		return ConstructOriginBundle
	}
	return ConstructOriginCore
}

// promotedConstruct reports whether this (kind, name) was promoted into the
// shared registries at runtime, and the marker recording what was promoted.
//
// The marker is keyed by REGISTRY, not by kind -- query / mutation / logic all
// share the function registry, spec and trait share the spec registry -- which
// is how PromoteAuthoredConstruct and DemoteAuthoredConstruct key it, and this
// must read it the same way or a demote would leave a construct reading as
// promoted.
//
// A concept's registry key is its CANONICAL ID (memql#3746), and `name` here is
// already that: catalogConcepts reports Concept.Name, which the concept registry
// is keyed by. Keying on a declaration name instead would find nothing and
// report every promoted concept as `core`.
func (e *MemQLEngine) promotedConstruct(kind, name string) (bool, promotedMarker) {
	key := ""
	switch kind {
	case ConstructKindQuery, ConstructKindMutation, ConstructKindLogic:
		key = "function:" + name
	case ConstructKindSpec, ConstructKindTrait:
		key = "spec:" + name
	case ConstructKindConcept:
		key = "concept:" + name
	default:
		return false, promotedMarker{}
	}
	v, ok := e.promotedAuthored.Load(key)
	if !ok {
		return false, promotedMarker{}
	}
	marker, _ := v.(promotedMarker)
	return true, marker
}

// catalogArgs returns the argument form for one construct.
//
// Only a runnable kind gets one: a view-only kind has no run to generate a form
// for, and inventing one would imply an execution semantic the design defers.
// The analysis is Sense's own, over the authored source (see this file's
// header).
func catalogArgs(kind, name string, runnable bool, source string) []sense.RunnableArg {
	empty := []sense.RunnableArg{}
	if !runnable || strings.TrimSpace(source) == "" {
		return empty
	}
	// Matched on (keyword, name) rather than taken as the sole result: the
	// source is a single declaration in both the file and promoted cases today,
	// and naming what is being looked for keeps that from being an assumption a
	// caller can quietly break by handing over a multi-construct bundle.
	for _, rc := range sense.AnalyzeRunnable(source) {
		if rc.Kind != constructKeyword[kind] || rc.Name != name {
			continue
		}
		if len(rc.Args) == 0 {
			return empty
		}
		return rc.Args
	}
	return empty
}

// --- per-registry walks -------------------------------------------------

func (e *MemQLEngine) catalogConcepts(add func(ConstructCatalogEntry)) {
	if e.concepts == nil {
		return
	}
	for _, c := range e.concepts.List() {
		if c == nil {
			continue
		}
		add(ConstructCatalogEntry{
			Name:        c.Name,
			Kind:        ConstructKindConcept,
			Namespace:   conceptNamespace(c.Name),
			Description: c.Description,
		})
	}
}

func (e *MemQLEngine) catalogFunctions(add func(ConstructCatalogEntry)) {
	if e.functions == nil {
		return
	}
	for _, fn := range e.functions.List() {
		if fn == nil {
			continue
		}
		kind := functionCatalogKind(fn)
		if kind == "" {
			continue
		}
		add(ConstructCatalogEntry{
			Name:         fn.Name,
			Kind:         kind,
			Description:  fn.Description,
			BoundConcept: fn.BoundConcept,
		})
	}
}

// functionCatalogKind maps a Function's kind to a catalog kind. An empty
// FunctionKind is a query -- the loader's own reading (`case "", "query":`
// appears wherever the engine tests for one).
func functionCatalogKind(fn *Function) string {
	switch strings.ToLower(strings.TrimSpace(fn.FunctionKind)) {
	case "", "query":
		return ConstructKindQuery
	case "mutation":
		return ConstructKindMutation
	case "logic":
		return ConstructKindLogic
	case "automation":
		return ConstructKindAutomation
	case FunctionTypeBuiltin:
		return ConstructKindBuiltin
	default:
		return ""
	}
}

func (e *MemQLEngine) catalogSpecs(add func(ConstructCatalogEntry)) {
	if e.specs == nil {
		return
	}
	for _, s := range e.specs.List() {
		if s == nil {
			continue
		}
		kind := ConstructKindSpec
		if s.IsTrait {
			kind = ConstructKindTrait
		}
		add(ConstructCatalogEntry{
			Name:         s.Name,
			Kind:         kind,
			Description:  s.Description,
			BoundConcept: s.BoundName,
		})
	}
}

func (e *MemQLEngine) catalogShapes(add func(ConstructCatalogEntry)) {
	if e.shapes == nil {
		return
	}
	for _, sh := range e.shapes.List() {
		if sh == nil {
			continue
		}
		bound := ""
		if len(sh.UseConcepts) > 0 {
			bound = sh.UseConcepts[0]
		}
		add(ConstructCatalogEntry{
			Name:         sh.Name,
			Kind:         ConstructKindShape,
			Description:  sh.Description,
			BoundConcept: bound,
		})
	}
}

func (e *MemQLEngine) catalogTools(add func(ConstructCatalogEntry)) {
	if e.tools == nil {
		return
	}
	for _, t := range e.tools.List() {
		if t == nil {
			continue
		}
		add(ConstructCatalogEntry{
			Name:        t.Name,
			Kind:        ConstructKindTool,
			Description: t.Description,
		})
	}
}

func (e *MemQLEngine) catalogPrompts(add func(ConstructCatalogEntry)) {
	if e.prompts == nil {
		return
	}
	for _, p := range e.prompts.List() {
		if p == nil {
			continue
		}
		add(ConstructCatalogEntry{
			Name:        p.Name,
			Kind:        ConstructKindPrompt,
			Description: p.Description,
		})
	}
}

func (e *MemQLEngine) catalogProviders(add func(ConstructCatalogEntry)) {
	if e.providers == nil {
		return
	}
	for _, name := range e.providers.Names() {
		entry, ok := e.providers.Entry(name)
		if !ok || entry == nil {
			continue
		}
		add(ConstructCatalogEntry{
			Name:        entry.Config.Name,
			Kind:        ConstructKindProvider,
			Description: entry.Config.Description,
		})
	}
}

func (e *MemQLEngine) catalogPolicies(add func(ConstructCatalogEntry)) {
	if e.policies == nil {
		return
	}
	for _, p := range e.policies.All() {
		if p == nil {
			continue
		}
		add(ConstructCatalogEntry{
			Name:        p.Name,
			Kind:        ConstructKindPolicy,
			Description: p.Description,
		})
	}
}

func (e *MemQLEngine) catalogSeeds(add func(ConstructCatalogEntry)) {
	if e.seeds == nil {
		return
	}
	for _, s := range e.seeds.All() {
		if s == nil {
			continue
		}
		add(ConstructCatalogEntry{
			Name:         s.Name,
			Kind:         ConstructKindSeed,
			Description:  s.Description,
			BoundConcept: s.UseConcept,
		})
	}
}

func (e *MemQLEngine) catalogAutomations(add func(ConstructCatalogEntry)) {
	if e.automationCataloger == nil {
		return
	}
	for _, a := range e.automationCataloger.CatalogAutomations() {
		add(ConstructCatalogEntry{
			Name:        a.Name,
			Kind:        ConstructKindAutomation,
			Description: a.Description,
		})
	}
}

// --- the source index ---------------------------------------------------

// constructSource is one construct's file and authored text.
type constructSource struct {
	path   string
	source string
}

// constructSourceIndex resolves (kind, name) to the file the construct was
// authored in and its declaration text.
type constructSourceIndex struct {
	byKindName map[string]constructSource
}

// lookup resolves one construct, by the registry-side name: a concept's
// canonical id, or the declared name for every other kind.
//
// It takes no namespace. The namespace is what the index TELLS a caller rather
// than what it needs to be asked -- for a concept the domain is already inside
// the canonical id, and for every other kind the flat registry makes the name
// unique on its own. The parameter used to be there for signature symmetry and
// was discarded with `_ = namespace`, which read as though it were consulted.
func (i constructSourceIndex) lookup(kind, name string) (constructSource, bool) {
	src, ok := i.byKindName[CatalogConstructKey(kind, name)]
	return src, ok
}

// buildConstructSourceIndex slices every .memql file in the mounted tree once
// per kind and records where each declaration lives and what it says.
//
// Built per call rather than cached: the tree is mounted at init and does not
// change under a running node, but a test that mounts an overlay and a cluster
// that never does must get the same answer from the same code, and a cache
// keyed on nothing would give the first caller's tree to the second.
func buildConstructSourceIndex() constructSourceIndex {
	index := constructSourceIndex{byKindName: map[string]constructSource{}}
	files := baseloader.ReadAll(nil)
	for _, f := range files {
		domain := treeDomainOf(f.Path)
		// record is first-wins. A duplicate declaration is the registry's
		// problem, not this index's: whichever the loader kept is what the
		// catalog reports, and pointing at the first file found is no more
		// wrong than pointing at the last.
		record := func(kind, name, source string) {
			key := DocumentConstructKey(kind, name, domain)
			if _, exists := index.byKindName[key]; exists {
				return
			}
			index.byKindName[key] = constructSource{path: f.Path, source: source}
		}

		for kind, keyword := range constructKeyword {
			for _, slice := range languageParser.ExtractDeclarationSlices(f.Content, keywordHeaderRegexp(keyword)) {
				record(kind, slice.Name, slice.Source)
			}
		}

		// The terse single-step automation form has no braces, so the header
		// regexp above -- which is anchored on the declaration's opening `{` --
		// cannot see it. Ten automations in the tree are authored that way, and
		// without this they were catalogued with an EMPTY source hash while the
		// language server computed a real one for the same line: a construct
		// that reads as `drifted` forever, because no edit can make an empty
		// hash match a real one (memql#3758, caught by the corpus parity gate).
		for _, slice := range languageParser.ExtractTerseAutomationSlices(f.Content) {
			record(ConstructKindAutomation, slice.Name, slice.Source)
		}
	}
	return index
}

// packDomainOrigins maps each top-level DSL domain to the pack browser's own
// label for it: "embedded" for a core domain, "pack:<name>" for a
// runtime-mounted product domain. Reusing that labelling rather than
// re-deriving it keeps the two browsers agreeing about which tree a file is in.
func packDomainOrigins() map[string]string {
	out := map[string]string{}
	for _, d := range memqldsl.ListPackDomains() {
		out[d.Name] = d.Origin
	}
	return out
}

// treeDomainOf returns the top-level domain of a tree-relative path.
func treeDomainOf(path string) string {
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return path
}

// conceptNamespace returns a canonical concept id's namespace -- everything
// between the version prefix and the declaration name, so v1:cognition:space
// gives "cognition" and v1:cognition:turn:state gives "cognition:turn".
func conceptNamespace(id string) string {
	parts := strings.Split(id, ":")
	if len(parts) < 3 {
		return ""
	}
	return strings.Join(parts[1:len(parts)-1], ":")
}

// conceptRootDomain returns the first namespace segment, which is the directory
// the concept is authored in.
func conceptRootDomain(id string) string {
	parts := strings.Split(id, ":")
	if len(parts) < 3 {
		return ""
	}
	return parts[1]
}

// conceptShortName returns a canonical concept id's declaration name.
func conceptShortName(id string) string {
	parts := strings.Split(id, ":")
	return parts[len(parts)-1]
}
