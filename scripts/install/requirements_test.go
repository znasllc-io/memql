// Behaviour gate for install.requirements (znasllc-io/memql#6256).
//
// The step refuses to create a cluster on a machine below the one-machine
// minimum requirements, and only then: a machine that already runs the cluster
// is warned, the explicit override continues, and a probe that cannot answer
// is not a shortfall. The figures themselves live in scripts/lib/requirements.sh
// and docs/public/operate/minimum-requirements.md; these tests pin the
// decisions made on them.
//
// Hermetic like detect_test.go: `uname`, `sysctl`, `sw_vers`, `free`, `nproc`,
// `nvidia-smi`, `docker`, `df` and `k3d` are stubs on a PATH the test owns.
package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type requirementsCheck struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Observed    string `json:"observed"`
	Minimum     string `json:"minimum"`
	Recommended string `json:"recommended"`
	Status      string `json:"status"`
}

type requirementsReport struct {
	MeetsMinimum     *bool               `json:"meetsMinimum"`
	MeetsRecommended *bool               `json:"meetsRecommended"`
	Checks           []requirementsCheck `json:"checks"`
	Shortfalls       []string            `json:"shortfalls"`
	Docs             string              `json:"docs"`
}

type requirementsResult struct {
	Requirements        requirementsReport `json:"requirements"`
	MeetsMinimum        *bool              `json:"meetsMinimum"`
	Ready               *bool              `json:"ready"`
	ClusterExists       bool               `json:"clusterExists"`
	AllowedBelowMinimum bool               `json:"allowedBelowMinimum"`
}

const gib = int64(1) << 30

// machine describes the stubs one test hands the step. Zero values mean "this
// probe is absent", which is how a test exercises an unknown answer.
type machine struct {
	kernel, arch   string
	memoryBytes    int64
	cores          int
	macOS          string
	dockerMemBytes int64
	gpus           []string // nvidia-smi csv lines: "<name>, <MiB>"
	freeDiskKiB    int64
	clusters       []string // what `k3d cluster list` prints
}

func (m machine) path(t *testing.T) string {
	t.Helper()
	dir := stubPATH(t, m.kernel, m.arch)
	if m.kernel == "Darwin" {
		writeStub(t, dir, "sysctl", fmt.Sprintf(`case "${2:-}" in
  hw.memsize) echo %d ;;
  hw.ncpu) echo %d ;;
  kern.osproductversion) echo %q ;;
  *) exit 1 ;;
esac
`, m.memoryBytes, m.cores, m.macOS))
		if m.macOS != "" {
			writeStub(t, dir, "sw_vers", fmt.Sprintf("echo %q\n", m.macOS))
		}
	} else {
		if m.memoryBytes > 0 {
			writeStub(t, dir, "free", fmt.Sprintf(`echo "               total        used        free"
echo "Mem:     %d  1000  2000"
echo "Swap:    0  0  0"
`, m.memoryBytes))
		}
		if m.cores > 0 {
			writeStub(t, dir, "nproc", fmt.Sprintf("echo %d\n", m.cores))
		}
		if len(m.gpus) > 0 {
			writeStub(t, dir, "nvidia-smi", fmt.Sprintf("printf '%%s\\n' %s\n", shellWords(m.gpus)))
		}
	}
	if m.dockerMemBytes > 0 {
		writeStub(t, dir, "docker", fmt.Sprintf(`case "${1:-}" in
  info) echo %d ;;
  *) exit 1 ;;
esac
`, m.dockerMemBytes))
	}
	if m.freeDiskKiB > 0 {
		// Replaces the real df stubPATH linked: the disk a runner happens to
		// have is not a fact these tests are about. The link goes first, or
		// writing the stub would write THROUGH it.
		if err := os.Remove(filepath.Join(dir, "df")); err != nil {
			t.Fatalf("remove the linked df: %v", err)
		}
		writeStub(t, dir, "df", fmt.Sprintf(`echo "Filesystem 1024-blocks Used Available Capacity Mounted on"
echo "/dev/disk1 999999999 1 %d 1%% /"
`, m.freeDiskKiB))
	}
	if m.clusters != nil {
		writeStub(t, dir, "k3d", fmt.Sprintf("printf '%%s 1/1 0/0 true\\n' %s\n", shellWords(m.clusters)))
	}
	return dir
}

func shellWords(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// A machine that meets the minimum everywhere, to vary one fact at a time.
func goodMac() machine {
	return machine{
		kernel: "Darwin", arch: "arm64",
		memoryBytes: 36 * gib, cores: 14, macOS: "15.3.1",
		dockerMemBytes: 8318976000, // Docker Desktop's "8 GB" reports 7.75 GiB
		freeDiskKiB:    600 * 1000 * 1000,
	}
}

func goodLinux() machine {
	return machine{
		kernel: "Linux", arch: "x86_64",
		memoryBytes: 63 * gib, cores: 16,
		gpus:        []string{"NVIDIA GeForce RTX 4090, 24564"},
		freeDiskKiB: 400 * 1000 * 1000,
	}
}

func runRequirements(t *testing.T, m machine, env []string, args ...string) (capEnvelope, requirementsResult, string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{scriptPath(t, "requirements.sh")}, args...)...)
	cmd.Env = append([]string{"PATH=" + m.path(t), "HOME=" + t.TempDir()}, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run requirements.sh: %v\noutput:\n%s", err, out)
	}
	line := lastJSONObject(string(out))
	if line == "" {
		t.Fatalf("requirements.sh emitted no JSON envelope (exit %d)\noutput:\n%s", code, out)
	}
	var envl capEnvelope
	if err := json.Unmarshal([]byte(line), &envl); err != nil {
		t.Fatalf("envelope is not valid JSON: %v\nline: %s", err, line)
	}
	var r requirementsResult
	if err := json.Unmarshal(envl.Result, &r); err != nil {
		t.Fatalf("result is not a requirements report: %v\nresult: %s", err, envl.Result)
	}
	if envl.Changed {
		t.Errorf("changed=true: the step only measures")
	}
	return envl, r, string(out), code
}

func check(t *testing.T, r requirementsResult, id string) requirementsCheck {
	t.Helper()
	for _, c := range r.Requirements.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no %q check in %+v", id, r.Requirements.Checks)
	return requirementsCheck{}
}

func TestRequirementsPassAMacThatMeetsTheMinimum(t *testing.T) {
	env, r, out, code := runRequirements(t, goodMac(), nil)
	if code != 0 || !env.OK {
		t.Fatalf("exit %d, ok=%v; want a pass\n%s", code, env.OK, out)
	}
	if r.Ready == nil || !*r.Ready || r.MeetsMinimum == nil || !*r.MeetsMinimum {
		t.Errorf("ready=%v meetsMinimum=%v, want both true", r.Ready, r.MeetsMinimum)
	}
	// 36 GB is between the 32 GB minimum and the 48 GB recommendation.
	if r.Requirements.MeetsRecommended == nil || *r.Requirements.MeetsRecommended {
		t.Errorf("meetsRecommended=%v, want false for 36 GB", r.Requirements.MeetsRecommended)
	}
	if c := check(t, r, "memory"); c.Status != "minimum" || c.Observed != "36 GB" {
		t.Errorf("memory check = %+v, want 36 GB at the minimum", c)
	}
	// Docker Desktop's "8 GB" setting reports 7.75 GiB and must still pass.
	if c := check(t, r, "docker"); c.Status != "minimum" {
		t.Errorf("docker check = %+v, want the 8 GB allocation to meet the minimum", c)
	}
	for _, id := range []string{"system", "cpu", "disk", "macos"} {
		if c := check(t, r, id); c.Status != "recommended" {
			t.Errorf("%s check = %+v, want it met", id, c)
		}
	}
}

func TestRequirementsRefuseAMacBelowTheMinimum(t *testing.T) {
	m := goodMac()
	m.memoryBytes = 24 * gib
	env, r, out, code := runRequirements(t, m, nil)
	if code != 3 || env.OK {
		t.Fatalf("exit %d, ok=%v; want a refusal (3)\n%s", code, env.OK, out)
	}
	if r.Ready == nil || *r.Ready {
		t.Errorf("ready=%v, want false", r.Ready)
	}
	msg := env.Error.Message
	for _, want := range []string{
		"does not meet the minimum requirements",
		"Memory: 24 GB. The minimum is 32 GB.",
		"minimum-requirements.md",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not say %q", msg, want)
		}
	}
	if len(r.Requirements.Shortfalls) != 1 || r.Requirements.Shortfalls[0] != "Memory: 24 GB. The minimum is 32 GB." {
		t.Errorf("shortfalls = %q, want exactly the memory one", r.Requirements.Shortfalls)
	}
}

// A repair or a continued setup runs the same graph; refusing it would strand
// a cluster that is already here.
func TestRequirementsWarnButContinueWhenTheClusterIsAlreadyHere(t *testing.T) {
	m := goodMac()
	m.memoryBytes = 16 * gib
	m.clusters = []string{"other", "memql"}
	env, r, out, code := runRequirements(t, m, nil, "--cluster=memql")
	if code != 0 || !env.OK {
		t.Fatalf("exit %d, ok=%v; want a pass for an existing cluster\n%s", code, env.OK, out)
	}
	if !r.ClusterExists || r.Ready == nil || !*r.Ready || *r.MeetsMinimum {
		t.Errorf("result %+v: want clusterExists, ready and meetsMinimum=false", r)
	}
	if !strings.Contains(out, "below the minimum requirements") {
		t.Errorf("no warning in the log:\n%s", out)
	}

	// A different cluster name is a NEW cluster, and is refused.
	if _, _, out, code := runRequirements(t, m, nil, "--cluster=fresh"); code != 3 {
		t.Errorf("exit %d for a cluster that is not here yet, want 3\n%s", code, out)
	}
}

func TestRequirementsOverrideContinuesBelowTheMinimum(t *testing.T) {
	m := goodLinux()
	m.gpus = nil
	for _, tc := range []struct {
		name string
		env  []string
		args []string
	}{
		{"flag", nil, []string{"--allow-below-minimum"}},
		{"environment", []string{"MEMQL_ALLOW_BELOW_MINIMUM=1"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, r, out, code := runRequirements(t, m, tc.env, tc.args...)
			if code != 0 || !env.OK || !r.AllowedBelowMinimum {
				t.Fatalf("exit %d, ok=%v, allowed=%v; want the override honoured\n%s", code, env.OK, r.AllowedBelowMinimum, out)
			}
		})
	}
	// And nothing sets it implicitly.
	if _, _, out, code := runRequirements(t, m, []string{"MEMQL_ALLOW_BELOW_MINIMUM=false"}); code != 3 {
		t.Errorf("exit %d with the override off, want 3\n%s", code, out)
	}
}

func TestRequirementsLinuxGPU(t *testing.T) {
	t.Run("none found is a shortfall", func(t *testing.T) {
		m := goodLinux()
		m.gpus = nil
		env, r, out, code := runRequirements(t, m, nil)
		if code != 3 {
			t.Fatalf("exit %d, want 3\n%s", code, out)
		}
		if c := check(t, r, "gpu"); c.Status != "below" || c.Observed != "none found" {
			t.Errorf("gpu check = %+v", c)
		}
		if !strings.Contains(env.Error.Message, "GPU: none found. The minimum is 16 GB of VRAM.") {
			t.Errorf("refusal %q does not name the GPU", env.Error.Message)
		}
	})
	t.Run("a 16 GB card reporting 16376 MiB meets the minimum", func(t *testing.T) {
		m := goodLinux()
		m.gpus = []string{"NVIDIA GeForce RTX 4080, 16376"}
		_, r, out, code := runRequirements(t, m, nil)
		if code != 0 {
			t.Fatalf("exit %d, want a pass\n%s", code, out)
		}
		if c := check(t, r, "gpu"); c.Status != "minimum" || c.Observed != "NVIDIA GeForce RTX 4080, 16 GB" {
			t.Errorf("gpu check = %+v", c)
		}
	})
	t.Run("the largest card is the one judged", func(t *testing.T) {
		m := goodLinux()
		m.gpus = []string{"NVIDIA GeForce RTX 3060, 12288", "NVIDIA RTX 6000 Ada Generation, 49140"}
		_, r, out, code := runRequirements(t, m, nil)
		if code != 0 {
			t.Fatalf("exit %d, want a pass\n%s", code, out)
		}
		if c := check(t, r, "gpu"); c.Status != "recommended" || !strings.HasPrefix(c.Observed, "NVIDIA RTX 6000 Ada Generation") {
			t.Errorf("gpu check = %+v", c)
		}
	})
	t.Run("a 12 GB card is below the minimum", func(t *testing.T) {
		m := goodLinux()
		m.gpus = []string{"NVIDIA GeForce RTX 3060, 12288"}
		if _, _, out, code := runRequirements(t, m, nil); code != 3 {
			t.Errorf("exit %d, want 3\n%s", code, out)
		}
	})
}

func TestRequirementsRefuseAPlatformTheInstallerDoesNotTarget(t *testing.T) {
	for _, tc := range []struct{ kernel, arch, observed string }{
		{"Darwin", "x86_64", "Intel Mac"},
		{"Linux", "aarch64", "Linux on arm64"},
	} {
		t.Run(tc.observed, func(t *testing.T) {
			m := goodMac()
			m.kernel, m.arch = tc.kernel, tc.arch
			_, r, out, code := runRequirements(t, m, nil)
			if code != 3 {
				t.Fatalf("exit %d, want 3\n%s", code, out)
			}
			if c := check(t, r, "system"); c.Status != "below" || c.Observed != tc.observed {
				t.Errorf("system check = %+v, want %q below", c, tc.observed)
			}
		})
	}
}

func TestRequirementsUnknownIsNotAShortfall(t *testing.T) {
	// No sysctl answers, no sw_vers, no docker: every probe but the disk is
	// silent, and silence must not refuse an install.
	m := machine{kernel: "Darwin", arch: "arm64", freeDiskKiB: 600 * 1000 * 1000}
	env, r, out, code := runRequirements(t, m, nil)
	if code != 0 || !env.OK {
		t.Fatalf("exit %d, ok=%v; want a pass when probes cannot answer\n%s", code, env.OK, out)
	}
	for _, id := range []string{"memory", "cpu", "macos", "docker"} {
		if c := check(t, r, id); c.Status != "unknown" {
			t.Errorf("%s check = %+v, want unknown", id, c)
		}
	}
}

func TestRequirementsFreeDisk(t *testing.T) {
	m := goodMac()
	m.freeDiskKiB = 60 * 1000 * 1000
	env, r, out, code := runRequirements(t, m, nil)
	if code != 3 {
		t.Fatalf("exit %d, want 3\n%s", code, out)
	}
	if c := check(t, r, "disk"); c.Status != "below" || c.Observed != "61 GB" {
		t.Errorf("disk check = %+v", c)
	}
	if !strings.Contains(env.Error.Message, "Free disk: 61 GB. The minimum is 100 GB.") {
		t.Errorf("refusal %q does not name the disk", env.Error.Message)
	}
}

// install.detect REPORTS the same verdict and never refuses over it: the VS
// Code wizard reads it to warn before an install starts.
func TestDetectReportsTheRequirementsWithoutRefusing(t *testing.T) {
	m := goodLinux()
	m.gpus = nil
	cmd := exec.Command("bash", scriptPath(t, "detect.sh"))
	cmd.Env = []string{"PATH=" + m.path(t), "HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("detect refused or failed: %v\n%s", err, out)
	}
	var envl capEnvelope
	if err := json.Unmarshal([]byte(lastJSONObject(string(out))), &envl); err != nil {
		t.Fatalf("no envelope: %v\n%s", err, out)
	}
	var r struct {
		Requirements requirementsReport `json:"requirements"`
	}
	if err := json.Unmarshal(envl.Result, &r); err != nil {
		t.Fatalf("result: %v", err)
	}
	if r.Requirements.MeetsMinimum == nil || *r.Requirements.MeetsMinimum {
		t.Fatalf("requirements = %+v, want meetsMinimum=false reported", r.Requirements)
	}
	if len(r.Requirements.Shortfalls) == 0 || !strings.HasPrefix(r.Requirements.Shortfalls[0], "GPU: none found.") {
		t.Errorf("shortfalls = %q", r.Requirements.Shortfalls)
	}
}
