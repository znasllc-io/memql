package shopify

import (
	"testing"
	"time"
)

func TestArrivingHistoryIsKeptBesideAnUnrelatedReason(t *testing.T) {
	for _, status := range []string{"queued", "running"} {
		t.Run(status, func(t *testing.T) {
			store := verdictStore()
			rows := setRow(cleanRows(store), "order", func(r map[string]any) { r["backfillStatus"] = status })
			in := verdictInputOf(store, rows)
			in.Privacy = privacyFailing{Count: 1, Since: verdictNow.Add(-48 * time.Hour)}
			reply := verdictPayload(computeVerdict(in))
			arriving, ok := payloadStrings(reply["arrivingConcepts"])
			if !ok || !heldBy(arriving, statisticsWalk) {
				t.Fatalf("arriving orders hidden by privacy reason: %v", reply)
			}
			if reply["state"] != VerdictNeedsAttention {
				t.Fatalf("unrelated reason lost: %v", reply)
			}
		})
	}
}
