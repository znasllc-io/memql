package harness

// planner_logic.go is the planner's PURE decision core (issue #587).
//
// The planner is the harness OUTER loop: it turns a user goal into a
// persisted plan + a step DAG (#582), and for each step decides -- by
// fitScore against existing agents (the cognition `fitScore` pattern
// generalized one step further) -- whether to ROUTE to an existing
// agent, UPGRADE a partial-fit agent with extra knowledge/tools, or
// PROVISION a brand-new agent composed from a seed role catalog. Before
// persisting a freshly-composed agent it DEDUPs against existing agents
// to prevent sprawl (merge into the near-duplicate instead).
//
// Everything in this file is deliberately free of DB, event bus, engine,
// and LLM receivers -- the route/upgrade/provision decision, the dedup
// decision, the role-composition, and the decomposition-result
// validation are all pure functions so they unit-test in isolation
// (the same discipline reconcile_logic.go follows). The impure half --
// the live LLM decompose, the embedding fitScore, and the mutation
// writes -- lives in planner.go and leans on these helpers for every
// decision.

import (
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Route-or-provision thresholds
// ---------------------------------------------------------------------------

// PlannerThresholds tune the route-or-provision decision. A step's best
// fitScore against the existing agent roster (cosine similarity in
// [0,1]) is compared to these bands:
//
//	bestFit >= RouteThreshold    -> ROUTE   (assign the existing agent)
//	bestFit >= UpgradeThreshold  -> UPGRADE (attach missing knowledge/tools)
//	otherwise                    -> PROVISION (compose a new agent)
//
// DedupThreshold gates the anti-sprawl merge: before persisting a
// freshly-composed (provisioned) agent, its capability embedding is
// similarity-checked against the roster; a match >= DedupThreshold
// merges into that existing agent instead of creating a new row.
type PlannerThresholds struct {
	RouteThreshold   float64
	UpgradeThreshold float64
	DedupThreshold   float64
}

// Default* values for the route-or-provision bands. Chosen so a clear
// specialist match routes, a same-domain-but-tool-gap match upgrades,
// and a genuinely novel step provisions. DedupThreshold is set high so
// only near-identical roles collapse (an HR agent and a Legal agent must
// stay distinct).
const (
	DefaultRouteThreshold   = 0.82
	DefaultUpgradeThreshold = 0.60
	DefaultDedupThreshold   = 0.90
)

// DefaultPlannerThresholds returns the conservative default bands. The
// planner uses these when no explicit override is configured.
func DefaultPlannerThresholds() PlannerThresholds {
	return PlannerThresholds{
		RouteThreshold:   DefaultRouteThreshold,
		UpgradeThreshold: DefaultUpgradeThreshold,
		DedupThreshold:   DefaultDedupThreshold,
	}
}

// withDefaults fills any zero/invalid band with its default and clamps to
// a sane ordering (route >= upgrade). Keeps a half-configured struct from
// producing nonsense decisions.
func (t PlannerThresholds) withDefaults() PlannerThresholds {
	if t.RouteThreshold <= 0 || t.RouteThreshold > 1 {
		t.RouteThreshold = DefaultRouteThreshold
	}
	if t.UpgradeThreshold <= 0 || t.UpgradeThreshold > 1 {
		t.UpgradeThreshold = DefaultUpgradeThreshold
	}
	if t.DedupThreshold <= 0 || t.DedupThreshold > 1 {
		t.DedupThreshold = DefaultDedupThreshold
	}
	if t.UpgradeThreshold > t.RouteThreshold {
		// An upgrade band above the route band is meaningless; clamp it.
		t.UpgradeThreshold = t.RouteThreshold
	}
	return t
}

// ---------------------------------------------------------------------------
// Route-or-provision decision
// ---------------------------------------------------------------------------

// RouteAction is the three-way decision the planner makes per step.
type RouteAction string

const (
	// ActionRoute assigns an existing high-fit agent unchanged.
	ActionRoute RouteAction = "route"
	// ActionUpgrade attaches missing knowledge/tools to a partial-fit
	// agent, then assigns it.
	ActionUpgrade RouteAction = "upgrade"
	// ActionProvision composes a brand-new agent from a seed role.
	ActionProvision RouteAction = "provision"
)

// AgentFit is the fitScore of one existing agent against one step. Mirrors
// the cognition `fitScore` (0..1) but is computed by cosine similarity
// between the step embedding and the agent's capability embedding rather
// than by an LLM, so it is cheap to run for every step x every agent.
type AgentFit struct {
	AgentID string
	Score   float64
}

// BestFit returns the highest-scoring agent fit, or (AgentFit{}, false)
// when the roster is empty. Ties resolve to the first encountered (stable
// for a stably-ordered input), so the decision is deterministic in tests.
func BestFit(fits []AgentFit) (AgentFit, bool) {
	best := AgentFit{}
	found := false
	for _, f := range fits {
		if !found || f.Score > best.Score {
			best = f
			found = true
		}
	}
	return best, found
}

// RouteDecision is the outcome of DecideRoute for a single step.
type RouteDecision struct {
	Action RouteAction
	// AgentID is the existing agent to route/upgrade to. Empty for a
	// provision decision (the new agent id is minted by the caller).
	AgentID string
	// Score is the best fitScore observed (0 when the roster was empty).
	Score float64
	// Rationale is a short human-readable explanation recorded on the
	// `decision` observation for auditability.
	Rationale string
}

// DecideRoute applies the route-or-provision bands to a step's per-agent
// fits. An empty roster always provisions. Pure: no I/O, no LLM -- the
// whole decision is the comparison of the best fit to the thresholds.
func DecideRoute(fits []AgentFit, thresholds PlannerThresholds) RouteDecision {
	t := thresholds.withDefaults()
	best, ok := BestFit(fits)
	if !ok {
		return RouteDecision{
			Action:    ActionProvision,
			Score:     0,
			Rationale: "no existing agents -- provisioning a new specialist",
		}
	}
	switch {
	case best.Score >= t.RouteThreshold:
		return RouteDecision{
			Action:    ActionRoute,
			AgentID:   best.AgentID,
			Score:     best.Score,
			Rationale: fmt.Sprintf("agent %s fits (%.2f >= route %.2f)", best.AgentID, best.Score, t.RouteThreshold),
		}
	case best.Score >= t.UpgradeThreshold:
		return RouteDecision{
			Action:    ActionUpgrade,
			AgentID:   best.AgentID,
			Score:     best.Score,
			Rationale: fmt.Sprintf("agent %s partial fit (%.2f in [%.2f,%.2f)) -- upgrading", best.AgentID, best.Score, t.UpgradeThreshold, t.RouteThreshold),
		}
	default:
		return RouteDecision{
			Action:    ActionProvision,
			Score:     best.Score,
			Rationale: fmt.Sprintf("best fit too low (%.2f < upgrade %.2f) -- provisioning a new specialist", best.Score, t.UpgradeThreshold),
		}
	}
}

// ---------------------------------------------------------------------------
// Dedup / merge decision (anti-sprawl)
// ---------------------------------------------------------------------------

// DedupDecision is the outcome of DecideDedup: whether a freshly-composed
// agent collapses onto an existing near-duplicate (merge) or is persisted
// as a new row (keep).
type DedupDecision struct {
	// Merge is true when an existing agent is a near-duplicate; assign
	// MergeInto rather than persisting a new agent.
	Merge bool
	// MergeInto is the existing agent id to merge into when Merge is true.
	MergeInto string
	// Score is the best similarity observed.
	Score     float64
	Rationale string
}

// DecideDedup checks a candidate (just-composed) agent's capability fits
// against the roster: if the best is at/above the dedup threshold the
// candidate is a near-duplicate and should merge into that existing agent
// instead of being persisted (prevents agent sprawl -- acceptance
// criterion). Pure: the similarity scores are computed by the caller.
func DecideDedup(candidateFits []AgentFit, thresholds PlannerThresholds) DedupDecision {
	t := thresholds.withDefaults()
	best, ok := BestFit(candidateFits)
	if !ok {
		return DedupDecision{Merge: false, Rationale: "no existing agents -- keeping new agent"}
	}
	if best.Score >= t.DedupThreshold {
		return DedupDecision{
			Merge:     true,
			MergeInto: best.AgentID,
			Score:     best.Score,
			Rationale: fmt.Sprintf("near-duplicate of agent %s (%.2f >= dedup %.2f) -- merging to avoid sprawl", best.AgentID, best.Score, t.DedupThreshold),
		}
	}
	return DedupDecision{
		Merge:     false,
		Score:     best.Score,
		Rationale: fmt.Sprintf("distinct enough from roster (best %.2f < dedup %.2f) -- keeping new agent", best.Score, t.DedupThreshold),
	}
}

// ---------------------------------------------------------------------------
// Seed role catalog + role composition
// ---------------------------------------------------------------------------

// SeedRole is one entry in the planner's role catalog -- the vocabulary
// the planner provisions FROM. A role = a set of knowledge domains + a
// scoped tool set + a system-prompt template. Tool scoping is a quality
// decision: piling every tool onto one agent degrades model performance,
// so a role grants only the tools its work needs (ties to #588).
type SeedRole struct {
	// Slug is the stable role identifier (researcher / builder / operator).
	Slug string
	// Name is the display name stamped on the provisioned agent.
	Name string
	// Description is the one-line role summary.
	Description string
	// KnowledgeDomains are the knowledge-domain ids the role carries.
	KnowledgeDomains []string
	// Tools are the scoped tool slugs the role grants (kept tight on
	// purpose; see #588).
	Tools []string
	// PromptTemplate is the system-prompt skeleton the agent gets, with a
	// {{goal}}-style hook the composer fills from the step.
	PromptTemplate string
	// Keywords drive the heuristic role pick when no LLM role hint is
	// available (a degraded-mode fallback so provisioning still works
	// without the decompose model naming a role).
	Keywords []string
}

// SeedRoleCatalog returns the planner's built-in role catalog. Three seed
// roles ship per the epic (#590): researcher / builder / operator. They
// are intentionally coarse -- the planner composes a concrete specialist
// by picking the closest role and layering the step's specifics on top;
// the catalog is the starting vocabulary, not the final agent.
//
// Choice rationale (called out in the PR): these three cover the dominant
// task shapes in the harness -- gather/synthesize information (researcher),
// produce/modify artifacts in the sandbox (builder), and act on the user's
// own systems/UI (operator). Each grants a deliberately scoped tool set so
// a provisioned agent is not handed the whole tool surface (#588).
func SeedRoleCatalog() []SeedRole {
	return []SeedRole{
		{
			Slug:        "researcher",
			Name:        "Researcher",
			Description: "Gathers, reads, and synthesizes information to answer a question or inform a decision.",
			KnowledgeDomains: []string{
				"research-methods",
			},
			Tools: []string{
				"workbenchHost", // sandboxed fs_read / http_fetch for gathering
			},
			Keywords: []string{
				"research", "find", "investigate", "analyze", "summarize",
				"compare", "gather", "read", "search", "learn", "explain",
				"understand", "review", "evaluate",
			},
			PromptTemplate: researcherPromptTemplate,
		},
		{
			Slug:        "builder",
			Name:        "Builder",
			Description: "Produces and modifies artifacts -- code, files, documents -- in the sandboxed workbench.",
			KnowledgeDomains: []string{
				"workbench",
				"software-engineering",
			},
			Tools: []string{
				"workbenchHost", // sandboxed exec / fs_write / fs_read
			},
			Keywords: []string{
				"build", "write", "create", "implement", "generate", "code",
				"draft", "make", "produce", "compose", "edit", "modify",
				"refactor", "fix", "develop",
			},
			PromptTemplate: builderPromptTemplate,
		},
		{
			Slug:        "operator",
			Name:        "Operator",
			Description: "Acts on the user's own systems and UI -- drives applications, runs commands on the user's machine.",
			KnowledgeDomains: []string{
				"workbench",
				"computer-use",
			},
			Tools: []string{
				"workbenchHost",
				// The hyphenated capability slug (see worker_caps.go); the
				// prior underscore form never matched the registry and
				// passed through unexpanded (latent since the slug split).
				"computer-use-headless",
			},
			Keywords: []string{
				"operate", "run", "execute", "deploy", "configure", "install",
				"navigate", "click", "automate", "control", "drive", "use",
				"open", "launch",
			},
			PromptTemplate: operatorPromptTemplate,
		},
	}
}

// Prompt-template skeletons for the seed roles. Each is a self-contained
// system prompt with a {{goal}} hook the composer substitutes. Kept terse
// -- the harness's per-turn instruction carries the specifics; the role
// prompt sets scope + posture. Specialists never talk to the human (#588),
// hence the "report results, do not address the user" line.
const (
	researcherPromptTemplate = `You are a research specialist working inside an agent harness.
Your job for this task: {{goal}}
Gather and synthesize the information needed, then report your findings as a structured result.
Use only the tools you have been granted. Do not address the end user directly -- you report results to the planner.`

	builderPromptTemplate = `You are a builder specialist working inside an agent harness.
Your job for this task: {{goal}}
Produce or modify the requested artifact in the sandboxed workbench, verify it, then report the result.
Use only the tools you have been granted. Do not address the end user directly -- you report results to the planner.`

	operatorPromptTemplate = `You are an operator specialist working inside an agent harness.
Your job for this task: {{goal}}
Carry out the requested actions on the available systems, confirm the outcome, then report the result.
Use only the tools you have been granted. Be conservative with irreversible actions. Do not address the end user directly -- you report results to the planner.`
)

// ComposedAgent is the concrete agent spec the planner provisions. It is
// assembled by ComposeAgent from a seed role + the step's specifics; the
// caller persists it via createAgent (or merges it into an
// existing agent on a dedup hit).
type ComposedAgent struct {
	Name             string
	Description      string
	RoleSlug         string
	KnowledgeDomains []string
	Tools            []string
	SystemPrompt     string
	// CapabilityText is the embedding source for the dedup similarity
	// check + future fitScore lookups (role + domains + tools + prompt).
	CapabilityText string
}

// PickSeedRole chooses the best seed role for a step. When roleHint names
// a catalog slug exactly, that role wins (the LLM decompose can name the
// role directly). Otherwise it falls back to a keyword overlap score
// against the step title + goal, so provisioning still works in degraded
// mode (no role hint). Returns the researcher role as a final default --
// the safest generalist when nothing matches.
func PickSeedRole(roleHint, stepText string, catalog []SeedRole) SeedRole {
	if len(catalog) == 0 {
		catalog = SeedRoleCatalog()
	}
	hint := strings.ToLower(strings.TrimSpace(roleHint))
	if hint != "" {
		for _, r := range catalog {
			if strings.ToLower(r.Slug) == hint {
				return r
			}
		}
	}
	lowerText := strings.ToLower(stepText)
	bestIdx := 0
	bestScore := -1
	for i, r := range catalog {
		score := 0
		for _, kw := range r.Keywords {
			if kw == "" {
				continue
			}
			if strings.Contains(lowerText, kw) {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	return catalog[bestIdx]
}

// ComposeAgent assembles a concrete agent from a seed role + the step's
// goal. The system prompt fills the role template's {{goal}} hook; the
// capability text concatenates role + domains + tools + prompt so the
// dedup similarity check (and later fitScore) compares like-for-like.
// Pure -- no id minting, no persistence.
func ComposeAgent(role SeedRole, stepTitle, stepGoal string) ComposedAgent {
	goal := strings.TrimSpace(stepGoal)
	if goal == "" {
		goal = strings.TrimSpace(stepTitle)
	}
	prompt := strings.ReplaceAll(role.PromptTemplate, "{{goal}}", goal)
	name := role.Name
	if t := strings.TrimSpace(stepTitle); t != "" {
		name = fmt.Sprintf("%s: %s", role.Name, truncateText(t, 48))
	}
	domains := dedupStrings(role.KnowledgeDomains)
	tools := dedupStrings(role.Tools)
	return ComposedAgent{
		Name:             name,
		Description:      role.Description,
		RoleSlug:         role.Slug,
		KnowledgeDomains: domains,
		Tools:            tools,
		SystemPrompt:     prompt,
		CapabilityText:   AgentCapabilityText(role.Slug, role.Description, domains, tools),
	}
}

// AgentCapabilityText renders the embedding source for an agent's
// capability vector -- the text whose embedding fitScore compares against
// a step embedding (route) and against the roster (dedup). Stable
// ordering so the same capabilities always produce the same text (and
// therefore the same cached embedding).
func AgentCapabilityText(roleSlug, description string, domains, tools []string) string {
	d := append([]string(nil), domains...)
	tl := append([]string(nil), tools...)
	sort.Strings(d)
	sort.Strings(tl)
	var b strings.Builder
	b.WriteString("role: ")
	b.WriteString(strings.TrimSpace(roleSlug))
	if desc := strings.TrimSpace(description); desc != "" {
		b.WriteString("\n")
		b.WriteString(desc)
	}
	if len(d) > 0 {
		b.WriteString("\nknowledge: ")
		b.WriteString(strings.Join(d, ", "))
	}
	if len(tl) > 0 {
		b.WriteString("\ntools: ")
		b.WriteString(strings.Join(tl, ", "))
	}
	return b.String()
}

// StepCapabilityText renders the embedding source for a step -- the text
// whose embedding is fitScored against each agent's capability text.
// Title + goal concatenated; mirrors AgentCapabilityText so the two
// embeddings live in a comparable space.
func StepCapabilityText(title, goal string) string {
	title = strings.TrimSpace(title)
	goal = strings.TrimSpace(goal)
	if goal == "" || goal == title {
		return title
	}
	return title + "\n" + goal
}

// ---------------------------------------------------------------------------
// Upgrade gap computation
// ---------------------------------------------------------------------------

// UpgradeGap is the set of knowledge domains + tools a partial-fit agent
// is MISSING for a step's role -- what the upgrade attaches. Computed by
// diffing the role's full capability set against what the agent already
// carries, so an upgrade is a no-op when the agent already has everything.
type UpgradeGap struct {
	AddDomains []string
	AddTools   []string
}

// Empty reports whether the upgrade would add nothing (the agent already
// covers the role's capabilities). The planner skips the mutation in that
// case and routes as-is.
func (g UpgradeGap) Empty() bool { return len(g.AddDomains) == 0 && len(g.AddTools) == 0 }

// ComputeUpgradeGap diffs a role's capabilities against an agent's current
// capabilities and returns what is missing. Pure set difference, stable
// order. The agent's existing domains/tools are passed in by the caller
// (read from the agent row).
func ComputeUpgradeGap(role SeedRole, currentDomains, currentTools []string) UpgradeGap {
	return UpgradeGap{
		AddDomains: missingFrom(role.KnowledgeDomains, currentDomains),
		AddTools:   missingFrom(role.Tools, currentTools),
	}
}

// missingFrom returns the items of `want` not present in `have`, deduped,
// in want-order.
func missingFrom(want, have []string) []string {
	haveSet := make(map[string]struct{}, len(have))
	for _, h := range have {
		haveSet[strings.TrimSpace(h)] = struct{}{}
	}
	seen := make(map[string]struct{}, len(want))
	out := make([]string, 0, len(want))
	for _, w := range want {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if _, ok := haveSet[w]; ok {
			continue
		}
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		out = append(out, w)
	}
	return out
}

// MergeCapabilities unions an agent's current capabilities with the gap an
// upgrade adds, producing the new full lists to write back. Stable order:
// current first (preserved), then the additions.
func MergeCapabilities(currentDomains, currentTools []string, gap UpgradeGap) (domains, tools []string) {
	domains = appendDeduped(currentDomains, gap.AddDomains)
	tools = appendDeduped(currentTools, gap.AddTools)
	return domains, tools
}

// ---------------------------------------------------------------------------
// Decomposition result validation
// ---------------------------------------------------------------------------

// PlanStep is one node of the decomposed step DAG the planner persists.
// The LLM decompose (or a degraded single-step fallback) produces these;
// they are validated before any write lands.
type PlanStep struct {
	// Key is a stable local id used only to wire dependsOn within the
	// decompose result (e.g. "s1"). The persisted step id is content-
	// addressed by the caller; Key never leaves the planner.
	Key string
	// Title is the human-readable unit of work.
	Title string
	// Goal is the per-step goal text fed to the fitScore + composer.
	Goal string
	// DependsOn lists the Keys of steps this one depends on (DAG in-edges).
	DependsOn []string
	// RoleHint optionally names a seed-role slug the decompose suggests.
	RoleHint string
}

// DecomposeResult is the validated output of the decompose step.
type DecomposeResult struct {
	Steps []PlanStep
}

// ValidateDecompose checks a raw decompose result is a well-formed DAG:
//   - at least one step;
//   - unique, non-empty keys;
//   - non-empty titles;
//   - every dependsOn references a known key (no dangling edges);
//   - no self-dependency;
//   - acyclic (a cycle would deadlock the reconciler -- no step ever
//     becomes ready).
//
// Returns the cleaned steps (trimmed, self-deps dropped) on success, or a
// descriptive error the caller surfaces (and falls back to a single-step
// plan). Pure -- the whole DAG check is in-memory.
func ValidateDecompose(steps []PlanStep) (DecomposeResult, error) {
	if len(steps) == 0 {
		return DecomposeResult{}, fmt.Errorf("decompose produced no steps")
	}
	keys := make(map[string]struct{}, len(steps))
	cleaned := make([]PlanStep, 0, len(steps))
	for i, s := range steps {
		key := strings.TrimSpace(s.Key)
		if key == "" {
			return DecomposeResult{}, fmt.Errorf("step %d has empty key", i)
		}
		if _, dup := keys[key]; dup {
			return DecomposeResult{}, fmt.Errorf("duplicate step key %q", key)
		}
		keys[key] = struct{}{}
		if strings.TrimSpace(s.Title) == "" {
			return DecomposeResult{}, fmt.Errorf("step %q has empty title", key)
		}
		cs := PlanStep{
			Key:      key,
			Title:    strings.TrimSpace(s.Title),
			Goal:     strings.TrimSpace(s.Goal),
			RoleHint: strings.TrimSpace(s.RoleHint),
		}
		cleaned = append(cleaned, cs)
	}
	// Second pass: validate + clean dependsOn now that every key is known.
	for i := range cleaned {
		deps := make([]string, 0, len(steps[i].DependsOn))
		seen := make(map[string]struct{})
		for _, dep := range steps[i].DependsOn {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			if dep == cleaned[i].Key {
				return DecomposeResult{}, fmt.Errorf("step %q depends on itself", cleaned[i].Key)
			}
			if _, ok := keys[dep]; !ok {
				return DecomposeResult{}, fmt.Errorf("step %q depends on unknown key %q", cleaned[i].Key, dep)
			}
			if _, dup := seen[dep]; dup {
				continue
			}
			seen[dep] = struct{}{}
			deps = append(deps, dep)
		}
		cleaned[i].DependsOn = deps
	}
	if cyclic, cycleKey := hasCycle(cleaned); cyclic {
		return DecomposeResult{}, fmt.Errorf("decompose DAG has a cycle through step %q", cycleKey)
	}
	return DecomposeResult{Steps: cleaned}, nil
}

// hasCycle reports whether the dependsOn graph over steps contains a
// cycle, returning a key on the cycle when so. Standard DFS three-color
// walk. dependsOn edges point from a step to its prerequisites.
func hasCycle(steps []PlanStep) (bool, string) {
	byKey := make(map[string]PlanStep, len(steps))
	for _, s := range steps {
		byKey[s.Key] = s
	}
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS stack
		black = 2 // fully explored
	)
	color := make(map[string]int, len(steps))
	var visit func(key string) (bool, string)
	visit = func(key string) (bool, string) {
		color[key] = gray
		for _, dep := range byKey[key].DependsOn {
			switch color[dep] {
			case gray:
				return true, dep
			case white:
				if cyc, k := visit(dep); cyc {
					return true, k
				}
			}
		}
		color[key] = black
		return false, ""
	}
	for _, s := range steps {
		if color[s.Key] == white {
			if cyc, k := visit(s.Key); cyc {
				return true, k
			}
		}
	}
	return false, ""
}

// SingleStepFallback builds a one-step plan from the raw goal. Used when
// the LLM decompose is unavailable or its result fails validation -- the
// plan still runs (as one big step) rather than failing outright.
func SingleStepFallback(goal string) DecomposeResult {
	goal = strings.TrimSpace(goal)
	title := truncateText(goal, 120)
	if title == "" {
		title = "complete the goal"
	}
	return DecomposeResult{
		Steps: []PlanStep{{
			Key:   "s1",
			Title: title,
			Goal:  goal,
		}},
	}
}

// ---------------------------------------------------------------------------
// small string helpers
// ---------------------------------------------------------------------------

func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func appendDeduped(base, extra []string) []string {
	out := dedupStrings(base)
	seen := make(map[string]struct{}, len(out))
	for _, s := range out {
		seen[s] = struct{}{}
	}
	for _, s := range extra {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func truncateText(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return strings.TrimSpace(s[:max-3]) + "..."
}
