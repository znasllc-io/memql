package sitetraffic

import (
	"context"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
)

// The rollup and the country refresh each admit their automation's
// maintenance principal, as its constructor minted it, and nobody else: not
// the automation's ordinary actor, and not an actor spelled with the id.
func TestTheSweepsAdmitOnlyTheirMintedMaintenancePrincipal(t *testing.T) {
	for _, tc := range []struct {
		automation string
		refusal    string
		call       func(*Integration, context.Context) error
	}{
		{rollupAutomation, "maintenance principal", func(i *Integration, ctx context.Context) error { _, err := i.handleRollup(ctx, nil, 0); return err }},
		{countryRefreshAutomation, "maintenance principal", func(i *Integration, ctx context.Context) error {
			_, err := i.handleCountryRefresh(ctx, nil, 0)
			return err
		}},
	} {
		real := auth.MaintenanceActor(auth.AutomationOriginEngine, tc.automation)
		touched := false
		i := NewIntegration(nil, func() *bun.DB { touched = true; return nil }, touchingSession(&touched), quietLogger())
		for name, ac := range map[string]*auth.AccessContext{
			"a system actor with the listed name":    auth.SystemActor("maintenance:" + tc.automation),
			"an actor built with the principal's id": {UserId: real.UserId, Role: real.Role, Unranked: true, Synthetic: true},
			"the automation's ordinary reader actor": auth.AutomationActor(auth.AutomationOriginEngine, tc.automation),
		} {
			if err := tc.call(i, auth.ContextWithAccess(context.Background(), ac)); err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Errorf("%s, %s: want the principal refusal, got %v", tc.automation, name, err)
			}
		}
		if touched {
			t.Errorf("%s: a refused call reached for the database", tc.automation)
		}
		if err := tc.call(i, auth.ContextWithAccess(context.Background(), real)); err != nil && strings.Contains(err.Error(), tc.refusal) {
			t.Errorf("%s: the real maintenance principal was refused: %v", tc.automation, err)
		}
	}
}
