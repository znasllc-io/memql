package planner

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

type refreshOriginEngine struct {
	*fakeEngine
	origins []bool
}

// WriteAuthoredConstruct records the member-construct write as the call the
// planner used to send (memql#6022).
func (e *refreshOriginEngine) WriteAuthoredConstruct(ctx context.Context, c memql.AuthoredConstructInput) error {
	_, err := e.Execute(ctx, constructCall(c))
	return err
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
