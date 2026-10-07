package installation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	pl "github.com/znasllc-io/memql/component/pipelines"
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
}

func decodePreparationRetirement(body []byte, state string) (*preparationRetirement, error) {
	var value *preparationRetirement
	if len(body) > 64<<10 {
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
	if (state != "retiring" && state != "cancelled") || !internalDigest.MatchString(value.WorkflowDigest) || len(value.Captures) != 2 {
		return nil, errors.New("preparation retirement identity is inconsistent")
	}
	for _, role := range []string{"candidate", "rollback"} {
		c, exists := value.Captures[role]
		if !exists || len(c.Page) > 128 || (c.Cursor != "" && !pl.ValidArtifactIntentIDs([]string{c.Cursor})) ||
			(c.Fenced && !value.ProducerStopped) || (c.PinsReleased && !c.Fenced) ||
			((c.Cursor != "" || len(c.Page) > 0 || c.Complete) && !c.PinsReleased) || (c.Complete && len(c.Page) > 0) {
			return nil, errors.New("preparation retirement progress is inconsistent")
		}
		previous := c.Cursor
		for _, artifact := range c.Page {
			if !pl.ValidArtifactIntentIDs([]string{artifact.IntentID}) || artifact.IntentID <= previous ||
				(artifact.ReceiptDigest != "" && !internalDigest.MatchString(artifact.ReceiptDigest)) {
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
// external producer/storage fence has a durable receipt. Ordinary capture
// mutations remain restricted to preparing, so late callbacks cannot reopen it.
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
		r.Retirement = &preparationRetirement{WorkflowDigest: cleanupWorkflow, Captures: map[string]preparationCaptureRetirement{"candidate": {}, "rollback": {}}}
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
