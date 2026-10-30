package memql

import (
	"sort"
	"sync"
)

// access_gate.go -- WHAT A SITE'S ACCESS CODE OPENS (design record
// 2026-10-29, section 7).
//
// # The sentence this file exists to make true
//
// A deployable can keep part of itself back until a visitor presents a code,
// and "part of itself" is a list of QUERIES AND MUTATIONS, not a path: the
// first version gates what the bff returns, because a static bundle is public
// by construction (D12). This registry is that list. A construct is in it
// because its author wrote @accessGate on it, and for no other reason.
//
// # Reach is declared on the construct, and checked where the call is built
//
// No credential in MemQL can be limited to named operations today
// (auth.ScopeAllows has no production caller), so a visitor's session cannot
// carry "may call these three constructs" and have the engine hold it to
// that. The check lives where the call is BUILT instead: the bff's access
// handler resolves the request in this registry for the site's deployable and
// the session's gate, and a request this registry does not answer is the same
// 404 as one for a construct that does not exist (D4). Deny by default, and a
// refusal teaches a stranger nothing.
//
// # Keyed by deployable (D5)
//
// Gate names are short ("dashboard", "overview") and will collide between
// products. The key is the deployable and the construct, and the handler
// refuses a construct whose deployable is not the site's own
// packageDeployableName, so one product's code cannot reach another product's
// operations. v1:platform:package records no DSL domains to scope by, which is
// why the annotation names its deployable instead of inferring it.
//
// # The construct's name, and what is allowed to be said about it
//
// The handler renders the call from THIS registry, never from the request: the
// request names a construct only to be matched against what is declared here.
// The same goes for argument names. dslCall writes them as raw text and
// JSON-encodes only the values, so a caller-supplied key is rendered as DSL,
// and the only defence is that none is ever rendered that was not declared.
// Args is that allow-list.
//
// Construct is the name as authored, and the engine resolves a bare name only
// when one namespace declares it. Two gated constructs sharing a name under
// one deployable are refused at load for exactly that reason; the same name
// under two deployables is a legal registry state, and a call for either only
// resolves while the engine can tell the two namespaces apart.
//
// # The registry is rebuilt on every load
//
// Like shopperExtensions and unlike a pack's forms: a pack declares once at
// init, a DSL domain loads during MemQLEngine.Init, so the view is rebuilt
// from the loaded constructs each time and a domain dropped from a bundle
// stops opening anything at the next boot. A construct that was refused at
// load is not in it, so MEMQL_DSL_ALLOW_SKIPS, the break-glass for strict
// boot, can never be the thing that opens one.

// The two kinds of construct a gate may reach.
const (
	AccessKindQuery    = "query"
	AccessKindMutation = "mutation"
)

// The four argument names the access handler stamps, last, on a construct that
// declares them. A caller sending one is refused, so a visitor can never choose
// which code, gate, subject or site a call runs as.
//
// They are how a product isolates data between codes: rows carry the stamped
// accessCodeId, or match accessSubject, and the construct's filter says so. A
// gated construct that reads rows with neither opens them to everyone holding
// any code for that gate, which its author is deciding deliberately by writing
// the annotation.
const (
	AccessArgSiteID  = "siteId"
	AccessArgCodeID  = "accessCodeId"
	AccessArgGate    = "accessGate"
	AccessArgSubject = "accessSubject"
)

// ReservedAccessArgs lists the stamped names. A construct that declares one
// must declare it as a string; a caller that sends one is refused.
var ReservedAccessArgs = []string{AccessArgSiteID, AccessArgCodeID, AccessArgGate, AccessArgSubject}

// reservedAccessArgSet is the same list as a set the load rules read. A copy,
// so a caller rewriting the exported slice cannot loosen what the load refuses.
var reservedAccessArgSet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(ReservedAccessArgs))
	for _, name := range ReservedAccessArgs {
		set[name] = struct{}{}
	}
	return set
}()

// AccessGateEntry is one gated construct, as the access handler needs it.
type AccessGateEntry struct {
	// Deployable and Gate are the declaration: the deployable the construct
	// belongs to and the gate whose codes reach it.
	Deployable string
	Gate       string
	// Construct is the construct's name as authored, and the name the handler
	// renders into the call. It comes from here and never from the request.
	Construct string
	// Kind is AccessKindQuery or AccessKindMutation, which decides the call's
	// keyword (AccessCallPrefix).
	Kind string
	// Args are the argument names a caller may send, in declaration order: every
	// name the construct declares except the reserved ones. The handler refuses
	// any other key BEFORE it renders anything.
	Args []string
	// Stamped are the reserved names the construct declares, in declaration
	// order: the names the handler stamps, last.
	Stamped []string
}

func (e AccessGateEntry) clone() AccessGateEntry {
	// A copy of each slice, so nothing a caller does to what it was handed
	// reaches the registry. Both are the allow-lists the handler trusts.
	e.Args = append([]string(nil), e.Args...)
	e.Stamped = append([]string(nil), e.Stamped...)
	return e
}

// AccessGateSummary is what the operator surface needs to know about one gate:
// that it exists, which constructs it reaches, and whether a code for it can
// carry a subject.
type AccessGateSummary struct {
	Gate string
	// UsesSubject is true when any construct on the gate declares
	// accessSubject, which is the only thing a code's subject is for. A gate
	// where it is false has nothing to scope by subject, and the operator
	// surface does not offer the field.
	UsesSubject bool
	// Constructs are the gate's construct names, sorted.
	Constructs []string
}

var (
	accessGateMu sync.RWMutex
	// accessGates is keyed by deployable and construct, because the handler's
	// question is "does this deployable declare this construct, and for which
	// gate" and a gate-keyed map would answer it with a scan. A deployable
	// name holds no "/" and a construct name is camelCase, so the key is
	// unambiguous.
	accessGates = map[string]AccessGateEntry{}
)

func accessGateKey(deployable, construct string) string {
	return deployable + "/" + construct
}

// AccessGateFor resolves one request: the construct a deployable's gate
// reaches, or nil.
//
// NIL ON ANY MISS, and the three misses are deliberately one answer: a
// construct nobody declared, one declared on another gate, and one declared on
// another deployable. The handler turns all three into the same 404, so a
// holder of one gate's code learns nothing about what another gate reaches.
// Matching is exact -- no trimming, no folding -- because a name that is
// "nearly" a declared one is a request for something nobody declared.
func AccessGateFor(deployable, gate, construct string) *AccessGateEntry {
	accessGateMu.RLock()
	entry, declared := accessGates[accessGateKey(deployable, construct)]
	accessGateMu.RUnlock()
	if !declared || entry.Gate != gate {
		return nil
	}
	out := entry.clone()
	return &out
}

// AccessGatesFor lists the gates a deployable declares, sorted by gate, for the
// operator surface: a code can only be minted for a gate that appears here.
// A deployable that declares none, or an empty name, yields an empty list.
func AccessGatesFor(deployable string) []AccessGateSummary {
	out := []AccessGateSummary{}
	if deployable == "" {
		return out
	}
	byGate := map[string]*AccessGateSummary{}
	accessGateMu.RLock()
	for _, entry := range accessGates {
		if entry.Deployable != deployable {
			continue
		}
		summary := byGate[entry.Gate]
		if summary == nil {
			summary = &AccessGateSummary{Gate: entry.Gate}
			byGate[entry.Gate] = summary
		}
		summary.Constructs = append(summary.Constructs, entry.Construct)
		for _, stamped := range entry.Stamped {
			if stamped == AccessArgSubject {
				summary.UsesSubject = true
			}
		}
	}
	accessGateMu.RUnlock()

	for _, summary := range byGate {
		sort.Strings(summary.Constructs)
		out = append(out, *summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Gate < out[j].Gate })
	return out
}

// AccessCallPrefix is the keyword a gated construct's invocation carries, or ""
// for a mutation -- `query deskBriefs(...)` versus `deskNote(...)`.
//
// HERE RATHER THAN IN THE HANDLER, because it is a property of the
// declaration, exactly as ShopperCallPrefix is.
func AccessCallPrefix(kind string) string {
	if kind == AccessKindQuery {
		return "query "
	}
	return ""
}

// publishAccessGates replaces the whole view atomically, so a request never
// sees half of a reload.
func publishAccessGates(next map[string]AccessGateEntry) {
	accessGateMu.Lock()
	defer accessGateMu.Unlock()
	accessGates = next
}

// SetAccessGatesForTest replaces the registry with exactly these entries, as
// given and unvalidated; with none it empties it. Test seam only: on a node the
// boot pass is the one writer, and it validates. It exists so a package that
// consumes the registry (the access handler, the operator integration) can
// stand up a gate without loading a whole DSL tree.
func SetAccessGatesForTest(entries ...AccessGateEntry) {
	next := make(map[string]AccessGateEntry, len(entries))
	for _, entry := range entries {
		next[accessGateKey(entry.Deployable, entry.Construct)] = entry.clone()
	}
	publishAccessGates(next)
}
