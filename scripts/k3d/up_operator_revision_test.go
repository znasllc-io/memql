package k3d

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Drive the actual registration function with a kubectl boundary that records
// every submitted Application. Applying main and then patching is too late:
// ArgoCD may already have started reconciling the first object.
func TestOperatorApplicationPinsItsFirstApply(t *testing.T) {
	for _, app := range []string{"cert-manager", "cnpg-operator"} {
		for _, tc := range []struct {
			name, repo, revision, want, failure string
		}{
			{"release", "https://github.com/znasllc-io/memql.git", "v0.24.0", "v0.24.0", ""},
			{"branch", "https://github.com/znasllc-io/memql.git", "codex/operator-pin", "codex/operator-pin", ""},
			{"json escaping", "https://github.com/znasllc-io/memql.git", "refs/heads/a\"b", "refs/heads/a\"b", ""},
			{"downstream", "https://github.com/example/product.git", "product-release", "main", ""},
			{"render failure", "https://github.com/znasllc-io/memql.git", "v0.24.0", "", "render"},
			{"apply failure", "https://github.com/znasllc-io/memql.git", "v0.24.0", "v0.24.0", "apply"},
		} {
			t.Run(app+"/"+tc.name, func(t *testing.T) {
				body, err := os.ReadFile(upDomainScript(t))
				if err != nil {
					t.Fatal(err)
				}
				src, _, found := strings.Cut(string(body), "\nfunction main()")
				if !found {
					t.Fatal("up.sh has no main boundary")
				}
				repo, err := filepath.Abs(filepath.Join("..", ".."))
				if err != nil {
					t.Fatal(err)
				}
				root := t.TempDir()
				for _, dir := range []string{"k3d", "bin"} {
					if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(filepath.Join(repo, "scripts", "lib"), filepath.Join(root, "lib")); err != nil {
					t.Fatal(err)
				}
				script := filepath.Join(root, "k3d", "harness.sh")
				src += `
REPO_ROOT="$MEMQL_OPERATOR_TEST_REPO"
REPO_URL="$MEMQL_OPERATOR_TEST_URL"
TARGET_REVISION="$MEMQL_OPERATOR_TEST_REVISION"
_register_operator_app "$MEMQL_OPERATOR_TEST_APP"
`
				if err := os.WriteFile(script, []byte(src), 0o755); err != nil {
					t.Fatal(err)
				}
				shim := "#!/bin/sh\nMEMQL_OPERATOR_KUBECTL_HELPER=1 exec \"$MEMQL_OPERATOR_TEST_BINARY\" -test.run=^TestOperatorKubectlHelper$ -- \"$@\"\n"
				if err := os.WriteFile(filepath.Join(root, "bin", "kubectl"), []byte(shim), 0o755); err != nil {
					t.Fatal(err)
				}
				binary, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				// /bin/bash is the real Bash 3.2 on supported macOS hosts. Linux
				// runs its native Bash; no cluster or kubectl installation is needed.
				cmd := exec.Command("/bin/bash", script)
				applied := filepath.Join(root, "applied.jsonl")
				cmd.Env = append(os.Environ(),
					"PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
					"MEMQL_OPERATOR_TEST_REPO="+repo, "MEMQL_OPERATOR_TEST_URL="+tc.repo,
					"MEMQL_OPERATOR_TEST_REVISION="+tc.revision, "MEMQL_OPERATOR_TEST_APP="+app,
					"MEMQL_OPERATOR_TEST_BINARY="+binary, "MEMQL_OPERATOR_TEST_APPLIED="+applied,
					"MEMQL_OPERATOR_TEST_FAILURE="+tc.failure)
				out, runErr := cmd.CombinedOutput()
				if (runErr != nil) != (tc.failure != "") {
					t.Fatalf("registration: %v\n%s", runErr, out)
				}
				data, err := os.ReadFile(applied)
				if tc.failure == "render" {
					if !os.IsNotExist(err) {
						t.Fatalf("failed local render reached apply: %q, %v", data, err)
					}
					return
				}
				if err != nil || strings.Count(string(data), "\n") != 1 {
					t.Fatalf("want exactly one applied Application: %q, %v", data, err)
				}
				var got map[string]any
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				original, err := os.ReadFile(filepath.Join(repo, "deploy", "argocd", "apps", app+".yaml"))
				if err != nil {
					t.Fatal(err)
				}
				var want map[string]any
				if err := yaml.Unmarshal(original, &want); err != nil {
					t.Fatal(err)
				}
				want["spec"].(map[string]any)["source"].(map[string]any)["targetRevision"] = tc.want
				// JSON-normalize numeric YAML fields before comparing the complete
				// object: all committed policy, namespace and source fields survive.
				encoded, _ := json.Marshal(want)
				json.Unmarshal(encoded, &want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("first apply changed the committed contract or lost its revision:\n%s", data)
				}
			})
		}
	}
}

// The helper implements only local rendering and a captured apply. Any remote
// patch or other kubectl operation fails, so these tests cannot touch a cluster.
func TestOperatorKubectlHelper(t *testing.T) {
	if os.Getenv("MEMQL_OPERATOR_KUBECTL_HELPER") != "1" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	flag := func(name string) string {
		for i, arg := range args {
			if arg == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	if len(args) == 0 || (args[0] != "apply" && (args[0] != "patch" || !strings.Contains(strings.Join(args, " "), "--local"))) {
		os.Exit(91)
	}
	var data []byte
	var err error
	if flag("-f") == "-" {
		data, err = os.ReadFile("/dev/stdin")
	} else {
		data, err = os.ReadFile(flag("-f"))
	}
	if err != nil {
		os.Exit(92)
	}
	var object map[string]any
	if yaml.Unmarshal(data, &object) != nil {
		os.Exit(93)
	}
	if args[0] == "patch" {
		var patch struct {
			Spec struct {
				Source struct{ TargetRevision string }
			}
		}
		if json.Unmarshal([]byte(flag("-p")), &patch) != nil || patch.Spec.Source.TargetRevision == "" || flag("-o") != "yaml" {
			os.Exit(94)
		}
		object["spec"].(map[string]any)["source"].(map[string]any)["targetRevision"] = patch.Spec.Source.TargetRevision
		encoded, _ := yaml.Marshal(object)
		os.Stdout.Write(encoded)
		// Even output followed by a render error must never be applied.
		if os.Getenv("MEMQL_OPERATOR_TEST_FAILURE") == "render" {
			os.Exit(17)
		}
	} else {
		file, err := os.OpenFile(os.Getenv("MEMQL_OPERATOR_TEST_APPLIED"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil || json.NewEncoder(file).Encode(object) != nil {
			os.Exit(95)
		}
		file.Close()
		if os.Getenv("MEMQL_OPERATOR_TEST_FAILURE") == "apply" {
			os.Exit(19)
		}
	}
	os.Exit(0)
}
