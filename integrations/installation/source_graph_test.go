package installation

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/argocd"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
	"github.com/znasllc-io/memql/integrations/workbench"
)

func graphCaptureFixture(t *testing.T) (sourceCaptureSpec, []byte, []byte) {
	t.Helper()
	root, dependencyArchive := captureFixture(t)
	var dependency map[string]any
	require.NoError(t, json.Unmarshal(root.Render.Source, &dependency))
	reference := "https://github.com/example/engine//overlay?ref=" + dependency["targetRevision"].(string) + "&timeout=120s&submodules=false"
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
	setting := add("blob", []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n - "+reference+"\n"))
	tree := add("tree", entry("100644", "kustomization.yaml", setting))
	top := add("tree", entry("40000", "overlay", tree))
	commit := add("commit", []byte("tree "+top+"\nauthor Fixture <fixture@example.invalid> 1 +0000\ncommitter Fixture <fixture@example.invalid> 1 +0000\n\ngraph fixture\n"))
	dependency["targetRevision"] = commit
	root.Render.Source, _ = json.Marshal(dependency)
	root.Fragment = &argocd.SourceFragmentSpec{Render: root.Render, AllowedDependencies: []argocd.SourceRepository{{Repository: "https://github.com/example/engine.git", Paths: []string{"overlay"}}}}
	var archive bytes.Buffer
	_, err := argocd.CaptureSourceFragmentArchive(context.Background(), objects, *root.Fragment, &archive)
	require.NoError(t, err)
	return root, archive.Bytes(), dependencyArchive
}

type graphCaptureFiles map[string]*captureFilesFixture

func (f graphCaptureFiles) PinRunFileReceipts(ctx context.Context, ref pipelinesteps.RunFileReference) ([]pipelinesteps.StoredFileReceipt, error) {
	file := f[ref.Scope.StepKey]
	if file == nil {
		return nil, errors.New("missing graph archive")
	}
	return file.PinRunFileReceipts(ctx, ref)
}
func (f graphCaptureFiles) OpenRunFileReceipt(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, intent string) (pipelinesteps.StoredFileReceipt, io.ReadCloser, error) {
	file := f[scope.StepKey]
	if file == nil {
		return pipelinesteps.StoredFileReceipt{}, nil, errors.New("missing graph archive")
	}
	return file.OpenRunFileReceipt(ctx, scope, intent)
}
func (f graphCaptureFiles) ReleaseRunFileReference(context.Context, pipelinesteps.RunFileReference) error {
	return nil
}
func (f graphCaptureFiles) RetireRunFile(context.Context, pipelinesteps.RunFileReceiptScope, string) (pipelinesteps.RetiredFileReceipt, error) {
	return pipelinesteps.RetiredFileReceipt{}, nil
}

func TestSourceGraphCapturesTraverseWorkbenchAndReopenOnFreshReplica(t *testing.T) {
	root, rootArchive, depArchive := graphCaptureFixture(t)
	ctx := captureOperator(auth.RoleOwner, root.OwnerUserID)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	files := graphCaptureFiles{}
	run := func(spec sourceCaptureSpec, archive []byte) (verifiedSourceCapture, sourceCaptureReceipt, *captureHopState) {
		capture, err := newSourceCapture(spec)
		require.NoError(t, err)
		port, file := capturePorts(capture, archive)
		files[spec.StepKey] = file
		result, err := json.Marshal(port.result)
		require.NoError(t, err)
		state := &captureHopState{result: result}
		executor := func(node string) *pipelinesteps.Executor {
			handler := workbench.NewForwardHandler(nil, logger)
			handler.SetPipelineRunner(captureHopRunner{state})
			return pipelinesteps.NewExecutor(pipelinesteps.ConfigFromEnv(func(string) string { return "" }), captureHopForwarder{handler, node}, nil, logger)
		}
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		receipt, err := capture.run(bounded, executor("a"), false, "")
		require.NoError(t, err)
		encoded, err := json.Marshal(spec)
		require.NoError(t, err)
		var recovered sourceCaptureSpec
		require.NoError(t, json.Unmarshal(encoded, &recovered))
		peer, err := newSourceCapture(recovered)
		require.NoError(t, err)
		again, err := peer.run(bounded, executor("b"), true, "")
		require.NoError(t, err)
		require.Equal(t, receipt, again)
		proof, err := peer.verify(bounded, files, again)
		require.NoError(t, err)
		require.Empty(t, proof.source.Digest())
		require.NotEmpty(t, proof.fragment.Digest())
		require.NoError(t, peer.acknowledge(bounded, executor("b"), again))
		require.NoError(t, state.err)
		require.Len(t, state.requests, 2)
		require.True(t, state.requests[1].RecoverOnly)
		require.Contains(t, state.requests[1].Command, " --fragment")
		return proof, receipt, state
	}
	rootProof, rootReceipt, _ := run(root, rootArchive)
	dependency := rootProof.fragment.Dependencies()[0]
	key := dependencyCaptureKey("candidate", dependency)
	fragment, err := rootProof.fragment.DependencySpec(dependency)
	require.NoError(t, err)
	depSpec, err := dependencyCaptureSpec(root, key, fragment)
	require.NoError(t, err)
	require.Zero(t, depSpec.CloneInstallationID)
	depProof, depReceipt, state := run(depSpec, depArchive)
	require.Empty(t, state.requests[0].Secrets)
	closed, err := argocd.AssembleSourceClosure(ctx, rootProof.fragment, []argocd.SourceFragment{depProof.fragment})
	require.NoError(t, err)
	record := preparationRecord{Scope: preparationScope{Captures: map[string]sourceCaptureSpec{"candidate": root}}, Captures: map[string]preparationCapture{
		"candidate": {Started: true, Receipt: &rootReceipt, FragmentDigest: rootProof.digest(), SourceDigest: closed.Digest(), ReceiptDigest: preparationReceiptDigest(rootProof.receipt), Acknowledged: true},
		key:         {Spec: &depSpec, Parent: "candidate", Started: true, Receipt: &depReceipt, FragmentDigest: depProof.digest(), ReceiptDigest: preparationReceiptDigest(depProof.receipt), Acknowledged: true},
	}}
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	var restored preparationRecord
	require.NoError(t, json.Unmarshal(encoded, &restored))
	reopened, err := reopenSourceGraph(ctx, restored, "candidate", files)
	require.NoError(t, err)
	require.Equal(t, closed.Digest(), reopened.source.Digest())
	require.Empty(t, reopened.fragment.Digest())
	delete(restored.Captures, key)
	_, err = reopenSourceGraph(ctx, restored, "candidate", files)
	require.Error(t, err)
	require.NotContains(t, fmt.Sprint(reopened), "private-source-contents")
}

func TestDependencyCaptureCannotChangeRootAuthorityOrCredentialScope(t *testing.T) {
	root, archive, _ := graphCaptureFixture(t)
	root.CloneInstallationID = 41
	fragment, err := argocd.VerifySourceFragmentArchive(context.Background(), archive, *root.Fragment)
	require.NoError(t, err)
	dependency := fragment.Dependencies()[0]
	child, err := fragment.DependencySpec(dependency)
	require.NoError(t, err)
	key := dependencyCaptureKey("candidate", dependency)
	spec, err := dependencyCaptureSpec(root, key, child)
	require.NoError(t, err)
	require.Zero(t, spec.CloneInstallationID, "the private product clone grant must not follow the public dependency")
	receipt := sourceCaptureReceipt{IntentID: strings.Repeat("a", 64)}
	record := preparationRecord{Scope: preparationScope{Captures: map[string]sourceCaptureSpec{"candidate": root}}, Captures: map[string]preparationCapture{"candidate": {FragmentDigest: fragment.Digest(), Receipt: &receipt}, key: {Spec: &spec, Parent: "candidate"}}}
	_, err = preparationCaptureSpec(record, key)
	require.NoError(t, err)
	for _, change := range []func(*sourceCaptureSpec){func(s *sourceCaptureSpec) { s.CloneInstallationID = 42 }, func(s *sourceCaptureSpec) { s.OwnerUserID = "other" }, func(s *sourceCaptureSpec) { s.RunID = "other" }, func(s *sourceCaptureSpec) { s.CollectorImage = "example/image@sha256:" + strings.Repeat("b", 64) }} {
		changed := spec
		change(&changed)
		record.Captures[key] = preparationCapture{Spec: &changed, Parent: "candidate"}
		_, err := preparationCaptureSpec(record, key)
		require.Error(t, err)
	}
	spec.CloneInstallationID = 42
	_, err = newSourceCapture(spec)
	require.Error(t, err)
}
