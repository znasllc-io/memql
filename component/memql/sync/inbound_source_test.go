package sync

import (
	"context"
	"errors"
	"testing"
)

// claimerStub claims through the claim-only interface, and says when it
// cannot tell.
type claimerStub struct {
	stubConnector
	claims map[string]bool
	err    error
}

func (c claimerStub) ClaimsInboundSource(_ context.Context, name string) (bool, error) {
	return c.claims[name], c.err
}

// A real connector implements both: the dispatcher routes through
// InboundSource, the receiver's collision check asks the claim.
func (c claimerStub) InboundSource(_ context.Context, name string) (InboundSource, bool) {
	return InboundSource{Name: name}, c.err == nil && c.claims[name]
}

// providerStub claims only through InboundSource, as a connector written
// before InboundSourceClaimer existed does.
type providerStub struct {
	stubConnector
	sources map[string]bool
}

func (p providerStub) InboundSource(_ context.Context, name string) (InboundSource, bool) {
	return InboundSource{Name: name}, p.sources[name]
}

// SourceClaimed is the receiver's collision answer and must agree with
// ConnectorForSource, the dispatcher's routing answer, wherever no connector
// errors; where one does, it must say so rather than answer "unclaimed",
// which is the answer that admits an env pin (memql#5707 follow-up).
func TestSourceClaimedAgreesWithRoutingAndReportsWhatItCannotTell(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)
	ctx := context.Background()
	for _, name := range []string{"shopify", "legacy"} {
		Declare(name)
	}
	if err := Bind(claimerStub{stubConnector: stubConnector{"shopify"}, claims: map[string]bool{"shopify-acme": true}}); err != nil {
		t.Fatal(err)
	}
	if err := Bind(providerStub{stubConnector: stubConnector{"legacy"}, sources: map[string]bool{"legacy-tenant": true}}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"shopify", true},       // a bound connector's own name, through Lookup
		{"shopify-acme", true},  // the claim-only interface
		{"legacy-tenant", true}, // the InboundSource fallback
		{"shopify-nobody", false},
		{"stripe", false},
	} {
		_, claimed, err := SourceClaimed(ctx, tc.name)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		_, routed := ConnectorForSource(ctx, tc.name)
		if claimed != tc.want || claimed != routed {
			t.Errorf("%s: SourceClaimed=%v ConnectorForSource=%v, want %v", tc.name, claimed, routed, tc.want)
		}
	}

	UnbindForTest("shopify")
	if err := Bind(claimerStub{stubConnector: stubConnector{"shopify"}, err: errors.New("list stores: timeout")}); err != nil {
		t.Fatal(err)
	}
	if c, claimed, err := SourceClaimed(ctx, "shopify-acme"); err == nil || claimed || c == nil || c.Name() != "shopify" {
		t.Errorf("a connector that could not tell answered c=%v claimed=%v err=%v; want an error naming it", c, claimed, err)
	}
}
