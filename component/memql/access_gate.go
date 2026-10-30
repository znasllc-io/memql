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
// # Bound to the package that declared it (D5)
//
// Gate names are short ("dashboard", "overview") and will collide between
// products, and so will deployable names and construct names. So a gate names
// its deployable AND records the DSL domain that declared it: the domain of
// the concept the construct is bound to, taken the way a shopper extension's
// declaring domain is taken. The bff reads the domains the site's package ships
// with the site, on every call, and asks this registry only about those. A
// construct another product declared is not in reach however its deployable
// happens to be spelled, and a package whose domains are unknown -- an empty
// list -- reaches nothing, so an unknown package fails closed.
//
// This is not a defense against a hostile author: DSL is already cluster-wide
// authority. It stops one product's code reaching another's operations by
// accident, and stops a colliding name from closing somebody else's gate -- a
// duplicate is a fault inside ONE domain, and the same name in another domain
// is that product's own declaration.
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
// The call it renders is Call(): namespace.name, the engine's own qualified
// form. The engine resolves a bare name CLUSTER-WIDE and refuses an ambiguous
// one, so a bare call would let a same-named construct anywhere -- a sibling
// that is disabled, a core construct a later release adds -- break a product's
// gate, and two products' gates that share a name could not both be called at
// all. A qualified call names the one construct the entry was made from.
//
// The qualified form has one limit, and the load enforces it. A nested
// directory's namespace is a path ("desk/sub"), the call syntax reads the
// slash as division, and no spelling reaches the construct, so a gated construct
// lives in the top-level directory of its domain.
//
// What a short name still has to pick out is ONE entry, because a visitor's
// request names a construct without a namespace. Two gated constructs sharing a
// name under one deployable in one domain are refused at load for that reason.
// The same name in two domains is a legal registry state -- two products -- and
// each answers for its own domain; a package that ships BOTH domains cannot tell
// them apart by that short name, so the lookup opens neither rather than guess.
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
	// Construct is the construct's name as authored: the short name a visitor's
	// request is matched against. The call the handler renders is Call(), which
	// comes from here and never from the request.
	Construct string
	// Kind is AccessKindQuery or AccessKindMutation, which decides the call's
	// keyword (AccessCallPrefix).
	Kind string
	// Domain is the DSL domain of the concept the construct is bound to, which
	// is the package that ships it: the lookup considers an entry only for a
	// site whose package ships this domain. Taken from the binding and not
	// from the file the construct is written in, as a shopper extension's
	// declaring domain is.
	Domain string
	// Namespace is the namespace the engine resolves the construct in: the
	// directory its file is in, which is what the function registry keys it by.
	// It is the file's and not the concept's -- Domain is the concept's, and the
	// two differ when a construct is written in one directory and bound to a
	// concept of another domain -- and Call names the construct through it.
	Namespace string
	// Args are the argument names a caller may send, in declaration order: every
	// name the construct declares except the reserved ones. The handler refuses
	// any other key BEFORE it renders anything.
	Args []string
	// Stamped are the reserved names the construct declares, in declaration
	// order: the names the handler stamps, last.
	Stamped []string
	// File is the construct's file in the DSL tree, relative to the DSL root
	// (desk/queries.memql), spelled as a LoadReport skip spells it. The
	// handler never reads it: package analysis does, to point an author at the
	// file a refusal about this gate is about.
	File string
}

// Call is the exact text the handler renders before the arguments: the call
// keyword and the construct's qualified name, namespace.name, which is the form
// the engine itself uses (`query platform.activeEmbedderBinding()`).
//
// QUALIFIED, because the engine resolves a bare name cluster-wide and refuses an
// ambiguous one. A bare call would let a same-named construct anywhere -- a
// sibling that is disabled, a core construct a later release adds -- break a
// product's gate. The qualified form names the one construct this entry was made
// from, by the registry's own key for it. An entry with no namespace, which only
// a hand-built one has, renders bare.
func (e AccessGateEntry) Call() string {
	return AccessCallPrefix(e.Kind) + QualifyConstruct(e.Namespace, e.Construct)
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

// accessGateSlot identifies one gated construct: the domain that declared it,
// the deployable it names and its own name. Two products' constructs that share
// a deployable name and a construct name differ in the first field, so neither
// can overwrite or close the other.
type accessGateSlot struct{ domain, deployable, construct string }

var (
	accessGateMu sync.RWMutex
	accessGates  = map[accessGateSlot]AccessGateEntry{}
)

// accessDomainSet is a package's domains as a set. A blank names no domain and
// must never match an entry that lacks one, so it is dropped.
func accessDomainSet(domains []string) map[string]struct{} {
	set := make(map[string]struct{}, len(domains))
	for _, domain := range domains {
		if domain != "" {
			set[domain] = struct{}{}
		}
	}
	return set
}

// AccessGateFor resolves one request: the construct a deployable's gate reaches
// among the DSL domains the site's package ships, or nil.
//
// domains are those the caller read with the site (package.dslDomains). An
// empty list reaches nothing, so a package whose domains are not known fails
// closed rather than falling back to every domain.
//
// NIL ON ANY MISS, and the misses are deliberately one answer: a construct
// nobody declared, one declared on another gate, one declared on another
// deployable, and one declared by a domain the package does not ship. The
// handler turns all of them into the same 404, so a holder of one gate's code
// learns nothing about what another gate, or another product, reaches. Matching
// is exact -- no trimming, no folding -- because a name that is "nearly" a
// declared one is a request for something nobody declared.
//
// A name two of the package's domains both declare under the deployable is a
// miss too, whichever gate is asked for: the request names the construct
// without a namespace, so the name alone cannot say which of the two it means,
// and picking one here would be a guess presented as an answer.
func AccessGateFor(deployable, gate, construct string, domains []string) *AccessGateEntry {
	accessGateMu.RLock()
	defer accessGateMu.RUnlock()

	var found AccessGateEntry
	declared := 0
	for domain := range accessDomainSet(domains) {
		if entry, ok := accessGates[accessGateSlot{domain, deployable, construct}]; ok {
			found = entry
			declared++
		}
	}
	if declared != 1 || found.Gate != gate {
		return nil
	}
	out := found.clone()
	return &out
}

// AccessGatesFor lists the gates a deployable declares in the DSL domains a
// package ships, sorted by gate, for the operator surface: a code can only be
// minted for a gate that appears here. A deployable that declares none, an
// empty name and an empty list of domains all yield an empty list.
//
// It reads the same domains AccessGateFor does and nothing else. A gate another
// domain declares under the same deployable, with the same name, is not merged
// into it, and neither is what that domain's constructs stamp. It offers only
// what AccessGateFor can answer for: a name two of the package's domains both
// declare is left out, so a gate the operator can pick is a gate a call can
// reach.
func AccessGatesFor(deployable string, domains []string) []AccessGateSummary {
	out := []AccessGateSummary{}
	inPackage := accessDomainSet(domains)
	if deployable == "" || len(inPackage) == 0 {
		return out
	}

	accessGateMu.RLock()
	byName := map[string][]AccessGateEntry{}
	for slot, entry := range accessGates {
		if slot.deployable != deployable {
			continue
		}
		if _, shipped := inPackage[slot.domain]; !shipped {
			continue
		}
		byName[slot.construct] = append(byName[slot.construct], entry)
	}
	byGate := map[string]*AccessGateSummary{}
	for name, entries := range byName {
		if len(entries) != 1 {
			continue
		}
		entry := entries[0]
		summary := byGate[entry.Gate]
		if summary == nil {
			summary = &AccessGateSummary{Gate: entry.Gate}
			byGate[entry.Gate] = summary
		}
		summary.Constructs = append(summary.Constructs, name)
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

// AccessGateEntries lists every gated construct the node serves, whatever its
// domain, sorted by deployable, gate, construct and domain, as copies. A request
// never reads it, because a request is always scoped to a site's package.
//
// Package analysis does not read it either: it validates a CANDIDATE, which is
// never published, so the node's own registry cannot hold the candidate's gates.
// It takes them from the load that built them (AnalyzePackageDSL's AccessGates).
func AccessGateEntries() []AccessGateEntry {
	accessGateMu.RLock()
	defer accessGateMu.RUnlock()
	return accessGateList(accessGates)
}

// accessGateList is a view's entries as copies, in the order AccessGateEntries
// documents. One definition of that order, because the registry a node serves
// and the view a load returns (loadAccessGates) are the same list from two
// places, and two orders would make "the same gates" depend on which was asked.
func accessGateList(view map[accessGateSlot]AccessGateEntry) []AccessGateEntry {
	out := make([]AccessGateEntry, 0, len(view))
	for _, entry := range view {
		out = append(out, entry.clone())
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Deployable != b.Deployable {
			return a.Deployable < b.Deployable
		}
		if a.Gate != b.Gate {
			return a.Gate < b.Gate
		}
		if a.Construct != b.Construct {
			return a.Construct < b.Construct
		}
		return a.Domain < b.Domain
	})
	return out
}

// AccessCallPrefix is the keyword a gated construct's invocation carries, or ""
// for a mutation -- `query desk.deskBriefs(...)` versus
// `desk.deskNote(...)`. AccessGateEntry.Call joins it to the qualified
// name; the handler renders that and not this on its own.
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
func publishAccessGates(next map[accessGateSlot]AccessGateEntry) {
	accessGateMu.Lock()
	defer accessGateMu.Unlock()
	accessGates = next
}

// SetAccessGatesForTest replaces the registry with exactly these entries, as
// given and unvalidated; with none it empties it. Test seam only: on a node the
// boot pass is the one writer, and it validates. It exists so a package that
// consumes the registry (the access handler, the operator integration) can
// stand up a gate without loading a whole DSL tree.
//
// An entry needs its Domain, as on a node: a lookup considers only the domains
// it is given, so an entry with none is one no site's package can reach.
func SetAccessGatesForTest(entries ...AccessGateEntry) {
	next := make(map[accessGateSlot]AccessGateEntry, len(entries))
	for _, entry := range entries {
		next[accessGateSlot{entry.Domain, entry.Deployable, entry.Construct}] = entry.clone()
	}
	publishAccessGates(next)
}
