package k3d

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// up_rendered_manifest_test.go -- znasllc-io/memql#4063.
//
// THE THIRD GATE IN THIS FAMILY, AND THE ONLY ONE AT THE LEVEL THE BUG LIVED AT.
//
// The narrative of #4063 is written out in up_db_operand_image_test.go and the
// classification rule in up_image_classification_test.go; neither is repeated
// here. What matters is what those two gates read: THE EMITTED STRING. They ask
// whether `kustomize_image_overrides` produced a sensible-looking line.
//
// An override is a REQUEST, not an outcome. Between the request and the objects
// that reach the API server sits kustomize, which can decline it in silence:
//
//   - kustomize's `images:` transformer knows the image paths in core workload
//     kinds and NOTHING else. A CustomResource that names an image somewhere of
//     its own choosing is invisible to it, and CNPG puts it at
//     `spec/imageName` on the Cluster. What teaches the transformer that path
//     is deploy/k8s/components/cnpg-db/kustomizeconfig/images.yaml.
//   - Delete that one file and the override for `memql-db` becomes a NO-OP. The
//     render still succeeds. The emitted line is still perfect, so both string
//     gates still pass. `imageName` comes out as a bare `memql-db` and the
//     failure lands at apply time (CNPG refusing a version it cannot read --
//     which is #4063's symptom exactly, reached by a different route) or at
//     pull time, against `memql-db:latest`, which nobody has ever pushed.
//
// So this gate renders the overlay WITH the installer's overrides applied the
// way ArgoCD applies them, and asserts about the manifest that would actually
// be applied. It is the level at which "the operand did not get the engine tag"
// and "the operand got nothing at all" are the same question.
//
// THE OVERRIDES COME FROM up.sh, NEVER FROM A LIST HERE. The emitter is the
// subject; a hand-written copy of its output would agree with itself forever
// and assert nothing. (Note the deliberate contrast with
// up_image_classification_test.go, which IS a hand-kept list -- there the point
// is a CLOSED SET that a new arrival cannot join silently, which discovery
// would destroy. Different questions, opposite techniques.)
//
// # WHAT THIS GATE CANNOT CATCH
//
// Stated because an over-claimed guarantee is worse than a modest one:
//
//   - It RENDERS; it does not APPLY. No admission webhook runs here. A manifest
//     CNPG would refuse for any reason other than the version prefix asserted
//     below still passes this gate.
//   - It says nothing about whether the images EXIST. A published-looking tag
//     nobody ever pushed pulls exactly as badly as `:local` does, and only a
//     registry can answer that.
//   - It covers the LOCAL overlay under the installer's override set. The cloud
//     overlay pins by digest through a different path and is not read here.
//   - It skips wholesale where neither renderer is installed (see renderKustomization).
//     A SKIP IS NOT A PASS. On a machine where kubectl lives off PATH -- which is
//     the normal shape here -- `export PATH="$HOME/.memql/bin:$PATH"` first, and
//     run with -v to see the "rendered N image references" log line that proves
//     this gate did work rather than declining to.

// minimumSupportedPostgresMajor is the oldest major CloudNativePG will accept on
// `Cluster.spec.imageName`. Its own refusal names the number:
//
//	Cluster.postgresql.cnpg.io "memql-db" is invalid: spec.imageName:
//	Invalid value: "ghcr.io/znasllc-io/memql-db:0.19.0":
//	Unsupported PostgreSQL version. Versions 13 or newer are supported
const minimumSupportedPostgresMajor = 13

// installRegistry / installTag are the values a real install passes. The tag is
// the engine release that shipped #4063, deliberately: the whole point of the
// operand rule is that this value must NOT reach the database image, so testing
// with the version that did makes the demonstration exact rather than abstract.
const (
	installRegistry = "ghcr.io/znasllc-io"
	installTag      = "0.19.0"
)

// overrideLine matches one emitted `kustomize.images` entry -- `- NAME=REF`.
// Strict, so that cap_info chatter on stderr (upEmit returns CombinedOutput)
// cannot be mistaken for an override.
var overrideLine = regexp.MustCompile(`^\s*-\s+(\S+)=(\S+)\s*$`)

// imageOverride is one `NAME=NEWREF` pair: the name as it appears in the BASE
// manifests (which is what kustomize matches on) and the reference to put there.
type imageOverride struct {
	name string
	ref  string
}

// installerImageOverrides runs the REAL emitter and parses what it produced.
func installerImageOverrides(t *testing.T) []imageOverride {
	t.Helper()
	block := upEmit(t, "memql.localhost", installRegistry, installTag, "kustomize_image_overrides")

	var out []imageOverride
	for _, line := range strings.Split(block, "\n") {
		m := overrideLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out = append(out, imageOverride{name: m[1], ref: m[2]})
	}
	if len(out) == 0 {
		t.Fatalf("kustomize_image_overrides emitted no parseable overrides; this gate would then render "+
			"the committed overlay and assert about an install nobody performed:\n%s", block)
	}
	return out
}

// repoRootFromScriptsK3d resolves the repository root from this package.
func repoRootFromScriptsK3d(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// renderKustomization builds a kustomization directory with whichever renderer
// the machine has -- the same discovery the sibling gates under
// deploy/k8s/overlays/local use, kept identical on purpose so a machine that can
// run one of them can run all of them.
func renderKustomization(t *testing.T, dir string) string {
	t.Helper()
	for _, cmd := range [][]string{
		{"kustomize", "build", dir},
		{"kubectl", "kustomize", dir},
	} {
		if _, err := exec.LookPath(cmd[0]); err != nil {
			continue
		}
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v\n%s", strings.Join(cmd, " "), err, out)
		}
		return string(out)
	}
	// IN CI, A MISSING RENDERER IS A FAILURE, NOT A SKIP.
	//
	// A developer without kubectl should get a skip: this gate is not what they
	// are working on, and the siblings under deploy/k8s/overlays/local behave
	// the same way. But CI is the only place this gate's verdict is load-bearing,
	// and the whole reason it exists is that #4063 reached a release with every
	// other gate green -- so "silently verified nothing" is precisely the state
	// it must not be allowed to reach there.
	//
	// The trigger is GITHUB'S OWN `CI` VARIABLE rather than a MemQL knob. A knob
	// only this test honours is one more thing to remember to set, and the
	// failure mode of forgetting is exactly the silence being closed here.
	// GitHub Actions sets CI=true on every runner, so this needs no workflow
	// change and cannot drift out of sync with one.
	//
	// It is reachable: `./scripts/...` is in ci.yml's gate-inputs selector, and
	// the hosted ubuntu image ships kubectl today. That is the point -- if a
	// future runner image drops it, this goes RED and names the fix, instead of
	// the gate quietly retiring itself and nobody noticing until the next
	// operand-shaped bug ships.
	if os.Getenv("CI") != "" {
		t.Fatal("neither kustomize nor kubectl is on PATH, and this is CI. " +
			"A skip here would verify NOTHING -- and verifying nothing while reporting green " +
			"is the failure this gate was written to stop (memql#4063 shipped with every other " +
			"gate green). Install a renderer in the job that runs ./scripts/... -- the hosted " +
			"ubuntu image has shipped kubectl, so if this fires the image changed.")
	}
	t.Skip("neither kustomize nor kubectl is installed; cannot render the overlay " +
		"(this machine keeps them in ~/.memql/bin -- put it on PATH). A skip here has verified NOTHING.")
	return ""
}

// copyTree copies src to dst recursively.
//
// The edit below has to happen in a kustomization ON DISK, because that is the
// only way kustomize consumes one -- and it must not happen in the working tree,
// because a test that leaves the overlay modified is a test that breaks the next
// one. deploy/k8s is ~768K, so copying it whole is cheaper than being clever
// about which of the base/component/overlay files the overlay actually reaches.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // symlinks / sockets: nothing in deploy/k8s needs them
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatalf("copying %s: %v", src, err)
	}
}

// applyArgoImageOverrides does to a kustomization.yaml what ArgoCD does to it.
//
// FIDELITY, AND WHAT IT COSTS. ArgoCD shells out to `kustomize edit set image
// <NAME>=<REF>` once per entry in `spec.source.kustomize.images` and then builds
// the result. `kustomize edit set image` MERGES BY NAME: an existing `images:`
// entry whose `name` matches has its `newName`/`newTag` replaced (and any
// `digest` dropped, the two being alternatives), rather than a second entry
// being appended -- which matters, because two entries for one name would leave
// the second matching nothing after the first had renamed the image.
//
// That merge is emulated here rather than executed, for a reason worth stating:
// `kubectl kustomize` -- the renderer most machines in this repo actually have --
// carries no `edit` subcommand at all, so requiring the real one would make this
// gate skip on exactly the machines the sibling gates run on. The cost is that
// we are trusting a reading of `kustomize edit set image` instead of running it.
//
// TestEveryInstallerOverrideReachesTheRenderedManifest is the mitigation and is
// not optional: an emulation that quietly did nothing would leave every override
// absent from the render, which that gate fails on by name.
//
// The other half of the worry is the reverse -- a re-encode that changes
// something it was never asked to. Measured, on the way in: copying the tree,
// round-tripping this file through yaml.v3 with NO overrides applied, and
// rendering produces output byte-identical to rendering the committed overlay.
// The round-trip is inert; what it does to comments and quoting does not survive
// into the manifests, because kustomize reads the semantics.
func applyArgoImageOverrides(t *testing.T, kustomization string, overrides []imageOverride) {
	t.Helper()

	raw, err := os.ReadFile(kustomization)
	if err != nil {
		t.Fatalf("reading the copied overlay: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the copied overlay: %v", err)
	}
	if len(doc.Content) == 0 {
		t.Fatal("the copied overlay parsed to an empty document")
	}
	images := mappingValue(doc.Content[0], "images")
	if images == nil || images.Kind != yaml.SequenceNode {
		t.Fatal("the local overlay declares no `images:` sequence; the emitter reads its keys out of that " +
			"block, so this gate is no longer reading what it claims to")
	}

	for _, o := range overrides {
		newName, newTag := splitImageRef(t, o.ref)
		var applied bool
		for _, entry := range images.Content {
			name := mappingValue(entry, "name")
			if name == nil || name.Value != o.name {
				continue
			}
			setMappingValue(entry, "newName", newName)
			setMappingValue(entry, "newTag", newTag)
			deleteMappingKey(entry, "digest")
			applied = true
		}
		if !applied {
			// Impossible unless the harness is broken: kustomize_image_overrides
			// DERIVES its keys from this very file. Loud rather than appended,
			// because an appended entry would silently make this gate assert
			// about an override the installer never emits.
			t.Fatalf("the override %q names an image the local overlay does not declare; the emitter reads "+
				"its keys out of that overlay, so either this harness parsed the wrong thing or the two "+
				"have come apart", o.name)
		}
	}

	edited, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatalf("re-encoding the copied overlay: %v", err)
	}
	if err := os.WriteFile(kustomization, edited, 0o644); err != nil {
		t.Fatalf("writing the copied overlay: %v", err)
	}
}

// renderInstalledOverlay is the whole subject of this file: the local overlay as
// an INSTALL renders it -- committed manifests plus the installer's overrides,
// nothing else.
func renderInstalledOverlay(t *testing.T) string {
	t.Helper()
	root := repoRootFromScriptsK3d(t)

	tmp := t.TempDir()
	copyTree(t, filepath.Join(root, "deploy", "k8s"), filepath.Join(tmp, "k8s"))

	overlay := filepath.Join(tmp, "k8s", "overlays", "local")
	applyArgoImageOverrides(t, filepath.Join(overlay, "kustomization.yaml"), installerImageOverrides(t))

	return renderKustomization(t, overlay)
}

// imageRef is one image reference in the rendered stream, with the field it came
// out of -- `image:` on a workload, `imageName:` on the CNPG Cluster.
type imageRef struct {
	field string
	value string
}

var renderedImageLine = regexp.MustCompile(`^\s*(image|imageName):\s*(\S+)\s*$`)

func imageRefsIn(t *testing.T, rendered string) []imageRef {
	t.Helper()
	var out []imageRef
	for _, line := range strings.Split(rendered, "\n") {
		if m := renderedImageLine.FindStringSubmatch(line); m != nil {
			out = append(out, imageRef{field: m[1], value: strings.Trim(m[2], `"'`)})
		}
	}
	if len(out) == 0 {
		t.Fatal("the rendered overlay names no images at all; this gate is no longer reading what it claims to")
	}
	return out
}

// splitImageRef splits `[registry[:port]/]name[:tag]` into name and tag. The tag
// is what follows the LAST colon, and only when no `/` follows it -- otherwise
// the colon belongs to a registry port and the reference carries no tag at all.
func splitImageRef(t *testing.T, ref string) (name, tag string) {
	t.Helper()
	// A digest pin (`name:tag@sha256:...` or `name@sha256:...`) is not a form
	// anything under test emits; splitting on it here keeps the tag parse honest
	// if one ever appears.
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i+1:], "/") {
		return ref, ""
	}
	return ref[:i], ref[i+1:]
}

// devOnlyTags reads the tags the COMMITTED overlay carries.
//
// Derived rather than listed, and that is the point: `local` and `16-dev` are
// not arbitrary bad words, they are precisely the tags `make dev` and
// `make db-image` produce on a developer's own machine. Whatever the overlay
// commits IS the set of tags an installer has never built, by construction --
// so a node added tomorrow with some third dev tag is covered without anybody
// remembering to extend a blocklist.
func devOnlyTags(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRootFromScriptsK3d(t),
		"deploy", "k8s", "overlays", "local", "kustomization.yaml"))
	if err != nil {
		t.Fatalf("reading the local overlay: %v", err)
	}
	var doc struct {
		Images []struct {
			NewTag string `yaml:"newTag"`
		} `yaml:"images"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the local overlay: %v", err)
	}
	out := map[string]bool{}
	for _, img := range doc.Images {
		if img.NewTag != "" {
			out[img.NewTag] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("the local overlay commits no image tags; this gate is no longer reading what it claims to")
	}
	// `latest` is committed nowhere and is still worth refusing: it is what an
	// image with no tag resolves to at pull time, which is how a silently
	// dropped override presents on the node rather than in the manifest.
	out["latest"] = true
	return out
}

// TestInstalledOverlayRendersOnlyPublishedImages is the broad half of the gate.
//
// Every image an install renders has to be one an install can PULL. The overlay
// commits `:local` and `16-dev` because they are right for the inner loop, and
// they are the reason the override mechanism exists at all (memql#3572): nobody
// installing a cluster has run `make dev`, so a survivor here is a pod in
// ImagePullBackOff -- and the install reports only that `workloadsReady` did not
// hold, which names nothing.
func TestInstalledOverlayRendersOnlyPublishedImages(t *testing.T) {
	rendered := renderInstalledOverlay(t)
	refs := imageRefsIn(t, rendered)
	dev := devOnlyTags(t)

	for _, ref := range refs {
		_, tag := splitImageRef(t, ref.value)
		switch {
		case tag == "" && !strings.Contains(ref.value, "@"):
			t.Errorf("%s: %q carries neither tag nor digest. Kubernetes resolves that to `:latest`, which "+
				"nothing in this project publishes, so the pod fails to pull -- and if this is the CNPG "+
				"Cluster it never gets that far, because the operator refuses an image whose PostgreSQL "+
				"version it cannot read.", ref.field, ref.value)
		case dev[tag]:
			t.Errorf("%s: %q still carries the dev-only tag %q after the installer's overrides. That image "+
				"exists only on a machine that has run `make dev` / `make db-image`; on an installed "+
				"cluster the pod sits in ImagePullBackOff and the install reports only that workloads "+
				"never became ready.", ref.field, ref.value, tag)
		}
	}
	t.Logf("rendered %d image references under the installer's overrides", len(refs))
}

// TestInstalledOverlayDatabaseImageCarriesAPostgresMajor is #4063 itself, at the
// level it happened.
//
// CNPG parses a PostgreSQL major off `Cluster.spec.imageName` and REFUSES what
// it cannot read. That refusal comes from an admission webhook, so the Cluster
// is never created at all: `memql-db-rw` never resolves, every node fails
// readiness against a DNS error, and the whole install reports that
// `workloadsReady` did not hold -- a verdict that names nothing and points at
// nothing. The distance between that symptom and this one line is why the check
// exists.
//
// THE SUBTLETY IS THE ENTIRE POINT. "The tag starts with a digit" is the
// seductive wrong encoding, and it is what the emitted-string gate in
// up_db_operand_image_test.go can afford (there, the negative half is carried by
// asserting the engine tag is absent). `0.19.0` -- the exact string that shipped
// the bug -- starts with a digit. What CNPG actually reads is the leading
// numeric component as a NUMBER, and refuses anything below 13. So that is what
// is encoded here.
func TestInstalledOverlayDatabaseImageCarriesAPostgresMajor(t *testing.T) {
	cluster, ok := findCnpgCluster(t, renderInstalledOverlay(t))
	if !ok {
		t.Fatal("the installed overlay renders no CNPG Cluster; either the database component came out of " +
			"the overlay or this gate is no longer reading what it claims to")
	}

	_, tag := splitImageRef(t, cluster)
	if tag == "" {
		t.Fatalf("the Cluster renders imageName=%q with no tag. Its `images:` override is not reaching "+
			"spec/imageName -- check that deploy/k8s/components/cnpg-db ships kustomizeconfig/images.yaml "+
			"and still lists it under `configurations:`; without that file the override is a silent no-op.",
			cluster)
	}

	major, err := leadingMajor(tag)
	if err != nil {
		t.Fatalf("the Cluster renders imageName=%q, whose tag %q begins with no number at all. CNPG reads "+
			"the PostgreSQL major off the front of the tag and refuses the Cluster through its admission "+
			"webhook when it cannot: no database is created, memql-db-rw never resolves, and the install "+
			"reports only that workloadsReady did not hold.", cluster, tag)
	}
	if major < minimumSupportedPostgresMajor {
		t.Errorf("the Cluster renders imageName=%q. CNPG reads PostgreSQL major %d off the tag %q and "+
			"supports %d or newer, so its admission webhook REFUSES the Cluster:\n"+
			"  Unsupported PostgreSQL version. Versions %d or newer are supported\n"+
			"No database is created, memql-db-rw never resolves, every node fails readiness on a DNS error, "+
			"and the install reports only that workloadsReady did not hold. This is memql#4063: the engine "+
			"release tag reached the database OPERAND, which is versioned on the PostgreSQL axis instead. "+
			"Note that %q does begin with a digit -- which is why the check is on the NUMBER.",
			cluster, major, tag, minimumSupportedPostgresMajor, minimumSupportedPostgresMajor, tag)
	}
}

// TestEveryInstallerOverrideReachesTheRenderedManifest is the no-op gate, and
// the one that keeps the two above honest.
//
// An `images:` entry that matches nothing is not an error in kustomize. It
// renders clean and changes nothing, which is the failure mode
// deploy/k8s/components/cnpg-db/kustomizeconfig/images.yaml exists to prevent
// for the CNPG Cluster: without that file the transformer has never heard of
// `spec/imageName`, so the operand keeps whatever the base said while the
// overlay and the emitted override both look exactly right.
//
// It doubles as this harness's own self-check. If the ArgoCD emulation in
// applyArgoImageOverrides silently stopped editing anything, every assertion
// above would go on reading the committed overlay -- and would still fail, but
// for reasons that point nowhere near the truth. This one fails by name.
func TestEveryInstallerOverrideReachesTheRenderedManifest(t *testing.T) {
	overrides := installerImageOverrides(t)
	rendered := renderInstalledOverlay(t)

	present := map[string]bool{}
	for _, ref := range imageRefsIn(t, rendered) {
		present[ref.value] = true
	}

	for _, o := range overrides {
		if present[o.ref] {
			continue
		}
		hint := ""
		if strings.Contains(o.name, "memql-db") {
			hint = "\nThis is the database operand, whose image lives at spec/imageName on a CNPG Cluster -- " +
				"a path kustomize's images transformer only knows about because " +
				"deploy/k8s/components/cnpg-db/kustomizeconfig/images.yaml teaches it. If that file is gone, " +
				"or the component stopped listing it under `configurations:`, the override is a SILENT no-op: " +
				"the render succeeds, the emitted line is perfect, and the operand keeps the base's untagged " +
				"image until CNPG or the kubelet refuses it."
		}
		t.Errorf("the installer overrides %q to %q, and no rendered manifest carries that reference. "+
			"The override matched nothing, which kustomize does not treat as an error.%s", o.name, o.ref, hint)
	}
}

// findCnpgCluster returns spec.imageName off the CNPG Cluster in a rendered
// stream.
//
// Decoded rather than grepped: `strings.Contains(rendered, "kind: Cluster")` is
// true of ClusterRole, ClusterRoleBinding and ClusterIssuer, and the front doors
// carry the last of those.
func findCnpgCluster(t *testing.T, rendered string) (string, bool) {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
			Spec       struct {
				ImageName string `yaml:"imageName"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return "", false
		}
		if err != nil {
			t.Fatalf("decoding the rendered manifests: %v", err)
		}
		if doc.Kind == "Cluster" && strings.HasPrefix(doc.APIVersion, "postgresql.cnpg.io/") {
			return doc.Spec.ImageName, true
		}
	}
}

// leadingMajor reads the PostgreSQL major off the front of a tag, the way CNPG
// does: the leading run of digits, up to the first `.` or `-`. `16` is 16,
// `16.15-timescaledb-2.29.1` is 16, and `0.19.0` is 0 -- not "fine, it starts
// with a digit".
func leadingMajor(tag string) (int, error) {
	end := strings.IndexAny(tag, ".-")
	if end < 0 {
		end = len(tag)
	}
	return strconv.Atoi(tag[:end])
}

// mappingValue returns the value node for key in a YAML mapping, or nil.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// setMappingValue sets key to a scalar value, appending the key if absent.
func setMappingValue(node *yaml.Node, key, value string) {
	if v := mappingValue(node, key); v != nil {
		v.SetString(value)
		return
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

// deleteMappingKey removes key and its value. `kustomize edit set image` does
// this to `digest` when it sets a tag: the two are alternatives, and leaving
// both would make the entry ambiguous.
func deleteMappingKey(node *yaml.Node, key string) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			return
		}
	}
}
