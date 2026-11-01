package memql

import (
	"context"
	"encoding/json"
	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"strings"
	"testing"
)

func TestEnergyPrivateAccessRejectsDelegatedCredentials(t *testing.T) {
	for _, class := range []string{"", "app_session", "service_account", "agent", "badge", "role_test", "user", "pat"} {
		ctx := auth.ContextWithClaims(context.Background(), map[string]any{"class": class, "sub": "owner", "role": "writer"})
		ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "owner", Role: auth.RoleWriter})
		if privateEnergyAccess(ctx) != (class == "" || class == "user" || class == "pat") {
			t.Fatalf("unexpected access for %q", class)
		}
		ctx = auth.ContextWithDelegation(ctx, &auth.DelegationContext{DelegationId: "delegated"})
		if privateEnergyAccess(ctx) {
			t.Fatalf("delegated %q read private settings", class)
		}
		// Exercise the handler boundary before any DB access, including read/save/confirm.
		for _, action := range []string{"read", "save", "confirm"} {
			if _, err := (&MemQLEngine{}).evaluateFleetEnergySettings(ctx, map[string]any{"action": action}); err == nil {
				t.Fatalf("%s bypassed the private gate", action)
			}
		}
	}
}
func TestEnergyInputClassificationRedactsNestedAddress(t *testing.T) {
	decl, err := languageParser.ParseBuiltinDecl(`@executor("fleetEnergySettings") builtin fleetEnergySettings { secretSettings object }`)
	if err != nil {
		t.Fatal(err)
	}
	fn, err := builtinDeclToFunction(decl, "energy-test")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]struct{}{}
	for _, field := range fn.BuiltinArgs.Fields {
		collectSecretArgNames(field, names)
	}
	const address = "123 Private Fixture Lane"
	args := map[string]any{"secretSettings": map[string]any{"address": address}, "action": "save"}
	raw, _ := json.Marshal(redactArgsForLog(args, names))
	if strings.Contains(string(raw), address) || !strings.Contains(string(raw), "save") {
		t.Fatalf("bad diagnostic: %s", raw)
	}
	if strings.Contains(redactSecretArgValues("invalid "+address, names, args), address) {
		t.Fatal("nested address leaked in error")
	}
}
func TestEnergyDecodeRejectsUncontractedPrivateFields(t *testing.T) {
	var research electricityResearch
	err := decodeEnergy(map[string]any{"status": "found", "address": "private"}, &research)
	if err == nil {
		t.Fatal("research accepted a precise address field")
	}
	if energyProfileID("owner-a", "machine") == energyProfileID("owner-b", "machine") {
		t.Fatal("profiles cross owners")
	}
}
