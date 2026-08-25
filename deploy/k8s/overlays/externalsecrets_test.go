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
	"livekit-secrets",
	"memql-secrets",
	"memql-secrets-identity",
	"telephony-secrets",
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

// voiceExternalSecrets are the two base objects the voice-off hold removes,
// mapped to the file that declares them. Listed, not discovered, for the same
// reason voiceOffLoadBalancers is: the failure worth catching is a third
// voice-only ExternalSecret arriving in base and nobody extending the hold,
// which discovery would wave through as "all of them are handled".
var voiceExternalSecrets = map[string]string{
	"livekit-secrets":   filepath.Join("..", "base", "externalsecret-livekit.yaml"),
	"telephony-secrets": filepath.Join("..", "base", "externalsecret-telephony.yaml"),
}

// deletePatch is the strategic-merge `$patch: delete` body an overlay uses to
// take a base resource out of the render. It is NOT a JSON 6902 op list, which
// is why this cannot reuse servicePatchOps.
type deletePatch struct {
	Patch      string `yaml:"$patch"`
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
}

// externalSecretDeletePatch returns the overlay's delete patch for the named
// ExternalSecret, or nil if it declares none.
func externalSecretDeletePatch(t *testing.T, overlay, name string) *deletePatch {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(overlay, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("reading the %s kustomization: %v", overlay, err)
	}
	var k kustomizationPatches
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatalf("parsing the %s kustomization: %v", overlay, err)
	}
	for _, p := range k.Patches {
		if p.Target.Kind != "ExternalSecret" || p.Target.Name != name {
			continue
		}
		var body deletePatch
		if err := yaml.Unmarshal([]byte(p.Patch), &body); err != nil {
			t.Fatalf("the %s patch targeting ExternalSecret %s does not parse: %v\n%s", overlay, name, err, p.Patch)
		}
		return &body
	}
	return nil
}

// baseAPIVersion reads the apiVersion the base manifest actually declares.
func baseAPIVersion(t *testing.T, file, name string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var doc struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
			Metadata   struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding %s: %v", file, err)
		}
		if doc.Kind == "ExternalSecret" && doc.Metadata.Name == name {
			return doc.APIVersion
		}
	}
	t.Fatalf("%s declares no ExternalSecret named %s", file, name)
	return ""
}

// TestCloudEntryDeletesTheVoiceExternalSecrets is the memql#4487 gate.
//
// cloud-entry holds voice off -- replicas 0 on the voice Deployments, ClusterIP
// on the LiveKit Services -- but base's two voice ExternalSecrets were never
// removed from the render. They resolve Key Vault entries that, for a voice-off
// install, DELIBERATELY do not exist, so ESO reported SecretSyncedError on both
// and the Application was `Degraded` on every entry install ever made.
//
// Voice-off is one decision, and this is the third thing it means.
func TestCloudEntryDeletesTheVoiceExternalSecrets(t *testing.T) {
	for name, baseFile := range voiceExternalSecrets {
		body := externalSecretDeletePatch(t, entryOverlay, name)
		if body == nil {
			t.Errorf("cloud-entry declares no patch for ExternalSecret %s; with voice off its Key Vault entries do not exist, so ESO reports SecretSyncedError and the Application is Degraded forever (memql#4487)", name)
			continue
		}
		if body.Patch != "delete" {
			t.Errorf("cloud-entry's patch on ExternalSecret %s is $patch=%q, want delete -- holding voice off means the object is not rendered, not that it is rendered differently", name, body.Patch)
		}
		if body.Metadata.Name != name {
			t.Errorf("cloud-entry's delete patch targeting %s names %q in its body", name, body.Metadata.Name)
		}
		// A delete patch whose apiVersion has drifted from base does not fail:
		// kustomize selects on `target`, so the body's GVK is not consulted,
		// and the drift sits there looking authoritative until something makes
		// it load-bearing. overlays/local carried `v1beta1` against a base of
		// `v1` for exactly that reason.
		if want := baseAPIVersion(t, baseFile, name); body.APIVersion != want {
			t.Errorf("cloud-entry's delete patch for %s says apiVersion %q; %s declares %q", name, body.APIVersion, baseFile, want)
		}
	}
}

// TestLocalDeletesTheVoiceExternalSecretsToo covers the other overlay that
// removes them, for a different reason (no ESO and no Key Vault locally) but
// under the same rule. It is here rather than under overlays/local because the
// apiVersion-drift assertion is the point, and that drift was found there.
func TestLocalDeletesTheVoiceExternalSecretsToo(t *testing.T) {
	for name, baseFile := range voiceExternalSecrets {
		body := externalSecretDeletePatch(t, "local", name)
		if body == nil {
			t.Errorf("overlays/local declares no delete patch for ExternalSecret %s; ESO and Key Vault do not exist locally", name)
			continue
		}
		if body.Patch != "delete" {
			t.Errorf("overlays/local's patch on ExternalSecret %s is $patch=%q, want delete", name, body.Patch)
		}
		if want := baseAPIVersion(t, baseFile, name); body.APIVersion != want {
			t.Errorf("overlays/local's delete patch for %s says apiVersion %q; %s declares %q", name, body.APIVersion, baseFile, want)
		}
	}
}

// TestCloudKeepsTheVoiceExternalSecrets is the reachable positive for the two
// gates above. Voice is ON in overlays/cloud, so its Key Vault entries exist
// and the objects belong in the render. If base simply stopped declaring them,
// the cloud-entry gate would pass for a reason that has nothing to do with the
// hold.
func TestCloudKeepsTheVoiceExternalSecrets(t *testing.T) {
	for name := range voiceExternalSecrets {
		if body := externalSecretDeletePatch(t, cloudOverlay, name); body != nil {
			t.Errorf("overlays/cloud deletes ExternalSecret %s; voice stays on there and the hold belongs to cloud-entry alone", name)
		}
	}
	found := walkExternalSecrets(t)
	for name, file := range voiceExternalSecrets {
		var saw bool
		for _, f := range found {
			if f.Metadata.Name == name {
				saw = true
			}
		}
		if !saw {
			t.Errorf("%s no longer declares ExternalSecret %s; the cloud-entry hold is written against it", file, name)
		}
	}
}

// engineCoreExternalSecrets are the ExternalSecrets every install renders no
// matter which modules are enabled. They belong to the engine itself, so no
// overlay holds them off and an overlay that did would be broken rather than
// minimal.
//
// Mapped to the file that declares them, matching voiceExternalSecrets, so the
// classification below reads as one table split by owner.
var engineCoreExternalSecrets = map[string]string{
	// BOTH live in deploy/external-secrets/, not deploy/k8s/base/ -- they are
	// wired onto an instance by scripts/deploy/wire-external-secrets.sh rather
	// than composed by an overlay, which is why no overlay has a patch for
	// either and why neither could ever be held off by one.
	"memql-secrets":          filepath.Join(deployRoot, "external-secrets", "externalsecret-memql.yaml"),
	"memql-secrets-identity": filepath.Join(deployRoot, "external-secrets", "externalsecret-memql.yaml"),
}

// TestEveryExternalSecretIsClassified is the memql#4488 ratchet: the thing that
// makes voiceExternalSecrets COMPLETE rather than merely correct.
//
// The hold gates above are keyed on voiceExternalSecrets, and that list is
// hand-maintained ON PURPOSE -- its own comment explains why discovery is the
// wrong instrument here, and it is right. "Which ExternalSecrets are voice's?"
// cannot be answered by scanning, because a third voice object arriving under a
// component name nobody thought of (app.kubernetes.io/name: sip, say) is not
// recognisable as voice's to any rule written today. Discovery would report it
// as not-voice and wave it through.
//
// But that leaves the list with a hole of exactly that shape. A new
// ExternalSecret in base is:
//
//   - walked by walkExternalSecrets, so it appears in the corpus;
//   - absent from voiceExternalSecrets, so NO gate requires a hold for it;
//   - therefore rendered by cloud-entry, resolving a Key Vault entry that a
//     voice-off install deliberately does not have.
//
// Which is ESO reporting SecretSyncedError, the Application reading Degraded
// forever on a correctly installed cluster, and the runbook eventually writing
// it down as expected noise. That is memql#4487 returning by the one route
// memql#4487's own gates cannot see.
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
//   - module-owned means the module's hold must ALSO learn about it, which is
//     the cloud-entry delete patch the gates above then require.
func TestEveryExternalSecretIsClassified(t *testing.T) {
	classified := map[string]string{}
	for name := range engineCoreExternalSecrets {
		classified[name] = "engine-core"
	}
	for name := range voiceExternalSecrets {
		if owner, dup := classified[name]; dup {
			t.Errorf("ExternalSecret %q is classified as both %s and voice-owned; it can only be one, "+
				"and the two buckets have opposite consequences for a voice-off render", name, owner)
			continue
		}
		classified[name] = "voice"
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
			"or as belonging to a module, by adding it to that module's map (voiceExternalSecrets today), "+
			"which then requires the module's overlay hold to remove it.\n"+
			"Unclassified means no gate requires a hold, so a voice-off install renders it, ESO cannot "+
			"resolve the Key Vault entry that deliberately does not exist, and the Application is "+
			"Degraded forever on a correctly installed cluster -- memql#4487 returning by the one route "+
			"its own gates cannot see (memql#4488).", name, f.file)
	}

	// The reverse direction: a classification naming an object the walk cannot
	// find is a stale entry, and a stale entry in voiceExternalSecrets silently
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

	t.Logf("classified %d ExternalSecret(s): %d engine-core, %d voice-owned",
		len(seen), len(engineCoreExternalSecrets), len(voiceExternalSecrets))
}

// allClassifiedFiles merges the two classification maps into one name -> file
// lookup, so the file-existence check reads both without repeating itself.
func allClassifiedFiles() map[string]string {
	out := map[string]string{}
	for name, file := range engineCoreExternalSecrets {
		out[name] = file
	}
	for name, file := range voiceExternalSecrets {
		out[name] = file
	}
	return out
}
