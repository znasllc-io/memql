// externalsecrets_test.go -- the two gates that make GitOps health mean
// something where ExternalSecrets are involved (epic memql#4491).
//
// Both defects below made an ArgoCD Application report `Degraded` or
// `OutOfSync` PERMANENTLY, on a correctly installed cluster, by design. The
// cost is not the red status. It is that an operator who learns the red is
// normal will not see a real one -- and in one case the permanent red had been
// written into the runbook as "expected noise", which converts a fixable
// configuration gap into a permanently disabled alarm.
//
// The rule both are instances of, and the reason they are gated together:
//
//	Enablement must be a fact about the DESIRED state, not a tolerated
//	failure of the live state.
//
// WHY TEXT-LEVEL AS WELL AS A RENDER. render() skips without kustomize or
// kubectl on the runner, and a guard that silently skips is not what stands
// between a defect and the cluster. This file reads the manifests and the
// kustomizations directly and has no such dependency;
// render_cloud_entry_test.go carries the rendered-output half beside it.
package overlays

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/znasllc-io/memql/core/repowalk"
)

const (
	// argoCompareOptions is the annotation ArgoCD reads to decide whether an
	// object it can see but cannot find in the repository is a problem.
	argoCompareOptions = "argocd.argoproj.io/compare-options"
	// ignoreExtraneous is the value that says the omission is deliberate. It
	// is one entry in a comma-separated list, so this is a membership test and
	// never an equality one -- a future `IgnoreExtraneous,ServerSideDiff=true`
	// must keep passing.
	ignoreExtraneous = "IgnoreExtraneous"
)

// deployRoot is the tree these gates walk, relative to this package.
var deployRoot = filepath.Join("..", "..")

// knownExternalSecrets is every ExternalSecret the engine ships today. Listed
// rather than discovered, as the reachable positive for the walk below: an
// empty walk asserts nothing, and a walk that silently stopped finding files
// would pass exactly as loudly as a clean tree.
//
// Adding one here is not the job -- the walk already covers a new object. This
// list exists so that the walk failing to SEE anything is itself a failure.
var knownExternalSecrets = []string{
	"memql-secrets",
	"memql-secrets-identity",
}

// externalSecret is the slice of a manifest these gates reason about.
type externalSecret struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
}

// foundExternalSecret pairs an object with the file it came out of, so a
// failure names something an author can open.
type foundExternalSecret struct {
	externalSecret
	file string
}

// unrenderedPlaceholder matches the `__NAME__` tokens deploy/k8s/components/
// tenant/template/ leaves for the fleet renderer to substitute. A file holding
// one is not a manifest yet -- `__HA_COMPONENT_LINE__` sits at zero indent
// inside a list and is not even valid YAML -- so it is excluded from the walk
// STRUCTURALLY rather than by a path list that would rot silently.
var unrenderedPlaceholder = regexp.MustCompile(`__[A-Z][A-Z0-9_]*__`)

// walkResult is what the walk saw, including what it did NOT examine. A gate
// that hides its own coverage turns a pass into a claim about the tool.
type walkResult struct {
	found    []foundExternalSecret
	excluded []string // files skipped as unrendered templates
	files    int
	docs     int
	// notObjects counts documents that are not YAML mappings -- deploy/ holds
	// JSON 6902 patch fragments (bare op sequences) beside its manifests, and a
	// sequence cannot be a Kubernetes object under any schema.
	notObjects int
}

// walkDeployManifests returns every ExternalSecret document under deploy/,
// plus what it could not examine.
//
// A file that will not parse is FATAL rather than skipped, and so is a mapping
// that will not decode: a checker that quietly drops what it could not read
// reports a pass about itself rather than about the tree. The only exclusions
// are the two structural ones above, and both are counted and named.
func walkDeployManifests(t *testing.T) walkResult {
	t.Helper()
	var res walkResult
	err := filepath.WalkDir(deployRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// The one skip list, shared with every other repo walker (memql#3678).
		// deploy/ is not an ancestor of .claude today, so this is prevention
		// rather than repair -- but a walker with its own idea of what to skip
		// is what let a nested worktree double-count results on one machine and
		// stay green on every other.
		if d.IsDir() {
			if repowalk.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if unrenderedPlaceholder.Match(raw) {
			res.excluded = append(res.excluded, path)
			return nil
		}
		res.files++
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		for i := 0; ; i++ {
			var node yaml.Node
			err := dec.Decode(&node)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("decoding document %d of %s: %v", i+1, path, err)
			}
			res.docs++
			doc := &node
			if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
				doc = doc.Content[0]
			}
			if doc.Kind != yaml.MappingNode {
				res.notObjects++
				continue
			}
			var es externalSecret
			if err := doc.Decode(&es); err != nil {
				t.Fatalf("decoding document %d of %s: %v", i+1, path, err)
			}
			if es.Kind != "ExternalSecret" {
				continue
			}
			res.found = append(res.found, foundExternalSecret{externalSecret: es, file: path})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", deployRoot, err)
	}
	sort.Slice(res.found, func(i, j int) bool { return res.found[i].Metadata.Name < res.found[j].Metadata.Name })
	sort.Strings(res.excluded)
	t.Logf("walked %d YAML file(s) under %s: %d document(s), %d not a mapping, %d ExternalSecret(s); excluded %d unrendered template(s): %v",
		res.files, deployRoot, res.docs, res.notObjects, len(res.found), len(res.excluded), res.excluded)
	return res
}

func walkExternalSecrets(t *testing.T) []foundExternalSecret {
	t.Helper()
	return walkDeployManifests(t).found
}

// TestTheExternalSecretWalkExcludesOnlyUnrenderedTemplates is the escape-hatch
// clause on the exclusion above. Without it the placeholder rule is a hole of
// unknown size: any manifest that happened to carry a `__NAME__` token would
// drop out of the gate silently, and a tenant template that grew an
// ExternalSecret would never be checked at all.
//
// Two assertions, and they are the repair instructions. An excluded file must
// live under a template directory -- if a real manifest starts matching, take
// the placeholder out of it rather than widening the rule. And an excluded file
// must declare no ExternalSecret -- if a template needs one, the gate has to
// learn to render templates first; it must not simply not see it.
func TestTheExternalSecretWalkExcludesOnlyUnrenderedTemplates(t *testing.T) {
	for _, path := range walkDeployManifests(t).excluded {
		if !strings.Contains(filepath.ToSlash(path), "/template/") {
			t.Errorf("%s is excluded from the ExternalSecret walk because it carries an unrendered __PLACEHOLDER__ token, but it is not under a template directory", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if strings.Contains(string(raw), "kind: ExternalSecret") {
			t.Errorf("%s declares an ExternalSecret and is excluded from the walk as an unrendered template; it is therefore checked by nothing (memql#4489)", path)
		}
	}
}

// TestTheExternalSecretWalkSeesTheOnesWeKnowAbout is the reachable positive
// for the gate below: it proves the walk can find anything at all. Without it,
// a walk rooted at the wrong directory, or one that stopped recognising the
// kind, would report a clean tree in exactly the words a clean tree uses.
func TestTheExternalSecretWalkSeesTheOnesWeKnowAbout(t *testing.T) {
	found := walkExternalSecrets(t)
	if len(found) == 0 {
		t.Fatalf("the walk over %s found no ExternalSecret at all; the engine ships %d, so this is the checker failing rather than the tree passing",
			deployRoot, len(knownExternalSecrets))
	}
	seen := map[string]string{}
	for _, f := range found {
		seen[f.Metadata.Name] = f.file
	}
	for _, name := range knownExternalSecrets {
		if _, ok := seen[name]; !ok {
			t.Errorf("the walk did not find ExternalSecret %q; it saw %v", name, sortedNames(found))
		}
	}
	t.Logf("examined %d ExternalSecret(s) under %s: %v", len(found), deployRoot, sortedNames(found))
}

// TestEveryExternalSecretIgnoresExtraneous is the memql#4489 gate.
//
// External Secrets copies an ExternalSecret's labels and annotations onto the
// target Secret it writes. ArgoCD's label tracking identifies its resources by
// `app.kubernetes.io/instance`, so the moment Argo tracks an ExternalSecret,
// the Secret it generates INHERITS the tracking label -- and Argo then reports
// an object that exists in no repository as OutOfSync, forever, with nothing
// to sync and nothing to fix. Observed live as Secret/memql-secrets carrying
// `app.kubernetes.io/instance: memql`.
//
// The fix uses that same inheritance against itself, so it has to be on EVERY
// ExternalSecret rather than on the ones that have caused trouble: any one
// without it re-claims its Secret for Argo the next time it reconciles, and
// where two objects merge into one Secret (memql-secrets), one of them is
// enough to lose the annotation for both.
func TestEveryExternalSecretIgnoresExtraneous(t *testing.T) {
	for _, f := range walkExternalSecrets(t) {
		opts, ok := f.Metadata.Annotations[argoCompareOptions]
		if !ok {
			t.Errorf("%s: ExternalSecret %s carries no %s annotation -- the Secret it generates inherits Argo's tracking label and is reported OutOfSync forever (memql#4489)",
				f.file, f.Metadata.Name, argoCompareOptions)
			continue
		}
		if !hasCompareOption(opts, ignoreExtraneous) {
			t.Errorf("%s: ExternalSecret %s has %s=%q, which does not include %s",
				f.file, f.Metadata.Name, argoCompareOptions, opts, ignoreExtraneous)
		}
	}
}

func hasCompareOption(value, want string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}

func sortedNames(found []foundExternalSecret) []string {
	out := make([]string, 0, len(found))
	for _, f := range found {
		out = append(out, f.Metadata.Name)
	}
	sort.Strings(out)
	return out
}

// moduleExternalSecrets are ExternalSecrets belonging to an OPTIONAL module --
// rendered where the module is on, held off with a `$patch: delete` where it is
// off. It is EMPTY today and that is a fact, not an oversight: the only two
// entries it ever had were livekit-secrets and telephony-secrets, and both left
// with the voice node type.
//
// It stays, empty, because it is half the classification ratchet below. Deleting
// it would leave a new module ExternalSecret with nowhere to be declared, which
// is the hole memql#4488 exists to close.
//
// Mapped to the file that declares them, matching engineCoreExternalSecrets, so
// the classification reads as one table split by owner.
var moduleExternalSecrets = map[string]string{}

// engineCoreExternalSecrets are the ExternalSecrets every install renders no
// matter which modules are enabled. They belong to the engine itself, so no
// overlay holds them off and an overlay that did would be broken rather than
// minimal.
var engineCoreExternalSecrets = map[string]string{
	// BOTH live in deploy/external-secrets/, not deploy/k8s/base/ -- they are
	// wired onto an instance by scripts/deploy/wire-external-secrets.sh rather
	// than composed by an overlay, which is why no overlay has a patch for
	// either and why neither could ever be held off by one.
	"memql-secrets":          filepath.Join(deployRoot, "external-secrets", "externalsecret-memql.yaml"),
	"memql-secrets-identity": filepath.Join(deployRoot, "external-secrets", "externalsecret-memql.yaml"),
}

// TestEveryExternalSecretIsClassified is the memql#4488 ratchet: the thing that
// makes moduleExternalSecrets COMPLETE rather than merely correct.
//
// A module's object list is hand-maintained ON PURPOSE -- "which
// ExternalSecrets belong to module X?" cannot be answered by scanning, because
// an object arriving under a component name nobody thought of is not
// recognisable as that module's to any rule written today. Discovery would
// report it as not-the-module's and wave it through.
//
// But that leaves the list with a hole of exactly that shape. A new
// ExternalSecret in base is:
//
//   - walked by walkExternalSecrets, so it appears in the corpus;
//   - absent from moduleExternalSecrets, so NO gate requires a hold for it;
//   - therefore rendered by every overlay, resolving a Key Vault entry that a
//     module-off install deliberately does not have.
//
// Which is ESO reporting SecretSyncedError, the Application reading Degraded
// forever on a correctly installed cluster, and the runbook eventually writing
// it down as expected noise. That is memql#4487 returning by the one route its
// own gates cannot see.
//
// So this does not try to classify anything. It requires that SOMEBODY has:
// every ExternalSecret the walk finds must be listed as engine-core (rendered
// everywhere) or as a module's (held off where that module is off). An
// unclassified one fails here, at the moment it is added, with both repairs
// named -- which is the only moment the answer is cheap and obvious.
//
// THE TWO REPAIRS ARE DIFFERENT DECISIONS, and neither is a formality:
//
//   - engine-core means "this renders on every install, including one with
//     every optional module off". If that is not true of the object, this is
//     the wrong bucket and the install it breaks is the minimal one nobody
//     tests.
//   - module-owned means the module's hold must ALSO learn about it -- a
//     `$patch: delete` in every overlay that runs with the module off.
func TestEveryExternalSecretIsClassified(t *testing.T) {
	classified := map[string]string{}
	for name := range engineCoreExternalSecrets {
		classified[name] = "engine-core"
	}
	for name := range moduleExternalSecrets {
		if owner, dup := classified[name]; dup {
			t.Errorf("ExternalSecret %q is classified as both %s and module-owned; it can only be one, "+
				"and the two buckets have opposite consequences for a module-off render", name, owner)
			continue
		}
		classified[name] = "module"
	}

	found := walkExternalSecrets(t)
	if len(found) == 0 {
		t.Fatal("the walk found no ExternalSecret at all, so this gate examined nothing. " +
			"See TestTheExternalSecretWalkSeesTheOnesWeKnowAbout -- the checker is failing, not the tree passing.")
	}

	seen := map[string]bool{}
	for _, f := range found {
		name := f.Metadata.Name
		if seen[name] {
			continue
		}
		seen[name] = true
		if _, ok := classified[name]; ok {
			continue
		}
		t.Errorf("ExternalSecret %q (%s) is classified by nothing.\n"+
			"Every ExternalSecret must be declared either engine-core -- rendered on EVERY install, "+
			"including one with every optional module off -- by adding it to engineCoreExternalSecrets, "+
			"or as belonging to a module, by adding it to moduleExternalSecrets, "+
			"which then requires the module's overlay hold to remove it.\n"+
			"Unclassified means no gate requires a hold, so a module-off install renders it, ESO cannot "+
			"resolve the Key Vault entry that deliberately does not exist, and the Application is "+
			"Degraded forever on a correctly installed cluster -- memql#4487 returning by the one route "+
			"its own gates cannot see (memql#4488).", name, f.file)
	}

	// The reverse direction: a classification naming an object the walk cannot
	// find is a stale entry, and a stale entry in moduleExternalSecrets silently
	// weakens the hold gates -- they iterate the map, so a name that no longer
	// exists in base simply stops being checked while still looking covered.
	for name, owner := range classified {
		if !seen[name] {
			t.Errorf("ExternalSecret %q is classified as %s but the walk does not find it in the tree. "+
				"Either it was renamed or removed and the classification is stale, or the walk has stopped "+
				"seeing the file that declares it.", name, owner)
		}
	}

	// The mapped file is part of the classification, so it has to be checked
	// too. A path nobody reads drifts silently: these were written from memory
	// as deploy/k8s/base/externalsecret-memql*.yaml, which do not exist, and
	// every assertion above still passed -- the map values were decoration.
	for name, rel := range allClassifiedFiles() {
		raw, err := os.ReadFile(rel)
		if err != nil {
			t.Errorf("ExternalSecret %q is mapped to %s, which cannot be read: %v", name, rel, err)
			continue
		}
		if !strings.Contains(string(raw), "name: "+name) {
			t.Errorf("ExternalSecret %q is mapped to %s, which does not declare it", name, rel)
		}
	}

	t.Logf("classified %d ExternalSecret(s): %d engine-core, %d module-owned",
		len(seen), len(engineCoreExternalSecrets), len(moduleExternalSecrets))
}

// allClassifiedFiles merges the two classification maps into one name -> file
// lookup, so the file-existence check reads both without repeating itself.
func allClassifiedFiles() map[string]string {
	out := map[string]string{}
	for name, file := range engineCoreExternalSecrets {
		out[name] = file
	}
	for name, file := range moduleExternalSecrets {
		out[name] = file
	}
	return out
}
