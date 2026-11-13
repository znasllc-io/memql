package emailstats

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/statsperiod"
)

type selectiveStatisticsGrants struct {
	fakeGrants
	denied                    map[string]bool
	failGrant, failCapability bool
	failAccount               string
	asked                     []string
}

func (g *selectiveStatisticsGrants) ResolveAccessGrant(ctx context.Context) memql.AccessGrant {
	if g.failGrant {
		auth.NoteReadFailure(ctx, errors.New("membership evidence unavailable"))
	}
	return g.grant
}

func (g *selectiveStatisticsGrants) OrganizationCapable(ctx context.Context, account, verb, resource string) bool {
	g.asked = append(g.asked, account+"/"+resource)
	if g.failCapability || (g.failAccount != "" && account == g.failAccount) {
		auth.NoteReadFailure(ctx, errors.New("capability evidence unavailable"))
	}
	return verb == auth.VerbRead && !g.denied[account+"/"+resource]
}

func TestStatisticsFiniteScopeAndDenialsStopBeforeFacts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested []string
		deny      map[string]bool
	}{
		{"foreign", []string{"foreign"}, nil},
		{"blank", []string{""}, nil},
		{"data denied", nil, map[string]bool{"own/data": true}},
		{"email denied", nil, map[string]bool{"own/app:email": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &selectiveStatisticsGrants{fakeGrants: fakeGrants{memberOf("own")}, denied: tc.deny}
			s, err := readStatistics(context.Background(), nil, g.grant, tc.requested, statsperiod.Last7Days, statsNow, g, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantNothing(t, tc.name, s)
			for _, asked := range g.asked {
				if strings.HasPrefix(asked, "foreign/") {
					t.Fatalf("foreign account reached capability resolution: %v", g.asked)
				}
			}
		})
	}
}

func TestStatisticsEveryAccountGlobalDenialsStopBeforeDiscovery(t *testing.T) {
	for _, resource := range []string{auth.ResourceData, "app:email"} {
		g := &selectiveStatisticsGrants{fakeGrants: fakeGrants{staff()}, denied: map[string]bool{"/" + resource: true}}
		s, err := readStatistics(context.Background(), nil, g.grant, nil, statsperiod.Last7Days, statsNow, g, nil)
		if err != nil {
			t.Fatal(err)
		}
		wantNothing(t, resource, s)
		for _, asked := range g.asked {
			if !strings.HasPrefix(asked, "/") {
				t.Fatalf("global denial reached account discovery: %v", g.asked)
			}
		}
	}
}

func TestStatisticsMissingAuthorizerAndFailedEvidenceAreClosed(t *testing.T) {
	s, err := readStatistics(context.Background(), nil, memberOf("own"), nil, statsperiod.Last7Days, statsNow, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantNothing(t, "missing authorizer", s)
	g := &selectiveStatisticsGrants{fakeGrants: fakeGrants{memberOf("own")}, failCapability: true}
	ctx, failures := auth.ContextWithReadFailures(context.Background())
	_, err = readStatistics(ctx, nil, g.grant, nil, statsperiod.Last7Days, statsNow, g, failures)
	if err == nil || !strings.Contains(err.Error(), "authorization could not be resolved") {
		t.Fatalf("failed authorization reached facts or looked like success: %v", err)
	}
}

func TestStatisticsHandlerRefusesFailedMembershipEvidence(t *testing.T) {
	i := NewIntegration(nil, nil, quietLogger())
	i.grants = &selectiveStatisticsGrants{fakeGrants: fakeGrants{memberOf("own")}, failGrant: true}
	nodes, err := i.handleStatistics(context.Background(), map[string]any{"period": "7d"}, 0)
	if err == nil || !strings.Contains(err.Error(), "membership evidence unavailable") || len(nodes) != 0 {
		t.Fatalf("failed membership evidence returned data or a misleading success: nodes=%v error=%v", nodes, err)
	}
}
