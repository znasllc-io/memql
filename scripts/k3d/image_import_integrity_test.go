package k3d

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const imageIntegrityHarness = `
IMAGE_SOURCE=checkout
export FAKE_IMAGE_A=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export FAKE_IMAGE_B=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
function docker() {
 printf 'docker %s\n' "$*" >> "$FAKE_CALLS"
 case "$1 $2" in
  'build '*) return 9 ;;
 esac
 case "$1" in
  build)
   local iid='' tag=''
   while [ "$#" -gt 0 ]; do
    case "$1" in --iidfile) iid="$2"; shift ;; --tag) tag="$2"; shift ;; esac
    shift
   done
   [ -z "$iid" ] || printf '%s\n' "$FAKE_IMAGE_A" > "$iid"
   if [ "$tag" = memql-agent:local ] && [ "${FAKE_CHANGED_BUILD:-}" = 1 ]; then touch "$FAKE_STATE"; fi
   return 0 ;;
  pull) return 0 ;;
  image)
   case "$*" in *RootFS.Layers*) echo "$FAKE_IMAGE_A"; return ;; esac
   case "$*" in *memql-bff:local*) if [ -f "$FAKE_STATE" ]; then echo "$FAKE_IMAGE_B"; return; fi ;; esac
   echo "$FAKE_IMAGE_A" ;;
  create) printf '%064d\n' 1 ;;
  cp)
   [ "${FAKE_COPY_FAIL:-}" != 1 ] || return 1
   cp "$FAKE_FILES/${2##*/}" "$3" ;;
  rm) return 0 ;;
  ps)
   [ "${FAKE_LIST_FAIL:-}" != 1 ] || return 1
   [ "${FAKE_EMPTY:-}" != 1 ] || return 0
   printf 'k3d-memql-server-0 server running\nk3d-memql-server-1 server running\nk3d-memql-agent-0 agent %s\nk3d-memql-serverlb loadbalancer running\nk3d-memql-tools tools running\n' "${FAKE_AGENT_STATE:-running}" ;;
  exec)
   local node="$2"
   local imported="${FAKE_IMPORTED}.${node}"
   [ "$node" != "${FAKE_EXEC_FAIL:-}" ] || return 1
   if [ "$3" = sh ]; then shift 2; FAKE_NODE="$node" "$@"; return; fi
   case "$*" in
    *'images ls'*)
     [ -f "$imported" ] || { echo 'REF TYPE DIGEST SIZE'; return 0; }
     local id="$FAKE_IMAGE_A"
     [ "$node" != "${FAKE_BAD_NODE:-}" ] || id="$FAKE_IMAGE_B"
     printf 'REF TYPE DIGEST SIZE\ndocker.io/library/memql-bff:local application/vnd.oci.image.index.v1+json %s 1MiB\n' "$id" ;;
    *'images check'*) [ -f "$imported" ] && [ "$node" != "${FAKE_INCOMPLETE_NODE:-}" ] || return 0; echo docker.io/library/memql-bff:local ;;
    *'crictl inspecti'*)
     [ -f "$imported" ] || return 1
     if [ "${FAKE_CLASSIC:-}" = 1 ]; then echo "$FAKE_IMAGE_A"; else echo "$FAKE_IMAGE_B"; fi ;;
    *) return 8 ;;
   esac ;;
  *) return 7 ;;
 esac
}
function k3d() {
 printf 'k3d %s\n' "$*" >> "$FAKE_CALLS"
 if [ "${FAKE_IMPORT_DRIFT:-}" = 1 ]; then touch "$FAKE_STATE"; fi
 if [ "${FAKE_BAD_NODE:-}" != '' ]; then echo 'ctr: short read: unexpected EOF' >&2; fi
 local count=0
 [ ! -f "$FAKE_IMPORT_COUNT" ] || count="$(cat "$FAKE_IMPORT_COUNT")"
 count=$((count + 1))
 printf '%s\n' "$count" > "$FAKE_IMPORT_COUNT"
 if [ "${FAKE_IMPORT_FAIL_AFTER_COMPLETE:-}" = 1 ]; then
  for node in k3d-memql-server-0 k3d-memql-server-1 k3d-memql-agent-0; do touch "${FAKE_IMPORTED}.${node}"; done
  return 1
 fi
 if [ "${FAKE_FAIL_FIRST_IMPORT:-}" = 1 ] && [ "$count" = 1 ]; then
  touch "${FAKE_IMPORTED}.k3d-memql-server-0"
  echo 'write unix @->/run/docker.sock: use of closed network connection' >&2
  return 1
 fi
 if [ "${FAKE_HANG_FIRST_IMPORT:-}" = 1 ] && [ "$count" = 1 ]; then
  touch "${FAKE_IMPORTED}.k3d-memql-server-0"
  echo 'ERRO Failed to copy read stream. write unix @->/run/docker.sock: use of closed network connection' >&2
  while :; do sleep 0.1; done
 fi
 if [ "${FAKE_IMPORT_EXIT:-0}" = 0 ]; then
  for node in k3d-memql-server-0 k3d-memql-server-1 k3d-memql-agent-0; do touch "${FAKE_IMPORTED}.${node}"; done
 fi
 return "${FAKE_IMPORT_EXIT:-0}"
}
function restart_deployment() { echo restarted >> "$FAKE_CALLS"; }
`

const imageProofCtrHarness = `#!/bin/sh
set -eu
printf 'proof %s %s\n' "$FAKE_NODE" "$*" >> "$FAKE_CALLS"
shift 2
root=$4
case "$1 $2" in
 'images mount')
  mkdir "$root/app"
  cp "$FAKE_FILES/memql" "$FAKE_FILES/healthcheck" "$root/app/"
  chain=$FAKE_IMAGE_A
  if [ "$FAKE_NODE" = "${FAKE_PROOF_NODE:-k3d-memql-server-1}" ]; then
   case "${FAKE_PROOF_CASE:-}" in
    empty-memql) : > "$root/app/memql" ;;
    empty-healthcheck) : > "$root/app/healthcheck" ;;
    bytes) printf 'different executable bytes\n' > "$root/app/memql" ;;
    mode) chmod 700 "$root/app/healthcheck" ;;
    missing) rm "$root/app/healthcheck" ;;
    chain) chain=$FAKE_IMAGE_B ;;
    mount) exit 1 ;;
   esac
  fi
  printf '%s\n%s\n' "$chain" "$root" ;;
 'images unmount')
  if [ "$FAKE_NODE" = "${FAKE_PROOF_NODE:-k3d-memql-server-1}" ] && [ "${FAKE_PROOF_CASE:-}" = cleanup ]; then exit 1; fi
  rm -f "$root/app/memql" "$root/app/healthcheck"
  rmdir "$root/app" ;;
 *) exit 1 ;;
esac
`

func runImageIntegrity(t *testing.T, body string, vars ...string) (bool, string, string) {
	t.Helper()
	tmp := t.TempDir()
	calls := filepath.Join(tmp, "calls")
	files := filepath.Join(tmp, "files")
	bin := filepath.Join(tmp, "bin")
	for _, dir := range []string{files, bin} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"memql", "healthcheck"} {
		if err := os.WriteFile(filepath.Join(files, name), []byte("executable fixture: "+name+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Run the real verification shell against a fake mount operation. The
	// fixture deliberately keeps image IDs and ready metadata valid while
	// changing the bytes/mode visible through one node's merged rootfs.
	if err := os.WriteFile(filepath.Join(bin, "ctr"), []byte(imageProofCtrHarness), 0o755); err != nil {
		t.Fatal(err)
	}
	sha, err := exec.LookPath("sha256sum")
	shaArgs := ""
	if err != nil {
		sha, err = exec.LookPath("shasum")
		shaArgs = " -a 256"
	}
	if err != nil {
		t.Fatal(err)
	}
	shaScript := "#!/bin/sh\nif [ \"${FAKE_NODE:-}\" = \"${FAKE_PROOF_NODE:-k3d-memql-server-1}\" ] && [ \"${FAKE_PROOF_CASE:-}\" = read ]; then exit 1; fi\nexec " + shellQuote(sha) + shaArgs + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "sha256sum"), []byte(shaScript), 0o755); err != nil {
		t.Fatal(err)
	}
	// A hung import must fail the test rather than hang it.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", "-c", "source \""+filepath.Join(repoRoot(t), "scripts/k3d/dev.sh")+"\"\n"+imageIntegrityHarness+"\n"+body)
	command.WaitDelay = time.Second
	command.Env = append(os.Environ(), "FAKE_CALLS="+calls, "FAKE_STATE="+filepath.Join(tmp, "changed"), "FAKE_IMPORTED="+filepath.Join(tmp, "imported"), "FAKE_IMPORT_COUNT="+filepath.Join(tmp, "import-count"), "FAKE_FILES="+files, "TMPDIR="+tmp, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	command.Env = append(command.Env, vars...)
	output, err := command.CombinedOutput()
	raw, _ := os.ReadFile(calls)
	// An unmount failure must preserve its mountpoint instead of recursively
	// deleting a potentially mounted filesystem. Every other exit cleans up.
	if !strings.Contains(strings.Join(vars, "\n"), "FAKE_PROOF_CASE=cleanup") {
		leftovers, globErr := filepath.Glob(filepath.Join(tmp, "memql-image-proof.*"))
		if globErr != nil || len(leftovers) != 0 {
			t.Errorf("verification left temporary copies/views behind: %v (%v)\n%s", leftovers, globErr, output)
		}
	}
	return err == nil, string(output), string(raw)
}

func TestEngineImageImportChecksUnpackedExecutables(t *testing.T) {
	ok, out, calls := runImageIntegrity(t, `IMAGE_SOURCE=images; install_node bff "$FAKE_IMAGE_A"`, "FAKE_IMPORT_FAIL_AFTER_COMPLETE=1")
	if !ok {
		t.Fatalf("verified engine import failed: %s\n%s", out, calls)
	}
	if !strings.Contains(calls, "docker create --pull=never --read-only --network none --entrypoint /app/memql sha256:") || strings.Contains(calls, "docker start") {
		t.Fatalf("source proof did not use the immutable, never-started image: %s", calls)
	}
	for _, node := range []string{"server-0", "server-1", "agent-0"} {
		prefix := "proof k3d-memql-" + node
		if !strings.Contains(calls, prefix+" -n k8s.io images mount ") || !strings.Contains(calls, prefix+" -n k8s.io images unmount --rm ") {
			t.Fatalf("node %s was not verified and cleaned: %s", node, calls)
		}
	}
	if !strings.Contains(calls, "restarted") || !strings.Contains(out, "Adopted complete image contents") {
		t.Fatalf("verified import was not adopted/restarted: %s\n%s", out, calls)
	}
}

func TestEngineImageImportRejectsCorruptSnapshotsAndCleansUp(t *testing.T) {
	for _, failure := range []string{"empty-memql", "empty-healthcheck", "bytes", "mode", "missing", "chain", "read", "mount", "cleanup"} {
		t.Run(failure, func(t *testing.T) {
			ok, out, calls := runImageIntegrity(t, `IMAGE_SOURCE=images; install_node bff "$FAKE_IMAGE_A"`, "FAKE_PROOF_CASE="+failure, "FAKE_IMPORT_FAIL_AFTER_COMPLETE=1")
			if ok || strings.Contains(calls, "restarted") || strings.Contains(out, "Adopted complete image contents") {
				t.Fatalf("unsafe engine contents were adopted: %s\n%s", out, calls)
			}
			if strings.Count(calls, "proof k3d-memql-server-1 -n k8s.io images unmount --rm ") != 2 || !strings.Contains(calls, "docker rm -v ") {
				t.Fatalf("failure did not clean both attempts and source container: %s", calls)
			}
		})
	}
}

func TestEngineImageProofReadFailureStopsBeforeImport(t *testing.T) {
	ok, out, calls := runImageIntegrity(t, `install_node bff "$FAKE_IMAGE_A"`, "FAKE_COPY_FAIL=1")
	if ok || strings.Contains(calls, "k3d image import") || !strings.Contains(calls, "docker rm -v ") {
		t.Fatalf("failed source proof imported or leaked its container: %s\n%s", out, calls)
	}
}

func TestGenericImageImportDoesNotRequireEngineFiles(t *testing.T) {
	ok, out, calls := runImageIntegrity(t, `import_image memql-bff:local "$FAKE_IMAGE_A"`, "FAKE_COPY_FAIL=1")
	if !ok || strings.Contains(calls, "docker create ") || strings.Contains(calls, "proof k3d-") {
		t.Fatalf("generic import assumed engine paths: %s\n%s", out, calls)
	}
}

func TestImageImportVerifiesEveryClusterNode(t *testing.T) {
	ok, out, calls := runImageIntegrity(t, `import_image memql-bff:local "$FAKE_IMAGE_A"`)
	if !ok {
		t.Fatalf("valid import failed: %s", out)
	}
	for _, node := range []string{"server-0", "server-1", "agent-0"} {
		if !strings.Contains(calls, "docker exec k3d-memql-"+node+" ctr -n k8s.io images check") {
			t.Errorf("%s content never verified:\n%s", node, calls)
		}
	}
	if !strings.Contains(calls, "--mode direct") {
		t.Errorf("shared tools-node import still used:\n%s", calls)
	}
	if strings.Contains(calls, "docker exec k3d-memql-serverlb") || strings.Contains(calls, "docker exec k3d-memql-tools") {
		t.Errorf("non-workload container verified:\n%s", calls)
	}
}

func TestImageImportRefusesFalseSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"zero exit short read", []string{"FAKE_BAD_NODE=k3d-memql-agent-0"}},
		{"nonzero import exit", []string{"FAKE_IMPORT_EXIT=1"}},
		{"missing layer", []string{"FAKE_INCOMPLETE_NODE=k3d-memql-server-1"}},
		{"stopped agent", []string{"FAKE_AGENT_STATE=exited"}},
		{"no nodes", []string{"FAKE_EMPTY=1"}},
		{"unreadable inventory", []string{"FAKE_LIST_FAIL=1"}},
		{"unreadable runtime", []string{"FAKE_EXEC_FAIL=k3d-memql-server-1"}},
		{"tag changed during import", []string{"FAKE_IMPORT_DRIFT=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, out, calls := runImageIntegrity(t, `install_node bff "$FAKE_IMAGE_A"`, tc.env...)
			if ok {
				t.Fatalf("unsafe import reported success:\n%s\n%s", out, calls)
			}
			if strings.Contains(calls, "restarted") {
				t.Errorf("restarted after failed verification:\n%s", calls)
			}
		})
	}
}

func TestImageImportReconcilesAndRetriesBoundedly(t *testing.T) {
	t.Run("adopts complete import despite command failure", func(t *testing.T) {
		ok, out, calls := runImageIntegrity(t, `import_image memql-bff:local "$FAKE_IMAGE_A"`, "FAKE_IMPORT_FAIL_AFTER_COMPLETE=1")
		if !ok {
			t.Fatalf("complete node contents were not adopted: %s\n%s", out, calls)
		}
		if got := strings.Count(calls, "k3d image import"); got != 1 {
			t.Fatalf("completed import was retried %d times:\n%s", got, calls)
		}
		if !strings.Contains(out, "Adopted complete image contents") {
			t.Fatalf("missing reconciliation evidence: %s", out)
		}
	})

	t.Run("retries incomplete transient import once", func(t *testing.T) {
		ok, out, calls := runImageIntegrity(t, `import_image memql-bff:local "$FAKE_IMAGE_A"`, "FAKE_FAIL_FIRST_IMPORT=1")
		if !ok {
			t.Fatalf("transient import was not recovered: %s\n%s", out, calls)
		}
		if got := strings.Count(calls, "k3d image import"); got != 2 {
			t.Fatalf("expected exactly one bounded retry, got %d:\n%s", got, calls)
		}
		if !strings.Contains(out, "retrying the verified import once") {
			t.Fatalf("partial node import was not diagnosed before retry: %s", out)
		}
	})

	// k3d's direct import feeds one stream to every node. When one node's ctr
	// stops reading early, k3d v5.9.0 stops feeding the others and waits on
	// them forever; the pipeline build node makes a second node the default.
	t.Run("a hung attempt is stopped and retried once", func(t *testing.T) {
		ok, out, calls := runImageIntegrity(t, `LOCAL_IMAGE_IMPORT_LIMIT_SECONDS=2 import_image memql-bff:local "$FAKE_IMAGE_A"`, "FAKE_HANG_FIRST_IMPORT=1")
		if !ok {
			t.Fatalf("a hung import was not recovered: %s\n%s", out, calls)
		}
		if got := strings.Count(calls, "k3d image import"); got != 2 {
			t.Fatalf("expected the hung attempt and one retry, got %d:\n%s", got, calls)
		}
		if !strings.Contains(out, "did not finish within 2s") {
			t.Fatalf("the hung attempt was not named: %s", out)
		}
	})

	t.Run("permanent import failure stops after retry", func(t *testing.T) {
		ok, out, calls := runImageIntegrity(t, `import_image memql-bff:local "$FAKE_IMAGE_A"`, "FAKE_IMPORT_EXIT=1")
		if ok {
			t.Fatalf("permanent import failure reported success: %s\n%s", out, calls)
		}
		if got := strings.Count(calls, "k3d image import"); got != 2 {
			t.Fatalf("expected exactly two bounded attempts, got %d:\n%s", got, calls)
		}
	})
}

func TestImageImportAcceptsClassicDockerConfigID(t *testing.T) {
	ok, out, _ := runImageIntegrity(t, `import_image memql-bff:local "$FAKE_IMAGE_A"`, "FAKE_CLASSIC=1", "FAKE_BAD_NODE=k3d-memql-agent-0")
	if !ok {
		t.Fatalf("classic Docker config ID was rejected: %s", out)
	}
}

func TestChangedBuildTagRefusesAllImports(t *testing.T) {
	ok, out, calls := runImageIntegrity(t, `build_and_import_nodes bff agent`, "FAKE_CHANGED_BUILD=1")
	if ok {
		t.Fatalf("another build replaced bff after its build, but update succeeded:\n%s", out)
	}
	if strings.Contains(calls, "k3d image import") {
		t.Errorf("changed build imported something:\n%s", calls)
	}
	if !strings.Contains(calls, "--iidfile") {
		t.Errorf("build did not capture its own ID:\n%s", calls)
	}
}

// Exercise the actual VSIX staging functions into a task-local tree. A helper
// working in a checkout but omitted from the staged runner breaks editor Update.
func TestImageIntegrityHelperShipsInStagedRunner(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "scripts/vscode/package.sh"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	var body strings.Builder
	body.WriteString("set -euo pipefail\nREPO_ROOT=" + shellQuote(root) + "\nEXT_DIR=" + shellQuote(filepath.Join(root, "editors/vscode")) + "\n")
	for _, name := range []string{"capability_script_relpaths", "support_file_relpaths", "stage_one", "verify_staged_sources"} {
		body.WriteString("function " + name + "() {" + functionBody(t, string(raw), name) + "\n}\n")
	}
	body.WriteString("staged=" + shellQuote(tmp) + "\nwhile read -r rel; do stage_one \"$rel\" \"$staged\"; done < <(support_file_relpaths)\n")
	body.WriteString("stage_one scripts/k3d/dev.sh \"$staged\"\n")
	for _, helper := range []string{"lib/local_images.sh", "lib/host_build_guard.py", "k3d/image_cache.py"} {
		body.WriteString("test -f \"$staged/scripts/" + helper + "\"\n")
	}
	body.WriteString("bash \"$staged/scripts/k3d/dev.sh\" --print-spec\n")
	out, err := exec.Command("bash", "-c", body.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("staged runner missing integrity helper: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"k3d.dev"`) {
		t.Fatalf("staged script did not load: %s", out)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func TestInfrastructureAndDatabaseImportsUseIntegrityGate(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"infra", `INFRA_IMAGES=(memql-bff:local); pull_and_import_infra`},
		{"database", `DB_IMAGE=memql-bff:local; function cluster_holds_db_image(){ return 1; }; function bash(){ :; }; ensure_db_image`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, out, calls := runImageIntegrity(t, tc.body, "FAKE_BAD_NODE=k3d-memql-agent-0")
			if ok {
				t.Fatalf("%s ignored failed runtime verification: %s", tc.name, out)
			}
			if !strings.Contains(calls, "--mode direct") || !strings.Contains(calls, "docker exec k3d-memql-agent-0") {
				t.Errorf("%s bypassed shared importer:\n%s", tc.name, calls)
			}
		})
	}
}
