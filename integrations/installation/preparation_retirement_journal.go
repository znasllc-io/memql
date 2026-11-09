package installation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/argocd"
)

type preparationRetirement struct {
	WorkflowDigest  string                                  `json:"workflowDigest"`
	ProducerStopped bool                                    `json:"producerStopped"`
	Captures        map[string]preparationCaptureRetirement `json:"captures"`
}

type preparationCaptureRetirement struct {
	Fenced       bool                            `json:"fenced"`
	PinsReleased bool                            `json:"pinsReleased"`
	Cursor       string                          `json:"cursor"`
	Page         []preparationArtifactRetirement `json:"page"`
	Complete     bool                            `json:"complete"`
}

type preparationArtifactRetirement struct {
	IntentID      string `json:"intentId"`
	ReceiptDigest string `json:"receiptDigest"`
	// Purged records the confirmed removal of the retired file's Library
	// entries, which only follows ReceiptDigest. It is omitted while false so
	// a page an earlier engine wrote keeps its canonical encoding and stays
	// readable; that engine's cleanup cannot resume under this recipe anyway,
	// because the cleanup digest binds the engine revision.
	Purged bool `json:"purged,omitempty"`
	// Retained records the Library store's refusal to retire a file it keeps
	// (pipelinesteps.ErrArtifactRetained): its owner filed, labelled, edited,
	// versioned or trained it, its history names another owner or source, or
	// its entry is not ready (which includes an upload this cancellation
	// interrupted before it was marked ready); or a durable reference pins it
	// (a capture's own is released before its first page). The bytes and
	// entries stay and the page may advance past it; read as unconfirmed, that
	// refusal held the page and the installation head forever. A kept file has
	// no receipt, so it is never removed from the Library. Omitted while false,
	// like Purged. An earlier engine cannot decode a page holding it, but a
	// page is dropped when it advances, and that engine could not resume this
	// cleanup anyway.
	Retained bool `json:"retained,omitempty"`
}

// settled: retired and removed from the Library, or kept. Only a settled
// entry may fall behind the cursor, which is never read again.
func (a preparationArtifactRetirement) settled() bool {
	return a.Retained || (a.ReceiptDigest != "" && a.Purged)
}

func decodePreparationRetirement(body []byte, state string) (*preparationRetirement, error) {
	var value *preparationRetirement
	if len(body) > 8<<20 {
		return nil, errors.New("preparation retirement exceeds its bound")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&value) != nil {
		return nil, errors.New("preparation retirement is malformed")
	}
	encoded, _ := json.Marshal(value)
	a, ea := canonicalJSON(body)
	b, eb := canonicalJSON(encoded)
	if ea != nil || eb != nil || !bytes.Equal(a, b) {
		return nil, errors.New("preparation retirement is noncanonical")
	}
	if value == nil {
		if state == "retiring" {
			return nil, errors.New("retiring preparation has no cleanup scope")
		}
		return nil, nil
	}
	if (state != "retiring" && state != "cancelled") || !internalDigest.MatchString(value.WorkflowDigest) || len(value.Captures) < 2 || len(value.Captures) > 16*(1+argocd.MaxSourceDependencies) {
		return nil, errors.New("preparation retirement identity is inconsistent")
	}
	for key, c := range value.Captures {
		if !captureStepKey.MatchString(key) || len(c.Page) > 128 || (c.Cursor != "" && !pl.ValidArtifactIntentIDs([]string{c.Cursor})) ||
			(c.Fenced && !value.ProducerStopped) || (c.PinsReleased && !c.Fenced) ||
			((c.Cursor != "" || len(c.Page) > 0 || c.Complete) && !c.PinsReleased) || (c.Complete && len(c.Page) > 0) {
			return nil, errors.New("preparation retirement progress is inconsistent")
		}
		previous := c.Cursor
		for _, artifact := range c.Page {
			if !pl.ValidArtifactIntentIDs([]string{artifact.IntentID}) || artifact.IntentID <= previous ||
				(artifact.ReceiptDigest != "" && !internalDigest.MatchString(artifact.ReceiptDigest)) ||
				(artifact.Purged && artifact.ReceiptDigest == "") || (artifact.Retained && artifact.ReceiptDigest != "") {
				return nil, errors.New("preparation retirement page is inconsistent")
			}
			previous = artifact.IntentID
		}
		if state == "cancelled" && !c.Complete {
			return nil, errors.New("cancelled preparation retains unfinished cleanup")
		}
	}
	return value, nil
}

// This is a native binding to the selected sealed cleanup recipe, not a
// caller-supplied approval. Retirement keeps the shared head until every
// external producer/storage fence has a durable receipt, or the Library store
// has refused to retire a file it keeps. Ordinary capture mutations remain
// restricted to preparing, so late callbacks cannot reopen it.
func (j *preparationJournal) beginRetirement(ctx context.Context, installation, key, workflow, cleanupWorkflow string) (preparationRecord, error) {
	r, err := j.get(ctx, installation, key)
	if err != nil {
		return preparationRecord{}, err
	}
	actor, err := preparationActor(ctx)
	if err != nil || actor != r.Scope.RequestedBy || workflow != r.Scope.WorkflowDigest || !internalDigest.MatchString(cleanupWorkflow) {
		return preparationRecord{}, errors.New("preparation retirement authority or workflow changed")
	}
	if r.Retirement != nil {
		if r.Retirement.WorkflowDigest != cleanupWorkflow {
			return preparationRecord{}, errors.New("preparation cleanup workflow changed")
		}
		return r, nil
	}
	return j.withRecord(ctx, installation, key, workflow, func(r *preparationRecord) error {
		captures := map[string]preparationCaptureRetirement{}
		for key := range r.Captures {
			captures[key] = preparationCaptureRetirement{}
		}
		r.Retirement = &preparationRetirement{WorkflowDigest: cleanupWorkflow, Captures: captures}
		r.State = "retiring"
		return nil
	})
}

func (j *preparationJournal) retirementRecord(ctx context.Context, installation, key, workflow, cleanupWorkflow string) (preparationRecord, error) {
	r, err := j.get(ctx, installation, key)
	if err != nil {
		return preparationRecord{}, err
	}
	actor, err := preparationActor(ctx)
	if err != nil || actor != r.Scope.RequestedBy || workflow != r.Scope.WorkflowDigest || r.State != "retiring" || r.Retirement == nil || r.Retirement.WorkflowDigest != cleanupWorkflow {
		return preparationRecord{}, errors.New("preparation retirement authority, state or workflow changed")
	}
	return r, nil
}

func (j *preparationJournal) withRetirement(ctx context.Context, installation, key, workflow, cleanupWorkflow string, update func(*preparationRecord) error) (preparationRecord, error) {
	return j.withRecordState(ctx, installation, key, workflow, "retiring", func(r *preparationRecord) error {
		if r.Retirement == nil || r.Retirement.WorkflowDigest != cleanupWorkflow {
			return errors.New("preparation cleanup workflow changed")
		}
		return update(r)
	})
}

func (j *preparationJournal) finishRetirement(ctx context.Context, installation, key, workflow, cleanupWorkflow string) (preparationRecord, error) {
	// Recover a lost final commit as history; never touch a successor's head.
	r, err := j.get(ctx, installation, key)
	if err != nil {
		return preparationRecord{}, err
	}
	actor, err := preparationActor(ctx)
	if err != nil || actor != r.Scope.RequestedBy || workflow != r.Scope.WorkflowDigest || r.Retirement == nil || r.Retirement.WorkflowDigest != cleanupWorkflow {
		return preparationRecord{}, errors.New("preparation retirement authority or workflow changed")
	}
	if r.State == "cancelled" {
		return r, nil
	}
	return j.withRetirement(ctx, installation, key, workflow, cleanupWorkflow, func(r *preparationRecord) error {
		if !r.Retirement.ProducerStopped {
			return errors.New("source producer cleanup is unconfirmed")
		}
		for _, c := range r.Retirement.Captures {
			if !c.Fenced || !c.PinsReleased || !c.Complete || len(c.Page) != 0 {
				return errors.New("source artifact cleanup is unconfirmed")
			}
		}
		r.State = "cancelled"
		return nil
	})
}
