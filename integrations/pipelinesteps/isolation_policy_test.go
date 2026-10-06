package pipelinesteps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const testPolicyPath = "/apis/networking.k8s.io/v1/namespaces/steps-ns/networkpolicies"

func policyFixture(cidr string, except []string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": "fixture"}, "spec": map[string]any{"podSelector": map[string]any{}, "policyTypes": []string{"Egress"}, "egress": []any{map[string]any{"to": []any{map[string]any{"ipBlock": map[string]any{"cidr": cidr, "except": except}}}}}}}
}
func policyAnswer(items ...any) kubeAnswer {
	raw, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		panic(err)
	}
	return kubeAnswer{code: 200, body: string(raw)}
}

func TestIsolationCIDRGuardsReadAllAdditiveGrants(t *testing.T) {
	safe := policyFixture("0.0.0.0/0", isolationProtectedCIDRs)
	cases := []struct {
		name   string
		answer kubeAnswer
		want   string
	}{
		{"baseline", policyAnswer(safe), ""},
		{"public endpoint", policyAnswer(safe, policyFixture("93.184.216.34/32", nil)), ""},
		{"only a probe policy remains", kubeAnswer{code: 200, body: `{"items":[{"spec":{"podSelector":{"matchLabels":{"memql.io/probe-role":"control"}},"policyTypes":["Egress"]}}]}`}, "no network policy isolates"},
		{"empty inventory", policyAnswer(), "no network policies"},
		{"unreadable inventory", kubeStatus(403, "Forbidden", "forbidden"), "reading pipeline network policies"},
		{"malformed inventory", kubeAnswer{code: 200, body: `{`}, "reading pipeline network policies"},
		{"partial inventory", kubeAnswer{code: 200, body: `{"metadata":{"continue":"next"},"items":[]}`}, "incomplete"},
		{"unknown IP family", policyAnswer(policyFixture("::/0", nil)), "unsupported IP grant"},
		{"invalid exclusion", policyAnswer(policyFixture("0.0.0.0/0", []string{"bad"})), "invalid exclusion"},
		{"all destinations", policyAnswer(map[string]any{"spec": map[string]any{"egress": []any{map[string]any{}}}}), "every address"},
		{"empty peer", policyAnswer(map[string]any{"spec": map[string]any{"egress": []any{map[string]any{"to": []any{map[string]any{}}}}}}), "unrestricted peer"},
	}
	for _, protected := range isolationProtectedCIDRs {
		cases = append(cases, struct {
			name   string
			answer kubeAnswer
			want   string
		}{"additive grant " + protected, policyAnswer(safe, policyFixture(protected, nil)), "protected IP range"})
		remaining := []string{}
		for _, p := range isolationProtectedCIDRs {
			if p != protected {
				remaining = append(remaining, p)
			}
		}
		cases = append(cases, struct {
			name   string
			answer kubeAnswer
			want   string
		}{"missing exclusion " + protected, policyAnswer(policyFixture("0.0.0.0/0", remaining)), "protected IP range"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, _ := newKubeFake(t, map[string]kubeAnswer{"GET " + testPolicyPath: tc.answer})
			err := k.CheckIsolationCIDRs(t.Context())
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestShippedIsolationCIDRPolicyPassesAudit(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "components", "pipelines", "networkpolicy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var policy map[string]any
	if err := yaml.Unmarshal(raw, &policy); err != nil {
		t.Fatal(err)
	}
	k, _ := newKubeFake(t, map[string]kubeAnswer{"GET " + testPolicyPath: policyAnswer(policy)})
	if err := k.CheckIsolationCIDRs(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestCachedPodProofCannotHideChangedCIDRGrants(t *testing.T) {
	h := newRunnerHarness(t) // already carries a fresh successful pod proof
	h.c.with(func(c *rtCluster) { answer := policyAnswer(policyFixture("0.0.0.0/0", nil)); c.policyAnswer = &answer })
	isoWantRefused(t, h, h.run(t, rtRun()))
	if createsOf(h.c, kubeJobs, isoName()) != 0 {
		t.Fatal("invalid policy started a probe")
	}
}
