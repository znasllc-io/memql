package pipelinesteps

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Exercise the API server's authorization decision, not just a rendered YAML
// comparison. These are fresh test identities in a disposable local namespace.
func TestPipelineRunnerGrantIsWorkbenchOnlyOnLocalCluster(t *testing.T) {
	_, _, ns, kubectl := localPipelineControls(t)
	for _, sa := range []string{"memql-engine", "memql-engine-workbench", "memql-pipelines-step", "memql-deploy"} {
		kubectl(nil, "-n", ns, "create", "serviceaccount", sa)
	}
	manifest, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "components", "pipelines", "rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest = bytes.ReplaceAll(manifest, []byte("namespace: memql-pipelines"), []byte("namespace: "+ns))
	manifest = regexp.MustCompile(`(?m)^(\s*namespace: )memql$`).ReplaceAll(manifest, []byte("${1}"+ns))
	kubectl(manifest, "apply", "-f", "-")
	for _, sa := range []string{"memql-engine", "memql-engine-workbench", "memql-pipelines-step", "memql-deploy"} {
		for _, action := range [][2]string{{"create", "jobs.batch"}, {"get", "secrets"}, {"patch", "pods"}, {"list", "networkpolicies.networking.k8s.io"}} {
			// SelfSubjectAccessReview returns a normal JSON response for both
			// decisions, so a refused request is distinct from a kubectl error.
			group, resource := "", action[1]
			switch resource {
			case "jobs.batch":
				group, resource = "batch", "jobs"
			case "networkpolicies.networking.k8s.io":
				group, resource = "networking.k8s.io", "networkpolicies"
			}
			body := []byte(`{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","spec":{"resourceAttributes":{"namespace":"` + ns + `","verb":"` + action[0] + `","group":"` + group + `","resource":"` + resource + `"}}}`)
			answer := kubectl(body, "--as=system:serviceaccount:"+ns+":"+sa, "create", "--validate=false", "-f", "-", "-o", "jsonpath={.status.allowed}")
			allowed := strings.TrimSpace(string(answer)) == "true"
			if allowed != (sa == "memql-engine-workbench") {
				t.Errorf("%s can %s %s = %s", sa, action[0], action[1], answer)
			}
		}
	}
}
