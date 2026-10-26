//go:build linux

package imageverify

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// InternalMain is a fixed installed-service entrypoint. It exposes no arbitrary CLI
// commands, network listener, credential reader or general manager operation.
func InternalMain(args []string) (bool, int) {
	if len(args) != 1 {
		return false, 0
	}
	switch args[0] {
	case "--render-image-verification-installation":
		if renderServiceInstallation(os.Stdin, os.Stdout) != nil {
			return true, 1
		}
		return true, 0
	case "--check-image-verification-installation":
		if checkServiceInstallationForOperator(os.Stdout) != nil {
			return true, 1
		}
		return true, 0
	case "--validate-image-verification-attestation":
		if validateServiceAttestationForOperator(os.Stdin, os.Stdout) != nil {
			return true, 1
		}
		return true, 0
	case "--serve-image-verification-socket":
		if serveSocket() != nil {
			return true, 1
		}
		return true, 0
	case "--image-verification-runtime":
		if serveNamespace() != nil {
			return true, 1
		}
		return true, 0
	default:
		return false, 0
	}
}

func serveSocket() error {
	c, err := LoadServiceConfig()
	if err != nil {
		return err
	}
	if c.OwnerUID != os.Geteuid() || os.Geteuid() == 0 {
		return errors.New("image service must use its fixed non-root identity")
	}
	if err = InspectServiceInstallation(c); err != nil {
		return err
	}
	self, err := os.Readlink("/proc/self/exe")
	if err != nil || self != c.Helper.Path {
		return errors.New("image service executable differs from its immutable installation")
	}
	connection, err := net.FileConn(os.Stdin)
	if err != nil {
		return err
	}
	defer connection.Close()
	unix, ok := connection.(*net.UnixConn)
	if !ok {
		return errors.New("image service requires its accepted Unix socket")
	}
	peer, err := peerUID(unix)
	if err != nil || peer != uint32(c.OwnerUID) {
		return errors.New("image service connection belongs to another account")
	}
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	first, err := readMessage(connection)
	if err != nil || first.Type != "hello" || !serviceNonce.MatchString(first.Challenge) || !serviceHash.MatchString(first.RequestSHA256) {
		return errors.New("image service hello is invalid")
	}
	current, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}
	group, err := parseUnifiedCgroup(string(current))
	if err != nil {
		return err
	}
	unit := filepath.Base(group)
	loaded, err := readLoadedService(context.Background(), c, unit)
	if err != nil {
		return err
	}
	hello := HelloIdentity{Challenge: first.Challenge, RequestSHA256: first.RequestSHA256, HelperSHA256: c.Helper.SHA256, Unit: unit, MainPID: os.Getpid(), OwnerUID: c.OwnerUID, InvocationID: loaded.InvocationID, ControlGroup: group}
	if err = verifyServiceIdentity(c, hello, ExpectedIdentity{first.Challenge, first.RequestSHA256}, loaded); err != nil {
		return err
	}
	writer := &framedWriter{out: connection}
	reply := func(message wireMessage) error {
		message.Challenge = first.Challenge
		message.RequestSHA256 = first.RequestSHA256
		return writer.send(message)
	}
	if err = reply(wireMessage{Type: "hello", Hello: &hello}); err != nil {
		return err
	}
	prepare, err := readMessage(connection)
	if err != nil || prepare.Type != "prepare" || prepare.Prepare == nil || !prepare.bound(first.Challenge, first.RequestSHA256) {
		return errors.New("image service preparation is invalid")
	}
	request := *prepare.Prepare
	hash, err := requestDigest(request)
	if err != nil || hash != first.RequestSHA256 {
		return errors.New("image service request hash changed")
	}
	if err = validateServiceRequest(c, request); err != nil {
		return err
	}
	child, err := admitChildGroup(group, request.Limits)
	if err != nil {
		return err
	}
	boot, err := currentBootID()
	if err != nil {
		return err
	}
	record := Record{ID: request.ID, Workspace: request.Workspace, Root: filepath.Join(c.AttemptRoot, request.ID), Identity: hello, ChildGroup: child, BootID: boot, AttemptSHA256: hash, SourceSHA: request.SourceSHA}
	if request.Previous != nil {
		record.AttemptSHA256 = request.Previous.AttemptSHA256
		record.SourceSHA = request.Previous.SourceSHA
	}
	created := false
	if request.Operation == "verify" {
		if err = createAttempt(c, record, request); err != nil {
			return err
		}
		created = true
	} else {
		if err = validateRecord(c, *request.Previous); err != nil {
			return err
		}
		if err = confirmInvocationStopped(context.Background(), c, *request.Previous); err != nil {
			return err
		}
		if err = prepareCleanupMarker(record); err != nil {
			return err
		}
	}
	started := false
	defer func() {
		if created && !started {
			_ = removePreparedAttempt(c, record)
		}
	}()
	if err = reply(wireMessage{Type: "ready", ChildGroup: child}); err != nil {
		return err
	}
	start, err := readMessage(connection)
	if err != nil || start.Type != "start" || !start.bound(first.Challenge, first.RequestSHA256) {
		return errors.New("image service start was not received")
	}
	started = true
	_ = connection.SetDeadline(time.Time{})
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, request.Limits.Timeout)
	defer timeout()
	go func() {
		message, e := readMessage(connection)
		if e != nil || message.Type == "cancel" || !message.bound(first.Challenge, first.RequestSHA256) {
			cancel()
			return
		}
		cancel()
	}()
	result := executeNamespace(ctx, c, record, request, func(stderr bool, data []byte) {
		_ = connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if e := reply(wireMessage{Type: "log", Log: data, Stderr: stderr}); e != nil {
			cancel()
		}
	})
	if result.Cleaned {
		if err = removePreparedAttempt(c, record); err != nil {
			result.Cleaned = false
			result.Error = "image verification private state cleanup is incomplete"
		}
	}
	_ = connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err = reply(wireMessage{Type: "result", Receipt: result.Receipt, Error: result.Error, Cleaned: result.Cleaned}); err != nil {
		return err
	}
	return nil
}

func peerUID(c *net.UnixConn) (uint32, error) {
	var credentials *syscall.Ucred
	var callErr error
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	if err = raw.Control(func(fd uintptr) {
		credentials, callErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if callErr != nil {
		return 0, callErr
	}
	if credentials == nil {
		return 0, errors.New("Unix peer identity unavailable")
	}
	return credentials.Uid, nil
}

func validateServiceRequest(c ServiceConfig, r serviceRequest) error {
	if r.Version != protocolVersion || !hash32.MatchString(r.ID) || validateLimits(r.Limits) != nil || r.Limits.CPUMilli > c.MaxCPUMilli || r.Limits.MemoryMiB > c.MaxMemoryMiB || r.Limits.Timeout > time.Duration(c.MaxRuntimeSeconds-10)*time.Second {
		return errors.New("image operation exceeds the fixed service contract")
	}
	if err := validatePrivateDirectory(c.AttemptRoot, c.OwnerUID); err != nil {
		return err
	}
	if r.Operation == "cleanup" {
		if r.Previous == nil || r.Previous.ID != r.ID || r.Previous.Workspace != r.Workspace {
			return errors.New("cleanup does not bind its previous attempt")
		}
		return nil
	}
	if r.Operation != "verify" || r.Previous != nil || r.Build.Validate(r.SourceSHA, "linux/amd64") != nil {
		return errors.New("invalid typed image operation")
	}
	if !serviceWithin(r.Workspace, c.WorkspaceRoot) || filepath.Clean(r.Workspace) != r.Workspace || !strings.HasPrefix(filepath.Base(r.Workspace), "step-") {
		return errors.New("image source is outside the fixed pipeline workspace")
	}
	if err := validatePrivateDirectory(c.WorkspaceRoot, c.OwnerUID); err != nil {
		return err
	}
	if err := validatePrivateDirectory(r.Workspace, c.OwnerUID); err != nil {
		return err
	}
	physical, err := filepath.EvalSymlinks(r.Workspace)
	if err != nil || physical != r.Workspace {
		return errors.New("image source crosses a symbolic path")
	}
	stat, err := os.Lstat(r.Workspace)
	if err != nil || !stat.IsDir() || stat.Mode().Perm() != 0700 {
		return errors.New("image source is not a private checkout")
	}
	owner, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != c.OwnerUID {
		return errors.New("image checkout belongs to another account")
	}
	if _, err = sourcePath(r.Workspace, r.Build.Context, true); err != nil {
		return err
	}
	if _, err = sourcePath(r.Workspace, r.Build.Dockerfile, false); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Tools["git"].Path, "--no-optional-locks", "--no-replace-objects", "-c", "safe.directory="+r.Workspace, "rev-parse", "--verify", "HEAD")
	cmd.Dir = r.Workspace
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	cmd.WaitDelay = time.Second
	out := &limitedBuffer{limit: 4096}
	cmd.Stdout = out
	cmd.Stderr = out
	if err = cmd.Run(); err != nil || out.exceeded || strings.TrimSpace(out.String()) != r.SourceSHA {
		return errors.New("image checkout does not match its exact source")
	}
	return nil
}

func removePreparedAttempt(c ServiceConfig, r Record) error {
	if err := validateRecord(c, r); err != nil {
		return err
	}
	stat, err := os.Lstat(r.Root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !stat.IsDir() || stat.Mode()&os.ModeSymlink != 0 {
		return errors.New("image state has the wrong type")
	}
	if err = validatePrivateDirectory(r.Root, c.OwnerUID); err != nil {
		return err
	}
	if err = os.RemoveAll(r.Root); err != nil {
		return err
	}
	if _, err = os.Lstat(r.Root); !os.IsNotExist(err) {
		return errors.New("image private state is still present")
	}
	return nil
}
