package local

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// applied_autoload_test.go -- memql#3961.
//
// THE DEFECT, ONE KIND OVER FROM memql#3797. render_autoload_test.go asserts
// that no node is left autoloading the genesis envelope on the local cluster.
// It filters `if !strings.Contains(doc, "kind: Deployment")`, and
// deploy/k8s/base/migrate-job.yaml is a Job -- envFrom memql-secrets, and
// MEMQL_GENESIS_AUTOLOAD=true. On a machine with no ~/.memql/genesis.znas the
// Job's container exits at boot with "MEMQL_GENESIS_AUTOLOAD=true but no
// envelope source", and `make up` dies at step 5/6 with "migrate Job did not
// complete" on an otherwise healthy cluster.
//
// WIDENING THAT FILTER PAST `kind: Deployment` FIXES NOTHING, AND THIS IS THE
// POINT OF THIS FILE. The Job is not a kustomization resource --
// deploy/k8s/base/kustomization.yaml says so in as many words ("Jobs don't
// belong in the kustomization -- they're one-shot"). scripts/k3d/bringup.sh
// applies it out of band, piping it through perl to swap the image and then
// `kubectl apply -f -`. So it never enters `kustomize build` output at all: a
// test widened to `kind: Job` still renders nothing to inspect, still passes,
// and `make up` still fails. The render contains no Job and no CronJob
// whatsoever -- conn-monitor is `$patch: delete`d by kustomization.yaml.
//
// AND THE RENDER-BASED GATE DOES NOT RUN IN CI AT ALL. render(t) shells out to
// `kustomize build .`, falls back to `kubectl kustomize .`, and t.Skips when
// neither is on PATH. No CI job installs either binary -- `deploy/k8s` is not
// mentioned once in .github/workflows/ci.yml -- so every assertion in
// render_autoload_test.go has been skipping silently since the day it landed.
// A gate that always skips is a larger blind spot than the one it was written
// to close.
//
// SO THIS CHECK READS FILES AND NOTHING ELSE. No renderer, no cluster, no
// network: it runs on every machine and on every CI runner, which is the only
// property that makes it a gate rather than a hope.
//
// THE PROPERTY. A workload that mounts memql-secrets and sets
// MEMQL_GENESIS_AUTOLOAD=true is fine in the cloud, where the envelope is the
// delivery mechanism, and wrong locally, where seed-secrets.sh writes the same
// values onto memql-secrets directly. The local overlay's one instrument for
// turning it off is patches/genesis-autoload-off.yaml. So:
//
//	Every workload manifest under deploy/k8s/ that mounts memql-secrets and
//	sets MEMQL_GENESIS_AUTOLOAD=true must be named by
//	patches/genesis-autoload-off.yaml.
//
// A workload the patch cannot name -- because it is applied out of band, or
// because nobody added it -- fails here. That is deliberately one property
// covering two different causes, because from the operator's chair they are the
// same event: a pod on the local cluster read an envelope that does not exist.
//
// It is KIND-AGNOSTIC by construction: nothing below tests `kind` for anything
// except reporting it. A CronJob, a DaemonSet or a StatefulSet acquiring the
// flag is caught on the same line a Deployment is.
//
// WHY MATCHING THE PATCH BY NAME IS SOUND RATHER THAN CIRCULAR. A strategic
// merge patch whose target is absent from the kustomize graph is an error
// kustomize raises at build time ("no matches for target"), so "the patch names
// it" cannot be satisfied by adding a document for a resource the overlay does
// not compose. Naming and membership therefore stand or fall together, and this
// check does not have to reimplement the resource graph to rely on that.

var (
	// Both spellings the tree actually uses. Base manifests write the flow
	// form on one line; the patch writes the block form across two. Anchoring
	// on the variable name and reading its value is what makes this read the
	// flag rather than a neighbouring entry -- and requiring `name:` to be
	// preceded by `{` or `- ` is what keeps it from matching the several
	// COMMENTS that mention MEMQL_GENESIS_AUTOLOAD in prose (azurite.yaml
	// discusses it at length and sets nothing).
	autoloadFlow  = regexp.MustCompile(`\{\s*name:\s*MEMQL_GENESIS_AUTOLOAD\s*,\s*value:\s*"?([A-Za-z]+)"?\s*\}`)
	autoloadBlock = regexp.MustCompile(`(?m)^\s*-\s+name:\s*MEMQL_GENESIS_AUTOLOAD\s*\n\s*value:\s*"?([A-Za-z]+)"?`)

	// The object's OWN name, read out of the top-level metadata block.
	//
	// The regex this replaces was `(?m)^  name: (\S+)`, anchored at exactly two
	// spaces and searched over the whole document. That is right for a rendered
	// Deployment and wrong for everything else: a CronJob nests a second
	// metadata under spec.jobTemplate, a container's `- name:` sits deeper
	// still, and a two-space `name:` inside spec would win purely by appearing
	// first. Scoping to the block that starts at column zero is what makes the
	// answer the object's identity rather than the first name-shaped line in
	// the file.
	topLevelMetadata = regexp.MustCompile(`(?m)^metadata:\n((?:[ \t]+[^\n]*\n)+)`)
	twoSpaceName     = regexp.MustCompile(`(?m)^  name:\s*(\S+)`)
	docKind          = regexp.MustCompile(`(?m)^kind:\s*(\S+)`)
)

// workloadRef identifies one workload document by the pair kustomize patches
// are matched on.
type workloadRef struct {
	kind string
	name string
	file string
}

// TestNoLocallyAppliedWorkloadAutoloadsTheEnvelope is the renderer-free gate.
func TestNoLocallyAppliedWorkloadAutoloadsTheEnvelope(t *testing.T) {
	root := repoRoot(t)

	covered := autoloadOffTargets(t, root)
	if len(covered) == 0 {
		t.Fatal("patches/genesis-autoload-off.yaml names no target at all; either the " +
			"file has been emptied or the parser below has rotted -- either way this " +
			"check is measuring nothing")
	}

	var inspected, flagged int
	for _, path := range manifestsUnder(t, filepath.Join(root, "deploy", "k8s")) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel, _ := filepath.Rel(root, path)

		for _, doc := range splitYAMLDocs(string(body)) {
			// A workload is a thing with containers. Services, ConfigMaps and
			// the memql-secrets Secret itself all mention the Secret's name
			// without ever running a process that could read an envelope.
			if !strings.Contains(doc, "containers:") {
				continue
			}
			if !strings.Contains(doc, "name: memql-secrets") {
				continue // the envelope is not this pod's business
			}
			inspected++

			value, found := autoloadSetting(doc)
			if !found || !strings.EqualFold(value, "true") {
				// Absent is SAFE and is the state migrate-job.yaml is being
				// moved to, not an omission to flag. AutoloadFromEnv requires
				// the literal string "true" (component/genesis/autoload.go:80),
				// and MEMQL_GENESIS_AUTOLOAD is not a key on memql-secrets --
				// seed-secrets.sh never writes one -- so there is no envFrom
				// value for an absent entry to inherit.
				continue
			}
			flagged++

			ref := workloadRef{kind: docValue(docKind, doc), name: objectName(doc), file: rel}
			if ref.name == "" {
				t.Errorf("%s: a workload mounting memql-secrets sets MEMQL_GENESIS_AUTOLOAD=true "+
					"and has no parseable top-level metadata.name", rel)
				continue
			}
			if _, ok := covered[ref.kind+"/"+ref.name]; ok {
				continue
			}

			t.Errorf(`%s %s (%s) sets MEMQL_GENESIS_AUTOLOAD=true and is NOT named by
patches/genesis-autoload-off.yaml, so on the local cluster it reads an envelope
that a developer or an operator has almost certainly never created.

seed-secrets.sh writes the values the envelope would carry onto memql-secrets
directly, so autoload is redundant locally and fatal when ~/.memql/genesis.znas
is absent -- the container exits with "MEMQL_GENESIS_AUTOLOAD=true but no
envelope source".

Two fixes, and which one applies depends on whether the local overlay composes
this resource at all:

  - It IS a kustomization resource -- add a document naming it to
    patches/genesis-autoload-off.yaml, exactly as the ten node Deployments do.

  - It is NOT (deploy/k8s/base/kustomization.yaml deliberately keeps one-shot
    Jobs out, and scripts/k3d/bringup.sh applies migrate-job.yaml by hand) --
    then no patch can reach it and the "true" must come out of the manifest.
    The cloud does not need it there either: the keys this workload reads are
    declared on memql-secrets in their own right.`,
				ref.kind, ref.name, rel)
		}
	}

	// Anti-vacuity, from both directions. Zero inspected means the walk or the
	// membership predicate broke; the mesh demonstrably runs ten node
	// Deployments that mount this Secret.
	if inspected < len(nodes) {
		t.Errorf("only %d workload documents mounting memql-secrets were found under "+
			"deploy/k8s/, but the local mesh runs %d node types (%s) -- the walk is "+
			"incomplete, so a workload could be missing from this check rather than "+
			"passing it", inspected, len(nodes), strings.Join(nodes, ", "))
	}
	// And zero FLAGGED would mean nothing in the tree sets the flag, which is
	// true only after the envelope is deleted outright (memql#3966). Until
	// then, a run that flags nothing has stopped reading the value.
	if flagged == 0 && anyManifestMentionsAutoload(t, root) {
		t.Error("no workload was found setting MEMQL_GENESIS_AUTOLOAD=true, yet the " +
			"manifests still mention the variable -- autoloadSetting() has stopped " +
			"matching the spelling the tree uses. When the envelope is deleted " +
			"(memql#3966) this whole file goes with it; until then a silent zero here " +
			"is the check failing open.")
	}
}

// autoloadOffTargets reads the patch and returns the kind/name pairs it names.
func autoloadOffTargets(t *testing.T, root string) map[string]struct{} {
	t.Helper()
	path := filepath.Join(root, "deploy", "k8s", "overlays", "local", "patches", "genesis-autoload-off.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]struct{}{}
	for _, doc := range splitYAMLDocs(string(body)) {
		kind, name := docValue(docKind, doc), objectName(doc)
		if kind == "" || name == "" {
			continue
		}
		out[kind+"/"+name] = struct{}{}
	}
	return out
}

// autoloadSetting reports the flag's value in one document, in either spelling.
func autoloadSetting(doc string) (string, bool) {
	if m := autoloadFlow.FindStringSubmatch(doc); m != nil {
		return m[1], true
	}
	if m := autoloadBlock.FindStringSubmatch(doc); m != nil {
		return m[1], true
	}
	return "", false
}

// objectName reads metadata.name from the top-level metadata block only.
func objectName(doc string) string {
	m := topLevelMetadata.FindStringSubmatch(doc)
	if m == nil {
		return ""
	}
	return docValue(twoSpaceName, m[1])
}

func docValue(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// splitYAMLDocs splits on document separators at column zero, so a `---`
// inside a block scalar or a comment does not split a document in half.
func splitYAMLDocs(body string) []string {
	var docs []string
	var cur strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimRight(line, " \t") == "---" {
			docs = append(docs, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}
	docs = append(docs, cur.String())
	return docs
}

func manifestsUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// One skip list for every repo walker (memql#3678). This walk is
		// scoped to deploy/k8s so it would not reach .claude/ anyway, but the
		// gate is about there being ONE list rather than about each walk's
		// reachable set -- a scoped walk that hand-waves the helper is exactly
		// how the list stops being one list.
		if d.IsDir() {
			if repowalk.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext == ".yaml" || ext == ".yml" {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	if len(out) == 0 {
		t.Fatalf("no manifests under %s", dir)
	}
	return out
}

func anyManifestMentionsAutoload(t *testing.T, root string) bool {
	t.Helper()
	for _, path := range manifestsUnder(t, filepath.Join(root, "deploy", "k8s")) {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(body), "MEMQL_GENESIS_AUTOLOAD") {
			return true
		}
	}
	return false
}

// repoRoot walks up from the package directory to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate the repository root (no go.mod above the package directory)")
	return ""
}
