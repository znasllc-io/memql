package retention

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

func TestProcessingMetadataExpiresOnlyAfterTheMaximumCallLifetime(t *testing.T) {
	for _, concept := range []string{"v1:calls:processingState", "v1:calls:processingReceipt", "v1:calls:processingRate"} {
		t.Run(concept, func(t *testing.T) {
			f, integration := statusRowSweep(t, concept)
			require.Equal(t, 24*time.Hour, f.concept.Retention.TTL)
			require.False(t, f.concept.Retention.KeepLatest, "finished rooms must not retain processing metadata forever")
			require.Equal(t, "none", f.concept.Retention.Archive)
			active, expired := concept+":active", concept+":expired"
			payload := `{"ownerUserId":"v1:identity:user:the-owner","siteId":"v1:platform:site:s","roomId":"v1:calls:room:r"}`
			// An early receipt in a maximum-length call still protects replay just
			// before the call's hard deadline; it expires only after the room does.
			f.insert(t, "MemoryNodes", active, 24*time.Hour-time.Minute, payload)
			f.insert(t, "MemoryNodes", expired, 25*time.Hour, payload)
			scheduled := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor(auth.AutomationOriginEngine, "retireDeclaredConceptVersions"))
			nodes, e := integration.sweep(scheduled, map[string]any{}, 0)
			require.NoError(t, e)
			require.Equal(t, 1, sweepReport(t, nodes).Deleted)
			require.Len(t, f.versionsOf(t, active), 1, "an active maximum-length call lost its replay guard")
			require.Empty(t, f.versionsOf(t, expired))
		})
	}
}
