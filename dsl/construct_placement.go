package dsl

import (
	"path"
	"strings"
)

// construct_placement.go -- which file of a domain each construct kind's loader
// reads it from (memql#5437).
//
// Every construct loader reads every .memql file of a domain -- concepts,
// queries, mutations, logic, shapes, specs, traits, tools, builtins, prompts,
// providers, seeds, policies, rules and actions alike -- with ONE exception:
// the automation loader, the walker that wires triggers into the scheduler
// (component/automations.LoadFromTree), reads a domain's automations from its
// automations.memql and from no other file.
//
// An automation declared anywhere else is therefore worse than absent. The
// passes that read the whole tree still read it -- the statement-body and
// sub-automation contract gates check its statements, and memqllint reported
// such a tree clean -- while nothing registers it and its trigger never fires:
// no skip, no warning, a green boot. Ten automations of the MemQL Cloud
// control plane sat in deploy/fleet/dsl/fleet/billing.memql and trial.memql
// that way, dunning and the trial clock among them, while the operator docs
// described them running.
//
// This file is the ONE statement of the rule. The automation loader asks it
// which files to read, and the construct-misplaced contract gate
// (component/memql/dslgate) asks it which declarations to refuse, so the
// loader and the refusal cannot disagree about a file. A kind restricted here
// is refused at load wherever its loader would not read it: in the embedded
// tree, a MEMQL_DSL_PATH bundle, a pack, a package and memqllint alike.

// AutomationsFile is the one file of a domain directory the automation loader
// reads.
const AutomationsFile = "automations.memql"

// constructFiles maps a construct keyword to the one file name its loader reads
// it from. A keyword absent here is read from every .memql file of its domain.
var constructFiles = map[string]string{
	"automation": AutomationsFile,
}

// ConstructFile returns the one file name the loader for a construct keyword
// reads it from, and false for a kind that is read from every file.
func ConstructFile(keyword string) (string, bool) {
	file, ok := constructFiles[keyword]
	return file, ok
}

// LoaderReadsConstruct reports whether the loader for keyword reads a construct
// declared in p, a slash-separated path within a DSL tree
// ("fleet/billing.memql").
//
// A kind with no file of its own is read from anywhere, and the answer is
// true. For a kind restricted to one file the answer is true only when p is
// that file inside a domain directory -- never at the root of a tree, where no
// domain claims it -- and no directory on the way to it is soft-disabled or
// hidden (a name beginning `_` or `.`), which the automation walker skips.
func LoaderReadsConstruct(keyword, p string) bool {
	file, restricted := constructFiles[keyword]
	if !restricted {
		return true
	}
	dir, base := path.Split(strings.TrimPrefix(path.Clean(p), "/"))
	if base != file || dir == "" {
		return false
	}
	for _, segment := range strings.Split(strings.TrimSuffix(dir, "/"), "/") {
		if strings.HasPrefix(segment, "_") || strings.HasPrefix(segment, ".") {
			return false
		}
	}
	return true
}

// ConstructHome is the file a construct declared in p belongs in, for a kind
// its loader reads from one file: that file, in p's own directory -- or, when a
// directory on the way is soft-disabled or hidden, in the last directory above
// it. It is "" for a kind read from every file, and for a p at the root of a
// tree, which is in no domain directory at all.
func ConstructHome(keyword, p string) string {
	file, restricted := constructFiles[keyword]
	if !restricted {
		return ""
	}
	dir := path.Dir(strings.TrimPrefix(path.Clean(p), "/"))
	if dir == "." {
		return ""
	}
	var kept []string
	for _, segment := range strings.Split(dir, "/") {
		if strings.HasPrefix(segment, "_") || strings.HasPrefix(segment, ".") {
			break
		}
		kept = append(kept, segment)
	}
	if len(kept) == 0 {
		return ""
	}
	return path.Join(append(kept, file)...)
}
