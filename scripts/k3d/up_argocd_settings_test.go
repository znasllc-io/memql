package k3d

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// up_argocd_settings_test.go -- memql#5492.
//
// THE GAP. install_argocd skips `kubectl apply -k deploy/argocd/bootstrap` when
// Argo CD is already installed, and keeps skipping it: the apply re-fetches the
// upstream install.yaml over the network on every bring-up. But skipping the
// apply also skipped every SETTING the bootstrap gained after the cluster was
// made -- the PersistentVolumeClaim health check in argocd-cm
// (deploy/argocd/bootstrap/pvc-health.yaml) never reached an existing dev
// cluster, whose Application then sat at Progressing on the unbound pipelines
// cache for as long as no pipeline had run.
//
// The repair, pinned here: the skip branch merges that file into the live
// argocd-cm with a JSON merge patch (it touches only the keys the file names),
// the fresh branch keeps getting it through the kustomization, and a merge
// that fails warns with the repair command rather than failing the bring-up.

// kubectlCalls runs install_argocd against the fake kubectl and returns the
// output, the exit code and every kubectl argv it recorded.
func kubectlCalls(t *testing.T, env ...string) (string, int, []string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "kubectl.log")
	out, code := runUpFunc(t, "install_argocd\n", append([]string{"FAKE_KUBECTL_LOG=" + log}, env...)...)
	body, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the fake kubectl recorded nothing at %s (%v); install_argocd never called it\noutput:\n%s", log, err, out)
	}
	return out, code, strings.Split(strings.TrimSpace(string(body)), "\n")
}

func settingsFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "deploy", "argocd", "bootstrap", "pvc-health.yaml")
}

func anyCall(calls []string, needle string) bool {
	for _, c := range calls {
		if strings.Contains(c, needle) {
			return true
		}
	}
	return false
}

// THE SKIP BRANCH, the one this is about: an Argo CD that is already there gets
// the bootstrap's argocd-cm setting merged in, and the bootstrap is still not
// re-applied.
func TestAnInstalledArgoCDStillGetsTheBootstrapSettings(t *testing.T) {
	out, code, calls := kubectlCalls(t)
	if code != 0 {
		t.Fatalf("install_argocd failed (exit %d) against an Argo CD that is installed and ready\noutput:\n%s", code, out)
	}
	want := "-n argocd patch configmap argocd-cm --type merge --patch-file " + settingsFile(t)
	if !anyCall(calls, want) {
		t.Errorf("the already-installed branch did not merge the bootstrap's setting into argocd-cm.\n"+
			"want a call containing:\n  kubectl %s\nrecorded:\n  %s\nA dev cluster made before the setting existed then never gets it, and its "+
			"Application stays Progressing on the unbound pipelines cache.", want, strings.Join(calls, "\n  "))
	}
	if anyCall(calls, "apply -k") {
		t.Errorf("the already-installed branch re-applied the bootstrap, which re-fetches the upstream install.yaml "+
			"over the network on every bring-up; it merges the setting instead\nrecorded:\n  %s", strings.Join(calls, "\n  "))
	}
	if _, err := os.Stat(settingsFile(t)); err != nil {
		t.Errorf("the file install_argocd merges does not exist: %v", err)
	}
}

// THE FRESH BRANCH is unchanged: the kustomization carries the same file, so
// there is no separate merge, and the bootstrap still lists it.
func TestAFreshArgoCDGetsTheSettingsThroughTheBootstrap(t *testing.T) {
	out, code, calls := kubectlCalls(t, "FAKE_ARGOCD_ABSENT=1")
	if code != 0 {
		t.Fatalf("install_argocd failed (exit %d) on a cluster with no Argo CD\noutput:\n%s", code, out)
	}
	if !anyCall(calls, "apply -k "+filepath.Join(repoRoot(t), "deploy", "argocd", "bootstrap")) {
		t.Errorf("a fresh install did not apply the bootstrap\nrecorded:\n  %s", strings.Join(calls, "\n  "))
	}
	if anyCall(calls, "patch configmap argocd-cm") {
		t.Errorf("a fresh install merged the setting separately; the bootstrap it just applied already carries it\n"+
			"recorded:\n  %s", strings.Join(calls, "\n  "))
	}
	kustomization, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "argocd", "bootstrap", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kustomization), "- path: pvc-health.yaml") {
		t.Error("deploy/argocd/bootstrap/kustomization.yaml does not list pvc-health.yaml under patches, so a fresh " +
			"install never gets the setting the already-installed branch merges")
	}
}

// NOT FATAL: the setting changes how Argo CD reports one claim's health, not
// whether the cluster comes up. A merge that fails is a warning naming the
// command that repairs it, and the bring-up continues to the readiness wait.
func TestAFailedSettingsMergeWarnsAndDoesNotFailTheBringUp(t *testing.T) {
	out, code, calls := kubectlCalls(t, "FAKE_PATCH_EXIT=1")
	if code != 0 {
		t.Fatalf("a failed argocd-cm merge failed the bring-up (exit %d); it should warn and continue\noutput:\n%s", code, out)
	}
	repair := "kubectl -n argocd patch configmap argocd-cm --type merge --patch-file " + settingsFile(t)
	if !strings.Contains(out, repair) {
		t.Errorf("the warning does not name the command that repairs it:\n  %s\noutput:\n%s", repair, out)
	}
	if !anyCall(calls, "rollout status deployment/argocd-server") {
		t.Errorf("after the failed merge the readiness wait never ran\nrecorded:\n  %s", strings.Join(calls, "\n  "))
	}
}
