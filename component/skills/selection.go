// Package skills holds the capability graph's selection rules -- how a set
// of skills is chosen for a step, and how the edges that steer that choice
// are proposed and committed.
//
// ===========================================================================
// WHY SELECTION IS STRUCTURAL AND NOT A SIMILARITY SEARCH
// ===========================================================================
// Skills depend on, conflict with, specialize and duplicate one another, and
// a vector index can see none of those things: it answers "which rows read
// like this request", which is a statement about text, not about whether the
// rows can be used together. Two skills that duplicate each other score
// almost identically by construction, so a top-k over the index returns both
// and the model is handed the same capability twice under two names. Two
// skills that conflict score independently, so a top-k returns both and the
// conflict surfaces at execution as a failure nobody can attribute.
//
// So the match is the INPUT to selection rather than its answer (spec
// section C, SkillDAG). Three readings fold into one ranking:
//
//  1. the vector match the caller already has, as candidates with scores;
//  2. the typed NEIGHBOURS of what matched -- a dependency comes because the
//     thing that matched does not work without it;
//  3. the CONFLICT signals -- a pair that cannot be held at once is resolved
//     here, deterministically, rather than at execution.
//
// Everything in this file is pure: rows in, a ranking out, no engine, no
// database, no clock. That is what lets the rules table be a test rather
// than a claim.
package skills

import "sort"

// EdgeType is the closed set of `v1:skills:skillEdge.type`. It is closed for
// the reason the engine's relationship `type` is closed and its `as` is not:
// each member here changes what SELECTION does, so a member the code does
// not know is not a label it can pass through -- it is a rule it cannot
// apply. An unrecognised value is therefore ignored rather than guessed at,
// which is the fail-quiet direction: a newer engine writing a fifth type
// costs this one a refinement it does not make, never a wrong drop.
type EdgeType string

const (
	// EdgeDependsOn -- the source does not work without the target. The
	// target is PULLED IN when the source is selected.
	EdgeDependsOn EdgeType = "dependsOn"
	// EdgeConflictsWith -- the two cannot be held at once. The lower-ranked
	// side is dropped.
	EdgeConflictsWith EdgeType = "conflictsWith"
	// EdgeSpecializes -- the source is a sharper form of the target. When
	// BOTH matched, the general one is dropped in favour of the specialist.
	EdgeSpecializes EdgeType = "specializes"
	// EdgeDuplicates -- the two cover the same ground. The lower-ranked side
	// is dropped, which is the one case where dropping loses nothing.
	EdgeDuplicates EdgeType = "duplicates"
)

// Edge status. Only a COMMITTED edge steers selection: a proposal is a
// hypothesis a run made and has not yet paid for, and letting one steer the
// next selection would let a single bad compile reshape the catalog for
// everybody. See ProposeFromRun / CommitProposals for the protocol.
const (
	EdgeProposed  = "proposed"
	EdgeCommitted = "committed"
)

// Evidence is where an edge came from: the run and step that showed it. A
// proposal with no evidence is refused at Propose time, because an edge
// nobody can trace back is one nobody can argue with later.
type Evidence struct {
	RunID   string `json:"runId"`
	StepKey string `json:"stepKey"`
}

// Edge is one typed relation between two skills -- the projected shape of a
// `v1:skills:skillEdge` row, carrying only what selection reads.
type Edge struct {
	FromSkillID string     `json:"fromSkillId"`
	ToSkillID   string     `json:"toSkillId"`
	Type        EdgeType   `json:"type"`
	Status      string     `json:"status"`
	ProposedBy  string     `json:"proposedBy"`
	Evidence    []Evidence `json:"evidence,omitempty"`
}

// Candidate is one row the vector match returned, with its score. Score is
// whatever the caller's index produced; nothing here interprets its scale
// beyond ordering, so a cosine similarity and a rank-inverted integer both
// work as long as higher means closer.
type Candidate struct {
	SkillID string
	Score   float64
	// Active is false for a skill row whose `active` flag is off or whose
	// `status` is deprecated. Such a row can still be REACHED as a
	// dependency -- something that matched declares it needs this -- but it
	// can never be a direct match, because the catalog stopped offering it.
	Active bool
}

// Why a skill is in the answer. This travels onto the step's binding, so a
// person reading a run can tell a skill the request matched from one that
// came along because something else needed it.
const (
	ReasonMatched    = "matched"
	ReasonDependency = "dependency"
)

// Selected is one skill in the answer.
type Selected struct {
	SkillID string  `json:"skillId"`
	Score   float64 `json:"score"`
	Reason  string  `json:"reason"`
	// Via names the skill that pulled this one in. Empty for a direct match.
	Via string `json:"via,omitempty"`
	// Dropped names what this skill displaced, and why -- a conflict, a
	// duplicate or a generalization. Empty when it displaced nothing. This
	// is the part a person actually reads when a run used a skill they did
	// not expect.
	Displaced []Displacement `json:"displaced,omitempty"`
}

// Displacement records one skill this one pushed out of the answer.
type Displacement struct {
	SkillID string   `json:"skillId"`
	Type    EdgeType `json:"type"`
}

// Options bound the walk.
type Options struct {
	// Limit caps the answer. Zero means DefaultLimit; a negative value means
	// no cap, which is only ever right in a test.
	Limit int
	// MinScore drops a candidate the index barely matched. A candidate below
	// it is not a direct match; it may still arrive as a dependency.
	MinScore float64
	// DependencyDepth is how far a dependsOn chain is followed. Zero means
	// DefaultDependencyDepth. A dependency of a dependency is a real case --
	// a script skill needing a runtime skill needing a credential skill --
	// and an unbounded walk over a graph with a cycle is not.
	DependencyDepth int
}

// The defaults. Limit is generous rather than tight because the cost of one
// extra skill in a prompt is some tokens, and the cost of a missing one is a
// step that cannot do its job.
const (
	DefaultLimit           = 8
	DefaultDependencyDepth = 3
)

func (o Options) limit() int {
	if o.Limit == 0 {
		return DefaultLimit
	}
	return o.Limit
}

func (o Options) depth() int {
	if o.DependencyDepth == 0 {
		return DefaultDependencyDepth
	}
	if o.DependencyDepth < 0 {
		return 0
	}
	return o.DependencyDepth
}

// Select ranks the candidates into the skills a step should be bound to.
//
// The order of the three passes is the whole design, and none of them
// commutes with another:
//
//   - MATCHES first, so the conflict and specialization passes have scores to
//     compare. Resolving a conflict before knowing the scores would have to
//     pick by id, which is deterministic and meaningless.
//   - DISPLACEMENT second, over matches only. A skill dropped here must not
//     drag its dependencies in, and running the dependency pass first would
//     have already pulled them.
//   - DEPENDENCIES last, from the survivors. This is also what makes the
//     result stable under a re-run: dependencies are derived, so they cannot
//     themselves displace anything.
//
// Determinism is a requirement rather than a nicety: two replicas selecting
// for the same step must agree, and they share no state to agree THROUGH.
// So every sort is total -- score descending, then skill id ascending -- and
// every map iteration is over a sorted key list.
func Select(candidates []Candidate, edges []Edge, opts Options) []Selected {
	index := newEdgeIndex(edges)

	// Pass 1 -- the matches. Sorted total, filtered by MinScore and by the
	// catalog's own offer (an inactive row is not on offer).
	matched := make([]Candidate, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		if c.SkillID == "" || seen[c.SkillID] {
			continue
		}
		seen[c.SkillID] = true
		if !c.Active {
			continue
		}
		if c.Score < opts.MinScore {
			continue
		}
		matched = append(matched, c)
	}
	sortCandidates(matched)

	// Pass 2 -- displacement. Walked highest-first, so for a conflict or a
	// duplicate the winner is simply the one already kept.
	//
	// SPECIALIZATION IS THE ONE RULE THAT RUNS BOTH WAYS, because it is the
	// one rule score does not decide. A specialist that arrives AFTER its
	// general -- which is the common case, since a general description
	// matches a vague request more strongly -- has to evict it, or the rule
	// would only fire when the ordering happened to be favourable and the
	// answer would depend on the index's scale rather than on the graph.
	//
	// Eviction marks rather than removes, so `keptAt` indices stay valid for
	// the rest of the pass; the marked entries are compacted out below.
	kept := make([]Selected, 0, len(matched))
	evicted := make([]bool, 0, len(matched))
	keptAt := make(map[string]int, len(matched))
	dropped := make(map[string]bool, len(matched))
	live := func(id string) (int, bool) {
		at, ok := keptAt[id]
		if !ok || evicted[at] {
			return 0, false
		}
		return at, true
	}
	for _, c := range matched {
		if holder, kind, refused := index.displacedBy(c.SkillID, live); refused {
			dropped[c.SkillID] = true
			at, _ := live(holder)
			kept[at].Displaced = append(kept[at].Displaced, Displacement{SkillID: c.SkillID, Type: kind})
			continue
		}
		keptAt[c.SkillID] = len(kept)
		kept = append(kept, Selected{SkillID: c.SkillID, Score: c.Score, Reason: ReasonMatched})
		evicted = append(evicted, false)
		// Now the other direction: anything already kept that THIS skill is
		// a sharper form of. A general that has already displaced something
		// takes that record with it -- the displaced row stays dropped
		// rather than being re-admitted, because the specialist covers the
		// same ground the general did and re-admitting would undo a decision
		// on the strength of an ordering accident.
		for _, general := range index.generalizedBy(c.SkillID) {
			at, ok := live(general)
			if !ok {
				continue
			}
			evicted[at] = true
			me := keptAt[c.SkillID]
			kept[me].Displaced = append(kept[me].Displaced, Displacement{SkillID: general, Type: EdgeSpecializes})
		}
	}
	surviving := kept[:0]
	compacted := make(map[string]int, len(kept))
	for i, s := range kept {
		if evicted[i] {
			dropped[s.SkillID] = true
			continue
		}
		compacted[s.SkillID] = len(surviving)
		surviving = append(surviving, s)
	}
	kept = surviving
	keptAt = compacted
	live = func(id string) (int, bool) {
		at, ok := keptAt[id]
		return at, ok
	}

	// Pass 3 -- dependencies, breadth-first from the survivors in rank order,
	// bounded by depth. A dependency already kept as a match keeps its match
	// reason: it earned its place twice and the stronger reading wins.
	//
	// A dependency that a survivor CONFLICTS with is not pulled in. That is
	// the case worth stating: a skill can name a dependency the caller's
	// other skill refuses, and quietly satisfying the dependency would put
	// the run in exactly the state the conflict edge exists to prevent.
	frontier := make([]string, 0, len(kept))
	for _, s := range kept {
		frontier = append(frontier, s.SkillID)
	}
	for depth := 0; depth < opts.depth() && len(frontier) > 0; depth++ {
		next := make([]string, 0, len(frontier))
		for _, from := range frontier {
			for _, to := range index.dependencies(from) {
				if _, already := keptAt[to]; already {
					continue
				}
				if dropped[to] {
					continue
				}
				if _, _, conflict := index.displacedBy(to, live); conflict {
					dropped[to] = true
					continue
				}
				keptAt[to] = len(kept)
				kept = append(kept, Selected{SkillID: to, Score: 0, Reason: ReasonDependency, Via: from})
				next = append(next, to)
			}
		}
		frontier = next
	}

	if lim := opts.limit(); lim >= 0 && len(kept) > lim {
		kept = kept[:lim]
	}
	return kept
}

// ---------------------------------------------------------------------------
// The edge index
// ---------------------------------------------------------------------------

type edgeIndex struct {
	// dependsOn, source -> targets, sorted.
	deps map[string][]string
	// conflictsWith and duplicates, symmetric: either direction drops the
	// lower-ranked side. Stored both ways so a lookup is one map read.
	refuses map[string]map[string]EdgeType
	// specializes, general -> specialists. Read in the general's direction
	// because that is one of the two questions asked: "is something sharper
	// than me already in the answer?"
	specializedBy map[string]map[string]bool
	// The same edges, specialist -> generals, sorted. The other question:
	// "am I sharper than something already in the answer?" Both directions
	// are indexed rather than one being derived on the fly, because the walk
	// asks each once per candidate.
	specializes map[string][]string
}

func newEdgeIndex(edges []Edge) *edgeIndex {
	ix := &edgeIndex{
		deps:          map[string][]string{},
		refuses:       map[string]map[string]EdgeType{},
		specializedBy: map[string]map[string]bool{},
		specializes:   map[string][]string{},
	}
	for _, e := range edges {
		// A PROPOSAL DOES NOT STEER. It is a hypothesis the run that made it
		// has not yet paid for, and one bad compile must not reshape what
		// every later selection sees.
		if e.Status != EdgeCommitted {
			continue
		}
		if e.FromSkillID == "" || e.ToSkillID == "" || e.FromSkillID == e.ToSkillID {
			continue
		}
		switch e.Type {
		case EdgeDependsOn:
			ix.deps[e.FromSkillID] = append(ix.deps[e.FromSkillID], e.ToSkillID)
		case EdgeConflictsWith, EdgeDuplicates:
			ix.addRefusal(e.FromSkillID, e.ToSkillID, e.Type)
			ix.addRefusal(e.ToSkillID, e.FromSkillID, e.Type)
		case EdgeSpecializes:
			if ix.specializedBy[e.ToSkillID] == nil {
				ix.specializedBy[e.ToSkillID] = map[string]bool{}
			}
			ix.specializedBy[e.ToSkillID][e.FromSkillID] = true
			ix.specializes[e.FromSkillID] = append(ix.specializes[e.FromSkillID], e.ToSkillID)
		}
		// Any other type: ignored on purpose. See EdgeType.
	}
	for k := range ix.deps {
		ix.deps[k] = dedupeSorted(ix.deps[k])
	}
	for k := range ix.specializes {
		ix.specializes[k] = dedupeSorted(ix.specializes[k])
	}
	return ix
}

func (ix *edgeIndex) addRefusal(a, b string, kind EdgeType) {
	if ix.refuses[a] == nil {
		ix.refuses[a] = map[string]EdgeType{}
	}
	// conflictsWith wins over duplicates when a pair carries both: it is the
	// stronger statement, and the displacement record should say the sharper
	// of the two true things.
	if existing, ok := ix.refuses[a][b]; ok && existing == EdgeConflictsWith {
		return
	}
	ix.refuses[a][b] = kind
}

func (ix *edgeIndex) dependencies(from string) []string {
	return ix.deps[from]
}

// displacedBy answers whether a skill already in the answer pushes this one
// out, and which one did it.
//
// `live` reports a kept skill's RANK POSITION, and the lowest position wins
// when several refuse the same candidate. That is what keeps the recorded
// displacement stable: iterating the refusal map alone would attribute the
// drop to a different holder run to run, and the attribution is the part a
// person reads when a run used a skill they did not expect.
func (ix *edgeIndex) displacedBy(id string, live func(string) (int, bool)) (string, EdgeType, bool) {
	best, bestAt, bestKind := "", -1, EdgeType("")
	consider := func(holder string, kind EdgeType) {
		at, ok := live(holder)
		if !ok {
			return
		}
		if bestAt == -1 || at < bestAt {
			best, bestAt, bestKind = holder, at, kind
		}
	}
	for _, holder := range sortedRefusals(ix.refuses[id]) {
		consider(holder, ix.refuses[id][holder])
	}
	// A generalization is displaced by any specialist already in the answer.
	// The direction matters: a specialist is never dropped for its general,
	// even when the general scored higher, because the general is by
	// definition the less precise answer to the same request.
	for _, specialist := range sortedSet(ix.specializedBy[id]) {
		consider(specialist, EdgeSpecializes)
	}
	if bestAt == -1 {
		return "", "", false
	}
	return best, bestKind, true
}

// generalizedBy is the other direction of the specializes edge: the skills
// this one is a sharper form of. Sorted, because it drives an eviction and
// two replicas must evict the same rows in the same order.
func (ix *edgeIndex) generalizedBy(specialist string) []string {
	return ix.specializes[specialist]
}

// ---------------------------------------------------------------------------
// Propose and commit
// ---------------------------------------------------------------------------

// StepBinding is what a run observed about one step: which skills it was
// bound to, and which steps it depended on. Propose reads runs, not
// prompts -- an edge asserted by a model and never executed is a guess, and
// the ladder this feeds is supposed to be evidence.
type StepBinding struct {
	Key       string
	SkillIDs  []string
	DependsOn []string
	// ConstructRefs are the catalog constructs the step actually called.
	// Two skills contributing the same construct is what a `duplicates`
	// proposal is made of.
	ConstructRefs []string
}

// ProposeFromRun reads a finished run's step bindings and proposes the edges
// the run is evidence for. It proposes only two of the four types, and the
// omissions are deliberate:
//
//   - dependsOn, when a step bound to A depended on a step bound to B. The
//     run ORDERED them, which is the observation the edge encodes.
//   - duplicates, when two skills in one run both contributed the same
//     construct. Two skills that own one construct are two names for one
//     capability whatever their descriptions say.
//   - conflictsWith is NOT proposed. A run that succeeded is evidence of
//     compatibility, not of conflict, and a run that failed cannot attribute
//     the failure to a pair without the symptom classifier's judgment (spec
//     section E). Proposing it from co-occurrence would make every unlucky
//     run poison a pair for everyone.
//   - specializes is NOT proposed. It is a claim about MEANING -- that one
//     skill is a sharper form of another -- and nothing in a run's trace
//     carries it. It stays a human or authoring-time act.
//
// Every proposal carries its evidence and is returned at status `proposed`.
// Nothing here writes; the caller does, which is what keeps this testable
// without an engine.
func ProposeFromRun(runID string, steps []StepBinding) []Edge {
	if runID == "" || len(steps) == 0 {
		return nil
	}
	byKey := make(map[string]StepBinding, len(steps))
	for _, s := range steps {
		if s.Key != "" {
			byKey[s.Key] = s
		}
	}

	proposals := map[string]Edge{}
	add := func(from, to string, kind EdgeType, ev Evidence) {
		if from == "" || to == "" || from == to {
			return
		}
		key := string(kind) + "|" + from + "|" + to
		e, ok := proposals[key]
		if !ok {
			e = Edge{FromSkillID: from, ToSkillID: to, Type: kind, Status: EdgeProposed, ProposedBy: "system"}
		}
		e.Evidence = append(e.Evidence, ev)
		proposals[key] = e
	}

	// dependsOn, from the run's own ordering.
	for _, step := range steps {
		for _, upstreamKey := range step.DependsOn {
			upstream, ok := byKey[upstreamKey]
			if !ok {
				continue
			}
			for _, from := range step.SkillIDs {
				for _, to := range upstream.SkillIDs {
					add(from, to, EdgeDependsOn, Evidence{RunID: runID, StepKey: step.Key})
				}
			}
		}
	}

	// duplicates, from two skills owning one construct. Ordered by skill id
	// so the pair is proposed once rather than twice in opposite directions
	// -- the edge is symmetric in meaning and a second row would be the same
	// fact stored twice.
	for _, step := range steps {
		for _, construct := range step.ConstructRefs {
			owners := dedupeSorted(append([]string(nil), step.SkillIDs...))
			if construct == "" || len(owners) < 2 {
				continue
			}
			for i := 0; i < len(owners); i++ {
				for j := i + 1; j < len(owners); j++ {
					add(owners[i], owners[j], EdgeDuplicates, Evidence{RunID: runID, StepKey: step.Key})
				}
			}
		}
	}

	out := make([]Edge, 0, len(proposals))
	for _, key := range sortedKeys(proposals) {
		out = append(out, proposals[key])
	}
	return out
}

// CommitProposals is the second half of the protocol: a run that SUCCEEDED
// commits the edges its compile proposed, and a run that did not leaves them
// proposed.
//
// The asymmetry is the point. Committing on failure would let a run that
// went wrong teach the catalog its mistake, and DELETING on failure would
// erase the evidence that a proposal keeps being made -- which is exactly
// the signal a person needs to decide it by hand. So a failed run's
// proposals persist, unpromoted, and say so.
func CommitProposals(proposals []Edge, runSucceeded bool) []Edge {
	if !runSucceeded {
		return nil
	}
	out := make([]Edge, 0, len(proposals))
	for _, e := range proposals {
		if e.FromSkillID == "" || e.ToSkillID == "" {
			continue
		}
		e.Status = EdgeCommitted
		out = append(out, e)
	}
	return out
}

// ---------------------------------------------------------------------------

func sortCandidates(in []Candidate) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Score != in[j].Score {
			return in[i].Score > in[j].Score
		}
		return in[i].SkillID < in[j].SkillID
	})
}

func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	sort.Strings(in)
	out := in[:0]
	last := ""
	for i, v := range in {
		if v == "" || (i > 0 && v == last) {
			continue
		}
		out = append(out, v)
		last = v
	}
	return out
}

func sortedRefusals(m map[string]EdgeType) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedSet(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys(m map[string]Edge) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
