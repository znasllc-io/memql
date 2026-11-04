package k3d

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The local cluster runs production's pipeline queue (memql#6387): the Kueue
// operator, and deploy/k8s/components/pipelines-capacity composed by the local
// overlay. These hold the two halves up.sh owns: the AppProject that lets the
// overlay's cluster-scoped queue objects through, and the Application settings
// that keep the Cluster app's capacity saves from being undone.

// localProjectManifest is the AppProject `make up` applies by default. It is
// the shared deploy/argocd/apps/project.yaml plus the queue's cluster-scoped
// kinds, and lives outside deploy/argocd/apps/ because root.yaml renders that
// directory on the cloud, which composes none of them.
const localProjectManifest = "deploy/argocd/local/project.yaml"

// queueClusterKinds are the cluster-scoped kinds pipelines-capacity renders,
// as AppProject whitelist entries. Exactly these: a wider list would let the
// mesh Application create any ClusterRoleBinding, on a cluster whose mesh
// otherwise holds no cluster-wide grant.
var queueClusterKinds = []map[string]string{
	{"group": "kueue.x-k8s.io", "kind": "ClusterQueue"},
	{"group": "kueue.x-k8s.io", "kind": "ResourceFlavor"},
	{"group": "kueue.x-k8s.io", "kind": "WorkloadPriorityClass"},
	{"group": "capacity.memql.io", "kind": "BackgroundCapacityPolicy"},
	{"group": "admissionregistration.k8s.io", "kind": "ValidatingAdmissionPolicy"},
	{"group": "admissionregistration.k8s.io", "kind": "ValidatingAdmissionPolicyBinding"},
	{"group": "rbac.authorization.k8s.io", "kind": "ClusterRole"},
	{"group": "rbac.authorization.k8s.io", "kind": "ClusterRoleBinding"},
}

func readYAMLMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := singleYAMLDocument(data)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc
}

// singleYAMLDocument decodes a file that must hold exactly one document. up.sh
// hands the project file to `kubectl apply -f` whole, with the person's own
// credentials, so a second document in it would reach the cluster unchecked.
func singleYAMLDocument(data []byte) (map[string]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("holds more than one YAML document, and every one of them is applied: %v", err)
	}
	return doc, nil
}

func TestTheProjectFileMayHoldOneDocumentOnly(t *testing.T) {
	if _, err := singleYAMLDocument([]byte("kind: AppProject\n---\nkind: ClusterRoleBinding\n")); err == nil {
		t.Error("a second document was accepted")
	}
	if _, err := singleYAMLDocument([]byte("kind: AppProject\n")); err != nil {
		t.Errorf("one document refused: %v", err)
	}
}

func whitelist(t *testing.T, project map[string]any) []map[string]string {
	t.Helper()
	spec, _ := project["spec"].(map[string]any)
	raw, _ := spec["clusterResourceWhitelist"].([]any)
	var out []map[string]string
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("clusterResourceWhitelist entry %v is not a mapping", item)
		}
		group, _ := entry["group"].(string)
		kind, _ := entry["kind"].(string)
		out = append(out, map[string]string{"group": group, "kind": kind})
	}
	return out
}

// Held to the shared file, so the two cannot drift apart on a source repo, a
// destination or a namespaced kind: a change to the cloud's project that never
// reached local's would be a difference in what each cluster is allowed to run.
func TestTheLocalAppProjectIsTheSharedOnePlusTheQueueKinds(t *testing.T) {
	root := repoRoot(t)
	shared := readYAMLMap(t, filepath.Join(root, "deploy", "argocd", "apps", "project.yaml"))
	local := readYAMLMap(t, filepath.Join(root, filepath.FromSlash(localProjectManifest)))

	if want := append(whitelist(t, shared), queueClusterKinds...); !reflect.DeepEqual(whitelist(t, local), want) {
		t.Errorf("%s whitelists %v, want the shared project's %v followed by exactly the queue's kinds %v",
			localProjectManifest, whitelist(t, local), whitelist(t, shared), queueClusterKinds)
	}
	for _, project := range []map[string]any{shared, local} {
		delete(project["spec"].(map[string]any), "clusterResourceWhitelist")
	}
	if !reflect.DeepEqual(shared, local) {
		t.Errorf("%s differs from deploy/argocd/apps/project.yaml in more than its cluster whitelist:\nshared: %v\nlocal:  %v",
			localProjectManifest, shared, local)
	}
}

// runApplyArgoApp drives the real apply_argocd_app against a kubectl that
// records every call and keeps every manifest handed to it on stdin.
func runApplyArgoApp(t *testing.T, snippet string) (calls, stdin string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRoot(t)
	dir := t.TempDir()
	fake := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> \"$FAKE_LOG\"\ncase \"$*\" in\n" +
		"  'apply -f -') { cat; printf '\\n---\\n'; } >> \"$FAKE_STDIN\" ;;\n" +
		"  *'get application'*) printf 'Synced\\n\\n.' ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	log, in := filepath.Join(dir, "calls"), filepath.Join(dir, "stdin")
	cmd := exec.Command("bash", "-c", "set -uo pipefail\nsource \"$UP_SH\"\n"+snippet+"\napply_argocd_app\n")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"UP_SH="+filepath.Join(root, "scripts", "k3d", "up.sh"),
		"FAKE_LOG="+log, "FAKE_STDIN="+in,
		"MEMQL_K3D_PROJECT_MANIFEST=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("apply_argocd_app: %v\n%s", err, out)
	}
	c, _ := os.ReadFile(log)
	s, _ := os.ReadFile(in)
	return string(c), string(s)
}

const engineApplication = `REPO_ROOT="$PWD"
TARGET_REVISION=main
APP_COMPARE_TIMEOUT=30
`

func TestUpAppliesTheLocalAppProjectByDefault(t *testing.T) {
	calls, _ := runApplyArgoApp(t, engineApplication)
	want := "apply -f " + filepath.Join(repoRoot(t), filepath.FromSlash(localProjectManifest))
	if !strings.Contains(calls, want+"\n") {
		t.Fatalf("apply_argocd_app did not apply %s by default, so the overlay's queue objects are refused at sync:\n%s",
			localProjectManifest, calls)
	}
	// A downstream stack still names its own.
	calls, _ = runApplyArgoApp(t, engineApplication+"PROJECT_MANIFEST=/elsewhere/project.yaml\n")
	if !strings.Contains(calls, "apply -f /elsewhere/project.yaml\n") || strings.Contains(calls, localProjectManifest) {
		t.Fatalf("an explicit --project-manifest must replace the default, not join it:\n%s", calls)
	}
}

// olderCheckout is a checkout from before the local queue: the operator and
// project manifests every release carries, and no deploy/argocd/local/. The
// installer runs THIS script against a cloned release (--repo-root), so a
// release that predates the queue is exactly this shape.
func olderCheckout(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	dir := t.TempDir()
	for _, rel := range []string{"deploy/argocd/apps/project.yaml", "deploy/argocd/apps/cert-manager.yaml", "deploy/argocd/apps/cnpg-operator.yaml", "deploy/kueue/application.yaml"} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A release that predates the queue is installed as it was: its overlay renders
// no queue object, so it gets the shared project, which every release carries,
// rather than a file it does not have (the apply would fail and stop the install).
func TestUpAppliesTheSharedProjectToACheckoutWithoutTheLocalQueue(t *testing.T) {
	older := olderCheckout(t)
	calls, _ := runApplyArgoApp(t, "REPO_ROOT="+older+"\nTARGET_REVISION=v0.31.12\nAPP_COMPARE_TIMEOUT=30\n")
	if want := "apply -f " + filepath.Join(older, "deploy", "argocd", "apps", "project.yaml") + "\n"; !strings.Contains(calls, want) {
		t.Fatalf("a checkout without %s must get the shared project:\n%s", localProjectManifest, calls)
	}
}

// Kueue is registered only for a checkout whose overlay needs it. On an older
// release its webhooks fail closed on every Job in memql-pipelines, so registering
// it there would bring back the controller-outage failure memql#6396 removed, on a
// cluster with no queue to gain from it.
func TestKueueIsRegisteredOnlyForACheckoutWithTheLocalQueue(t *testing.T) {
	stack := `
function _register_operator_app() { printf 'register %s\n' "$1" >> "$FAKE_KUBECTL_LOG"; }
install_operator_stack
echo "ready=${OPERATOR_STACK_READY}"
`
	for _, tc := range []struct {
		name, root string
		kueue      bool
	}{
		{"with the local queue", repoRoot(t), true},
		{"an older checkout", olderCheckout(t), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code, calls := sourceRecovery(t, "REPO_ROOT="+tc.root+"\n"+stack)
			if code != 0 || !strings.Contains(out, "ready=true") {
				t.Fatalf("operator stack: %d\n%s\n%s", code, out, calls)
			}
			registered := strings.Contains(calls, "register deploy/kueue/application.yaml")
			waited := strings.Contains(calls, "rollout status deployment/kueue-controller-manager")
			if registered != tc.kueue || waited != tc.kueue {
				t.Fatalf("Kueue registered=%v waited=%v, want %v:\n%s", registered, waited, tc.kueue, calls)
			}
			if !strings.Contains(calls, "register deploy/argocd/apps/cnpg-operator.yaml") {
				t.Fatalf("the older operators must register either way:\n%s", calls)
			}
		})
	}
}

// The Cluster app saves the queue's budget and its admission switch by patching
// the ClusterQueue; the Application must ignore exactly those fields, as the
// first installation's does, or selfHeal reverts every save. Pool selection,
// namespace selection, preemption and the ceiling stay reconciled.
func TestTheMeshApplicationLeavesTheQueuesOwnerControlsAlone(t *testing.T) {
	_, stdin := runApplyArgoApp(t, engineApplication)
	var app struct {
		Kind string
		Spec struct {
			SyncPolicy struct {
				SyncOptions []string `yaml:"syncOptions"`
			} `yaml:"syncPolicy"`
			IgnoreDifferences []struct {
				Group, Kind, Name string
				JSONPointers      []string `yaml:"jsonPointers"`
			} `yaml:"ignoreDifferences"`
		}
	}
	found := false
	for _, doc := range strings.Split(stdin, "\n---\n") {
		if err := yaml.Unmarshal([]byte(doc), &app); err == nil && app.Kind == "Application" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no Application reached kubectl apply:\n%s", stdin)
	}
	want := []string{
		"/spec/stopPolicy",
		"/spec/resourceGroups/0/flavors/0/resources/0/nominalQuota",
		"/spec/resourceGroups/0/flavors/0/resources/1/nominalQuota",
		"/spec/resourceGroups/0/flavors/0/resources/2/nominalQuota",
		"/spec/resourceGroups/0/flavors/0/resources/3/nominalQuota",
		"/metadata/annotations/memql.io~1capacity-change",
	}
	var queue []string
	for _, d := range app.Spec.IgnoreDifferences {
		if d.Group == "kueue.x-k8s.io" && d.Kind == "ClusterQueue" {
			if d.Name != "memql-background" {
				t.Errorf("the ClusterQueue entry names %q; an unnamed or other entry would release every queue's fields", d.Name)
			}
			queue = append(queue, d.JSONPointers...)
		}
	}
	if !reflect.DeepEqual(queue, want) {
		t.Errorf("the ClusterQueue's ignored fields are %v, want %v", queue, want)
	}
	respect := false
	for _, o := range app.Spec.SyncPolicy.SyncOptions {
		respect = respect || o == "RespectIgnoreDifferences=true"
	}
	if !respect {
		t.Error("without RespectIgnoreDifferences=true a sync still writes the ignored fields back")
	}
}
