package telephony

import (
	"context"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
)

// consentEngine returns a result whose bundle has optOutRows root ids for the
// consent query, and records mutation queries.
type consentEngine struct {
	memql.IntegrationEngineAccess
	optOutRows int
	queries    []string
}

func (e *consentEngine) Execute(_ context.Context, q string) (*memql.ExecuteResult, error) {
	e.queries = append(e.queries, q)
	res := &memql.ExecuteResult{}
	if strings.Contains(q, "consentOptOut") {
		ids := make([]string, e.optOutRows)
		for i := range ids {
			ids[i] = "consent:row"
		}
		res.Bundle = &memqlv1.GraphBundle{RootIds: ids}
	}
	return res, nil
}

func TestCheckOutboundAllowed(t *testing.T) {
	// Missing caller-ID is blocked regardless of engine.
	i := &Integration{}
	if err := i.CheckOutboundAllowed(context.Background(), "+18005550111", ""); err == nil {
		t.Fatal("empty from should be blocked")
	}

	// No engine -> nothing to check against, allowed (with caller-ID).
	if err := i.CheckOutboundAllowed(context.Background(), "+18005550111", "+14155550100"); err != nil {
		t.Fatalf("no-engine allow: %v", err)
	}

	// Opted-out callee is blocked.
	blocked := &Integration{engine: &consentEngine{optOutRows: 1}}
	err := blocked.CheckOutboundAllowed(context.Background(), "+18005550111", "+14155550100")
	if err == nil || !strings.Contains(err.Error(), "opted out") {
		t.Fatalf("opted-out callee should be blocked, got %v", err)
	}

	// Not opted out -> allowed.
	ok := &Integration{engine: &consentEngine{optOutRows: 0}}
	if err := ok.CheckOutboundAllowed(context.Background(), "+18005550111", "+14155550100"); err != nil {
		t.Fatalf("non-opted-out should be allowed: %v", err)
	}
}

func TestComplianceCapsGated(t *testing.T) {
	i := &Integration{}
	ctx := context.Background()
	if _, err := i.handleSetConsent(ctx, map[string]any{"phoneNumber": "+1", "partitionId": "p", "status": "opted_out"}, 0); err == nil {
		t.Error("setConsent must require owner/admin")
	}
	if _, err := i.handleRegisterE911(ctx, map[string]any{"e164": "+1"}, 0); err == nil {
		t.Error("registerE911 must require owner/admin")
	}
}
