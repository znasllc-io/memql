// restructure-dsl emits the new domain-first DSL tree alongside the
// old kind-first tree, per the 11 locked decisions in
// docs/dsl-import-model-refactor.md.
//
// This is Pass 1 of the restructure migration: additive emit. The
// old tree at dsl/v1/<kind>/v1/<domain>/<name>.memql stays put;
// the new tree is written to dsl/<domain>/<entity>.memql alongside
// it. The engine continues to load from the old paths until the
// loader cutover (Pass 2) lands.
//
// Output structure (per the design doc):
//
//	dsl/
//	├── cognition/
//	│   ├── space.memql            # concept + queries + mutations + shapes + specs
//	│   ├── participant.memql
//	│   ├── ... (one file per entity or entity-cluster)
//	│   ├── automations.memql      # all cognition automations + their logic
//	│   ├── builtins.memql         # @executor wrappers
//	│   ├── tools.memql            # SI-callable tool defs
//	│   └── prompts/<name>.{memql,tmpl}
//	├── copresent/ identity/ cluster/ platform/ knowledge/ data/ worker/ router/ memql/
//	├── common/
//	│   ├── shapes.memql           # trait + caller shapes
//	│   ├── specs.memql            # cross-concept specs
//	│   └── builtins.memql         # cross-cutting integration wrappers
//	├── providers/<vendor>.memql   # base + all models bundled per vendor
//	└── policies/{routing,bff}.memql
//
// Usage:
//
//	go run ./scripts/restructure-dsl                 # apply
//	go run ./scripts/restructure-dsl --dry-run       # report only
//	go run ./scripts/restructure-dsl --clean         # delete dsl/<domain>/* before emit
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	oldRoot = "dsl/v1"
	newRoot = "dsl"
)

var (
	useConceptRe  = regexp.MustCompile(`(?m)^@useConcept\(([^)]+)\)`)
	conceptDeclRe = regexp.MustCompile(`(?m)^concept[ \t]+([A-Za-z][A-Za-z0-9]*)[ \t]*\{`)
)

// clusterParent maps a sub-concept path-segment to its parent name.
// Sub-concepts get merged into the parent's entity file (Q3 signal 1).
var clusterParent = map[string]string{
	"cognition/space/context":        "space",
	"cognition/participant/presence": "participant",
	"cognition/turn/state":           "turn",
	"cognition/text/chunk":           "textChunk",
	"cognition/client/tool/request":  "clientTools",
	"cognition/client/tool/response": "clientTools",
	"copresent/guardrail/health":     "guardrailHealth",
	"copresent/unmet/capability":     "unmetCapability",
}

// bareNameCluster maps a bare concept name to its (domain, target) so
// queries that @useConcept(<bareName>) for a nested concept land in
// the correct entity file. The bare names come from the concept's
// trailing path segment, which is what @useConcept resolves against.
var bareNameCluster = map[[2]string]string{
	// (domain, bareName) -> target entity file (relative to newRoot)
	{"cognition", "context"}:    "cognition/space",
	{"cognition", "presence"}:   "cognition/participant",
	{"cognition", "state"}:      "cognition/turn",
	{"cognition", "chunk"}:      "cognition/textChunk",
	{"cognition", "request"}:    "cognition/clientTools",
	{"cognition", "response"}:   "cognition/clientTools",
	{"copresent", "health"}:     "copresent/guardrailHealth",
	{"copresent", "capability"}: "copresent/unmetCapability",
}

// parallelCluster maps groups of parallel-shape concepts (Q3 signal 2)
// to a single entity file in their domain.
var parallelCluster = map[string]string{
	"cognition/audioOverride":    "cognition/presence",
	"cognition/videoOverride":    "cognition/presence",
	"cognition/micState":         "cognition/presence",
	"cluster/cluster":            "cluster/topology",
	"cluster/database":           "cluster/topology",
	"cluster/identityProvider":   "cluster/topology",
	"platform/globalSecret":      "platform/secrets",
	"platform/partitionSecret":   "platform/secrets",
	"platform/globalVariable":    "platform/variables",
	"platform/partitionVariable": "platform/variables",
	"knowledge/spreadsheetRow":   "knowledge/items",
	"knowledge/imageRegion":      "knowledge/items",
	"knowledge/validationEvent":  "knowledge/items",
}

// crossDomainConceptRelocate moves concepts that today live in
// common/ to their natural domain (Q10).
var crossDomainConceptRelocate = map[string]string{
	"common/agent":           "copresent/agent",
	"common/documentChunk":   "knowledge/document", // cluster with document
	"common/knowledgeDomain": "knowledge/domain",
	"common/knowledgeBridge": "knowledge/domain", // cluster with domain
}

// crossCuttingBuiltins are integrations that are not domain-scoped
// and land in common/builtins.memql.
var crossCuttingBuiltins = map[string]bool{
	"embedding": true, "similarity": true, "files": true, "gcs": true,
	"email": true, "auth": true,
}

type Source struct {
	Path    string // path relative to old root
	Content string
}

type Target struct {
	Path    string // path relative to new root
	Sources []*Source
}

func main() {
	dryRun := flag.Bool("dry-run", false, "report changes without writing")
	clean := flag.Bool("clean", false, "delete dsl/<domain>/ subdirs before emit")
	flag.Parse()

	if *clean && !*dryRun {
		cleanNewTree()
	}

	sources, err := collectSources()
	if err != nil {
		die("collect: %v", err)
	}
	fmt.Printf("Read %d source files from %s\n", len(sources), oldRoot)

	targets := bucket(sources)
	fmt.Printf("Bucketed into %d target files\n", len(targets))

	if *dryRun {
		reportDryRun(targets)
		return
	}

	for _, t := range targets {
		if err := writeTarget(t); err != nil {
			die("write %s: %v", t.Path, err)
		}
	}
	fmt.Printf("Wrote %d target files under %s/\n", len(targets), newRoot)
}

// collectSources walks the old tree and reads every .memql + .tmpl.
func collectSources() ([]*Source, error) {
	var out []*Source
	err := filepath.Walk(oldRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".memql") && !strings.HasSuffix(name, ".tmpl") {
			return nil
		}
		// Skip schema-reference files and READMEs. `_base.memql`
		// looks like a doc file by its underscore prefix but is
		// real content (provider vendor base declarations) -- DO
		// include it.
		if strings.HasPrefix(name, "_") && name != "_base.memql" {
			return nil
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(oldRoot, p)
		if err != nil {
			return err
		}
		out = append(out, &Source{Path: filepath.ToSlash(rel), Content: string(content)})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

// bucket assigns each source to a target file in the new tree.
func bucket(sources []*Source) []*Target {
	byPath := make(map[string]*Target)
	getTarget := func(path string) *Target {
		if t, ok := byPath[path]; ok {
			return t
		}
		t := &Target{Path: path}
		byPath[path] = t
		return t
	}

	for _, src := range sources {
		path := classify(src)
		t := getTarget(path)
		t.Sources = append(t.Sources, src)
	}

	var out []*Target
	for _, t := range byPath {
		// Stable source ordering inside each target: concepts first,
		// then specs, then shapes, then queries, then mutations,
		// then everything else. Alphabetical within each bucket.
		sort.SliceStable(t.Sources, func(i, j int) bool {
			return classifyOrder(t.Sources[i]) < classifyOrder(t.Sources[j]) ||
				(classifyOrder(t.Sources[i]) == classifyOrder(t.Sources[j]) &&
					t.Sources[i].Path < t.Sources[j].Path)
		})
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// classify returns the target path (relative to new root) for a source.
func classify(src *Source) string {
	parts := strings.Split(src.Path, "/")
	if len(parts) == 0 {
		return ""
	}
	kind := parts[0]

	switch kind {
	case "concepts":
		return classifyConcept(src, parts)
	case "queries", "mutations", "shapes", "specs":
		return classifyFunctionLike(src, parts, kind)
	case "automations":
		return classifyAutomation(src, parts)
	case "logic":
		return classifyLogic(src, parts)
	case "tools":
		return classifyTool(src, parts)
	case "prompts":
		return classifyPrompt(src, parts)
	case "providers":
		return classifyProvider(src, parts)
	case "policies":
		return classifyPolicy(src, parts)
	default:
		// Unknown kind -- park in common/_unsorted.memql for review.
		return newRoot + "/common/_unsorted.memql"
	}
}

// classifyConcept maps concepts/v1/<ns>/<name>/[<sub>/]concept.memql
// to the appropriate domain entity file, accounting for clusters
// and cross-domain relocations.
func classifyConcept(src *Source, parts []string) string {
	// parts = ["concepts", "v1", <ns>, <name>, ...optional sub..., "concept.memql"]
	if len(parts) < 4 {
		return newRoot + "/common/_unsorted.memql"
	}
	domain := parts[2]
	conceptParts := parts[3 : len(parts)-1] // strip "concepts" "v1" and "concept.memql"
	conceptPath := domain + "/" + strings.Join(conceptParts, "/")

	// Apply cross-domain relocation first.
	if relocated, ok := crossDomainConceptRelocate[conceptPath]; ok {
		return newRoot + "/" + relocated + ".memql"
	}

	// Cluster sub-concepts under parent (signal 1).
	if parent, ok := clusterParent[conceptPath]; ok {
		return newRoot + "/" + domain + "/" + parent + ".memql"
	}

	// Parallel-cluster groups (signal 2).
	if grouped, ok := parallelCluster[conceptPath]; ok {
		return newRoot + "/" + grouped + ".memql"
	}

	// Default: one concept per file.
	name := conceptParts[0]
	if len(conceptParts) > 1 {
		// Sub-concept with no explicit cluster mapping -- camelCase
		// the sub-path so e.g. participant/presence -> participantPresence.
		// (Should be rare; the explicit map above catches the common cases.)
		name = conceptParts[0] + capitalize(conceptParts[1])
		for _, p := range conceptParts[2:] {
			name += capitalize(p)
		}
	}
	return newRoot + "/" + domain + "/" + name + ".memql"
}

// classifyFunctionLike maps queries / mutations / shapes / specs
// to their entity file based on @useConcept (or trait-shape ref for
// cross-concept specs).
func classifyFunctionLike(src *Source, parts []string, kind string) string {
	// parts = ["queries", "v1", <ns>, ...path..., "<name>.memql"]
	if len(parts) < 4 {
		return newRoot + "/common/_unsorted.memql"
	}
	domain := parts[2]

	// Builtins under <kind>/v1/<ns>/builtin/<name>.memql.
	if len(parts) >= 5 && parts[3] == "builtin" {
		return newRoot + "/" + domain + "/builtins.memql"
	}

	// Specs and shapes that live under <kind>/v1/common/ are
	// cross-concept by definition -> common/specs.memql / common/shapes.memql.
	// Same for files using the `trait NAME { ... }` struct form or
	// explicit @useShape pointing at a trait shape.
	if kind == "specs" {
		if domain == "common" {
			return newRoot + "/common/specs.memql"
		}
		if strings.Contains(src.Content, "\ntrait ") || strings.HasPrefix(src.Content, "trait ") {
			return newRoot + "/common/specs.memql"
		}
		if strings.Contains(src.Content, "@useShape(") {
			matches := regexp.MustCompile(`@useShape\(([^)]+)\)`).FindStringSubmatch(src.Content)
			if len(matches) > 1 && strings.HasSuffix(strings.TrimSpace(matches[1]), "Trait") {
				return newRoot + "/common/specs.memql"
			}
		}
	}
	if kind == "shapes" && domain == "common" {
		return newRoot + "/common/shapes.memql"
	}

	// Generic case: bucket by @useConcept.
	conceptName := extractFirstUseConcept(src.Content)
	if conceptName != "" {
		// Bare-name lookup for sub-concept clusters (the bare name
		// resolves against the trailing segment of a nested concept
		// path; redirect to the entity file the parent landed in).
		if target, ok := bareNameCluster[[2]string{domain, conceptName}]; ok {
			return newRoot + "/" + target + ".memql"
		}
		conceptPath := domain + "/" + conceptName
		if relocated, ok := crossDomainConceptRelocate[conceptPath]; ok {
			return newRoot + "/" + relocated + ".memql"
		}
		if parent, ok := clusterParent[conceptPath]; ok {
			return newRoot + "/" + domain + "/" + parent + ".memql"
		}
		if grouped, ok := parallelCluster[conceptPath]; ok {
			return newRoot + "/" + grouped + ".memql"
		}
		return newRoot + "/" + domain + "/" + conceptName + ".memql"
	}

	// No @useConcept binding (cross-concept or introspective function).
	// Park in <domain>/<kind>Misc.memql for the user to disposition.
	return newRoot + "/" + domain + "/" + kind + "Misc.memql"
}

// classifyAutomation: automations/v1/<ns>/<name>/automation.memql
// -> <ns>/automations.memql.
func classifyAutomation(src *Source, parts []string) string {
	if len(parts) < 4 {
		return newRoot + "/common/_unsorted.memql"
	}
	domain := parts[2]
	return newRoot + "/" + domain + "/automations.memql"
}

// classifyLogic: logic/v1/<ns>/<name>.memql -> <ns>/automations.memql
// (logic blocks bundle with the automations that use them).
func classifyLogic(src *Source, parts []string) string {
	if len(parts) < 4 {
		return newRoot + "/common/_unsorted.memql"
	}
	domain := parts[2]
	return newRoot + "/" + domain + "/automations.memql"
}

// classifyTool: tools/v1/<ns>/<path>/<name>.memql -> <ns>/tools.memql.
//
// The `agent` and `claw` tool sub-domains are copresent product
// surfaces; relocate them under copresent/tools.memql.
func classifyTool(src *Source, parts []string) string {
	if len(parts) < 4 {
		return newRoot + "/common/_unsorted.memql"
	}
	domain := parts[2]
	switch domain {
	case "agent", "claw":
		domain = "copresent"
	}
	return newRoot + "/" + domain + "/tools.memql"
}

// classifyPrompt: prompts/v1/<ns>/<name>.{memql,tmpl}
// -> <ns>/prompts/<name>.{memql,tmpl}.
//
// The `agent` and `claw` prompt sub-domains are copresent product
// surfaces, not real domains; relocate them under copresent/prompts/.
func classifyPrompt(src *Source, parts []string) string {
	if len(parts) < 4 {
		return newRoot + "/common/_unsorted.memql"
	}
	domain := parts[2]
	switch domain {
	case "agent", "claw":
		domain = "copresent"
	case "common":
		// `prompts/v1/common/...` -- shared prompt scaffolds, keep in common.
		// (Rare; today there are no such files but the path exists.)
	}
	name := parts[len(parts)-1]
	return newRoot + "/" + domain + "/prompts/" + name
}

// classifyProvider: providers/v1/<vendor>/<file>.memql
// -> providers/<vendor>.memql.
func classifyProvider(src *Source, parts []string) string {
	if len(parts) < 4 {
		return newRoot + "/common/_unsorted.memql"
	}
	vendor := parts[2]
	return newRoot + "/providers/" + vendor + ".memql"
}

// classifyPolicy: policies/v1/<file>.memql -> policies/routing.memql.
// policies/bff/<area>/<file>.memql -> policies/bff.memql.
// policies/core/<area>/<file>.memql -> policies/core.memql.
func classifyPolicy(src *Source, parts []string) string {
	if len(parts) < 3 {
		return newRoot + "/common/_unsorted.memql"
	}
	tier := parts[1]
	switch tier {
	case "v1":
		return newRoot + "/policies/routing.memql"
	case "bff":
		return newRoot + "/policies/bff.memql"
	case "core":
		return newRoot + "/policies/core.memql"
	default:
		return newRoot + "/common/_unsorted.memql"
	}
}

// classifyOrder gives a sort key so the merged file reads logically:
// concept, then specs, then shapes, then queries, then mutations,
// then automations, then logic, then everything else.
func classifyOrder(src *Source) int {
	parts := strings.Split(src.Path, "/")
	if len(parts) == 0 {
		return 99
	}
	switch parts[0] {
	case "concepts":
		return 0
	case "specs":
		return 1
	case "shapes":
		return 2
	case "queries":
		return 3
	case "mutations":
		return 4
	case "automations":
		return 5
	case "logic":
		return 6
	case "tools":
		return 7
	case "policies":
		return 8
	case "providers":
		return 9
	case "prompts":
		return 10
	default:
		return 99
	}
}

// extractFirstUseConcept pulls the first concept name from a file's
// @useConcept(<name>) annotation (or the comma-separated head from
// @useConcept(a, b, c)).
func extractFirstUseConcept(content string) string {
	m := useConceptRe.FindStringSubmatch(content)
	if len(m) < 2 {
		return ""
	}
	first := strings.TrimSpace(strings.Split(m[1], ",")[0])
	return first
}

// importBlockRe matches an `import (...)` block at file top so
// scripts/add-imports-to-legacy's output can be stripped during
// consolidation. The new-tree files don't need those imports --
// they'd be invalid relative paths post-relocation, and the
// transitional state still relies on @useConcept resolution for
// concept references inside consolidated files.
var importBlockRe = regexp.MustCompile(`(?m)^import[ \t]*\([^)]*\)\s*`)

// stripImportBlocks removes any `import (...)` block from source.
// Used to clean legacy-file content before bundling into new-tree
// consolidated files where the relative paths would no longer
// resolve.
func stripImportBlocks(source string) string {
	return importBlockRe.ReplaceAllString(source, "")
}

// writeTarget emits one consolidated target file.
func writeTarget(t *Target) error {
	dir := filepath.Dir(t.Path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Templates (.tmpl) are copied verbatim, one per source.
	if strings.HasSuffix(t.Path, ".tmpl") {
		if len(t.Sources) != 1 {
			return fmt.Errorf("template %s has %d sources; expected 1", t.Path, len(t.Sources))
		}
		return os.WriteFile(t.Path, []byte(t.Sources[0].Content), 0644)
	}

	// .memql files: concatenate sources with section comments.
	var sb strings.Builder
	sb.WriteString("// Generated by scripts/restructure-dsl.\n")
	sb.WriteString("// Consolidates content from the legacy kind-first tree.\n")
	sb.WriteString("// Sources:\n")
	for _, s := range t.Sources {
		sb.WriteString("//   - " + s.Path + "\n")
	}
	sb.WriteString("\n")

	for i, s := range t.Sources {
		if i > 0 {
			sb.WriteString("\n// ---- " + s.Path + " ----\n\n")
		} else {
			sb.WriteString("// ---- " + s.Path + " ----\n\n")
		}
		// Strip any `import (...)` block the legacy file
		// accumulated -- the relative paths no longer resolve
		// from the consolidated file's location, and the
		// transitional concept resolution still works via
		// @useConcept trailing-segment match.
		content := stripImportBlocks(s.Content)
		sb.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			sb.WriteString("\n")
		}
	}

	return os.WriteFile(t.Path, []byte(sb.String()), 0644)
}

// cleanNewTree removes every domain directory we are about to emit
// into, so the new tree starts fresh on each run. Preserves dsl/v1/
// (legacy tree) AND any top-level .go files (embed.go, embed_test.go
// that exposes dsl.Tree() to the unified loader).
func cleanNewTree() {
	entries, err := os.ReadDir(newRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == "v1" {
			continue
		}
		if !e.IsDir() {
			// Preserve top-level files (embed.go etc.).
			continue
		}
		_ = os.RemoveAll(filepath.Join(newRoot, e.Name()))
	}
}

func reportDryRun(targets []*Target) {
	fmt.Printf("\n%d target files (dry run):\n", len(targets))
	for _, t := range targets {
		fmt.Printf("  %-60s <-  %d source(s)\n", t.Path, len(t.Sources))
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] = r[0] - 'a' + 'A'
	}
	return string(r)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(2)
}
