package pipelinerun

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
)

// train_land.go -- approval and landing (Task 7 of the plan fills these in).

func (i *Integration) awaitApproval(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, now time.Time) error {
	return nil
}

func (i *Integration) land(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train) error {
	return nil
}
