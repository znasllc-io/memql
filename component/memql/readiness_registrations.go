package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/znasllc-io/memql/component/memql/readiness"
)

// readInferenceRegistrations reads EVERY machine in the cluster, for the `ai`
// readiness arm (design record 2026-09-07-core-gate-and-honest-install, D3).
//
// ===========================================================================
// allWorkersWithStatus, AND THE ACTOR IS WHY
// ===========================================================================
// That query already exists for the Fleet page's operator view, and its filter
// is `actor.isClusterOwner==true`. The evaluation context satisfies it
// (readinessEvaluateContext) and nothing else here does.
//
// Under any narrower actor it does not fail -- it answers ZERO ROWS AND NO
// ERROR, because v1:worker:registration declares the composite owner tier. A
// cluster full of machines would then report `ai` unconfigured on every node,
// forever, with nothing in any log. That is the silence the maintenance
// principal exists for, and it is why the read and the context landed
// together rather than one at a time.
//
// ===========================================================================
// THE PAGE CEILING IS ACCEPTED, NOT PAGED THROUGH
// ===========================================================================
// The query paginates at 200. ONE qualifying machine configures the module, so
// a 201st can only matter on a cluster where the first 200 hold no door at all
// -- and the failure direction there is a gate that stays UP, which is the
// direction that cannot let somebody into a console whose features refuse.
// Paging would cost every node a full walk of the fleet on every recompute to
// change an answer that is already correct in every case anybody has.

// readinessRegistrationQuery is the DSL query the `ai` arm runs, named so the
// caller and the gate that asserts it LOADS cannot drift apart.
const readinessRegistrationQuery = "allWorkersWithStatus"

func (e *MemQLEngine) readInferenceRegistrations(ctx context.Context) ([]readiness.RegistrationFacts, error) {
	result, err := e.Execute(ctx, "query "+readinessRegistrationQuery+"()")
	if err != nil {
		return nil, fmt.Errorf("module readiness: registrations: %w", err)
	}
	// The BUNDLE, not the output. allWorkersWithStatus declares a shape, and a
	// shape sets result.output while LEAVING Bundle standing -- maybeClearBundle
	// nils it only inside ToAPIResult, the API-response path a client crosses.
	// An in-process Go caller is on the other side of that boundary. The trap
	// runs the other way for a CLIENT, which is why readiness_read.go writes
	// the same note rather than either file assuming it.
	if result == nil || result.Bundle == nil {
		return nil, nil
	}
	out := make([]readiness.RegistrationFacts, 0, len(result.Bundle.Nodes))
	for _, n := range result.Bundle.Nodes {
		raw, mErr := protojson.Marshal(n.GetPayload())
		if mErr != nil {
			continue
		}
		var row struct {
			OwnerUserId     string                      `json:"ownerUserId"`
			Labels          map[string]string           `json:"labels"`
			OperatorLabels  map[string]string           `json:"operatorLabels"`
			Apps            []readiness.RegistrationApp `json:"apps"`
			ConnectedNodeId string                      `json:"connectedNodeId"`
			LastSeenAt      string                      `json:"lastSeenAt"`
			RevokedAt       string                      `json:"revokedAt"`
		}
		// A row this cannot decode is SKIPPED rather than failing the read.
		// One malformed registration must not take the whole verdict with it;
		// the rest of the fleet still answers the question.
		if uErr := json.Unmarshal(raw, &row); uErr != nil {
			continue
		}
		out = append(out, readiness.RegistrationFacts{
			OwnerUserId:     row.OwnerUserId,
			Labels:          row.Labels,
			OperatorLabels:  row.OperatorLabels,
			Apps:            row.Apps,
			ConnectedNodeId: row.ConnectedNodeId,
			LastSeenAt:      parseReadinessTime(row.LastSeenAt),
			RevokedAt:       parseReadinessTime(row.RevokedAt),
		})
	}
	return out, nil
}

// parseReadinessTime answers the ZERO TIME for anything it cannot read, which
// every reader of these two fields already treats as "never" -- never as "now".
// A revokedAt that failed to parse therefore reads as not revoked, and a
// lastSeenAt that failed to parse reads as never seen: both the fail-closed
// direction for the field they are on.
func parseReadinessTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	// Worker rows write RFC3339Nano; RFC3339 alone rejects fractional seconds
	// and left Setup reading a connected machine as never seen.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
