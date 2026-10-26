//go:build linux

package imageverify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"
)

func Available(ctx context.Context) bool { return QualifiedServiceAvailable(ctx) }

func Run(ctx context.Context, workspace, sha string, q Request, l Limits, persist Persist, emit Emit) error {
	if err := q.Validate(sha, "linux/amd64"); err != nil {
		return err
	}
	if err := validateLimits(l); err != nil {
		return err
	}
	id, err := randomHex(16)
	if err != nil {
		return err
	}
	request := serviceRequest{Version: protocolVersion, Operation: "verify", ID: id, Workspace: workspace, SourceSHA: sha, Build: q, Limits: l}
	destination, err := reserveReceiptDestination(workspace)
	if err != nil {
		return err
	}
	defer destination.Close()
	receipt, err := callService(ctx, request, persist, emit, QualifiedService)
	if err != nil {
		return err
	}
	if err = validateReturnedReceipt(receipt, q, sha); err != nil {
		return err
	}
	return destination.Write(receipt)
}

func Reconcile(ctx context.Context, record Record, persist Persist) error {
	c, err := LoadServiceConfig()
	if err != nil {
		return err
	}
	// Cleanup must work after qualification expires, but it still executes only
	// the currently pinned immutable tools. Validate before the first metadata read.
	if err = InspectServiceInstallation(c); err != nil {
		return err
	}
	if err = validatePrivateDirectory(c.AttemptRoot, c.OwnerUID); err != nil {
		return err
	}
	if err = validateRecord(c, record); err != nil {
		return err
	}
	if err = confirmInvocationStopped(ctx, c, record); err != nil {
		return fmt.Errorf("%w: %v", ErrCleanup, err)
	}
	if _, err = os.Lstat(record.Root); os.IsNotExist(err) {
		return nil
	}
	request := serviceRequest{Version: protocolVersion, Operation: "cleanup", ID: record.ID, Workspace: record.Workspace, Previous: &record, Limits: Limits{CPUMilli: min(c.MaxCPUMilli, 1000), MemoryMiB: min(c.MaxMemoryMiB, 1024), MinFreeBytes: 1 << 30, Timeout: time.Duration(min(60, c.MaxRuntimeSeconds-10)) * time.Second}}
	_, err = callService(ctx, request, persist, nil, nil)
	return err
}

func callService(ctx context.Context, request serviceRequest, persist Persist, emit Emit, admit func(context.Context, ServiceConfig) error) (receipt []byte, err error) {
	if persist == nil {
		return nil, errors.New("durable capacity reservation is required")
	}
	c, err := LoadServiceConfig()
	if err != nil {
		return nil, ErrUnavailable
	}
	if c.OwnerUID != os.Geteuid() || runtime.GOARCH != "amd64" {
		return nil, ErrUnavailable
	}
	if request.Operation == "verify" && admit == nil {
		return nil, ErrUnavailable
	}
	if admit != nil {
		if err = admit(ctx, c); err != nil {
			return nil, ErrUnavailable
		}
	} else if err = InspectServiceInstallation(c); err != nil {
		return nil, ErrUnavailable
	}
	if err = validatePrivateDirectory(c.AttemptRoot, c.OwnerUID); err != nil {
		return nil, ErrUnavailable
	}
	socket, err := readLoadedSocket(ctx, c)
	if err != nil || verifyServiceSocket(installedServiceFiles{}, c, socket) != nil {
		return nil, ErrUnavailable
	}
	hash, err := requestDigest(request)
	if err != nil {
		return nil, err
	}
	challenge, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	writer := &framedWriter{out: conn}
	if err = writer.send(wireMessage{Type: "hello", Challenge: challenge, RequestSHA256: hash}); err != nil {
		return nil, err
	}
	hello, err := readMessage(conn)
	if err != nil || hello.Type != "hello" || hello.Hello == nil || !hello.bound(challenge, hash) {
		return nil, errors.New("image service did not establish its identity")
	}
	loaded, err := readLoadedService(ctx, c, hello.Hello.Unit)
	if err != nil {
		return nil, err
	}
	if err = verifyServiceIdentity(c, *hello.Hello, ExpectedIdentity{Challenge: challenge, RequestSHA256: hash}, loaded); err != nil {
		return nil, err
	}
	boot, err := currentBootID()
	if err != nil {
		return nil, err
	}
	record := Record{ID: request.ID, Workspace: request.Workspace, Root: filepath.Join(c.AttemptRoot, request.ID), Identity: *hello.Hello, ChildGroup: filepath.Join(hello.Hello.ControlGroup, "build"), BootID: boot, AttemptSHA256: hash, SourceSHA: request.SourceSHA}
	if request.Previous != nil {
		record.AttemptSHA256 = request.Previous.AttemptSHA256
		record.SourceSHA = request.Previous.SourceSHA
	}
	if err = validateRecord(c, record); err != nil {
		return nil, err
	}
	// Source execution cannot start until this exact invocation and request
	// hash are durable. Uncertain transport after this point never falls back
	// to another worker; reconciliation must settle this attempt first.
	if err = persist(record); err != nil {
		return nil, err
	}
	defer func() {
		_ = conn.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if e := awaitServiceCleanup(cleanup, c, record); e != nil {
			err = fmt.Errorf("%w: %v", ErrCleanup, e)
		}
	}()
	if err = writer.send(wireMessage{Type: "prepare", Challenge: challenge, RequestSHA256: hash, Prepare: &request}); err != nil {
		return nil, err
	}
	ready, err := readMessage(conn)
	if err != nil || !ready.bound(challenge, hash) {
		return nil, errors.New("image preparation response is missing")
	}
	if ready.Type != "ready" || ready.ChildGroup != record.ChildGroup {
		return nil, errors.New("image service refused preparation")
	}
	// Revalidate immediately before START. The public Run always supplies full
	// real-host qualification; only the separately run owner fixture supplies
	// installation inspection for establishing that proof in the first place.
	if admit != nil {
		if err = admit(ctx, c); err != nil {
			return nil, err
		}
	} else if err = InspectServiceInstallation(c); err != nil {
		return nil, err
	}
	loaded, err = readLoadedService(ctx, c, record.Identity.Unit)
	if err != nil {
		return nil, err
	}
	if err = verifyServiceIdentity(c, record.Identity, ExpectedIdentity{Challenge: challenge, RequestSHA256: hash}, loaded); err != nil {
		return nil, err
	}
	limits, err := readCgroupLimits(record.ChildGroup)
	if err != nil {
		return nil, err
	}
	if err = checkCgroupLimits(limits, request.Limits); err != nil {
		return nil, err
	}
	actual, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", record.Identity.MainPID))
	if err != nil {
		return nil, err
	}
	group, err := parseUnifiedCgroup(string(actual))
	if err != nil || group != record.ChildGroup {
		return nil, errors.New("image helper is outside its admitted child group")
	}
	_ = conn.SetDeadline(time.Time{})
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	if err = writer.send(wireMessage{Type: "start", Challenge: challenge, RequestSHA256: hash}); err != nil {
		return nil, err
	}
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			_ = writer.send(wireMessage{Type: "cancel", Challenge: challenge, RequestSHA256: hash})
			_ = conn.Close()
		case <-watchDone:
		}
	}()
	for {
		message, e := readMessage(conn)
		if e != nil {
			return nil, errors.New("image operation ended without a complete result")
		}
		if !message.bound(challenge, hash) {
			return nil, errors.New("image operation identity changed")
		}
		switch message.Type {
		case "log":
			if emit != nil {
				emit(message.Stderr, message.Log)
			}
		case "result":
			if !message.Cleaned {
				return nil, ErrCleanup
			}
			if message.Error != "" {
				return nil, errors.New(message.Error)
			}
			return append([]byte(nil), message.Receipt...), nil
		default:
			return nil, errors.New("unexpected image service response")
		}
	}
}

func validateRecord(c ServiceConfig, r Record) error {
	if !hash32.MatchString(r.ID) || r.Root != filepath.Join(c.AttemptRoot, r.ID) || !serviceWithin(r.Workspace, c.WorkspaceRoot) || filepath.Clean(r.Workspace) != r.Workspace || !strings.HasPrefix(filepath.Base(r.Workspace), "step-") || r.Identity.OwnerUID != c.OwnerUID || !serviceNonce.MatchString(r.Identity.Challenge) || !serviceHash.MatchString(r.Identity.RequestSHA256) || !serviceNonce.MatchString(r.Identity.InvocationID) || !validServiceCgroup(r.Identity.ControlGroup, r.Identity.Unit) || r.ChildGroup != filepath.Join(r.Identity.ControlGroup, "build") || !serviceHash.MatchString(r.AttemptSHA256) || !hash40.MatchString(r.SourceSHA) || len(r.BootID) != 36 {
		return errors.New("image attempt record is not the fixed owned operation")
	}
	return nil
}

func confirmInvocationStopped(ctx context.Context, c ServiceConfig, r Record) error {
	boot, err := currentBootID()
	if err != nil {
		return err
	}
	if boot != r.BootID {
		return nil
	} // A different kernel boot cannot contain the recorded processes.
	p, err := serviceProperties(ctx, c, r.Identity.Unit)
	if err != nil {
		return err
	}
	if p["LoadState"] != "not-found" {
		if p["InvocationID"] != "" && p["InvocationID"] != r.Identity.InvocationID {
			return errors.New("service instance identity changed")
		}
		if p["ActiveState"] != "inactive" && p["ActiveState"] != "failed" {
			return errors.New("image service is still active")
		}
	}
	return confirmGroupEmpty(r.Identity.ControlGroup)
}

func awaitServiceCleanup(ctx context.Context, c ServiceConfig, r Record) error {
	for {
		if err := confirmInvocationStopped(ctx, c, r); err == nil {
			if _, err = os.Lstat(r.Root); os.IsNotExist(err) {
				return nil
			}
			return errors.New("image service retained private state")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func validateReturnedReceipt(raw []byte, q Request, sha string) error {
	if len(raw) == 0 || len(raw) > MaxReceiptBytes {
		return errors.New("image receipt is missing or oversized")
	}
	var receipt Receipt
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid image receipt")
	}
	metadata, err := json.Marshal(receipt.BuildMetadata)
	if err != nil || receipt.SchemaVersion != 1 || receipt.Kind != "image-verification" || receipt.SourceSHA != sha || receipt.Platform != "linux/amd64" || receipt.Context != q.Context || receipt.Dockerfile != q.Dockerfile || receipt.Target != q.Target || !reflect.DeepEqual(receipt.Args, q.Args) || !receipt.SuccessfulBuild || receipt.RuntimeProfile != Profile || !serviceHash.MatchString(receipt.OCISHA256) || receipt.OCIBytes <= 0 || receipt.OCIBytes > MaxStateBytes || receipt.BuildMetadata == nil || len(metadata) > MaxMetadataBytes {
		return errors.New("image receipt does not prove the exact requested build")
	}
	return nil
}
