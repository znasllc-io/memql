package dsl

import (
	"embed"
	"io/fs"
	"strings"
)

// Source is the layer of the DSL tree a file was served from.
//
// It is what the engine knows about WHERE a construct came from, and it is
// decided by the tree that served the file -- never by anything the file says,
// and never by a name its author chose. A domain directory's name is chosen by
// whoever wrote the domain, so it cannot be the answer; which layer of the
// layered view holds the directory can.
//
// The zero value is SourceUnknown, which is not the engine: a tree this package
// did not build -- a test's map, a migration's rewrite, a caller's bundle --
// carries no claim to be the engine's own.
type Source uint8

const (
	// SourceUnknown: the file was not served by a tree this package built.
	SourceUnknown Source = iota
	// SourceEngine: the engine's own compiled-in dsl/ tree (the go:embed in
	// embed.go). An embedded domain is always served from it; no overlay can
	// be served under its name.
	SourceEngine
	// SourcePack: a compiled pack's tree, registered with RegisterTree.
	SourcePack
	// SourceRuntime: a runtime domain -- one MountRuntimeDomainsFromEnv mounted
	// from MEMQL_DSL_PATH, which is how a package's staged DSL reaches a node,
	// or a package analysis' candidate (CandidateTree).
	SourceRuntime
)

// String names the source for logs and diagnostics.
func (s Source) String() string {
	switch s {
	case SourceEngine:
		return "engine"
	case SourcePack:
		return "pack"
	case SourceRuntime:
		return "runtime"
	default:
		return "unknown"
	}
}

// SourceOf reports which layer of tree serves path (a slash path from the
// tree's root, as fs.WalkDir yields it).
//
// tree is the engine's own view only when it is a value Tree or CandidateTree
// returned; for any other fs.FS the answer is SourceUnknown.
func SourceOf(tree fs.FS, path string) Source {
	domain, _ := splitFirst(strings.TrimPrefix(path, "./"))
	if domain == "" || domain == "." {
		return SourceUnknown
	}
	switch t := tree.(type) {
	case embed.FS:
		if t == embedFS && IsCoreDomain(domain) {
			return SourceEngine
		}
	case *layeredFS:
		if t == nil || t.base != fs.FS(embedFS) {
			return SourceUnknown
		}
		if t.embeddedHas(domain) {
			return SourceEngine
		}
		if _, ok := t.overlayFor(domain); ok {
			if t.runtime[domain] {
				return SourceRuntime
			}
			return SourcePack
		}
	}
	return SourceUnknown
}
