package memql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/core/costestimate"
)

// The native DB lane must run this with MEMQL_REQUIRE_DB=1. A skipped test is
// not proof of sealed persistence, owner admission or revision fencing.
func TestEnergyProfileDBPrivacyRevisionAndResearch(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(secret.EnvMasterKey, strings.Repeat("ab", 32))
	peer, err := New(db)
	require.NoError(t, err)
	require.NoError(t, peer.Init(memorynodes.DefaultRegistry()))
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("energy")}
	owner := f.person("owner", true)
	other := f.person("other", true)
	machine := f.machine("machine", owner)
	ctx := shareDirActor(context.Background(), owner, auth.RoleWriter)
	private := map[string]any{"address": "123 Synthetic Fixture Lane", "city": "Fixture City", "region": "AZ", "postalCode": "85000", "country": "US", "timezone": "America/Phoenix", "verified": true, "lookupAllowed": true, "watts": 200, "energySource": "Owner estimate"}
	_, err = eng.evaluateFleetEnergySettings(ctx, map[string]any{"registrationId": machine, "action": "save", "revision": "", "secretSettings": private})
	require.NoError(t, err)
	row, pid, err := peer.energyProfile(ctx, machine)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", energyProfileConcept).Where("id = ?", energyProfileConcept+":"+pid).Exec(context.Background())
	})
	raw, _ := json.Marshal(row)
	require.NotContains(t, string(raw), "123 Synthetic Fixture Lane")
	sealed := mapString(row, "encryptedAddress")
	require.NotEmpty(t, sealed)
	clear, err := secret.Decrypt(sealed)
	require.NoError(t, err)
	require.Equal(t, private["address"], clear)
	_, err = peer.evaluateFleetEnergySettings(shareDirActor(context.Background(), other, auth.RoleOwner), map[string]any{"registrationId": machine, "action": "read"})
	require.Error(t, err)
	_, err = peer.evaluateFleetEnergySettings(ctx, map[string]any{"registrationId": machine, "action": "save", "revision": "old", "secretSettings": private})
	require.ErrorContains(t, err, "changed")
	stale := map[string]any{"registrationId": machine, "locationRevision": "old", "research": map[string]any{"status": "unavailable", "notes": "No official source found."}}
	_, err = peer.evaluateFleetRecordElectricityResearch(ctx, stale)
	require.Error(t, err)
	stale["locationRevision"] = mapString(row, "locationRevision")
	_, err = peer.evaluateFleetRecordElectricityResearch(ctx, stale)
	require.NoError(t, err)
	row, _, err = eng.energyProfile(ctx, machine)
	require.NoError(t, err)
	require.NotEmpty(t, mapString(row, "revision"))
	require.Equal(t, "unavailable", energyMap(row["research"])["status"])
	// Generic raw insertion must not bypass the builtin's owner attestation.
	_, err = eng.Execute(ctx, `insert("v1:worker:energyProfile", id=`+parser.QuoteString(pid)+`, payload={"settings":{"verified":true}})`)
	require.ErrorContains(t, err, "owner settings and research tools")
	// Complete the persisted research -> owner confirmation -> priced-call path.
	now := time.Now().UTC()
	quote := costestimate.Tariff{Provider: "Fixture utility", Name: "TOU", Currency: "USD", Timezone: "America/Phoenix", SourceURLs: []string{"https://utility.example/tariff"}, CheckedAt: now.Add(-time.Minute).Format(time.RFC3339), RefreshAfter: now.Add(24 * time.Hour).Format(time.RFC3339), EffectiveFrom: now.Add(-24 * time.Hour).Format("2006-01-02"), RateBands: []costestimate.RateBand{{Name: "off-peak", PerKWh: .1, AppliesWhen: "off-peak hours"}, {Name: "peak", PerKWh: .3, AppliesWhen: "peak hours"}}}
	_, err = peer.evaluateFleetRecordElectricityResearch(ctx, map[string]any{"registrationId": machine, "locationRevision": mapString(row, "locationRevision"), "research": electricityResearch{Status: "found", Tariffs: []costestimate.Tariff{quote}}})
	require.NoError(t, err)
	row, _, err = eng.energyProfile(ctx, machine)
	require.NoError(t, err)
	_, err = eng.evaluateFleetEnergySettings(ctx, map[string]any{"registrationId": machine, "action": "confirm", "revision": mapString(row, "revision"), "provider": quote.Provider, "tariffName": quote.Name})
	require.NoError(t, err)
	priced := peer.EstimateLocalElectricity(ctx, machine, owner, time.Minute.Seconds())
	require.Equal(t, "range", priced["status"])
	require.Greater(t, priced["minimum"].(float64), 0.0)
	row, _, err = eng.energyProfile(ctx, machine)
	require.NoError(t, err)
	// A changed location invalidates all previously submitted utility evidence.
	private["city"] = "Changed locality"
	_, err = eng.evaluateFleetEnergySettings(ctx, map[string]any{"registrationId": machine, "action": "save", "revision": mapString(row, "revision"), "secretSettings": private})
	require.NoError(t, err)
	changed, _, err := peer.energyProfile(ctx, machine)
	require.NoError(t, err)
	require.NotEqual(t, mapString(row, "locationRevision"), mapString(changed, "locationRevision"))
	require.Empty(t, energyMap(changed["research"]))
	require.Equal(t, "unavailable", eng.EstimateLocalElectricity(ctx, machine, owner, time.Minute.Seconds())["status"])
	// Two independent engine instances cannot both accept the same revision.
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, engine := range []*MemQLEngine{eng, peer} {
		go func(e *MemQLEngine) {
			<-start
			_, err := e.evaluateFleetEnergySettings(ctx, map[string]any{"registrationId": machine, "action": "save", "revision": mapString(changed, "revision"), "secretSettings": private})
			results <- err
		}(engine)
	}
	close(start)
	accepted := 0
	for i := 0; i < 2; i++ {
		if <-results == nil {
			accepted++
		}
	}
	require.Equal(t, 1, accepted)
}
