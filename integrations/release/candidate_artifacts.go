package release

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/ociregistry"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

func candidateArtifactReferences(c pl.ReleaseCandidate, key string) []pipelinesteps.RunFileReference {
	groups := map[pipelinesteps.RunFileReceiptScope][]string{}
	add := func(run, step string, attempt int, ids []string) {
		scope := pipelinesteps.RunFileReceiptScope{OwnerUserID: c.OwnerUserID, WorkRunID: run, StepKey: step, Attempt: attempt}
		groups[scope] = append(groups[scope], ids...)
	}
	for _, evidence := range c.Evidence {
		add(evidence.WorkRunID, evidence.StepKey, evidence.Attempt, evidence.ArtifactIntentIDs)
	}
	for _, component := range c.Components {
		for _, artifact := range component.Artifacts {
			r := artifact.Receipt
			add(r.WorkRunID, r.StepKey, r.Attempt, []string{r.IntentID})
		}
	}
	out := []pipelinesteps.RunFileReference{}
	for scope, ids := range groups {
		if len(ids) == 0 {
			continue
		}
		slices.Sort(ids)
		out = append(out, pipelinesteps.RunFileReference{Scope: scope, ReferenceID: key, IntentIDs: slices.Compact(ids)})
	}
	slices.SortFunc(out, func(a, b pipelinesteps.RunFileReference) int {
		if n := strings.Compare(a.Scope.WorkRunID, b.Scope.WorkRunID); n != 0 {
			return n
		}
		if n := strings.Compare(a.Scope.StepKey, b.Scope.StepKey); n != 0 {
			return n
		}
		if a.Scope.Attempt < b.Scope.Attempt {
			return -1
		}
		if a.Scope.Attempt > b.Scope.Attempt {
			return 1
		}
		return 0
	})
	return out
}

func receiptScope(r pipelinesteps.StoredFileReceipt) pipelinesteps.RunFileReceiptScope {
	return pipelinesteps.RunFileReceiptScope{OwnerUserID: r.OwnerUserID, WorkRunID: r.WorkRunID, StepKey: r.StepKey, Attempt: r.Attempt}
}

func (s *candidatePrepareScope) pin(ctx context.Context, _ map[string]any) (any, error) {
	if !s.recorded || !s.evidenceOK {
		return nil, errors.New("artifact pin requires a recorded, verified candidate")
	}
	s.pinned, s.bytesOK = false, false
	s.receipts = map[string]pipelinesteps.StoredFileReceipt{}
	// The scope retains the caller's owner identity; only these bounded native
	// artifact operations receive internal origin. Evidence reads stay client.
	for _, ref := range candidateArtifactReferences(s.candidate, s.key) {
		rows, err := s.p.library.PinRunFileReceipts(auth.ContextWithInternalOrigin(ctx), ref)
		if err != nil {
			return nil, err
		}
		if len(rows) != len(ref.IntentIDs) {
			return nil, errors.New("artifact pin returned incomplete receipts")
		}
		for _, row := range rows {
			if !slices.Contains(ref.IntentIDs, row.IntentID) || receiptScope(row) != ref.Scope || s.receipts[row.IntentID].IntentID != "" ||
				row.ETag == "" || row.Size < 0 || row.Size > 2<<30 || !candidateArtifactDigest(row.SHA256) {
				return nil, errors.New("artifact pin returned inconsistent receipts")
			}
			s.receipts[row.IntentID] = row
		}
	}
	if len(s.receipts) == 0 {
		return nil, errors.New("candidate pinned no artifacts")
	}
	s.pinned = true
	return nil, nil
}

func candidateArtifactDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, ch := range s {
		if !(ch >= '0' && ch <= '9') && !(ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

func (s *candidatePrepareScope) verifyBytes(ctx context.Context, _ map[string]any) (any, error) {
	if !s.recorded || !s.pinned {
		return nil, errors.New("artifact verification requires durable candidate pins")
	}
	s.bytesOK = false
	// Verify all evidence artifacts as well as the products to publish. An
	// unretained or unreadable security report cannot be silently omitted.
	verified := map[string]bool{}
	for _, component := range s.candidate.Components {
		for _, a := range component.Artifacts {
			receipt, exists := s.receipts[a.Receipt.IntentID]
			if !exists || receipt.Size != a.Size || "sha256:"+receipt.SHA256 != a.Digest {
				return nil, fmt.Errorf("artifact %s/%s differs from its pinned receipt", component.Name, a.Name)
			}
			if err := s.readArtifact(ctx, receipt, &a); err != nil {
				return nil, err
			}
			verified[receipt.IntentID] = true
		}
	}
	for id, receipt := range s.receipts {
		if !verified[id] {
			if err := s.readArtifact(ctx, receipt, nil); err != nil {
				return nil, err
			}
		}
	}
	s.bytesOK = true
	return nil, nil
}

func (s *candidatePrepareScope) readArtifact(ctx context.Context, want pipelinesteps.StoredFileReceipt, product *pl.ReleaseArtifact) (err error) {
	got, body, err := s.p.library.OpenRunFileReceipt(auth.ContextWithInternalOrigin(ctx), receiptScope(want), want.IntentID)
	if err != nil {
		return err
	}
	if body == nil {
		return errors.New("artifact receipt returned no byte stream")
	}
	defer func() { err = errors.Join(err, body.Close()) }()
	if got != want {
		return errors.New("artifact changed after its receipt was pinned")
	}
	if product != nil && product.Kind == "oci" {
		image, err := ociregistry.Verify(ctx, body, ociregistry.Expected{ArchiveSHA256: product.Digest, ArchiveSize: product.Size, ImageDigest: product.ImageDigest, Platform: product.Platform}, ociregistry.Limits{})
		if err != nil {
			return err
		}
		return image.Close()
	}
	// OpenRunFileReceipt verifies the receipt digest before EOF. The additional
	// limit prevents a broken adapter from streaming indefinitely, and +1 makes
	// a forged EOF at exactly Size distinguishable from the provider's real EOF.
	n, err := io.Copy(io.Discard, io.LimitReader(&candidateContextReader{ctx: ctx, r: body}, want.Size+1))
	if err != nil {
		return err
	}
	if n != want.Size {
		return errors.New("artifact size differs from its pinned receipt")
	}
	return nil
}

type candidateContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *candidateContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// retire releases only this candidate's references, after the journal's
// permanent retirement fence. All references are released, even if a former
// preparer had not pinned them yet; a delayed pin is then refused by storage.
func (p *candidatePreparer) retire(ctx context.Context, key string) (candidateRecord, error) {
	if _, err := candidateOwner(ctx); err != nil {
		return candidateRecord{}, err
	}
	if p == nil || p.ledger == nil || p.library == nil {
		return candidateRecord{}, errors.New("candidate retirement is unavailable")
	}
	rec, err := p.ledger.retire(ctx, key)
	if err != nil {
		return candidateRecord{}, err
	}
	var releaseErr error
	for _, ref := range candidateArtifactReferences(rec.Manifest, rec.ID) {
		releaseErr = errors.Join(releaseErr, p.library.ReleaseRunFileReference(auth.ContextWithInternalOrigin(ctx), ref))
	}
	return rec, releaseErr
}
