package customdomain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type guidanceEngine struct {
	*fakeEngine
	seen context.Context
}

func (e *guidanceEngine) Execute(ctx context.Context, query string) (any, error) {
	e.seen = ctx
	return e.fakeEngine.Execute(ctx, query)
}

func TestDNSGuidanceUsesCallerAndConfiguredTargetsWithoutWrites(t *testing.T) {
	e := &guidanceEngine{fakeEngine: &fakeEngine{t: t, rows: []map[string]any{{"hostname": "acme.com"}}}}
	i := &Integration{store: NewStore(e), cfg: Config{EdgeHost: "routing.example.net"}, resolver: &fakeResolver{hosts: map[string][]string{
		"routing.example.net": {"2001:db8::1", "203.0.113.11", "203.0.113.11", "not-an-ip"},
	}}}
	ctx := context.Background()
	nodes, err := i.handleDNSGuidance(ctx, map[string]any{"domainId": "bound-site"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if e.seen != ctx {
		t.Fatal("guidance changed the caller context")
	}
	if len(e.mutations()) != 0 {
		t.Fatal("guidance wrote state")
	}
	var data struct {
		Hostname string
		EdgeHost string
		IPv4     []string
		IPv6     []string
	}
	if err := json.Unmarshal(nodes[0].Payload, &data); err != nil {
		t.Fatal(err)
	}
	if data.Hostname != "acme.com" || data.EdgeHost != "routing.example.net" || len(data.IPv4) != 1 || data.IPv4[0] != "203.0.113.11" || len(data.IPv6) != 1 || data.IPv6[0] != "2001:db8::1" {
		t.Fatalf("wrong guidance: %+v", data)
	}
}

func TestDNSGuidanceRefusesUnavailableBindingsAndUnresolvedTargets(t *testing.T) {
	e := &fakeEngine{t: t}
	i := &Integration{store: NewStore(e), cfg: Config{EdgeHost: testEdgeHost}, resolver: &fakeResolver{fail: map[string]bool{testEdgeHost: true}}}
	if _, err := i.handleDNSGuidance(context.Background(), map[string]any{"domainId": "inaccessible"}, 0); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("expected caller-scoped refusal, got %v", err)
	}
	e.rows = []map[string]any{{"hostname": "acme.com"}}
	if _, err := i.handleDNSGuidance(context.Background(), map[string]any{"domainId": "available"}, 0); err == nil {
		t.Fatal("unresolved target produced instructions")
	}
}

func TestCheckPointingRefusesMixedOldAndCurrentAddresses(t *testing.T) {
	for _, old := range []string{"198.51.100.20", "2001:db8::99"} {
		r := &fakeResolver{hosts: map[string][]string{testEdgeHost: {"203.0.113.10"}, "acme.com": {"203.0.113.10", old}}}
		if result := CheckPointing(context.Background(), r, "acme.com", testEdgeHost); result.OK {
			t.Fatalf("accepted stale destination %s beside current target", old)
		}
	}
}
