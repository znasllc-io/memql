// Package capability is the authoritative registry of the action-construct
// capability vocabulary and its side-effect classification (memql#2219,
// behavioral-constructs ADR §2.3 + §3 + §7).
//
// An authored action performs EXACTLY ONE external capability call. A
// capability verb lives in one of five namespaces:
//
//	fs.*           filesystem
//	shell.*        process execution
//	http.*         outbound HTTP
//	integration.*  a named integration backend (integration.<domain>.<fn>)
//	mcp.*          an MCP tool invocation
//
// Each capability carries a coarse sideEffectClass -- read / write / exec.
// CRUCIALLY the class is declared HERE, on the capability, NOT on the action:
// an authored or planner-generated action cannot spoof its own risk class by
// claiming `@sideEffect("read")` over a `shell.exec` capability. The action's
// `@sideEffect` annotation is reconciled against this registry at load
// (component/actions.DeclToAction); a mismatch is rejected.
//
// The same classification drives the call-graph contract's read-only-builtin
// rule (ADR §3 corollary): a builtin is reached from a query/logic only if its
// `@executor("integration.<domain>.<fn>")` resolves to a READ capability here;
// a side-effecting integration is reachable only through an action.
// ClassifierFromDir / ClassifierFromFS / DefaultClassifier scan a DSL tree, map
// each builtin name to its executor capability, and report whether it is
// side-effecting -- the classifier the whole-tree gate
// (dsl/callgraph_contract_test.go) and the authoring sandbox
// (component/memql/authoring_sandbox_crossref.go) inject.
//
// This package is a deliberate LEAF: it depends only on the standard library
// and the embedded DSL tree (github.com/znasllc-io/memql/dsl). It must NOT
// import component/language/parser or component/memql, so that component/memql
// can consume the classifier without an import cycle (component/actions itself
// is poisoned for that use -- it imports the parser, which imports
// component/memql).
package capability

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// Side-effect classes (ADR §7).
const (
	ClassRead  = "read"  // observes external state; no mutation
	ClassWrite = "write" // mutates external/auxiliary state
	ClassExec  = "exec"  // runs/dispatches an external process or invocation
)

// Namespaces is the closed capability vocabulary (ADR §2.3).
var Namespaces = map[string]bool{
	"fs":          true,
	"shell":       true,
	"http":        true,
	"integration": true,
	"mcp":         true,
}

// Namespace returns the capability's leading namespace segment (the text before
// the first '.'), e.g. "shell" for "shell.exec".
func Namespace(capability string) string {
	if i := strings.IndexByte(capability, '.'); i >= 0 {
		return capability[:i]
	}
	return capability
}

// ValidNamespace reports whether a capability's namespace is in the vocabulary.
func ValidNamespace(capability string) bool {
	return Namespaces[Namespace(capability)]
}

// fsReadVerbs / fsWriteVerbs split the filesystem verbs by effect.
var (
	fsReadVerbs = map[string]bool{
		"read": true, "readfile": true, "stat": true, "list": true,
		"exists": true, "glob": true, "readdir": true,
	}
	fsWriteVerbs = map[string]bool{
		"write": true, "writefile": true, "append": true, "remove": true,
		"delete": true, "mkdir": true, "rmdir": true, "copy": true,
		"move": true, "rename": true, "chmod": true, "truncate": true,
	}
	httpReadVerbs  = map[string]bool{"get": true, "head": true, "options": true}
	httpWriteVerbs = map[string]bool{"post": true, "put": true, "patch": true, "delete": true}
)

// integrationClass is the per-function audit (ADR §3): integration.<domain>.<fn>
// (the key omits the "integration." prefix). A read-only integration stays a
// builtin callable from queries/logic; a write/exec integration is reachable
// only through an action. Functions absent here are treated as UNCLASSIFIED
// (CapabilityClass known=false), which the builtin classifier reads
// conservatively as not-side-effecting -- so a newly added integration must be
// audited in to be enforced.
var integrationClass = map[string]string{
	// --- read: lookups, permission checks, pure compute, external reads ---
	"auth.resolveUser":                 ClassRead,
	"auth.checkPermission":             ClassRead,
	"rbac.governPrincipal":             ClassRead,
	"rbac.canCreatePrincipal":          ClassRead,
	"identity.validateScope":           ClassRead,
	"identity.resolveDelegation":       ClassRead,
	"deployversion.suggestNextVersion": ClassRead,
	"database.stats":                   ClassRead,
	"database.healthCheck":             ClassRead,
	"voice.resolve":                    ClassRead,
	"voice.pickForGender":              ClassRead,
	"timeutil.dateKeyInTimezone":       ClassRead,
	"similarity.similarTo":             ClassRead,
	"router.listPolicies":              ClassRead,
	"router.listModels":                ClassRead,
	"files.extractText":                ClassRead,
	"agentworker.status":               ClassRead,
	"agentworker.listWorkers":          ClassRead,
	"cognition.scoreUtterance":         ClassRead,
	"knowledge.webSearch":              ClassRead,
	"knowledge.fetchUrl":               ClassRead,
	"stt.transcribe":                   ClassRead,
	"harnessRecall.recall":             ClassRead,
	"harnessTrace.trace":               ClassRead,
	"actionSearch.searchActions":       ClassRead,
	"telephony.searchNumbers":          ClassRead,

	// --- write: mutates external/auxiliary state ---
	"storage.upload":                    ClassWrite,
	"library.editDocument":              ClassWrite,
	"library.editDocumentAsAssistant":   ClassWrite,
	"library.restoreDocumentVersion":    ClassWrite,
	"identity.createDelegation":         ClassWrite,
	"identity.revokeDelegation":         ClassWrite,
	"knowledge.ingest":                  ClassWrite,
	"knowledge.seedStandardDomains":     ClassWrite,
	"knowledge.seedDomainContent":       ClassWrite,
	"knowledge.seedAllDomainContent":    ClassWrite,
	"knowledge.embedChunk":              ClassWrite,
	"knowledge.embedDomainItems":        ClassWrite,
	"knowledge.augmentDomainGenerate":   ClassWrite,
	"knowledge.augmentDomainAnalyze":    ClassWrite,
	"knowledge.ensureKnowledgeBridge":   ClassWrite,
	"cognition.trackPresence":           ClassWrite,
	"telephony.buyNumber":               ClassWrite,
	"telephony.releaseNumber":           ClassWrite,
	"telephony.configureInboundNumber":  ClassWrite,
	"workbench.teardownDirectory":       ClassWrite,
	"agentworker.requestScope":          ClassWrite,
	"agents.produceArtifact":            ClassWrite,
	"agents.ensureForGoal":              ClassWrite,
	"openairealtime.createClientSecret": ClassWrite,
	// ADR §3 worked examples (not present in the current tree, kept so the
	// reference skeletons + planner few-shots classify):
	"github.tagRelease": ClassWrite,
	"slack.post":        ClassWrite,

	// --- exec: runs/dispatches an external process or invocation ---
	"workbench.dispatchHost":       ClassExec,
	"agentworker.dispatchHost":     ClassExec,
	"agentworker.dispatchComputer": ClassExec,
	"agents.invoke":                ClassExec,
	"agents.askSpecialist":         ClassExec,
	"agents.requestUserFeedback":   ClassExec,
	"training.trainAgent":          ClassExec,
	"training.trainAgentRetryStep": ClassExec,
	"telephony.placeCall":          ClassExec,
	"telephony.transferCall":       ClassExec,
	"telephony.sendDtmf":           ClassExec,
	"telephony.endCall":            ClassExec,
	"openaiVoice.synthesize":       ClassExec,
}

// CapabilityClass returns the authoritative sideEffectClass for a capability
// verb (e.g. "shell.exec" -> "exec", "fs.readFile" -> "read",
// "integration.github.tagRelease" -> "write"). known is false for an
// unrecognized capability the registry cannot classify.
func CapabilityClass(capability string) (class string, known bool) {
	ns := Namespace(capability)
	rest := ""
	if i := strings.IndexByte(capability, '.'); i >= 0 {
		rest = capability[i+1:]
	}
	switch ns {
	case "shell":
		return ClassExec, true
	case "mcp":
		// Invoking an MCP tool is an external invocation.
		return ClassExec, true
	case "fs":
		verb := strings.ToLower(rest)
		if i := strings.IndexByte(verb, '.'); i >= 0 {
			verb = verb[:i]
		}
		if fsReadVerbs[verb] {
			return ClassRead, true
		}
		if fsWriteVerbs[verb] {
			return ClassWrite, true
		}
		return "", false
	case "http":
		verb := strings.ToLower(rest)
		if i := strings.IndexByte(verb, '.'); i >= 0 {
			verb = verb[:i]
		}
		if httpReadVerbs[verb] {
			return ClassRead, true
		}
		if httpWriteVerbs[verb] {
			return ClassWrite, true
		}
		return "", false
	case "integration":
		if c, ok := integrationClass[rest]; ok {
			return c, true
		}
		return "", false
	default:
		return "", false
	}
}

// SideEffecting reports whether a capability's class is write or exec (i.e. it
// touches the outside world). An unknown capability is treated conservatively
// as not-side-effecting (false) so the gate is not tripped by an un-audited
// verb; audit it into integrationClass to enforce it.
func SideEffecting(capability string) bool {
	class, known := CapabilityClass(capability)
	return known && (class == ClassWrite || class == ClassExec)
}

// --- builtin -> executor scanning ------------------------------------------

var (
	builtinHeaderRE = regexp.MustCompile(`(?m)^[ \t]*builtin[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	executorRE      = regexp.MustCompile(`@executor\([ \t]*"([^"]*)"[ \t]*\)`)
)

// builtinExecutors scans a single .memql source and returns each builtin
// declaration's name -> its governing @executor capability. The governing
// executor is the LAST @executor annotation appearing between the previous
// builtin header (or the file start) and this builtin's header.
func builtinExecutors(source string) map[string]string {
	out := map[string]string{}
	headers := builtinHeaderRE.FindAllStringSubmatchIndex(source, -1)
	prev := 0
	for _, h := range headers {
		name := source[h[2]:h[3]]
		region := source[prev:h[0]]
		execs := executorRE.FindAllStringSubmatch(region, -1)
		if len(execs) > 0 {
			out[name] = execs[len(execs)-1][1]
		}
		prev = h[1]
	}
	return out
}

// Classifier reports whether a builtin (by name) is side-effecting.
type Classifier func(builtinName string) bool

// classifierFromExecutors builds a name->side-effecting classifier from a
// name->executor map using CapabilityClass.
func classifierFromExecutors(execByName map[string]string) Classifier {
	side := map[string]bool{}
	for name, ex := range execByName {
		side[name] = SideEffecting(ex)
	}
	return func(name string) bool { return side[name] }
}

// ClassifierFromFS scans a DSL tree (skipping underscore dirs, mirroring the
// engine walker), maps every builtin name to its executor's side-effect class,
// and returns the classifier.
func ClassifierFromFS(tree fs.FS) (Classifier, error) {
	execByName := map[string]string{}
	err := fs.WalkDir(tree, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if p != "." && strings.HasPrefix(d.Name(), "_") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".memql") || strings.HasPrefix(d.Name(), "_") {
			return nil
		}
		raw, rerr := fs.ReadFile(tree, p)
		if rerr != nil {
			return rerr
		}
		for name, ex := range builtinExecutors(string(raw)) {
			execByName[name] = ex
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return classifierFromExecutors(execByName), nil
}

// ClassifierFromDir is ClassifierFromFS over an on-disk DSL root.
func ClassifierFromDir(root string) (Classifier, error) {
	return ClassifierFromFS(os.DirFS(filepath.Clean(root)))
}

var (
	defaultOnce       sync.Once
	defaultClassifier Classifier
)

// DefaultClassifier returns a process-wide classifier built once from the
// embedded DSL tree. It never returns nil; on a (practically impossible) scan
// error it degrades to "nothing is side-effecting" so the authoring sandbox
// keeps working.
func DefaultClassifier() Classifier {
	defaultOnce.Do(func() {
		c, err := ClassifierFromFS(memqldsl.Tree())
		if err != nil || c == nil {
			c = func(string) bool { return false }
		}
		defaultClassifier = c
	})
	return defaultClassifier
}
