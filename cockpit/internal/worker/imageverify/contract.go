// Package imageverify executes the fixed, non-publishing image verification
// contract. A successful receipt is evidence of a build, never a release asset.
package imageverify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const Artifact = ".memql-image-build/verification.json"
const MaxStateBytes int64 = 15 << 30
const MaxReceiptBytes = 512 << 10
const MaxMetadataBytes = 64 << 10
const Profile = "rootless-buildkit-v1"

var ErrUnavailable = errors.New("isolated Linux image verification is unavailable")
var ErrCleanup = errors.New("image verification cleanup could not be confirmed")
var hash40 = regexp.MustCompile(`^[0-9a-f]{40}$`)
var hash32 = regexp.MustCompile(`^[0-9a-f]{32}$`)
var name = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var targetName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type Request struct {
	Context          string            `json:"context"`
	Dockerfile       string            `json:"dockerfile"`
	Target           string            `json:"target,omitempty"`
	Args             map[string]string `json:"args,omitempty"`
	VerificationOnly bool              `json:"verificationOnly"`
}

// Limits are admitted by the existing machine pipeline policy. Disk is a
// sampled stop condition, like the cluster's measured ephemeral storage limit;
// it is not a filesystem quota and can overshoot between observations.
type Limits struct {
	CPUMilli     int
	MemoryMiB    int
	MinFreeBytes uint64
	Timeout      time.Duration
}

// Record is durable before PREPARE/START. Identity is the actual socket-
// activated service observed through systemd, not an instance name invented by
// the worker. Interrupted operations are reconciled, never replayed.
type Record struct {
	ID            string        `json:"id"`
	Workspace     string        `json:"workspace"`
	Root          string        `json:"root"`
	Identity      HelloIdentity `json:"identity"`
	ChildGroup    string        `json:"childGroup"`
	BootID        string        `json:"bootID"`
	AttemptSHA256 string        `json:"attemptSHA256"`
	SourceSHA     string        `json:"sourceSHA"`
}

type Receipt struct {
	SchemaVersion   int               `json:"schemaVersion"`
	Kind            string            `json:"kind"`
	SourceSHA       string            `json:"sourceSHA"`
	Platform        string            `json:"platform"`
	Context         string            `json:"context"`
	Dockerfile      string            `json:"dockerfile"`
	Target          string            `json:"target,omitempty"`
	Args            map[string]string `json:"args"`
	SuccessfulBuild bool              `json:"successfulBuild"`
	RuntimeProfile  string            `json:"runtimeProfile"`
	OCISHA256       string            `json:"ociSHA256"`
	OCIBytes        int64             `json:"ociBytes"`
	BuildMetadata   map[string]any    `json:"buildMetadata"`
}

func (r Request) Validate(sha, platform string) error {
	if !r.VerificationOnly || !hash40.MatchString(sha) || platform != "linux/amd64" {
		return errors.New("image verification requires its explicit mode, exact source and linux/amd64 platform")
	}
	if !cleanRelative(r.Context, true) || !cleanRelative(r.Dockerfile, false) || (r.Target != "" && !targetName.MatchString(r.Target)) {
		return errors.New("image verification context, Dockerfile or target is invalid")
	}
	if len(r.Args) > 66 {
		return errors.New("too many image build arguments")
	}
	total := 0
	for k, v := range r.Args {
		if !name.MatchString(k) || strings.ContainsRune(v, 0) {
			return errors.New("invalid image build argument")
		}
		total += len(k) + len(v)
	}
	if total > MaxMetadataBytes || r.Args["MEMQL_COMMIT"] != sha {
		return errors.New("image build arguments exceed their bound or do not bind the source")
	}
	if _, ok := r.Args["MEMQL_RELEASE"]; !ok {
		return errors.New("image verification requires explicit release provenance")
	}
	return nil
}

func cleanRelative(s string, dot bool) bool {
	return s != "" && len(s) <= 1024 && !strings.ContainsAny(s, "\\\x00\r\n") && !path.IsAbs(s) && path.Clean(s) == s && s != ".." && !strings.HasPrefix(s, "../") && (dot || s != ".")
}

// No source-provided symlink may turn a context into a host directory. Resolve
// every path component before invoking BuildKit, then retain the fresh checkout
// under the worker's exclusive attempt lease for the entire build.
func sourcePath(workspace, relative string, directory bool) (string, error) {
	p := workspace
	for _, part := range strings.Split(relative, "/") {
		if part == "." {
			continue
		}
		p = filepath.Join(p, part)
		s, err := os.Lstat(p)
		if err != nil || s.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("source path is absent or crosses a symlink")
		}
	}
	s, err := os.Lstat(p)
	if err != nil || (directory && !s.IsDir()) || (!directory && !s.Mode().IsRegular()) {
		return "", errors.New("source path has the wrong file type")
	}
	return p, nil
}

func validateLimits(l Limits) error {
	if l.CPUMilli < 1 || l.CPUMilli > 256000 || l.MemoryMiB < 256 || l.MemoryMiB > 32768 || l.MinFreeBytes < 1<<30 || l.Timeout < time.Second || l.Timeout > 24*time.Hour {
		return errors.New("image verification limits are invalid")
	}
	return nil
}

// buildReceipt runs in the inner helper after all build workload processes
// have stopped. It hashes the actual OCI export, while RootlessKit and the outer
// helper still deliver the result. The caller writes the final workspace
// evidence only after its deferred exact service/cgroup cleanup check succeeds.
func buildReceipt(record Record, request Request, sha string) ([]byte, error) {
	oci := filepath.Join(record.Root, "output", "image.oci.tar")
	s, err := os.Lstat(oci)
	if err != nil || !s.Mode().IsRegular() || s.Size() <= 0 || s.Size() > MaxStateBytes {
		return nil, errors.New("image verification OCI export is absent or invalid")
	}
	f, err := os.Open(oci)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	count, copyErr := io.Copy(h, io.LimitReader(f, MaxStateBytes+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || count != s.Size() {
		return nil, errors.New("cannot verify OCI export")
	}
	metadataPath := filepath.Join(record.Root, "output", "metadata.json")
	ms, err := os.Lstat(metadataPath)
	if err != nil || !ms.Mode().IsRegular() || ms.Size() > MaxMetadataBytes {
		return nil, errors.New("image build metadata exceeds its bound or has the wrong type")
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, err
	}
	var metadata map[string]any
	if json.Unmarshal(data, &metadata) != nil || metadata == nil {
		return nil, errors.New("image build metadata must be an object")
	}
	receipt := Receipt{1, "image-verification", sha, "linux/amd64", request.Context, request.Dockerfile, request.Target, request.Args, true, Profile, hex.EncodeToString(h.Sum(nil)), count, metadata}
	data, err = json.Marshal(receipt)
	if err != nil || len(data) > MaxReceiptBytes {
		return nil, errors.New("image verification receipt exceeds its bound")
	}
	return append(data, '\n'), nil
}

// Emit receives bounded log chunks. Persist must durably bind the record to the
// caller's existing capacity reservation. It must not release that reservation
// when Run returns ErrCleanup.
type Emit func(bool, []byte)
type Persist func(Record) error

type runner interface {
	Output(context.Context, string, []string, []string) ([]byte, error)
}
