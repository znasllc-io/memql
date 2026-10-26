//go:build linux

package imageverify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type namespaceInput struct {
	Config                 ServiceConfig  `json:"config"`
	Record                 Record         `json:"record"`
	Request                serviceRequest `json:"request"`
	ParentPIDNamespace     string         `json:"parentPIDNamespace"`
	ParentNetworkNamespace string         `json:"parentNetworkNamespace"`
	RootlessState          string         `json:"rootlessState"`
}
type namespaceResult struct {
	Receipt json.RawMessage
	Error   string
	Cleaned bool
}

const isolatedNetworkRules = `table inet memql_image_verification {
 chain output { type filter hook output priority -100; policy drop;
  ct state established,related accept
  ip daddr 10.0.2.3 udp dport 53 accept
  ip daddr 10.0.2.3 tcp dport 53 accept
  ip daddr { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 168.63.129.16, 224.0.0.0/4, 240.0.0.0/4 } drop
  meta nfproto ipv4 accept
 }
 chain input { type filter hook input priority -100; policy drop;
  ct state established,related accept
 }
 chain forward { type filter hook forward priority -100; policy drop; }
}
`

func toolEnvironment(c ServiceConfig, r Record) []string {

	return []string{"PATH=" + ServiceToolPath(c), "HOME=" + filepath.Join(r.Root, "home"), "XDG_RUNTIME_DIR=" + filepath.Join(r.Root, "runtime"), "TMPDIR=" + filepath.Join(r.Root, "runtime"), "DOCKER_CONFIG=" + filepath.Join(r.Root, "home", "docker"), "LANG=C", "LC_ALL=C"}
}

func executeNamespace(ctx context.Context, c ServiceConfig, r Record, q serviceRequest, emit Emit) namespaceResult {
	pidns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return namespaceResult{Error: "PID namespace identity unavailable"}
	}
	netns, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return namespaceResult{Error: "network namespace identity unavailable"}
	}
	state := filepath.Join(r.Root, "ns-"+r.Identity.InvocationID)
	input := namespaceInput{c, r, q, pidns, netns, state}
	data, err := json.Marshal(input)
	if err != nil || len(data) > maxPrepareBytes {
		return namespaceResult{Error: "bounded namespace request unavailable"}
	}
	args := []string{"--state-dir=" + state, "--pidns", "--copy-up=/etc", "--subid-source=static", "--port-driver=none", "--propagation=rprivate"}
	if q.Operation == "verify" {
		args = append(args, "--net=slirp4netns", "--slirp4netns-binary="+c.Tools["slirp4netns"].Path, "--cidr=10.0.2.0/24", "--disable-host-loopback")
	} else {
		args = append(args, "--net=none")
	}
	args = append(args, "--", c.Helper.Path, "--image-verification-runtime")
	cmd := exec.Command(c.Tools["rootlesskit"].Path, args...)
	cmd.Env = toolEnvironment(c, r)
	cmd.Stdin = bytes.NewReader(data)
	cmd.WaitDelay = 2 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, output, err := os.Pipe()
	if err != nil {
		return namespaceResult{Error: "namespace output unavailable"}
	}
	defer stdout.Close()
	cmd.Stdout = output
	stderr := &limitedBuffer{limit: 4096}
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		output.Close()
		return namespaceResult{Error: "isolated image runtime could not start"}
	}
	output.Close()
	// Wait runs concurrently with framed output so its pipe timeout is active.
	// On cancellation a bounded graceful interval lets the inner helper clean
	// mapped state, then the original process group is killed and output closed.
	// Detached descendants are still in the service cgroup; an uncertain result
	// exits this helper so systemd kills that group before the worker releases it.
	return joinNamespaceProcess(ctx, cmd, stdout, r, emit)
}

func joinNamespaceProcess(ctx context.Context, cmd *exec.Cmd, stdout *os.File, r Record, emit Emit) namespaceResult {
	results := make(chan namespaceResult, 1)
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	go func() {
		result := namespaceResult{Error: "isolated image runtime ended without cleanup evidence"}
		for {
			message, e := readMessage(stdout)
			if e != nil || !message.bound(r.Identity.Challenge, r.Identity.RequestSHA256) {
				break
			}
			if message.Type == "log" {
				if emit != nil {
					emit(message.Stderr, message.Log)
				}
				continue
			}
			if message.Type == "result" {
				result = namespaceResult{message.Receipt, message.Error, message.Cleaned}
			}
			break
		}
		results <- result
	}()
	stop := func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = stdout.Close() }
	defer stop()
	var result namespaceResult
	var waitErr error
	haveResult, haveWait := false, false
	done := ctx.Done()
	var expiry <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	beginStop := func() {
		if timer == nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			timer = time.NewTimer(2 * time.Second)
			expiry = timer.C
		}
	}
	for !haveResult || !haveWait {
		select {
		case result = <-results:
			haveResult = true
			if !result.Cleaned {
				beginStop()
			}
		case waitErr = <-waited:
			haveWait = true
			if !haveResult {
				beginStop()
			}
		case <-done:
			done = nil
			beginStop()
		case <-expiry:
			stop()
			if !haveWait {
				select {
				case waitErr = <-waited:
					haveWait = true
				case <-time.After(2 * time.Second):
					return namespaceResult{Error: "namespace termination could not be confirmed"}
				}
			}
			if !haveResult {
				select {
				case result = <-results:
					haveResult = true
				case <-time.After(time.Second):
					return namespaceResult{Error: "namespace output did not close"}
				}
			}
			expiry = nil
		}
	}
	if waitErr != nil && !result.Cleaned {
		result.Error = "isolated image runtime failed; mapped state requires reconciliation"
	}
	return result
}

func serveNamespace() error {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, maxPrepareBytes+1))
	if err != nil || len(data) > maxPrepareBytes {
		return errors.New("namespace request exceeded its bound")
	}
	var in namespaceInput
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil {
		return errors.New("invalid namespace request")
	}
	if err = validateNamespace(in); err != nil {
		return err
	}
	writer := &framedWriter{out: os.Stdout}
	reply := func(message wireMessage) error {
		message.Challenge = in.Record.Identity.Challenge
		message.RequestSHA256 = in.Record.Identity.RequestSHA256
		return writer.send(message)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, in.Request.Limits.Timeout)
	defer cancel()
	result := namespaceResult{}
	if in.Request.Operation == "verify" {
		result.Error = runNamespaceBuild(ctx, in, func(stderr bool, p []byte) {
			for len(p) > 0 {
				n := min(len(p), 16384)
				if reply(wireMessage{Type: "log", Log: p[:n], Stderr: stderr}) != nil {
					cancel()
				}
				p = p[n:]
			}
		})
	}
	// This signal is legal only after proving that PID 1 is the installed
	// RootlessKit in a different PID namespace. Linux excludes PID 1 and the
	// caller from kill(-1); no host process can be targeted from this scope.
	if err = stopNamespaceProcesses(in); err != nil {
		result.Error = "namespace process cleanup is incomplete"
	} else {
		if in.Request.Operation == "verify" && result.Error == "" {
			result.Receipt, err = buildReceipt(in.Record, in.Request.Build, in.Request.SourceSHA)
			if err != nil {
				result.Error = "image export evidence is invalid"
			}
		}
		if err = clearMappedState(in); err == nil {
			result.Cleaned = true
		} else {
			result.Error = "mapped image state cleanup is incomplete"
		}
	}
	if err = reply(wireMessage{Type: "result", Receipt: result.Receipt, Error: result.Error, Cleaned: result.Cleaned}); err != nil {
		return err
	}
	return nil
}

func validateNamespace(in namespaceInput) error {
	if ValidateServiceConfig(in.Config) != nil || validateRecord(in.Config, in.Record) != nil || in.RootlessState != filepath.Join(in.Record.Root, "ns-"+in.Record.Identity.InvocationID) || os.Geteuid() != 0 {
		return errors.New("namespace identity is invalid")
	}
	pidns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil || pidns == in.ParentPIDNamespace || !strings.HasPrefix(pidns, "pid:[") {
		return errors.New("image process is not in its isolated PID namespace")
	}
	init, err := os.Readlink("/proc/1/exe")
	if err != nil || init != in.Config.Tools["rootlesskit"].Path {
		return errors.New("image namespace init is not the fixed RootlessKit")
	}
	uidmap, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		return err
	}
	fields := strings.Fields(string(uidmap))
	if len(fields) < 3 || fields[0] != "0" || fields[1] != strconv.Itoa(in.Config.OwnerUID) || fields[2] != "1" {
		return errors.New("rootless owner mapping differs")
	}
	cg, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}
	group, err := parseUnifiedCgroup(string(cg))
	if err != nil || group != in.Record.ChildGroup {
		return errors.New("image namespace escaped the bounded child group")
	}
	limits, err := readCgroupLimits(group)
	if err != nil {
		return err
	}
	if err = checkCgroupLimits(limits, in.Request.Limits); err != nil {
		return err
	}
	hash, err := requestDigest(in.Request)
	if err != nil || hash != in.Record.Identity.RequestSHA256 {
		return errors.New("namespace request changed")
	}
	return nil
}

func runNamespaceBuild(ctx context.Context, in namespaceInput, emit Emit) string {
	c, r, q := in.Config, in.Record, in.Request
	netns, err := os.Readlink("/proc/self/ns/net")
	if err != nil || netns == in.ParentNetworkNamespace {
		return "image network namespace is not isolated"
	}
	nft := exec.CommandContext(ctx, c.Tools["nft"].Path, "-f", "-")
	nft.Env = toolEnvironment(c, r)
	nft.Stdin = strings.NewReader(isolatedNetworkRules)
	nft.Stdout = io.Discard
	nft.Stderr = io.Discard
	if nft.Run() != nil {
		return "namespace-only network filter could not be established"
	}
	config := filepath.Join(r.Root, "buildkitd.toml")
	if os.WriteFile(config, []byte("insecure-entitlements = []\n[worker.oci]\n gc = false\n noProcessSandbox = false\n"), 0600) != nil {
		return "fixed builder configuration could not be written"
	}
	addr := "unix://" + filepath.Join(r.Root, "buildkit.sock")
	daemon := exec.CommandContext(ctx, c.Tools["buildkitd"].Path, "--rootless", "--root", filepath.Join(r.Root, "state"), "--addr", addr, "--oci-worker=true", "--containerd-worker=false", "--oci-worker-binary", c.Tools["runc"].Path, "--oci-worker-snapshotter=native", "--oci-worker-net=host", "--config", config)
	daemon.Env = toolEnvironment(c, r)
	daemon.Stdout = io.Discard
	daemon.Stderr = io.Discard
	daemon.WaitDelay = 2 * time.Second
	if daemon.Start() != nil {
		return "rootless BuildKit could not start"
	}
	defer func() { _ = daemon.Process.Kill(); _ = daemon.Wait() }()
	ready, readyCancel := context.WithTimeout(ctx, 20*time.Second)
	defer readyCancel()
	for {
		probe := exec.CommandContext(ready, c.Tools["buildctl"].Path, "--addr", addr, "debug", "workers")
		probe.Env = toolEnvironment(c, r)
		probe.Stdout = io.Discard
		probe.Stderr = io.Discard
		probe.WaitDelay = time.Second
		if probe.Run() == nil {
			break
		}
		select {
		case <-ready.Done():
			return "rootless BuildKit did not become ready"
		case <-time.After(100 * time.Millisecond):
		}
	}
	contextPath, err := sourcePath(r.Workspace, q.Build.Context, true)
	if err != nil {
		return "image context is invalid"
	}
	dockerfile, err := sourcePath(r.Workspace, q.Build.Dockerfile, false)
	if err != nil {
		return "image Dockerfile is invalid"
	}
	args := imageBuildArgs(r, q.Build, contextPath, dockerfile)
	buildCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watch := make(chan error, 1)
	go func() { watch <- watchNamespaceStorage(buildCtx, in, cancel) }()
	cmd := exec.CommandContext(buildCtx, c.Tools["buildctl"].Path, args...)
	cmd.Env = toolEnvironment(c, r)
	cmd.Stdout = logWriter{emit, false}
	cmd.Stderr = logWriter{emit, true}
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	cancel()
	storageErr := <-watch
	if storageErr != nil {
		return "image storage or free-space bound was reached"
	}
	if err != nil || ctx.Err() != nil {
		return "image build failed or was cancelled"
	}
	return ""
}

type logWriter struct {
	emit   Emit
	stderr bool
}

func (w logWriter) Write(p []byte) (int, error) { w.emit(w.stderr, p); return len(p), nil }
func imageBuildArgs(r Record, q Request, contextPath, dockerfile string) []string {
	args := []string{"--addr", "unix://" + filepath.Join(r.Root, "buildkit.sock"), "build", "--frontend=dockerfile.v0", "--local", "context=" + contextPath, "--local", "dockerfile=" + filepath.Dir(dockerfile), "--opt", "filename=" + filepath.Base(dockerfile), "--opt", "platform=linux/amd64", "--output", "type=oci,dest=" + filepath.Join(r.Root, "output", "image.oci.tar"), "--metadata-file", filepath.Join(r.Root, "output", "metadata.json"), "--progress=plain"}
	if q.Target != "" {
		args = append(args, "--opt", "target="+q.Target)
	}
	keys := make([]string, 0, len(q.Args))
	for k := range q.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--opt", "build-arg:"+k+"="+q.Args[k])
	}
	return args
}

func stopNamespaceProcesses(in namespaceInput) error {
	if err := validateNamespace(in); err != nil {
		return err
	}
	if err := syscall.Kill(-1, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return err
		}
		alive := false
		for _, entry := range entries {
			pid, e := strconv.Atoi(entry.Name())
			if e != nil || pid == 1 || pid == os.Getpid() {
				continue
			}
			status, e := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return e
			}
			if !strings.Contains(string(status), "State:\tZ") {
				alive = true
			}
		}
		if !alive {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("image namespace retained live processes")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func clearMappedState(in namespaceInput) error {
	entries, err := os.ReadDir(in.Record.Root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		p := filepath.Join(in.Record.Root, entry.Name())
		if p == in.RootlessState || entry.Name() == "attempt.json" {
			continue
		}
		if err = os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

func watchNamespaceStorage(ctx context.Context, in namespaceInput, cancel context.CancelFunc) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := measureNamespaceStorage(in); err != nil {
			cancel()
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
func measureNamespaceStorage(in namespaceInput) error {
	var used int64
	for _, root := range []string{in.Record.Workspace, in.Record.Root} {
		if _, err := os.Lstat(root); err != nil {
			return err
		}
		err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			info, err := entry.Info()
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return errors.New("mapped filesystem accounting unavailable")
			}
			size := max(info.Size(), stat.Blocks*512)
			if size > in.Config.MaxStorageBytes-used {
				return errors.New("private storage limit exceeded")
			}
			used += size
			return nil
		})
		if err != nil {
			return err
		}
		var stat syscall.Statfs_t
		if err = syscall.Statfs(root, &stat); err != nil {
			return err
		}
		if stat.Bsize <= 0 || stat.Bavail < in.Request.Limits.MinFreeBytes/uint64(stat.Bsize)+1 {
			return errors.New("filesystem headroom exhausted")
		}
	}
	return nil
}
