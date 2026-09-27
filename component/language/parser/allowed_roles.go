package parser

// allowed_roles.go -- carrying a deprecated @allowedRoles(...) across
// (memql#5438).
//
// @allowedRoles compared ONE role string whose meaning depended on who was
// calling: the acting agent's role (assistant / specialist) in an agent's tool
// loop, the person's cluster role over MCP. So a list was one of two things,
// and each has its own annotation now:
//
//   - a list of AGENT roles becomes @requiresAgentRole with the same values;
//   - a list of PERSON roles becomes @requiresRank("<the list's lowest rung>"),
//     which admits that rung and everything ranked above it.
//
// The decision is made here, once, for both places that make it --
// `memqlmigrate --rewrite=allowed-roles` over a tree and the language server's
// quick fix over one use -- and it is made from the two vocabularies'
// DECLARATIONS rather than from lists kept beside them: which values are agent
// roles is the v1:agents:agent concept's `role` enum, and where a person role
// sits is the rank dsl/rbac seeds it at (RoleVocabularyFromDSL). The parser
// holds no copy of either.
//
// A list is carried across only when the result means what the list meant on
// the ladder it was written against, and is otherwise LEFT, with the reason,
// for its author -- never guessed:
//
//   - a list mixing agent roles and person roles gated two axes at once, which
//     is the defect the deprecation exists to end; its author decides which
//     gate the tool needs (it may need both);
//   - a list naming a value neither vocabulary knows -- a roleSlug such as
//     "system-planner", a custom role, a typo -- has no answer to compute;
//   - a person list that skips a rung ranked above its lowest one ("owner",
//     "writer" but not admin or developer) is not a floor, and @requiresRank
//     cannot say it.
//
// The one deliberate difference in meaning is the one the owner-approved
// design records: a rank floor admits a CUSTOM role ranked at or above it,
// which a list of slugs could not name.

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
)

// The two declarations a RoleVocabulary is read from, as paths in a DSL tree
// whose domains are its top-level directories (the embedded tree's layout).
const (
	AgentConceptsPath = "agents/concepts.memql"
	RoleSeedsPath     = "rbac/seeds.memql"
)

// RoleVocabularyFromTree reads a RoleVocabulary from the two declarations in
// fsys (AgentConceptsPath, RoleSeedsPath) -- the embedded tree for every tool
// that ships with the engine, since both domains are core and a bundle cannot
// redeclare them.
func RoleVocabularyFromTree(fsys fs.FS) (*RoleVocabulary, error) {
	concepts, err := fs.ReadFile(fsys, AgentConceptsPath)
	if err != nil {
		return nil, err
	}
	seeds, err := fs.ReadFile(fsys, RoleSeedsPath)
	if err != nil {
		return nil, err
	}
	return RoleVocabularyFromDSL(string(concepts), string(seeds))
}

// RoleVocabulary is what carrying an @allowedRoles list across needs to know:
// the agent roles, and the person-role ladder with its aliases.
type RoleVocabulary struct {
	agentRoles map[string]bool
	// rank maps a role slug or alias to its rank; rung maps it to the slug of
	// the rung it names (an alias to its role, a slug to itself).
	rank map[string]int
	rung map[string]string
}

// RoleVocabularyFromDSL reads the two vocabularies from their declarations:
// agentConcepts is a source declaring `concept agent` with a `role` enum field
// (dsl/agents/concepts.memql), and rbacSeeds a source seeding the ladder's
// `seed role` rows with their slug, rank and aliases (dsl/rbac/seeds.memql).
func RoleVocabularyFromDSL(agentConcepts, rbacSeeds string) (*RoleVocabulary, error) {
	v := &RoleVocabulary{agentRoles: map[string]bool{}, rank: map[string]int{}, rung: map[string]string{}}

	concepts, err := parseVocabularySource(agentConcepts)
	if err != nil {
		return nil, fmt.Errorf("reading the agent roles: %w", err)
	}
	for _, d := range concepts.Definitions {
		c, ok := d.(*ast.ConceptDecl)
		if !ok || c.Name != "agent" {
			continue
		}
		for _, p := range c.Properties {
			if p.Name == "role" && p.Type != nil && p.Type.Kind == "enum" {
				for _, value := range p.Type.EnumValues {
					v.agentRoles[value] = true
				}
			}
		}
	}
	if len(v.agentRoles) == 0 {
		return nil, fmt.Errorf("reading the agent roles: the source declares no `concept agent` with a `role` enum")
	}

	seeds, err := parseVocabularySource(rbacSeeds)
	if err != nil {
		return nil, fmt.Errorf("reading the role ladder: %w", err)
	}
	for _, d := range seeds.Definitions {
		s, ok := d.(*ast.SeedDecl)
		if !ok || s.SignatureConcept != "role" || s.Body == nil {
			continue
		}
		slug := seedString(s.Body, "slug")
		rank := seedInt(s.Body, "rank")
		if slug == "" || rank <= 0 {
			continue
		}
		v.rank[slug], v.rung[slug] = rank, slug
		if aliases, ok := s.Body.Fields["aliases"]; ok && aliases != nil {
			for _, alias := range aliases.StringArray {
				v.rank[alias], v.rung[alias] = rank, slug
			}
		}
	}
	if len(v.rank) == 0 {
		return nil, fmt.Errorf("reading the role ladder: the source seeds no `seed role` with a slug and a rank")
	}
	return v, nil
}

// parseVocabularySource parses a whole DSL source the way the loaders read one.
func parseVocabularySource(src string) (*ast.File, error) {
	norm, err := NormaliseAll(src)
	if err != nil {
		return nil, err
	}
	return ParseFile(norm)
}

func seedString(b *ast.SeedBlock, key string) string {
	if f, ok := b.Fields[key]; ok && f != nil && f.Kind == ast.SeedValueString {
		return strings.TrimSpace(f.String)
	}
	return ""
}

func seedInt(b *ast.SeedBlock, key string) int {
	if f, ok := b.Fields[key]; ok && f != nil && f.Kind == ast.SeedValueInt {
		return int(f.Int)
	}
	return 0
}

// AgentRoles is the agent-role vocabulary, sorted.
func (v *RoleVocabulary) AgentRoles() []string {
	out := make([]string, 0, len(v.agentRoles))
	for r := range v.agentRoles {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// ladderSlugs is the ladder's rungs by their slug, lowest rank first.
func (v *RoleVocabulary) ladderSlugs() []string {
	var out []string
	for name, slug := range v.rung {
		if name == slug {
			out = append(out, slug)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if v.rank[out[i]] != v.rank[out[j]] {
			return v.rank[out[i]] < v.rank[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// AllowedRolesRewrite is the decision on one @allowedRoles list: the
// annotation to write in its place, or -- when Annotation is empty -- why the
// list is left for its author.
type AllowedRolesRewrite struct {
	Annotation string
	Reason     string
}

// RewriteAllowedRoles decides what the @allowedRoles list values becomes. See
// the file comment for when a list is left rather than rewritten.
func (v *RoleVocabulary) RewriteAllowedRoles(values []string) AllowedRolesRewrite {
	if v == nil {
		return AllowedRolesRewrite{Reason: "no role vocabulary was read, so no value can be classified"}
	}
	if len(values) == 0 {
		return AllowedRolesRewrite{Reason: "the list names no role"}
	}
	var agents, persons, unknown []string
	for _, value := range values {
		isAgent, isPerson := v.agentRoles[value], v.rank[value] > 0
		switch {
		case isAgent && isPerson:
			return AllowedRolesRewrite{Reason: fmt.Sprintf("%q is both an agent role and a person role, so the list cannot say which gate it meant", value)}
		case isAgent:
			agents = append(agents, value)
		case isPerson:
			persons = append(persons, value)
		default:
			unknown = append(unknown, value)
		}
	}
	if len(unknown) > 0 {
		return AllowedRolesRewrite{Reason: fmt.Sprintf(
			"%s is neither an agent role (%s) nor a role on the ladder (%s); write @requiresAgentRole or @requiresRank by hand",
			quoteJoin(unknown), strings.Join(v.AgentRoles(), ", "), strings.Join(v.ladderSlugs(), ", "))}
	}
	if len(agents) > 0 && len(persons) > 0 {
		return AllowedRolesRewrite{Reason: fmt.Sprintf(
			"the list mixes agent roles (%s) with person roles (%s); a tool that needs both writes @requiresAgentRole and @requiresRank",
			quoteJoin(agents), quoteJoin(persons))}
	}
	if len(agents) > 0 {
		return AllowedRolesRewrite{Annotation: "@requiresAgentRole(" + quoteJoin(agents) + ")"}
	}

	// A person list: its floor is its lowest rung, and it must be a floor --
	// every rung ranked above the lowest named, or @requiresRank would admit
	// a role the list excluded.
	lowest := persons[0]
	named := map[string]bool{}
	for _, p := range persons {
		named[v.rung[p]] = true
		if v.rank[p] < v.rank[lowest] {
			lowest = p
		}
	}
	var skipped []string
	for _, slug := range v.ladderSlugs() {
		if v.rank[slug] > v.rank[lowest] && !named[slug] {
			skipped = append(skipped, slug)
		}
	}
	if len(skipped) > 0 {
		return AllowedRolesRewrite{Reason: fmt.Sprintf(
			"the list admits %q but not %s, ranked above it, so it is not a floor and @requiresRank(%q) would admit what it excluded",
			lowest, quoteJoin(skipped), lowest)}
	}
	return AllowedRolesRewrite{Annotation: fmt.Sprintf("@requiresRank(%q)", lowest)}
}

// quoteJoin renders values as an annotation's argument list: "a", "b".
func quoteJoin(values []string) string {
	quoted := make([]string, len(values))
	for i, s := range values {
		quoted[i] = QuoteString(s)
	}
	return strings.Join(quoted, ", ")
}

// AllowedRolesFinding is one @allowedRoles use a rewrite left as written, and
// why: what `memqlmigrate --rewrite=allowed-roles` reports for its author.
type AllowedRolesFinding struct {
	Line, Column int
	Text         string
	Reason       string
}

// RewriteAllowedRoles rewrites every @allowedRoles use in src that vocabulary
// can carry across exactly (DeprecatedUse.AllowedRolesReplacement), in place
// and touching nothing else, and returns the uses it left, in source order. It
// is the migration channel of the deprecated_allowed_roles window: lexical,
// so it runs over a file whatever else is wrong with it, and idempotent,
// because what it writes is not the form it rewrites.
func RewriteAllowedRoles(src string, vocabulary *RoleVocabulary) (string, []AllowedRolesFinding) {
	var (
		out      []rune
		findings []AllowedRolesFinding
		runes    = []rune(src)
		next     int
	)
	for _, s := range scanDeprecatedUses(src) {
		if s.use.Rule != ruleDeprecatedAllowedRoles {
			continue
		}
		decision := s.use.AllowedRolesReplacement(vocabulary)
		if decision.Annotation == "" {
			findings = append(findings, AllowedRolesFinding{Line: s.use.Line, Column: s.use.Column, Text: s.use.Text, Reason: decision.Reason})
			continue
		}
		out = append(out, runes[next:s.start]...)
		out = append(out, []rune(decision.Annotation)...)
		next = s.end
	}
	if out == nil {
		return src, findings
	}
	out = append(out, runes[next:]...)
	return string(out), findings
}
