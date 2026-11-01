package memql

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/secret"
)

type energyWritePauseKey struct{}
type energyWritePause struct {
	once, resumeOnce sync.Once
	prepared, resume chan struct{}
}

func (p *energyWritePause) release() { p.resumeOnce.Do(func() { close(p.resume) }) }

type energyWritePauseHook struct{}

func (energyWritePauseHook) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	pause, _ := ctx.Value(energyWritePauseKey{}).(*energyWritePause)
	if pause != nil && event.Query == "BEGIN" {
		// The handler has already read and prepared its payload, but has not
		// acquired transactional fences. Pausing at INSERT would deadlock a
		// correctly fenced second writer behind the first writer's lock.
		pause.once.Do(func() {
			close(pause.prepared)
			select {
			case <-pause.resume:
			case <-ctx.Done():
			}
		})
	}
	return ctx
}
func (energyWritePauseHook) AfterQuery(context.Context, *bun.QueryEvent) {}

func pausedEnergyWrite(t *testing.T, ctx context.Context, write func(context.Context) error) (*energyWritePause, <-chan error) {
	t.Helper()
	pause := &energyWritePause{prepared: make(chan struct{}), resume: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.WithValue(ctx, energyWritePauseKey{}, pause), 30*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(pause.release)
	done := make(chan error, 1)
	go func() { done <- write(ctx) }()
	select {
	case <-pause.prepared:
	case err := <-done:
		t.Fatalf("write finished before the transaction boundary: %v", err)
	case <-ctx.Done():
		t.Fatal("write did not reach the transaction boundary")
	}
	return pause, done
}

func TestEnergyProfileDBFencesPreparedDecisions(t *testing.T) {
	// Private pool and context-bound hook: never pause another test's work.
	eng, db, _ := readMergeTestEngine(t)
	db.AddQueryHook(energyWritePauseHook{})
	t.Setenv(secret.EnvMasterKey, strings.Repeat("ab", 32))
	peer, err := New(db)
	require.NoError(t, err)
	t.Cleanup(peer.cache.close)
	require.NoError(t, peer.Init(memorynodes.DefaultRegistry()))
	for _, mode := range []string{"first-create", "save", "research-location", "research-consent"} {
		t.Run(mode, func(t *testing.T) {
			f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("energy-fence")}
			owner := f.person("owner", true)
			machine := f.machine("machine", owner)
			ctx := shareDirActor(context.Background(), owner, auth.RoleWriter)
			pid := energyProfileID(owner, machine)
			t.Cleanup(func() {
				_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", energyProfileConcept).Where("id = ?", energyProfileConcept+":"+pid).Exec(context.Background())
			})
			settings := map[string]any{"address": "123 Synthetic Fixture Lane", "city": "Fixture City", "region": "AZ", "postalCode": "85000", "country": "US", "timezone": "America/Phoenix", "verified": true, "lookupAllowed": true, "watts": 200, "energySource": "Owner estimate"}
			save := func(e *MemQLEngine, callCtx context.Context, revision string, input map[string]any) error {
				_, err := e.evaluateFleetEnergySettings(callCtx, map[string]any{"registrationId": machine, "action": "save", "revision": revision, "secretSettings": input})
				return err
			}
			if mode != "first-create" {
				require.NoError(t, save(eng, ctx, "", settings))
			}
			row, _, err := eng.energyProfile(ctx, machine)
			require.NoError(t, err)
			revision := mapString(row, "revision")
			loserInput := energyMap(settings)
			loserInput["watts"] = 900
			loser, losingResult := pausedEnergyWrite(t, ctx, func(callCtx context.Context) error {
				if strings.HasPrefix(mode, "research-") {
					_, err := peer.evaluateFleetRecordElectricityResearch(callCtx, map[string]any{"registrationId": machine, "locationRevision": mapString(row, "locationRevision"), "research": map[string]any{"status": "unavailable", "notes": "Prepared against the old location and consent."}})
					return err
				}
				return save(peer, callCtx, revision, loserInput)
			})
			winnerInput := energyMap(settings)
			switch mode {
			case "research-location":
				winnerInput["city"] = "Changed locality"
			case "research-consent":
				winnerInput["lookupAllowed"] = false
			}
			winner, winningResult := pausedEnergyWrite(t, ctx, func(callCtx context.Context) error {
				return save(eng, callCtx, revision, winnerInput)
			})
			// Both decisions now hold the same pre-write state. Commit one and
			// require the other to fail specifically on the changed row witness.
			winner.release()
			require.NoError(t, <-winningResult)
			persisted, _, err := eng.energyProfile(ctx, machine)
			require.NoError(t, err)
			loser.release()
			require.ErrorIs(t, <-losingResult, ErrRowVersionChanged)
			after, _, err := peer.energyProfile(ctx, machine)
			require.NoError(t, err)
			require.Equal(t, persisted, after, "the rejected decision must not overwrite the winner")
			require.Equal(t, winnerInput["city"], energyMap(after["settings"])["city"])
			require.Equal(t, winnerInput["lookupAllowed"], energyMap(after["settings"])["lookupAllowed"])
			require.Equal(t, float64(200), energyMap(after["settings"])["watts"])
			require.Empty(t, energyMap(after["research"]))
		})
	}
}
