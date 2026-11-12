package emailstats

import (
	"context"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
)

// The rollup admits the maintenance principal of emailStatisticsRollupSweep, as
// its constructor minted it: an actor spelled with its id is not it.
func TestTheRollupRefusesAPrincipalSpelledWithItsId(t *testing.T) {
	real := auth.MaintenanceActor(auth.AutomationOriginEngine, rollupAutomation)
	touched := false
	i := NewIntegration(nil, func() *bun.DB { touched = true; return nil }, quietLogger())
	for name, ac := range map[string]*auth.AccessContext{
		"a system actor with the listed name":    auth.SystemActor("maintenance:" + rollupAutomation),
		"an actor built with the principal's id": {UserId: real.UserId, Role: real.Role, Unranked: true, Synthetic: true},
	} {
		if _, err := i.handleRollup(auth.ContextWithAccess(context.Background(), ac), nil, 0); err == nil || !strings.Contains(err.Error(), "maintenance principal") {
			t.Errorf("%s: want the principal refusal, got %v", name, err)
		}
	}
	if touched {
		t.Fatal("a refused call reached for the database")
	}
	_, _ = i.handleRollup(auth.ContextWithAccess(context.Background(), real), nil, 0)
	if !touched {
		t.Error("the real maintenance principal did not get past the check")
	}
}
