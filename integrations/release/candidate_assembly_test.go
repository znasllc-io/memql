package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

func assemblyTestPlan() candidateAssemblyPlan {
	return candidateAssemblyPlan{Name: "coordinated", Components: []candidateAssemblyComponent{{Name: "engine", Runs: []string{"engine_arm64"},
		Artifacts: []candidateAssemblyArtifact{{Name: "image", Run: "engine_arm64", StepKey: "build.image", Path: "build/image.oci.tar", Kind: "oci", Platform: "linux/arm64", ImageMetadataPath: "build/metadata.json"}}}}, Targets: []string{"registry"}}
}

func TestCandidateAssemblyDeclarationIsBoundedAndClosed(t *testing.T) {
	versions := &candidateVersionReader{sources: map[string]candidateVersionSource{"engine": {Component: "engine"}}}
	targets := &candidateTargetReader{targets: map[string]candidateRegistryTarget{"registry": {ID: "registry", Component: "engine", Artifact: "image"}}}
	for _, failure := range []string{"valid", "duplicate plans", "unconfigured component", "duplicate aliases", "case-folded aliases", "undeclared run", "duplicate artifacts", "unknown kind", "metadata missing", "metadata same path", "unsupported platform", "traversal", "duplicate targets", "unknown target", "unknown compatibility", "too many plans"} {
		t.Run(failure, func(t *testing.T) {
			p := assemblyTestPlan()
			plans := []candidateAssemblyPlan{p}
			switch failure {
			case "duplicate plans":
				plans = append(plans, p)
			case "unconfigured component":
				p.Components[0].Name = "other"
			case "duplicate aliases":
				p.Components[0].Runs = []string{"engine_arm64", "engine_arm64"}
			case "case-folded aliases":
				p.Components[0].Runs = []string{"engine_arm64", "Engine_arm64"}
			case "undeclared run":
				p.Components[0].Artifacts[0].Run = "another"
			case "duplicate artifacts":
				p.Components[0].Artifacts = append(p.Components[0].Artifacts, p.Components[0].Artifacts[0])
			case "unknown kind":
				p.Components[0].Artifacts[0].Kind = "script"
			case "metadata missing":
				p.Components[0].Artifacts[0].ImageMetadataPath = ""
			case "metadata same path":
				p.Components[0].Artifacts[0].ImageMetadataPath = p.Components[0].Artifacts[0].Path
			case "unsupported platform":
				p.Components[0].Artifacts[0].Platform = "darwin/arm64"
			case "traversal":
				p.Components[0].Artifacts[0].Path = "../image"
			case "duplicate targets":
				p.Targets = append(p.Targets, p.Targets[0])
			case "unknown target":
				p.Targets = []string{"elsewhere"}
			case "unknown compatibility":
				p.Compatibility = []pl.ReleaseCompatibility{{Component: "engine", Requires: "unknown", MinVersion: "1.0.0", MaxExclusive: "2.0.0"}}
			case "too many plans":
				plans = make([]candidateAssemblyPlan, 65)
			}
			plans[0] = p
			err := validateAssemblyPlans(plans, versions, targets)
			if (err == nil) != (failure == "valid") {
				t.Fatal("declaration result", failure, err)
			}
		})
	}
	for _, path := range []string{"", ".", "..", "/absolute", "../outside", "a/../b", "a//b", "a\\b", "a\nb"} {
		if assemblyArtifactPath(path) {
			t.Fatal("unsafe path", path)
		}
	}
}

func TestCandidateAssemblyOwnerAndScopedPorts(t *testing.T) {
	var p *candidatePreparer
	for _, ctx := range []context.Context{context.Background(), actorContext(auth.RoleAdmin), actorContext(auth.RoleDeveloper)} {
		if _, err := p.assemble(ctx, "plan", nil); RefusalCode(err) != CodeNotOwner {
			t.Fatal("nonowner entered assembly", err)
		}
	}
	s := &candidateAssemblyScope{owner: "different-owner"}
	for name, op := range s.operations() {
		if _, err := op(ownerCtx(), nil); err == nil {
			t.Fatal("wrong owner called", name)
		}
	}
	for _, capability := range workflowhost.ScopedCapabilities(s.operations()) {
		if _, err := capability.Handler(ownerCtx(), map[string]any{"approved": true}, 0); err == nil {
			t.Fatal("public call acquired assembly scope", capability.Name)
		}
	}
}

// Discovery is a bounded ready-receipt read. Only the metadata may be opened
// before pins; archive reads still use the existing pin-enforcing fixture.
type assemblyLibraryFixture struct {
	*candidateLibraryFixture
	metadata        pipelinesteps.StoredFileReceipt
	metadataBody    []byte
	metadataErr     error
	metadataChanged bool
	metadataCloses  int
	rows            func([]pipelinesteps.StoredFileReceipt) []pipelinesteps.StoredFileReceipt
}

func (l *assemblyLibraryFixture) PinRunFileReceipts(ctx context.Context, ref pipelinesteps.RunFileReference) ([]pipelinesteps.StoredFileReceipt, error) {
	rows, err := l.ReadRunFileReceipts(ctx, ref.Scope, ref.IntentIDs)
	if err != nil {
		return nil, err
	}
	primary := ref
	primary.IntentIDs = []string{l.receipt.IntentID}
	if _, err := l.candidateLibraryFixture.PinRunFileReceipts(ctx, primary); err != nil {
		return nil, err
	}
	return rows, nil
}

func (l *assemblyLibraryFixture) ReadRunFileReceipts(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, ids []string) ([]pipelinesteps.StoredFileReceipt, error) {
	l.check(ctx, scope)
	if scope != receiptScope(l.receipt) || !slices.Contains(ids, l.receipt.IntentID) {
		return nil, errors.New("wrong discovery scope")
	}
	rows := []pipelinesteps.StoredFileReceipt{l.receipt}
	if slices.Contains(ids, l.metadata.IntentID) {
		rows = append(rows, l.metadata)
	}
	if l.rows != nil {
		rows = l.rows(rows)
	}
	return rows, nil
}

type assemblyMetadataStream struct {
	io.Reader
	close func() error
}

func (r assemblyMetadataStream) Close() error { return r.close() }

func (l *assemblyLibraryFixture) OpenRunFileReceipt(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, id string) (pipelinesteps.StoredFileReceipt, io.ReadCloser, error) {
	if id != l.metadata.IntentID {
		return l.candidateLibraryFixture.OpenRunFileReceipt(ctx, scope, id)
	}
	l.check(ctx, scope)
	if scope != receiptScope(l.metadata) {
		return pipelinesteps.StoredFileReceipt{}, nil, errors.New("wrong metadata scope")
	}
	row := l.metadata
	if l.metadataChanged {
		row.ETag = "changed"
	}
	stream := io.Reader(bytes.NewReader(l.metadataBody))
	if l.metadataErr != nil {
		stream = io.MultiReader(stream, assemblyErrorReader{l.metadataErr})
	}
	return row, assemblyMetadataStream{Reader: stream, close: func() error { l.metadataCloses++; return nil }}, nil
}

type assemblyErrorReader struct{ err error }

func (r assemblyErrorReader) Read([]byte) (int, error) { return 0, r.err }

func (l *assemblyLibraryFixture) setMetadata(body []byte) {
	l.metadataBody = body
	l.metadata.Size = int64(len(body))
	sum := sha256.Sum256(body)
	l.metadata.SHA256 = hex.EncodeToString(sum[:])
}

func TestCandidateAssemblyMetadataMustMatchReceiptBytes(t *testing.T) {
	owner, _ := candidateOwner(ownerCtx())
	for _, failure := range []string{"valid", "duplicate key", "folded key", "missing digest", "trailing JSON", "wrong digest", "receipt changed", "hash changed", "stream failure", "oversized", "not object"} {
		t.Run(failure, func(t *testing.T) {
			l := &assemblyLibraryFixture{candidateLibraryFixture: &candidateLibraryFixture{t: t}, metadata: pipelinesteps.StoredFileReceipt{OwnerUserID: owner, WorkRunID: "run", StepKey: "build", Attempt: 1, IntentID: strings.Repeat("f", 64), ETag: "v1"}}
			digest := "sha256:" + strings.Repeat("a", 64)
			body := `{"containerimage.digest":"` + digest + `"}`
			switch failure {
			case "duplicate key":
				body = `{"containerimage.digest":"` + digest + `","containerimage.digest":"` + digest + `"}`
			case "folded key":
				body = `{"containerimage.digest":"` + digest + `","Containerimage.digest":"` + digest + `"}`
			case "missing digest":
				body = `{}`
			case "trailing JSON":
				body += `{}`
			case "wrong digest":
				body = `{"containerimage.digest":"sha256:short"}`
			case "not object":
				body = `[]`
			case "receipt changed":
				l.metadataChanged = true
			case "stream failure":
				l.metadataErr = errors.New("checksum failed")
			}
			l.setMetadata([]byte(body))
			if failure == "hash changed" {
				l.metadataBody[0] = ' '
			}
			if failure == "oversized" {
				l.metadata.Size = 256<<10 + 1
			}
			s := candidateAssemblyScope{p: &candidatePreparer{library: l}}
			got, err := s.imageDigest(ownerCtx(), l.metadata)
			if failure == "valid" {
				if err != nil || got != digest {
					t.Fatal(got, err)
				}
			} else if err == nil {
				t.Fatal("invalid metadata accepted", failure)
			}
			if failure != "oversized" && l.metadataCloses != 1 {
				t.Fatal("metadata stream leaked")
			}
		})
	}
}

func assemblyOrdinaryMap(t *testing.T, value any) map[string]any {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
