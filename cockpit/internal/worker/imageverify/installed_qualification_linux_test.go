//go:build linux

package imageverify

// This file is an explicitly invoked owner fixture. Ordinary tests cannot
// enable the installed capability, and portable proof cannot mint its result.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const qualificationResultName = "qualification-result.json"

type installedQualificationResult struct {
	SchemaVersion            int                      `json:"schemaVersion"`
	SourceSHA                string                   `json:"sourceSHA"`
	InstallationSHA256       string                   `json:"installationSHA256"`
	HelperSHA256             string                   `json:"helperSHA256"`
	TestBinarySHA256         string                   `json:"testBinarySHA256"`
	Passed                   bool                     `json:"passed"`
	CleanupConfirmed         bool                     `json:"cleanupConfirmed"`
	Fixtures                 []installedFixtureResult `json:"fixtures"`
	CandidateAttestationPath string                   `json:"candidateAttestationPath,omitempty"`
}
type installedFixtureResult struct {
	Name           string `json:"name"`
	Result         string `json:"result"`
	EvidenceSHA256 string `json:"evidenceSHA256"`
	EvidencePath   string `json:"evidencePath"`
}
type installedFixture struct {
	c                          ServiceConfig
	output, source, executable string
	lock                       *os.File
	host                       ServiceHostFacts
	result                     installedQualificationResult
}
type fixtureProbe struct {
	Mode, Nonce               string
	Paths                     []string
	Socket                    string
	Namespaces                map[string]string
	PrivateEndpoint           string
	MemoryBytes, StorageBytes int64
}
type fixtureBuild struct {
	Request                             serviceRequest    `json:"request"`
	Record                              Record            `json:"record"`
	Receipt                             json.RawMessage   `json:"receipt,omitempty"`
	Error                               string            `json:"error,omitempty"`
	Logs                                string            `json:"logs"`
	Limits                              map[string]string `json:"limits"`
	CPUThrottled, OOMKills              uint64
	MappedUIDObserved, OversizeObserved bool
	CleanupConfirmed                    bool
	monitorErr                          string
	mu                                  sync.Mutex
}

func TestInstalledImageVerificationQualification(t *testing.T) {
	if os.Getenv("MEMQL_IMAGE_QUALIFICATION") != "1" {
		t.Skip("requires the reviewed owner-run installed-host wrapper")
	}
	f, err := newInstalledFixture()
	if err != nil {
		t.Fatal(err)
	}
	defer f.lock.Close()
	defer func() {
		if err := fixtureWriteJSON(filepath.Join(f.output, qualificationResultName), f.result, false); err != nil {
			t.Error(err)
		}
	}()
	cases := []struct {
		name string
		run  func(*installedFixture) (any, error)
	}{
		{"target-export", fixtureTargetExport},
		{"resource-limits", fixtureResourceLimits},
		{"namespace-isolation", fixtureNamespaceIsolation},
		{"network-isolation", fixtureNetworkIsolation},
		{"cancel-and-timeout", fixtureCancelTimeout},
		{"worker-restart-recovery", fixtureWorkerRestart},
		{"ambiguous-start-and-cleanup", fixtureAmbiguousStart},
		{"mapped-state-accounting-cleanup", fixtureMappedState},
	}
	for _, c := range cases {
		if err := f.stillStopped(); err != nil {
			t.Fatal(err)
		}
		evidence, runErr := c.run(f)
		path := filepath.Join(f.output, c.name+".json")
		body := struct {
			Name, RuntimeSource, Error string
			Passed                     bool
			Evidence                   any
		}{c.name, f.source, "", runErr == nil, evidence}
		if runErr != nil {
			body.Error = runErr.Error()
		}
		if err := fixtureWriteJSON(path, body, true); err != nil {
			t.Fatal(err)
		}
		digest, err := fixtureHash(path)
		if err != nil {
			t.Fatal(err)
		}
		status := "passed"
		if runErr != nil {
			status = "failed"
		}
		f.result.Fixtures = append(f.result.Fixtures, installedFixtureResult{c.name, status, digest, path})
		if runErr != nil {
			t.Fatalf("%s: %v; preserve stopped worker and retained evidence", c.name, runErr)
		}
	}
	if err := f.stillStopped(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(f.c.AttemptRoot); err != nil || len(entries) != 0 {
		t.Fatal("qualification left private attempts; do not resume worker")
	}
	if err := InspectServiceInstallation(f.c); err != nil {
		t.Fatal(err)
	}
	host, err := collectServiceHostFacts(context.Background(), f.c, installedServiceHostReader{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(host, f.host) {
		t.Fatal("host runtime facts changed during qualification")
	}
	q := ServiceQualification{SchemaVersion: 1, Profile: Profile, VerifiedAt: time.Now().UTC().Format(time.RFC3339), InstallationSHA256: f.result.InstallationSHA256, Host: host}
	for _, r := range f.result.Fixtures {
		q.Fixtures = append(q.Fixtures, ServiceQualificationFixture{r.Name, r.Result, r.EvidenceSHA256})
	}
	raw, err := json.Marshal(q)
	if err != nil || verifyServiceQualification(f.c, host, raw, time.Now()) != nil {
		t.Fatal("real qualification receipt did not validate")
	}
	path := filepath.Join(f.output, "qualification.json")
	if err := fixtureWriteJSON(path, q, true); err != nil {
		t.Fatal(err)
	}
	f.result.Passed = true
	f.result.CleanupConfirmed = true
	f.result.CandidateAttestationPath = path
}

func newInstalledFixture() (*installedFixture, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" || os.Geteuid() == 0 {
		return nil, errors.New("qualification requires the existing non-root Linux amd64 account")
	}
	source := os.Getenv("MEMQL_IMAGE_QUALIFICATION_SOURCE")
	output := os.Getenv("MEMQL_IMAGE_QUALIFICATION_OUTPUT")
	if !hash40.MatchString(source) || !filepath.IsAbs(output) || filepath.Clean(output) != output {
		return nil, errors.New("exact runtime source and private output directory are required")
	}
	if err := validatePrivateDirectory(output, os.Geteuid()); err != nil {
		return nil, err
	}
	for _, name := range []string{qualificationResultName, "qualification.json"} {
		if _, err := os.Lstat(filepath.Join(output, name)); !os.IsNotExist(err) {
			return nil, errors.New("qualification output already has a result; preserve it and use a new directory")
		}
	}
	c, err := LoadServiceConfig()
	if err != nil {
		return nil, err
	}
	if err = InspectServiceInstallation(c); err != nil {
		return nil, err
	}
	if c.OwnerUID != os.Geteuid() || c.MaxCPUMilli < 1000 || c.MaxMemoryMiB < 1024 || c.MaxRuntimeSeconds < 90 {
		return nil, errors.New("installed fixture requires at least 1 CPU, 1 GiB and 90 seconds within existing service ceilings")
	}
	if serviceWithin(output, c.AttemptRoot) || serviceWithin(output, c.WorkspaceRoot) {
		return nil, errors.New("evidence must be outside cleanup roots")
	}
	if err = validatePrivateDirectory(c.AttemptRoot, c.OwnerUID); err != nil {
		return nil, err
	}
	if err = validatePrivateDirectory(c.WorkspaceRoot, c.OwnerUID); err != nil {
		return nil, err
	}
	if entries, e := os.ReadDir(c.AttemptRoot); e != nil || len(entries) != 0 {
		return nil, errors.New("existing private attempts require reconciliation before qualification")
	}
	lock, err := fixtureInheritedLock()
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	testHash, err := fixtureHash(executable)
	if err != nil {
		return nil, err
	}
	digest, err := ServiceInstallationDigest(c)
	if err != nil {
		return nil, err
	}
	host, err := collectServiceHostFacts(context.Background(), c, installedServiceHostReader{})
	if err != nil {
		return nil, err
	}
	f := &installedFixture{c: c, output: output, source: source, executable: executable, lock: lock, host: host, result: installedQualificationResult{SchemaVersion: 1, SourceSHA: source, InstallationSHA256: digest, HelperSHA256: c.Helper.SHA256, TestBinarySHA256: testHash}}
	return f, f.stillStopped()
}
func (f *installedFixture) stillStopped() error {
	unit, err := fixtureWorkerUnit()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.c.Tools["systemctl"].Path, "--user", "show", unit, "--property=Id,LoadState,ActiveState,SubState,MainPID", "--no-pager")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "XDG_RUNTIME_DIR=/run/user/" + strconv.Itoa(f.c.OwnerUID), "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + strconv.Itoa(f.c.OwnerUID) + "/bus"}
	data, err := cmd.Output()
	if err != nil || len(data) > 4096 {
		return errors.New("existing worker stopped state is unavailable")
	}
	p := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			p[k] = v
		}
	}
	if p["Id"] != unit || p["LoadState"] != "loaded" || p["MainPID"] != "0" || (p["ActiveState"] != "inactive" && p["ActiveState"] != "failed") {
		return errors.New("existing worker must remain stopped throughout real qualification")
	}
	return nil
}
func fixtureHash(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), e
}
func fixtureWriteJSON(path string, v any, exclusive bool) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	b = append(b, '\n')
	flags := os.O_WRONLY | os.O_CREATE
	if exclusive {
		flags |= os.O_EXCL
	} else {
		flags |= os.O_TRUNC
	}
	f, e := os.OpenFile(path, flags, 0600)
	if e != nil {
		return e
	}
	n, e := f.Write(b)
	if e == nil && n != len(b) {
		e = io.ErrShortWrite
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	dir, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func fixtureReadJSON(path string, v any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	if len(b) > 1<<20 {
		return errors.New("fixture evidence exceeded its bound")
	}
	return decodeClosedServiceObject(b, v)
}

func (f *installedFixture) request(mode string, extra fixtureProbe, timeout time.Duration) (serviceRequest, error) {
	nonce, err := randomHex(16)
	if err != nil {
		return serviceRequest{}, err
	}
	dir, err := os.MkdirTemp(f.c.WorkspaceRoot, "step-qualification-")
	if err != nil {
		return serviceRequest{}, err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return serviceRequest{}, err
	}
	probe, err := os.Open(f.executable)
	if err != nil {
		return serviceRequest{}, err
	}
	out, err := os.OpenFile(filepath.Join(dir, "probe"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		probe.Close()
		return serviceRequest{}, err
	}
	_, err = io.Copy(out, probe)
	probe.Close()
	closeErr := out.Close()
	if err != nil {
		return serviceRequest{}, err
	}
	if closeErr != nil {
		return serviceRequest{}, closeErr
	}
	extra.Mode = mode
	extra.Nonce = nonce
	if err = fixtureWriteJSON(filepath.Join(dir, "probe.json"), extra, true); err != nil {
		return serviceRequest{}, err
	}
	dockerfile := "FROM scratch AS verify\nCOPY probe /probe\nCOPY probe.json /probe.json\nARG MEMQL_COMMIT\nARG MEMQL_RELEASE\nRUN [\"/probe\",\"-test.run=^TestImageVerificationQualificationPayload$\",\"-test.v\"]\nFROM scratch AS unselected\nCOPY --from=verify /qualified /qualified\nRUN [\"/must-not-exist\"]\n"
	if err = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0600); err != nil {
		return serviceRequest{}, err
	}
	git := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, f.c.Tools["git"].Path, args...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=MemQL qualification", "GIT_AUTHOR_EMAIL=qualification@localhost", "GIT_COMMITTER_NAME=MemQL qualification", "GIT_COMMITTER_EMAIL=qualification@localhost"}
		b, e := cmd.CombinedOutput()
		if len(b) > 8192 {
			return "", errors.New("fixture Git response exceeded bound")
		}
		return strings.TrimSpace(string(b)), e
	}
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "--", "Dockerfile", "probe.json", "probe"}, {"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgSign=false", "commit", "--quiet", "-m", "Fixed installed-image qualification fixture"}} {
		if _, err = git(args...); err != nil {
			return serviceRequest{}, errors.New("private fixture commit could not be prepared")
		}
	}
	sha, err := git("rev-parse", "--verify", "HEAD")
	if err != nil || !hash40.MatchString(sha) {
		return serviceRequest{}, errors.New("private fixture source is unavailable")
	}
	id, err := randomHex(16)
	if err != nil {
		return serviceRequest{}, err
	}
	return serviceRequest{Version: protocolVersion, Operation: "verify", ID: id, Workspace: dir, SourceSHA: sha, Build: Request{Context: ".", Dockerfile: "Dockerfile", Target: "verify", Args: map[string]string{"MEMQL_COMMIT": sha, "MEMQL_RELEASE": "qualification"}, VerificationOnly: true}, Limits: Limits{CPUMilli: 1000, MemoryMiB: 1024, MinFreeBytes: 1 << 30, Timeout: timeout}}, nil
}

func (f *installedFixture) run(q serviceRequest, cancelOn string) (*fixtureBuild, error) {
	result := &fixtureBuild{Request: q, Limits: map[string]string{}}
	ctx, cancel := context.WithTimeout(context.Background(), q.Limits.Timeout+45*time.Second)
	defer cancel()
	stop := make(chan struct{})
	done := make(chan struct{})
	started := false
	persist := func(r Record) error {
		result.Record = r
		path := filepath.Join(f.output, "record-"+q.ID+".json")
		if err := fixtureWriteJSON(path, r, true); err != nil {
			return err
		}
		started = true
		go func() { defer close(done); fixtureMonitor(stop, result, f.c) }()
		return nil
	}
	emit := func(_ bool, p []byte) {
		result.mu.Lock()
		if len(result.Logs) < 128<<10 {
			n := min(len(p), (128<<10)-len(result.Logs))
			result.Logs += string(p[:n])
		}
		matched := cancelOn != "" && strings.Contains(result.Logs, cancelOn)
		result.mu.Unlock()
		if matched {
			cancel()
		}
	}
	raw, err := callService(ctx, q, persist, emit, func(_ context.Context, c ServiceConfig) error { return InspectServiceInstallation(c) })
	if started {
		close(stop)
		<-done
	}
	result.Receipt = raw
	if err != nil {
		result.Error = err.Error()
	}
	if result.Record.ID != "" {
		cleanup, c := context.WithTimeout(context.Background(), 35*time.Second)
		e := awaitServiceCleanup(cleanup, f.c, result.Record)
		c()
		result.CleanupConfirmed = e == nil
	}
	if !result.CleanupConfirmed {
		return result, errors.New("exact service/cgroup/private-state cleanup was not proved")
	}
	if result.monitorErr != "" {
		return result, errors.New(result.monitorErr)
	}
	if e := os.RemoveAll(q.Workspace); e != nil {
		return result, e
	}
	return result, err
}
func fixtureMonitor(stop <-chan struct{}, r *fixtureBuild, c ServiceConfig) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		fixtureSample(r, c)
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}
func fixtureSample(r *fixtureBuild, c ServiceConfig) {
	limits, e := readCgroupLimits(r.Record.ChildGroup)
	if e == nil {
		r.Limits = limits
	}
	root, e := kernelCgroupPath(r.Record.ChildGroup)
	if e == nil {
		for _, p := range []struct {
			file, key string
			out       *uint64
		}{{"cpu.stat", "nr_throttled", &r.CPUThrottled}, {"memory.events", "oom_kill", &r.OOMKills}} {
			b, e := os.ReadFile(filepath.Join(root, p.file))
			if e == nil {
				fields := strings.Fields(string(b))
				for i := 0; i+1 < len(fields); i += 2 {
					if fields[i] == p.key {
						n, _ := strconv.ParseUint(fields[i+1], 10, 64)
						*p.out = max(*p.out, n)
					}
				}
			}
		}
	}
	_ = filepath.WalkDir(r.Record.Root, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		s, e := d.Info()
		if e != nil {
			return nil
		}
		if s.Size() > c.MaxStorageBytes {
			r.OversizeObserved = true
		}
		if x, ok := s.Sys().(*syscall.Stat_t); ok && x.Uid != 0 && int(x.Uid) != c.OwnerUID {
			r.MappedUIDObserved = true
		}
		return nil
	})
}
func fixtureWitness(r *fixtureBuild, want string) error {
	if !strings.Contains(r.Logs, "MEMQL_IMAGE_FIXTURE "+want) {
		return errors.New("real RUN witness missing")
	}
	return nil
}
func fixtureTargetExport(f *installedFixture) (any, error) {
	q, e := f.request("target", fixtureProbe{}, 45*time.Second)
	if e != nil {
		return nil, e
	}
	r, e := f.run(q, "")
	if e != nil {
		return r, e
	}
	if e = fixtureWitness(r, "target passed"); e != nil {
		return r, e
	}
	return r, validateReturnedReceipt(r.Receipt, q.Build, q.SourceSHA)
}
func fixtureResourceLimits(f *installedFixture) (any, error) {
	q, e := f.request("cpu", fixtureProbe{}, 45*time.Second)
	if e != nil {
		return nil, e
	}
	q.Limits.CPUMilli = 500
	r, e := f.run(q, "")
	evidence := []*fixtureBuild{r}
	if e != nil {
		return evidence, e
	}
	if e = checkCgroupLimits(r.Limits, q.Limits); e != nil {
		return evidence, e
	}
	if e = fixtureWitness(r, "cpu passed"); e != nil {
		return evidence, e
	}
	if r.CPUThrottled == 0 {
		return evidence, errors.New("CPU stress did not produce actual cgroup throttling")
	}
	q, e = f.request("memory", fixtureProbe{MemoryBytes: 2 << 30}, 45*time.Second)
	if e != nil {
		return evidence, e
	}
	r, e = f.run(q, "")
	evidence = append(evidence, r)
	if e == nil || r == nil || !r.CleanupConfirmed || r.OOMKills == 0 {
		return evidence, errors.New("memory stress did not prove bounded OOM and cleanup")
	}
	if e = fixtureWitness(r, "memory running"); e != nil {
		return evidence, e
	}
	return evidence, checkCgroupLimits(r.Limits, q.Limits)
}
func fixtureNamespaceIsolation(f *installedFixture) (any, error) {
	home, e := os.UserHomeDir()
	if e != nil {
		return nil, e
	}
	canary, e := os.CreateTemp(home, ".memql-image-qualification-")
	if e != nil {
		return nil, e
	}
	path := canary.Name()
	defer os.Remove(path)
	canary.WriteString("benign qualification canary")
	canary.Close()
	state := filepath.Join(f.c.AttemptRoot, "qualification-canary")
	if e = os.WriteFile(state, []byte("benign"), 0600); e != nil {
		return nil, e
	}
	defer os.Remove(state)
	socket := filepath.Join(f.output, "host-canary.sock")
	listener, e := net.Listen("unix", socket)
	if e != nil {
		return nil, e
	}
	defer listener.Close()
	defer os.Remove(socket)
	namespaces := map[string]string{}
	for _, name := range []string{"pid", "net", "mnt"} {
		v, err := os.Readlink("/proc/self/ns/" + name)
		if err != nil {
			return nil, err
		}
		namespaces[name] = v
	}
	q, e := f.request("isolation", fixtureProbe{Paths: []string{path, state, "/run/docker.sock", "/var/run/docker.sock", f.c.SocketPath}, Socket: socket, Namespaces: namespaces}, 45*time.Second)
	if e != nil {
		return nil, e
	}
	r, e := f.run(q, "")
	if e != nil {
		return r, e
	}
	return r, fixtureWitness(r, "isolation passed")
}
func fixtureNetworkIsolation(f *installedFixture) (any, error) {
	addresses, e := net.InterfaceAddrs()
	if e != nil {
		return nil, e
	}
	var ip net.IP
	for _, address := range addresses {
		n, ok := address.(*net.IPNet)
		if ok && n.IP.To4() != nil && n.IP.IsPrivate() && !n.IP.IsLoopback() {
			ip = n.IP
			break
		}
	}
	if ip == nil {
		return nil, errors.New("a real host private IPv4 endpoint is required for the network negative")
	}
	listener, e := net.Listen("tcp4", net.JoinHostPort(ip.String(), "0"))
	if e != nil {
		return nil, e
	}
	defer listener.Close()
	probe, e := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if e != nil {
		return nil, errors.New("host network canary was not reachable before the isolated negative")
	}
	probe.Close()
	q, e := f.request("network", fixtureProbe{PrivateEndpoint: listener.Addr().String()}, 45*time.Second)
	if e != nil {
		return nil, e
	}
	r, e := f.run(q, "")
	filterHash := sha256.Sum256([]byte(isolatedNetworkRules))
	evidence := struct {
		Build                                            *fixtureBuild
		PrivateEndpoint, CompiledFilterSHA256, NFTSHA256 string
		HostCanaryReachable, LiveFilterReadback          bool
	}{r, listener.Addr().String(), hex.EncodeToString(filterHash[:]), f.c.Tools["nft"].SHA256, true, false}
	if e != nil {
		return evidence, e
	}
	return evidence, fixtureWitness(r, "network passed")
}
func fixtureCancelTimeout(f *installedFixture) (any, error) {
	out := []*fixtureBuild{}
	for _, cancelled := range []bool{true, false} {
		limit := 12 * time.Second
		if cancelled {
			limit = 45 * time.Second
		}
		q, e := f.request("wait", fixtureProbe{}, limit)
		if e != nil {
			return out, e
		}
		cancelOn := ""
		if cancelled {
			cancelOn = "MEMQL_IMAGE_FIXTURE wait running"
		}
		r, e := f.run(q, cancelOn)
		out = append(out, r)
		if e == nil || r == nil || !r.CleanupConfirmed {
			return out, errors.New("live cancellation/timeout did not fail with cleanup")
		}
		if e = fixtureWitness(r, "wait running"); e != nil {
			return out, e
		}
	}
	return out, nil
}

// Payload is intentionally a closed fixture case set. It runs only inside a
// scratch build root containing the explicit probe.json and this static binary.
func TestImageVerificationQualificationPayload(t *testing.T) {
	if os.Getenv("MEMQL_IMAGE_QUALIFICATION") == "1" {
		t.Skip("payload is not a host qualification entrypoint")
	}
	var p fixtureProbe
	if e := fixtureReadJSON("/probe.json", &p); e != nil {
		t.Skip("requires fixture scratch root")
	}
	if !hash32.MatchString(p.Nonce) {
		t.Fatal("invalid fixture nonce")
	}
	passed := func() {
		if e := os.WriteFile("/qualified", []byte(p.Nonce), 0600); e != nil {
			t.Fatal(e)
		}
		fmt.Println("MEMQL_IMAGE_FIXTURE " + p.Mode + " passed " + p.Nonce)
	}
	switch p.Mode {
	case "target":
		passed()
	case "cpu":
		var wg sync.WaitGroup
		deadline := time.Now().Add(4 * time.Second)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var x uint64
				for time.Now().Before(deadline) {
					for j := 0; j < 100000; j++ {
						x = x*1664525 + 1013904223
					}
				}
				runtime.KeepAlive(x)
			}()
		}
		wg.Wait()
		passed()
	case "memory":
		if p.MemoryBytes != 2<<30 {
			t.Fatal("invalid memory fixture")
		}
		fmt.Println("MEMQL_IMAGE_FIXTURE memory running " + p.Nonce)
		blocks := [][]byte{}
		for int64(len(blocks))*(16<<20) < p.MemoryBytes {
			b := make([]byte, 16<<20)
			for i := range b {
				b[i] = 0x5a
			}
			blocks = append(blocks, b)
		}
		runtime.KeepAlive(blocks)
		t.Fatal("memory fixture exceeded its expected cgroup limit")
	case "isolation":
		for _, path := range p.Paths {
			if _, e := os.Stat(path); !os.IsNotExist(e) {
				t.Fatalf("host path was exposed: %v", e)
			}
		}
		c, e := net.DialTimeout("unix", p.Socket, 200*time.Millisecond)
		if e == nil {
			c.Close()
			t.Fatal("host socket was reachable")
		}
		if e = os.WriteFile("/sys/fs/cgroup/cgroup.procs", []byte("0"), 0600); e == nil {
			t.Fatal("RUN could alter its cgroup")
		}
		for _, name := range []string{"pid", "net", "mnt"} {
			v, e := os.Readlink("/proc/self/ns/" + name)
			if e != nil || v == "" || p.Namespaces[name] == "" || v == p.Namespaces[name] {
				t.Fatal("distinct namespace evidence unavailable")
			}
			fmt.Printf("MEMQL_IMAGE_NAMESPACE %s %s\n", name, v)
		}
		passed()
	case "network":
		if p.PrivateEndpoint == "" {
			t.Fatal("missing live private-network canary")
		}
		for _, address := range []string{p.PrivateEndpoint, "127.0.0.1:22", "10.0.2.2:22", "10.1.1.1:443", "169.254.169.254:80", "168.63.129.16:80", "192.168.1.1:443", "[::1]:22", "[2606:4700:4700::1111]:53"} {
			c, e := net.DialTimeout("tcp", address, 250*time.Millisecond)
			if e == nil {
				c.Close()
				t.Fatalf("denied route reached %s", address)
			}
		}
		resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "udp", "10.0.2.3:53")
		}}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, e := resolver.LookupIPAddr(ctx, "example.com"); e != nil {
			t.Fatal("permitted namespace DNS relay did not work", e)
		}
		passed()
	case "wait", "mapped":
		if p.Mode == "mapped" {
			if e := os.WriteFile("/mapped-state", []byte(p.Nonce), 0600); e != nil {
				t.Fatal(e)
			}
			if e := os.Chown("/mapped-state", 1234, 1234); e != nil {
				t.Fatal(e)
			}
		}
		fmt.Println("MEMQL_IMAGE_FIXTURE " + p.Mode + " running " + p.Nonce)
		for {
			time.Sleep(time.Second)
		}
	case "sparse":
		if p.StorageBytes < 1<<30 || p.StorageBytes > MaxStateBytes {
			t.Fatal("invalid sparse fixture")
		}
		file, e := os.Create("/oversized-state")
		if e != nil {
			t.Fatal(e)
		}
		if e = file.Truncate(p.StorageBytes + 1); e != nil {
			t.Fatal(e)
		}
		file.Close()
		fmt.Println("MEMQL_IMAGE_FIXTURE sparse running " + p.Nonce)
		for {
			time.Sleep(time.Second)
		}
	default:
		t.Fatal("unknown fixed payload fixture")
	}
}

// Subprocess and deliberate START ambiguity helpers are below. They keep the
// original source/marker immutable and never expose a generic command surface.
func fixtureWorkerRestart(f *installedFixture) (any, error) {
	return f.crashAndRecover("restart", false)
}
func fixtureAmbiguousStart(f *installedFixture) (any, error) {
	return f.crashAndRecover("ambiguous", false)
}
func fixtureMappedState(f *installedFixture) (any, error) {
	crash, e := f.crashAndRecover("mapped", true)
	if e != nil {
		return crash, e
	}
	q, e := f.request("sparse", fixtureProbe{StorageBytes: f.c.MaxStorageBytes}, 45*time.Second)
	if e != nil {
		return crash, e
	}
	r, e := f.run(q, "")
	evidence := struct {
		Crash  any
		Sparse *fixtureBuild
	}{crash, r}
	if e == nil || r == nil || !r.CleanupConfirmed || !r.OversizeObserved {
		return evidence, errors.New("real apparent-size storage limit and cleanup were not observed")
	}
	if !strings.Contains(r.Error, "storage") {
		return evidence, errors.New("oversize fixture failed for a reason other than the storage watchdog")
	}
	return evidence, nil
}

type fixtureChildResult struct {
	Passed bool
	Record Record
	Error  string
}

func (f *installedFixture) child(q serviceRequest, mode string) (*exec.Cmd, *os.File, error) {
	requestPath := filepath.Join(f.output, q.ID+"-request.json")
	if _, e := os.Stat(requestPath); os.IsNotExist(e) {
		if e = fixtureWriteJSON(requestPath, q, true); e != nil {
			return nil, nil, e
		}
	}
	log, e := os.OpenFile(filepath.Join(f.output, q.ID+"-"+mode+"-child.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, nil, e
	}
	cmd := exec.Command(f.executable, "-test.run=^TestImageVerificationQualificationChild$", "-test.count=1", "-test.timeout=100s")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "HOME=" + os.Getenv("HOME"), "GOMAXPROCS=2", "MEMQL_IMAGE_QUALIFICATION=1", "MEMQL_IMAGE_QUALIFICATION_WORKER_UNIT=" + os.Getenv("MEMQL_IMAGE_QUALIFICATION_WORKER_UNIT"), "MEMQL_IMAGE_QUALIFICATION_CHILD=" + mode, "MEMQL_IMAGE_QUALIFICATION_OUTPUT=" + f.output, "MEMQL_IMAGE_QUALIFICATION_SOURCE=" + f.source, "MEMQL_IMAGE_QUALIFICATION_REQUEST=" + requestPath, "MEMQL_HOST_BUILD_LOCK_FD=3"}
	cmd.ExtraFiles = []*os.File{f.lock}
	cmd.Stdout = log
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		log.Close()
		return nil, nil, e
	}
	return cmd, log, nil
}
func (f *installedFixture) crashAndRecover(mode string, killHelper bool) (any, error) {
	payload := "wait"
	if mode == "mapped" {
		payload = "mapped"
	}
	q, e := f.request(payload, fixtureProbe{}, 60*time.Second)
	if e != nil {
		return nil, e
	}
	cmd, log, e := f.child(q, mode)
	if e != nil {
		return nil, e
	}
	defer log.Close()
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	statePath := filepath.Join(f.output, q.ID+"-record.json")
	var record Record
	deadline := time.Now().Add(35 * time.Second)
	live := false
	observations := &fixtureBuild{Request: q}
	for time.Now().Before(deadline) {
		if e = fixtureReadJSON(statePath, &record); e == nil {
			observations.Record = record
			fixtureSample(observations, f.c)
		}
		if mode == "ambiguous" {
			_, e = os.Stat(filepath.Join(f.output, q.ID+"-start-written"))
			live = e == nil
		} else {
			_, e = os.Stat(filepath.Join(f.output, q.ID+"-run-live"))
			live = e == nil
		}
		if live && record.ID == q.ID && (!killHelper || observations.MappedUIDObserved) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !live || record.ID != q.ID || killHelper && !observations.MappedUIDObserved {
		return observations, errors.New("real child execution/retained identity was not observed")
	}
	if killHelper {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		loaded, err := readLoadedService(ctx, f.c, record.Identity.Unit)
		cancel()
		if err != nil || verifyServiceIdentity(f.c, record.Identity, ExpectedIdentity{record.Identity.Challenge, record.Identity.RequestSHA256}, loaded) != nil {
			return observations, errors.New("exact owned helper identity could not be checked before its deliberate crash")
		}
		if e = syscall.Kill(record.Identity.MainPID, syscall.SIGKILL); e != nil {
			return observations, e
		}
	}
	if mode != "ambiguous" {
		_ = cmd.Process.Kill()
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case <-wait:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		return observations, errors.New("fixture client did not exit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	for {
		e = confirmInvocationStopped(ctx, f.c, record)
		if e == nil {
			break
		}
		select {
		case <-ctx.Done():
			return observations, errors.New("original service/cgroup did not stop after client/helper crash")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if killHelper {
		if _, e = os.Lstat(record.Root); e != nil {
			return observations, errors.New("mapped-state crash did not retain the state that cleanup must prove")
		}
	}
	replacement, recoveryLog, e := f.child(q, "recover")
	if e != nil {
		return observations, e
	}
	defer recoveryLog.Close()
	if e = replacement.Wait(); e != nil {
		return observations, errors.New("fresh-process cleanup failed; retain worker stop and evidence")
	}
	var after fixtureChildResult
	if e = fixtureReadJSON(filepath.Join(f.output, q.ID+"-recovered.json"), &after); e != nil {
		return observations, e
	}
	if !after.Passed || after.Record.ID != record.ID || after.Record.AttemptSHA256 != record.AttemptSHA256 || after.Record.SourceSHA != record.SourceSHA {
		return observations, errors.New("fresh cleanup changed original source or marker identity")
	}
	if _, e = os.Lstat(record.Root); !os.IsNotExist(e) {
		return observations, errors.New("recovery retained private attempt state")
	}
	if e = os.RemoveAll(q.Workspace); e != nil {
		return observations, e
	}
	observations.CleanupConfirmed = true
	return struct {
		Before                               *fixtureBuild
		After                                fixtureChildResult
		OriginalClientPID, RecoveryClientPID int
	}{observations, after, cmd.Process.Pid, replacement.Process.Pid}, nil
}

func TestImageVerificationQualificationChild(t *testing.T) {
	mode := os.Getenv("MEMQL_IMAGE_QUALIFICATION_CHILD")
	if mode == "" {
		t.Skip("only the owner fixture launches its closed child modes")
	}
	if os.Getenv("MEMQL_IMAGE_QUALIFICATION") != "1" || !hash40.MatchString(os.Getenv("MEMQL_IMAGE_QUALIFICATION_SOURCE")) {
		t.Fatal("missing owner qualification scope")
	}
	if mode != "restart" && mode != "mapped" && mode != "ambiguous" && mode != "recover" {
		t.Fatal("unknown fixture child mode")
	}
	output := os.Getenv("MEMQL_IMAGE_QUALIFICATION_OUTPUT")
	if e := validatePrivateDirectory(output, os.Geteuid()); e != nil {
		t.Fatal(e)
	}
	path := os.Getenv("MEMQL_IMAGE_QUALIFICATION_REQUEST")
	if filepath.Dir(path) != output {
		t.Fatal("child request escaped evidence directory")
	}
	var q serviceRequest
	if e := fixtureReadJSON(path, &q); e != nil {
		t.Fatal(e)
	}
	c, e := LoadServiceConfig()
	if e != nil {
		t.Fatal(e)
	}
	if e = InspectServiceInstallation(c); e != nil {
		t.Fatal(e)
	}
	lock, e := fixtureInheritedLock()
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Close()
	if e = (&installedFixture{c: c}).stillStopped(); e != nil {
		t.Fatal(e)
	}
	if e = validateServiceRequest(c, q); e != nil {
		t.Fatal(e)
	}
	statePath := filepath.Join(output, q.ID+"-record.json")
	persist := func(r Record) error { return fixtureReplaceRecord(statePath, r) }
	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Second)
	defer cancel()
	if mode == "recover" {
		var record Record
		if e = fixtureReadJSON(statePath, &record); e != nil {
			t.Fatal(e)
		}
		e = Reconcile(ctx, record, persist)
		var after Record
		if e2 := fixtureReadJSON(statePath, &after); e2 != nil {
			t.Fatal(e2)
		}
		result := fixtureChildResult{Passed: e == nil, Record: after}
		if e != nil {
			result.Error = e.Error()
		}
		if e2 := fixtureWriteJSON(filepath.Join(output, q.ID+"-recovered.json"), result, true); e2 != nil {
			t.Fatal(e2)
		}
		if e != nil {
			t.Fatal(e)
		}
		return
	}
	if mode == "ambiguous" {
		if e = fixtureWriteAmbiguousStart(ctx, c, q, persist); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(output, q.ID+"-start-written"), []byte("START written; result deliberately unobserved\n"), 0600); e != nil {
			t.Fatal(e)
		}
		os.Exit(81)
	}
	var once sync.Once
	emit := func(_ bool, p []byte) {
		if bytes.Contains(p, []byte("MEMQL_IMAGE_FIXTURE ")) && bytes.Contains(p, []byte(" running ")) {
			once.Do(func() {
				_ = os.WriteFile(filepath.Join(output, q.ID+"-run-live"), []byte("actual RUN emitted a liveness witness\n"), 0600)
			})
		}
	}
	_, e = callService(ctx, q, persist, emit, func(_ context.Context, c ServiceConfig) error { return InspectServiceInstallation(c) })
	if e != nil {
		t.Fatal(e)
	}
	t.Fatal("crash fixture unexpectedly completed normally")
}
func fixtureReplaceRecord(path string, r Record) error {
	var old Record
	if e := fixtureReadJSON(path, &old); e == nil {
		if old.ID != r.ID || old.Workspace != r.Workspace || old.Root != r.Root || old.AttemptSHA256 != r.AttemptSHA256 || old.SourceSHA != r.SourceSHA {
			return errors.New("fixture recovery changed immutable original identity")
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	temp := path + ".pending"
	if e := fixtureWriteJSON(temp, r, true); e != nil {
		return e
	}
	if e := os.Rename(temp, path); e != nil {
		return e
	}
	dir, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func fixtureWriteAmbiguousStart(ctx context.Context, c ServiceConfig, q serviceRequest, persist Persist) error {
	socket, e := readLoadedSocket(ctx, c)
	if e != nil {
		return e
	}
	if e = verifyServiceSocket(installedServiceFiles{}, c, socket); e != nil {
		return e
	}
	hash, e := requestDigest(q)
	if e != nil {
		return e
	}
	challenge, e := randomHex(16)
	if e != nil {
		return e
	}
	conn, e := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", c.SocketPath)
	if e != nil {
		return e
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	w := &framedWriter{out: conn}
	if e = w.send(wireMessage{Type: "hello", Challenge: challenge, RequestSHA256: hash}); e != nil {
		return e
	}
	m, e := readMessage(conn)
	if e != nil || m.Type != "hello" || m.Hello == nil || !m.bound(challenge, hash) {
		return errors.New("fixture service hello failed")
	}
	loaded, e := readLoadedService(ctx, c, m.Hello.Unit)
	if e != nil {
		return e
	}
	if e = verifyServiceIdentity(c, *m.Hello, ExpectedIdentity{challenge, hash}, loaded); e != nil {
		return e
	}
	boot, e := currentBootID()
	if e != nil {
		return e
	}
	r := Record{ID: q.ID, Workspace: q.Workspace, Root: filepath.Join(c.AttemptRoot, q.ID), Identity: *m.Hello, ChildGroup: filepath.Join(m.Hello.ControlGroup, "build"), BootID: boot, AttemptSHA256: hash, SourceSHA: q.SourceSHA}
	if e = validateRecord(c, r); e != nil {
		return e
	}
	if e = persist(r); e != nil {
		return e
	}
	if e = w.send(wireMessage{Type: "prepare", Challenge: challenge, RequestSHA256: hash, Prepare: &q}); e != nil {
		return e
	}
	m, e = readMessage(conn)
	if e != nil || m.Type != "ready" || m.ChildGroup != r.ChildGroup || !m.bound(challenge, hash) {
		return errors.New("fixture service preparation failed")
	}
	if e = InspectServiceInstallation(c); e != nil {
		return e
	}
	loaded, e = readLoadedService(ctx, c, r.Identity.Unit)
	if e != nil {
		return e
	}
	if e = verifyServiceIdentity(c, r.Identity, ExpectedIdentity{challenge, hash}, loaded); e != nil {
		return e
	}
	limits, e := readCgroupLimits(r.ChildGroup)
	if e != nil {
		return e
	}
	if e = checkCgroupLimits(limits, q.Limits); e != nil {
		return e
	}
	raw, e := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", r.Identity.MainPID))
	if e != nil {
		return e
	}
	group, e := parseUnifiedCgroup(string(raw))
	if e != nil || group != r.ChildGroup {
		return errors.New("fixture helper escaped admitted group")
	}
	// Deliberately do not read a result. A successful socket write cannot tell
	// whether the server consumed START; fresh reconciliation must settle it.
	return w.send(wireMessage{Type: "start", Challenge: challenge, RequestSHA256: hash})
}

func fixtureInheritedLock() (*os.File, error) {
	fd, err := strconv.Atoi(os.Getenv("MEMQL_HOST_BUILD_LOCK_FD"))
	if err != nil || fd < 3 {
		return nil, errors.New("inherited host reservation is required")
	}
	lock := os.NewFile(uintptr(fd), "inherited-host-reservation")
	actual, err := lock.Stat()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	p := filepath.Join(home, ".memql", "worker-capacity", "pipeline.lock")
	want, err := os.Lstat(p)
	if err != nil || !want.Mode().IsRegular() || !os.SameFile(actual, want) {
		return nil, errors.New("inherited reservation is not this account's fixed pipeline lock")
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.New("inherited host reservation is unavailable")
	}
	probe, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	probe.Close()
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, errors.New("host reservation exclusivity was not proved")
	}
	return lock, nil
}

func TestQualificationRecordKeepsOriginalIdentity(t *testing.T) {
	for _, field := range []string{"id", "workspace", "root", "source", "marker", "interrupted-update", "new-invocation"} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "record.json")
			original := Record{ID: strings.Repeat("a", 32), Workspace: "/owned/step-source", Root: "/attempts/exact", SourceSHA: strings.Repeat("b", 40), AttemptSHA256: strings.Repeat("c", 64)}
			if e := fixtureReplaceRecord(path, original); e != nil {
				t.Fatal(e)
			}
			next := original
			switch field {
			case "id":
				next.ID = strings.Repeat("d", 32)
			case "workspace":
				next.Workspace += "-foreign"
			case "root":
				next.Root += "-foreign"
			case "source":
				next.SourceSHA = strings.Repeat("d", 40)
			case "marker":
				next.AttemptSHA256 = strings.Repeat("d", 64)
			case "interrupted-update":
				if e := os.WriteFile(path+".pending", []byte("retained interrupted update"), 0600); e != nil {
					t.Fatal(e)
				}
			case "new-invocation":
				next.Identity.InvocationID = strings.Repeat("d", 32)
			}
			e := fixtureReplaceRecord(path, next)
			var actual Record
			if readErr := fixtureReadJSON(path, &actual); readErr != nil {
				t.Fatal(readErr)
			}
			if field == "new-invocation" {
				if e != nil || !reflect.DeepEqual(actual, next) {
					t.Fatal("legitimate fresh cleanup identity was not retained", e)
				}
			} else if e == nil || !reflect.DeepEqual(actual, original) {
				t.Fatal("refused update changed durable original source", e)
			}
		})
	}
}
func TestQualificationEvidenceRefusesOverwriteAndUnboundedRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "evidence.json")
	if e := fixtureWriteJSON(p, map[string]bool{"actual": true}, true); e != nil {
		t.Fatal(e)
	}
	before, e := fixtureHash(p)
	if e != nil {
		t.Fatal(e)
	}
	if fixtureWriteJSON(p, map[string]bool{"fabricated": true}, true) == nil {
		t.Fatal("historical evidence was overwritten")
	}
	after, e := fixtureHash(p)
	if e != nil || before != after {
		t.Fatal("refused evidence replacement changed bytes")
	}
	for _, raw := range []string{`{"ID":"a","ID":"b"}`, `{"unknown":true}`, strings.Repeat(" ", (1<<20)+1)} {
		if e = os.WriteFile(filepath.Join(dir, "invalid.json"), []byte(raw), 0600); e != nil {
			t.Fatal(e)
		}
		var r Record
		if fixtureReadJSON(filepath.Join(dir, "invalid.json"), &r) == nil {
			t.Fatal("invalid fixture record accepted")
		}
	}
}
func TestQualificationInheritedLockRequiresExactExclusiveFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MEMQL_HOST_BUILD_LOCK_FD", "")
	if _, e := fixtureInheritedLock(); e == nil {
		t.Fatal("missing host lock accepted")
	}
	dir := filepath.Join(home, ".memql", "worker-capacity")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	f, e := os.OpenFile(filepath.Join(dir, "pipeline.lock"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	foreign, e := os.CreateTemp(home, "foreign")
	if e != nil {
		t.Fatal(e)
	}
	defer foreign.Close()
	t.Setenv("MEMQL_HOST_BUILD_LOCK_FD", strconv.Itoa(int(foreign.Fd())))
	if _, e = fixtureInheritedLock(); e == nil {
		t.Fatal("foreign inherited file accepted")
	}
	t.Setenv("MEMQL_HOST_BUILD_LOCK_FD", strconv.Itoa(int(f.Fd())))
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		t.Fatal(e)
	}
	got, e := fixtureInheritedLock()
	if e != nil {
		t.Fatal(e)
	}
	// got and f intentionally name the same inherited descriptor. Keep it alive
	// through this assertion without independently closing that descriptor twice.
	if got.Fd() != f.Fd() {
		t.Fatal("inherited reservation identity changed")
	}
	runtime.KeepAlive(got)
}

func fixtureWorkerUnit() (string, error) {
	unit := os.Getenv("MEMQL_IMAGE_QUALIFICATION_WORKER_UNIT")
	if len(unit) > 128 || !regexp.MustCompile(`^memql[-A-Za-z0-9_.@]*\.service$`).MatchString(unit) {
		return "", errors.New("the exact reviewed worker unit is required")
	}
	return unit, nil
}
func TestQualificationWorkerUnitIsExact(t *testing.T) {
	for _, unit := range []string{"", "other.service", "--all", "memql.service\nother.service", "memql/worker.service", "memql-worker.service", "memql-worker@owned.service"} {
		t.Run(fmt.Sprintf("%q", unit), func(t *testing.T) {
			t.Setenv("MEMQL_IMAGE_QUALIFICATION_WORKER_UNIT", unit)
			got, err := fixtureWorkerUnit()
			valid := unit == "memql-worker.service" || unit == "memql-worker@owned.service"
			if valid && (err != nil || got != unit) || !valid && err == nil {
				t.Fatal("worker unit identity or argument boundary changed")
			}
		})
	}
}
