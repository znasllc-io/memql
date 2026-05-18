// Package timeutil exposes thin time + timezone helpers as DSL-callable
// integration capabilities. The MemQL DSL has no native IANA-timezone
// math; automations that need per-user "what is today" date keys (the
// daily-space provisioning + rollover automations are the first
// consumer) reach for these capabilities instead.
package timeutil

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// resultConcept is the synthetic MemoryNode concept the capabilities
// return. Same shape as integrations/agents/integration.go's
// envelopeConcept -- an in-flight integration result, never persisted.
const resultConcept = "integration:timeutil:result"

// Integration exposes the time + timezone helpers as DSL capabilities.
// Stateless -- holds no fields; the constructor exists only so the
// PluginContext factory pattern stays uniform.
type Integration struct{}

// NewIntegration constructs the stateless integration. Pure helper so
// the plug-in factory + any test reach the same instance.
func NewIntegration() *Integration { return &Integration{} }

// IntegrationName implements memql.IntegrationProvider.
func (t *Integration) IntegrationName() string { return "timeutil" }

// Capabilities implements memql.IntegrationProvider. One capability
// today (dateKeyInTimezone); add siblings here as we hit other
// timezone-aware needs (e.g. todayBoundsInTimezone for the rollover
// archive sweep).
func (t *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name: "dateKeyInTimezone",
			Description: "Compute the YYYY-MM-DD date key in the given IANA timezone. " +
				"Falls back to UTC when the timezone is empty or unparseable. The result " +
				"is the deterministic date stamp the daily-space automations use to " +
				"build idempotent space ids (`daily-<userHash>-<dateKey>`).",
			Handler: t.handleDateKeyInTimezone,
			ArgsSchema: map[string]string{
				"timezone": "string (optional) -- IANA name like \"America/Los_Angeles\". Empty / invalid -> UTC.",
				"now":      "string (optional) -- RFC3339 timestamp to convert. Default: server wall clock at call time.",
			},
		},
	}
}

// handleDateKeyInTimezone returns one MemoryNode whose payload carries
// `dateKey` (YYYY-MM-DD) plus `tzUsed` (the timezone we actually
// resolved against -- "UTC" when fallback fires) so the caller can
// log + assert which path ran without re-parsing the args.
//
// Args:
//
//	args["timezone"]  string  optional -- IANA name
//	args["now"]       string  optional -- RFC3339; defaults to now
//
// Errors are exclusively for misuse (non-string args). An unparseable
// timezone is NOT an error: it falls back to UTC with the fallback
// recorded in the result. Same for an unparseable `now` -- it falls
// back to the server wall clock. The daily-space automations run
// non-interactively; hard-failing on a user with a typo'd preference
// would block their daily indefinitely, while UTC fallback at least
// gives them SOMETHING.
func (t *Integration) handleDateKeyInTimezone(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	tzName := strings.TrimSpace(asString(args["timezone"]))
	nowArg := strings.TrimSpace(asString(args["now"]))

	loc := time.UTC
	tzUsed := "UTC"
	if tzName != "" {
		if resolved, err := time.LoadLocation(tzName); err == nil {
			loc = resolved
			tzUsed = tzName
		}
		// Unparseable / unknown IANA name: silently fall through to
		// UTC. The result payload's tzUsed=="UTC" while the caller
		// asked for something else is the breadcrumb a future operator
		// follows when "why is my daily landing at 5pm" comes up.
	}

	var t0 time.Time
	if nowArg != "" {
		if parsed, err := time.Parse(time.RFC3339, nowArg); err == nil {
			t0 = parsed
		} else {
			t0 = time.Now()
		}
	} else {
		t0 = time.Now()
	}

	dateKey := t0.In(loc).Format("2006-01-02")

	payload := map[string]any{
		"dateKey": dateKey,
		"tzUsed":  tzUsed,
	}
	bytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("timeutil.dateKeyInTimezone: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("timeutil:dateKey:%s:%s:%d", tzUsed, dateKey, time.Now().UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   bytes,
	}}, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
