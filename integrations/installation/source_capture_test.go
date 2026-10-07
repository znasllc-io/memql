package installation

import (
	"bytes"
	"context"
	"crypto/sha1" // Git's existing object protocol, not a new signature.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/argocd"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type captureObjects map[string][]byte

func (o captureObjects) OpenGitObject(_ context.Context, kind, oid string) (io.ReadCloser, error) {
	body, ok := o[kind+"/"+oid]
	if !ok {
		return nil, errors.New("missing fixture object")
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func captureFixture(t *testing.T) (sourceCaptureSpec, []byte) {
	t.Helper()
	objects := captureObjects{}
	add := func(kind string, body []byte) string {
		h := sha1.New()
		fmt.Fprintf(h, "%s %d\x00", kind, len(body))
		h.Write(body)
		key := hex.EncodeToString(h.Sum(nil))
		objects[kind+"/"+key] = body
		return key
	}
	entry := func(mode, name, key string) []byte {
		raw, err := hex.DecodeString(key)
		require.NoError(t, err)
		return append([]byte(mode+" "+name+"\x00"), raw...)
	}
	config := add("blob", []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: private-config}\ndata: {message: private-source-contents}\n"))
	kustomization := add("blob", []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [config.yaml]\n"))
	tree := add("tree", append(entry("100644", "config.yaml", config), entry("100644", "kustomization.yaml", kustomization)...))
	root := add("tree", entry("40000", "overlay", tree))
	commit := add("commit", []byte("tree "+root+"\nauthor Fixture <fixture@example.invalid> 1 +0000\ncommitter Fixture <fixture@example.invalid> 1 +0000\n\nprivate-source-commit\n"))
	spec := sourceCaptureSpec{OwnerUserID: "operator-one", RunID: "capture-run", WorkRunID: "capture-work", StepKey: "prepare.source", Attempt: 1,
		RunStartedAt: time.Now().UTC().Format(time.RFC3339Nano), Repository: pl.Repository{Owner: "example", Name: "installation", CloneURL: "https://github.com/example/installation.git"},
		CollectorImage: "registry.example/collector@sha256:" + strings.Repeat("a", 64), Platform: "linux/arm64",
		Render: argocd.RenderSpec{Source: json.RawMessage(`{"repoURL":"https://github.com/example/installation.git","path":"overlay","targetRevision":"` + commit + `"}`),
			AppName: "installation", AppLabelKey: "app.kubernetes.io/instance", Namespace: "memql", ProjectName: "installation",
			ProjectSourceRepos: []string{"https://github.com/example/installation.git"}, TrackingMethod: "annotation+label", InstallationID: "installation",
			KubeVersion: "1.35.7", APIVersions: []string{"v1/ConfigMap"}}}
	var archive bytes.Buffer
	_, err := argocd.CaptureSourceArchive(context.Background(), objects, spec.Render, &archive)
	require.NoError(t, err)
	return spec, archive.Bytes()
}

type captureExecutorFixture struct {
	requests     []pl.StepRequest
	result       pl.StepResult
	executeError error
	acked        int
	ackError     error
	cancelledRun string
}

func (f *captureExecutorFixture) Cancel(_ context.Context, run string) error {
	f.cancelledRun = run
	return nil
}

func (f *captureExecutorFixture) Execute(ctx context.Context, req pl.StepRequest) (pl.StepResult, error) {
	f.requests = append(f.requests, req)
	if err := ctx.Err(); err != nil {
		return pl.StepResult{}, err
	}
	return f.result, f.executeError
}
func (f *captureExecutorFixture) AcknowledgeReceipt(context.Context, pl.StepRequest) error {
	f.acked++
	return f.ackError
}

type captureFilesFixture struct {
	row       pipelinesteps.StoredFileReceipt
	body      []byte
	refs      []pipelinesteps.RunFileReference
	closed    int
	opens     int
	readError error
}

func (f *captureFilesFixture) PinRunFileReceipts(ctx context.Context, ref pipelinesteps.RunFileReference) ([]pipelinesteps.StoredFileReceipt, error) {
	if auth.OriginFromContext(ctx) != auth.OriginInternal {
		return nil, errors.New("not internal")
	}
	f.refs = append(f.refs, ref)
	return []pipelinesteps.StoredFileReceipt{f.row}, nil
}
func (f *captureFilesFixture) ReleaseRunFileReference(context.Context, pipelinesteps.RunFileReference) error {
	return nil
}
func (f *captureFilesFixture) RetireRunFile(context.Context, pipelinesteps.RunFileReceiptScope, string) (pipelinesteps.RetiredFileReceipt, error) {
	return pipelinesteps.RetiredFileReceipt{}, nil
}

type captureBody struct {
	io.Reader
	close func()
}

func (r captureBody) Close() error { r.close(); return nil }

type captureErrorReader struct{ err error }

func (r captureErrorReader) Read([]byte) (int, error) { return 0, r.err }
func (f *captureFilesFixture) OpenRunFileReceipt(context.Context, pipelinesteps.RunFileReceiptScope, string) (pipelinesteps.StoredFileReceipt, io.ReadCloser, error) {
	f.opens++
	var r io.Reader = bytes.NewReader(f.body)
	if f.readError != nil {
		r = io.MultiReader(r, captureErrorReader{f.readError})
	}
	return f.row, captureBody{Reader: r, close: func() { f.closed++ }}, nil
}
func capturePorts(c sourceCapture, body []byte) (*captureExecutorFixture, *captureFilesFixture) {
	id := strings.Repeat("c", 64)
	sum := sha256.Sum256(body)
	return &captureExecutorFixture{result: pl.StepResult{Status: pl.OutcomeSucceeded, ExitCode: 0, Where: pl.Where{Surface: "cluster", JobName: pipelinesteps.JobName(c.request.RunID, c.request.StepKey, c.request.Attempt)}, ArtifactIntentIDs: []string{id}}},
		&captureFilesFixture{body: body, row: pipelinesteps.StoredFileReceipt{IntentID: id, FileID: "file", OwnerUserID: c.request.OwnerUserID, WorkRunID: c.request.WorkRunID, StepKey: c.request.StepKey, Attempt: c.request.Attempt, Path: c.path, Container: "private", Object: "owned/source", URL: "https://storage.example/private/source", ETag: "version-one", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}}
}

func TestSourceCaptureRecoversAndReverifiesWithoutOriginalProcess(t *testing.T) {
	spec, body := captureFixture(t)
	first, err := newSourceCapture(spec)
	require.NoError(t, err)
	executor, files := capturePorts(first, body)
	ctx := operator(auth.RoleDeveloper, spec.OwnerUserID)
	receipt, err := first.run(ctx, executor, false, "")
	require.NoError(t, err)
	require.Zero(t, executor.acked)
	// The only shared state is the journal's serialized native specification
	// and receipt, plus the executor/storage's external durable records.
	encoded, err := json.Marshal(spec)
	require.NoError(t, err)
	encoded, err = canonicalJSON(encoded)
	require.NoError(t, err)
	var recoveredSpec sourceCaptureSpec
	require.NoError(t, json.Unmarshal(encoded, &recoveredSpec))
	peer, err := newSourceCapture(recoveredSpec)
	require.NoError(t, err)
	require.Equal(t, first.digest, peer.digest)
	again, err := peer.run(ctx, executor, true, "")
	require.NoError(t, err)
	require.Equal(t, receipt, again)
	require.True(t, executor.requests[1].RecoverOnly)
	verified, err := peer.verify(ctx, files, receipt)
	require.NoError(t, err)
	require.NotEmpty(t, verified.source.Digest())
	require.Len(t, verified.source.Files(), 2)
	require.Len(t, files.refs, 1)
	require.Equal(t, 1, files.closed)
	require.NotContains(t, fmt.Sprintf("%+v %#v", verified, verified), "private-source")
	require.NoError(t, peer.acknowledge(ctx, executor, receipt))
	require.Equal(t, 1, executor.acked)
	// Ack does not retire the private artifact; a fresh replica verifies it.
	_, err = peer.verify(ctx, files, receipt)
	require.NoError(t, err)
	req := executor.requests[0]
	require.Equal(t, pl.ComputeCluster, req.Compute)
	require.Empty(t, req.Step.Needs)
	require.Empty(t, req.Step.Caches)
	require.Empty(t, req.Step.Services)
	require.Empty(t, req.Secrets)
	require.Contains(t, req.Step.Run, "/app/installation-source")
	require.Len(t, req.Step.Artifacts, 1)
}

func TestSourceCaptureRefusesChangedScopeAndArtifactBytes(t *testing.T) {
	for _, fault := range []string{"owner", "receipt-scope", "path", "attempt", "digest", "truncated", "bad-source", "read-error", "missing-artifact", "wrong-job", "failed", "lost-outcome"} {
		t.Run(fault, func(t *testing.T) {
			spec, body := captureFixture(t)
			c, err := newSourceCapture(spec)
			require.NoError(t, err)
			executor, files := capturePorts(c, body)
			ctx := operator(auth.RoleAdmin, spec.OwnerUserID)
			switch fault {
			case "missing-artifact":
				executor.result.ArtifactIntentIDs = nil
			case "wrong-job":
				executor.result.Where.JobName = "other"
			case "failed":
				executor.result.Status = pl.OutcomeFailed
			case "lost-outcome":
				executor.executeError = errors.New("private-upstream-error")
			}
			receipt, err := c.run(ctx, executor, false, "")
			if fault == "missing-artifact" || fault == "wrong-job" || fault == "failed" || fault == "lost-outcome" {
				require.Error(t, err)
				require.Zero(t, executor.acked)
				return
			}
			require.NoError(t, err)
			switch fault {
			case "owner":
				ctx = operator(auth.RoleAdmin, "other")
			case "receipt-scope":
				receipt.ScopeDigest = "sha256:" + strings.Repeat("f", 64)
			case "path":
				files.row.Path = "other.tar"
			case "attempt":
				files.row.Attempt++
			case "digest":
				files.row.SHA256 = strings.Repeat("f", 64)
			case "truncated":
				files.body = body[:len(body)-1]
			case "bad-source":
				files.body = bytes.Clone(body)
				files.body[600] ^= 1
				sum := sha256.Sum256(files.body)
				files.row.SHA256 = hex.EncodeToString(sum[:])
			case "read-error":
				files.readError = errors.New("provider verification failed at EOF")
			}
			verified, err := c.verify(ctx, files, receipt)
			require.Error(t, err)
			require.Empty(t, verified.source.Digest())
			require.Zero(t, executor.acked)
		})
	}
}

func TestSourceCaptureConfigurationRefusesBeforeDispatch(t *testing.T) {
	for _, fault := range []string{"mutable-image", "platform", "no-owner", "no-start", "no-attempt", "different-repo", "credential-host", "reserved-secret", "source-duplicate"} {
		t.Run(fault, func(t *testing.T) {
			spec, _ := captureFixture(t)
			switch fault {
			case "mutable-image":
				spec.CollectorImage = "registry.example/collector:latest"
			case "platform":
				spec.Platform = "darwin/arm64"
			case "no-owner":
				spec.OwnerUserID = ""
			case "no-start":
				spec.RunStartedAt = ""
			case "no-attempt":
				spec.Attempt = 0
			case "different-repo":
				spec.Repository.CloneURL = "https://github.com/example/other.git"
			case "credential-host":
				spec.Repository.CloneURL = "https://github.com@evil.example/example/installation.git"
			case "reserved-secret":
				spec.ImagePullSecret = "GIT_TOKEN"
			case "source-duplicate":
				spec.Render.Source = []byte(strings.Replace(string(spec.Render.Source), `"path":"overlay"`, `"path":"unreviewed","path":"overlay"`, 1))
			}
			_, err := newSourceCapture(spec)
			require.Error(t, err)
		})
	}
}

func TestSourceCaptureActorCancellationAndCleanupFailures(t *testing.T) {
	spec, body := captureFixture(t)
	c, err := newSourceCapture(spec)
	require.NoError(t, err)
	executor, files := capturePorts(c, body)
	for _, ctx := range []context.Context{context.Background(), operator(auth.RoleReader, spec.OwnerUserID), operator(auth.RoleOwner, "other")} {
		_, err = c.run(ctx, executor, false, "")
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(operator(auth.RoleOwner, spec.OwnerUserID))
	cancel()
	_, err = c.run(ctx, executor, false, "")
	require.Error(t, err)
	require.Empty(t, executor.requests)
	ctx = operator(auth.RoleOwner, spec.OwnerUserID)
	receipt, err := c.run(ctx, executor, false, "")
	require.NoError(t, err)
	_, err = c.verify(ctx, files, receipt)
	require.NoError(t, err)
	executor.ackError = errors.New("delete not confirmed")
	require.Error(t, c.acknowledge(ctx, executor, receipt))
	// A cleanup failure does not discard the re-readable receipt or repeat work.
	_, err = c.verify(ctx, files, receipt)
	require.NoError(t, err)
	require.Len(t, executor.requests, 1)
	require.NoError(t, c.cancel(ctx, executor))
	require.Equal(t, spec.RunID, executor.cancelledRun)
}

func TestSourceCaptureBindsChangedNativeInputsAndConfinesPullCredential(t *testing.T) {
	spec, body := captureFixture(t)
	spec.ImagePullSecret = "COLLECTOR_PULL"
	c, err := newSourceCapture(spec)
	require.NoError(t, err)
	executor, _ := capturePorts(c, body)
	ctx := operator(auth.RoleDeveloper, spec.OwnerUserID)
	_, err = c.run(ctx, executor, false, "")
	require.Error(t, err)
	require.Empty(t, executor.requests)
	_, err = c.run(ctx, executor, false, "private-pull-credential")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"COLLECTOR_PULL": "private-pull-credential"}, executor.requests[0].Secrets)
	require.NotContains(t, executor.requests[0].Step.Run, "private-pull-credential")
	require.NotContains(t, executor.requests[0].Environment(), "COLLECTOR_PULL")
	encoded, err := json.Marshal(spec)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-pull-credential")
	for _, change := range []func(*sourceCaptureSpec){
		func(s *sourceCaptureSpec) { s.Attempt++ },
		func(s *sourceCaptureSpec) { s.WorkRunID = "another-run" },
		func(s *sourceCaptureSpec) { s.RunStartedAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano) },
		func(s *sourceCaptureSpec) {
			s.CollectorImage = "registry.example/collector@sha256:" + strings.Repeat("b", 64)
		},
		func(s *sourceCaptureSpec) { s.ImagePullSecret = "OTHER_PULL" },
		func(s *sourceCaptureSpec) { s.CloneInstallationID = 123 },
		func(s *sourceCaptureSpec) { s.Render.Namespace = "another-namespace" },
	} {
		changed := spec
		change(&changed)
		other, err := newSourceCapture(changed)
		require.NoError(t, err)
		require.NotEqual(t, c.digest, other.digest)
	}
	// An executor receiving a request cannot mutate the scope retained by its
	// caller; the next replica derives the same immutable artifact path.
	executor.requests[0].Step.Artifacts[0] = "mutated"
	require.NotEqual(t, "mutated", c.request.Step.Artifacts[0])
}
