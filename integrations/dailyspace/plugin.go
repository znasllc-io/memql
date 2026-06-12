package dailyspace

import (
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/polyphon"
)

// init self-registers the dailyspace integration as a core plug-in.
// Always on: the daily-space automations live in core DSL and need
// these capabilities on every node-type binary that runs cognition
// automations. The plug-in needs the engine handle to re-enter
// Execute (read user, write space), so the factory plucks it off
// PluginContext.
//
// When LiveKit is configured (POLYPHON_LIVEKIT_* -- shared cluster-wide
// via the genesis envelope, so every replica sees the same value), the
// rollover sweep additionally gets the room-provider's RoomService
// client so it can delete a rolled-over daily's polyphon room
// (memql#1384). Without LiveKit there is no room to delete and the
// sweep simply skips that step -- same code path in every environment,
// only the configuration varies.
func init() {
	memql.RegisterPlugin("dailyspace", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		integration := NewIntegration(pctx.Engine)
		if cfg := polyphon.ConfigFromEnv(); cfg.LiveKitConfigured() {
			integration.SetRoomDeleter(polyphon.NewLocalRoomProvider(cfg))
		}
		return integration, nil
	})
}
