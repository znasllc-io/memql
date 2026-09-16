package k3d

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise both real drivers through the ingress configuration seam. Creation,
// Docker builds and Argo are stubbed; kubectl captures the actual Helm values.
// An existing cluster takes the same up path, since create_cluster returns
// without doing anything and configuration must still follow it.
func TestUpAndDevConfigureStreamingRequestsOnExistingClusters(t *testing.T) {
	for _, driver := range []string{"up", "dev"} {
		t.Run(driver, func(t *testing.T) {
			root := repoRoot(t)
			tmp := t.TempDir()
			manifest := filepath.Join(tmp, "manifest")
			args := filepath.Join(tmp, "args")
			fake := `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$TRAEFIK_ARGS"
case "$*" in
 *"apply -f -"*|*"create -f -"*) cat >> "$TRAEFIK_MANIFEST"; exit "${TRAEFIK_APPLY_EXIT:-0}" ;;
esac
exit 0
`
			if err := os.WriteFile(filepath.Join(tmp, "kubectl"), []byte(fake), 0755); err != nil {
				t.Fatal(err)
			}
			script := `set -euo pipefail
source "$TRAEFIK_DRIVER"
check_prerequisites(){ :; }
create_cluster(){ :; }
install_argocd(){ exit 0; }
ensure_db_image(){ :; }
prewarm_build_frontend(){ :; }
build_and_import_nodes(){ :; }
point_application_at_local_images(){ exit 0; }
main --cluster=timeout-fixture --node=bff --image-source=checkout --repo-root="$TRAEFIK_ROOT"
`
			// Up's closed parameter set does not contain dev's build options.
			if driver == "up" {
				script = strings.Replace(script, " --node=bff --image-source=checkout", "", 1)
			}
			cmd := exec.Command("bash", "-c", script)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"), "TRAEFIK_DRIVER="+filepath.Join(root, "scripts", "k3d", driver+".sh"), "TRAEFIK_ROOT="+root, "TRAEFIK_MANIFEST="+manifest, "TRAEFIK_ARGS="+args)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("driver failed: %v\n%s", err, out)
			}
			raw, err := os.ReadFile(manifest)
			if err != nil {
				t.Fatalf("%s did not reconcile ingress config for existing cluster: %v", driver, err)
			}
			text := string(raw)
			for _, want := range []string{"kind: HelmChartConfig", "name: traefik", "namespace: kube-system", "allowExternalNameServices: true", "readTimeout: 0s"} {
				if !strings.Contains(text, want) {
					t.Errorf("%s omitted %q:\n%s", driver, want, text)
				}
			}
			call, _ := os.ReadFile(args)
			if !strings.Contains(string(call), "--context k3d-timeout-fixture") {
				t.Fatalf("ingress update did not explicitly target selected local cluster: %s", call)
			}

			// A failed apply must stop the real driver with its operation-failure
			// code, rather than continuing to report a working installation.
			failed := exec.Command("bash", "-c", script)
			failed.Dir = root
			failed.Env = append(cmd.Env, "TRAEFIK_APPLY_EXIT=23")
			failureOutput, failure := failed.CombinedOutput()
			status, ok := failure.(*exec.ExitError)
			if !ok || status.ExitCode() != 5 {
				t.Fatalf("failed ingress apply did not stop %s: %v\n%s", driver, failure, failureOutput)
			}
			if driver == "dev" {
				if err := os.Remove(manifest); err != nil {
					t.Fatal(err)
				}
				failedBuild := exec.Command("bash", "-c", strings.Replace(script, "build_and_import_nodes(){ :; }", "build_and_import_nodes(){ cap_fail 5 'fixture build failure'; }", 1))
				failedBuild.Dir = root
				failedBuild.Env = cmd.Env
				_, err := failedBuild.CombinedOutput()
				if err == nil {
					t.Fatal("build unexpectedly succeeded")
				}
				if _, err := os.Stat(manifest); !os.IsNotExist(err) {
					t.Fatal("failed rebuild changed ingress")
				}
			}
		})
	}
}

func TestStreamingIngressPolicyHasOneSource(t *testing.T) {
	root := repoRoot(t)
	for _, driver := range []string{"up", "dev"} {
		raw, err := os.ReadFile(filepath.Join(root, "scripts", "k3d", driver+".sh"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "kind: HelmChartConfig") {
			t.Fatalf("%s still carries its own ingress policy instead of the shared helper", driver)
		}
	}
}

func TestStreamingIngressPreservesExistingChartConfiguration(t *testing.T) {
	root := repoRoot(t)
	for _, hasContent := range []bool{false, true} {
		for _, existing := range []bool{false, true} {
			t.Run(map[bool]string{false: "first_overlay", true: "already_referenced"}[existing], func(t *testing.T) {
				tmp := t.TempDir()
				fake := `#!/usr/bin/env bash
case "$*" in
 *" get helmchartconfig "*) printf '42\n[{"name":"operator-values","keys":["custom.yaml"],"ignoreUpdates":true}]\n%s\n' "$TRAEFIK_OWN_REF"; printf '%s' "$TRAEFIK_CONTENT" ;;
 *"apply -f -"*) cat > "$TRAEFIK_MANIFEST" ;;
 *" patch helmchartconfig "*) printf '%s' "${@: -1}" > "$TRAEFIK_PATCH" ;;
 *) printf 'unexpected kubectl operation: %s\n' "$*" >&2; exit 99 ;;
esac
`
				if err := os.WriteFile(filepath.Join(tmp, "kubectl"), []byte(fake), 0755); err != nil {
					t.Fatal(err)
				}
				owned := ""
				if existing {
					owned = "memql-local-traefik"
				}
				cmd := exec.Command("bash", "-c", `source scripts/lib/capability.sh; source scripts/lib/local_traefik.sh; ensure_local_traefik test`)
				cmd.Dir = root
				content := ""
				if hasContent {
					content = "operator: preserve\nmore: values"
				}
				cmd.Env = append(os.Environ(), "TRAEFIK_CONTENT="+content, "PATH="+tmp+":"+os.Getenv("PATH"), "TRAEFIK_OWN_REF="+owned, "TRAEFIK_MANIFEST="+filepath.Join(tmp, "manifest"), "TRAEFIK_PATCH="+filepath.Join(tmp, "patch"))
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("helper: %v\n%s", err, out)
				}
				raw, err := os.ReadFile(filepath.Join(tmp, "manifest"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), "kind: HelmChartConfig") {
					t.Fatal("apply can overwrite existing valuesContent or references")
				}
				for _, want := range []string{"kind: Secret", "name: memql-local-traefik", "allowExternalNameServices: true", "readTimeout: 0s"} {
					if !strings.Contains(string(raw), want) {
						t.Errorf("missing %q", want)
					}
				}
				raw, err = os.ReadFile(filepath.Join(tmp, "patch"))
				if existing && hasContent {
					if !os.IsNotExist(err) {
						t.Fatal("already-referenced overlay changed chart")
					}
					return
				}
				if err != nil {
					t.Fatal("no preserving chart patch:", err)
				}
				var patch struct {
					Metadata map[string]any `json:"metadata"`
					Spec     map[string]any `json:"spec"`
				}
				if err = json.Unmarshal(raw, &patch); err != nil {
					t.Fatal(err)
				}
				if patch.Metadata["resourceVersion"] != "42" {
					t.Fatal("patch may overwrite a concurrent operator update")
				}
				if !hasContent {
					if patch.Spec["valuesContent"] != "{}" {
						t.Fatalf("empty inline values must enable the older controller's Secret projection: %s", raw)
					}
				} else if _, changed := patch.Spec["valuesContent"]; changed {
					t.Fatal("overwrote operator valuesContent")
				}
				if existing {
					if _, changed := patch.Spec["valuesSecrets"]; changed {
						t.Fatal("changed existing Secret references")
					}
					return
				}
				refs, ok := patch.Spec["valuesSecrets"].([]any)
				if !ok || len(refs) != 2 {
					t.Fatalf("lost existing values references: %s", raw)
				}
				first := refs[0].(map[string]any)
				if first["name"] != "operator-values" || first["ignoreUpdates"] != true || first["keys"].([]any)[0] != "custom.yaml" {
					t.Fatalf("changed operator reference: %s", raw)
				}
				own := refs[1].(map[string]any)
				if own["name"] != "memql-local-traefik" || own["keys"].([]any)[0] != "values.yaml" {
					t.Fatalf("missing overlay reference: %s", raw)
				}
			})
		}
	}
}
