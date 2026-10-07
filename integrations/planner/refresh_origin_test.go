package planner

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

type refreshOriginEngine struct {
	*fakeEngine
	origins []bool
}

func (e *refreshOriginEngine) Execute(ctx context.Context, query string) (any, error) {
	e.origins = append(e.origins, auth.OriginFromContext(ctx).IsInternal())
	return e.fakeEngine.Execute(ctx, query)
}

func TestRefreshSweepRequiresAndPreservesSchedulerOrigin(t *testing.T) {
	e := &refreshOriginEngine{fakeEngine: &fakeEngine{}}
	c := NewRefreshCron(e, nil)
	if err := c.run(context.Background()); err == nil || len(e.origins) != 0 {
		t.Fatalf("client reached cross-owner read: %v, %v", err, e.origins)
	}
	if err := c.run(auth.ContextWithInternalOrigin(context.Background())); err != nil {
		t.Fatal(err)
	}
	if len(e.origins) != 1 || !e.origins[0] {
		t.Fatalf("scheduler origin lost: %v", e.origins)
	}
}
