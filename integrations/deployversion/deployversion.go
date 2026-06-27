// Package deployversion exposes pure version-arithmetic as a DSL-callable,
// READ-ONLY integration capability. The MemQL DSL has no string-split /
// integer-parse, so a `logic` that needs the next release version cannot
// compute it inline (ADR §2: logic decides, but only over what the language
// can express). Per the _reference/_logic blessed pattern ("a read-only
// builtin does the mapping"), the next-version decision routes through this
// capability: pure, deterministic, no side effects -- safe to call from a
// `logic` body under the call-graph contract.
//
// The semver contract MIRRORS component/deploycontrol/semver.go (the
// cut-version flow #1877): a clean three-part numeric version (classic semver
// 1.2.3 or CalVer 2026.6.21), bumped on major/minor/patch, with no
// pre-release/build suffix -- exactly what scripts/release/release.sh accepts.
// It is duplicated here (not imported) to keep this always-loaded plug-in free
// of the deploycontrol package's heavy k8s/deploy dependencies; the parity
// test (deployversion_test.go) locks the two impls together.
package deployversion

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// resultConcept is the synthetic, never-persisted MemoryNode concept the
// capability returns (same convention as integrations/timeutil).
const resultConcept = "integration:deployversion:result"

// Integration exposes the version helpers as DSL capabilities. Stateless --
// the constructor exists only so the PluginContext factory pattern stays
// uniform with the other plug-ins.
type Integration struct{}

// NewIntegration constructs the stateless integration.
func NewIntegration() *Integration { return &Integration{} }

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "deployversion" }

// Capabilities implements memql.IntegrationProvider. One read-only capability:
// suggestNextVersion. Add siblings here (e.g. compareVersions) as the deploy
// bundle needs them.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name: "suggestNextVersion",
			Description: "Compute the next release version by bumping a clean three-part " +
				"numeric version (X.Y.Z; classic semver or CalVer). bump in {major, minor, " +
				"patch} (default patch). major/minor zero the lower parts. Pure + read-only: " +
				"safe to call from a logic body. Mirrors deploycontrol/semver.go.",
			Handler: i.handleSuggestNextVersion,
			ArgsSchema: map[string]string{
				"current": "string (required) -- the current version, clean X.Y.Z (e.g. \"0.9.9\" or \"2026.6.21\").",
				"bump":    "string (optional) -- \"major\" | \"minor\" | \"patch\". Default \"patch\".",
			},
		},
	}
}

// semver is a three-part numeric version (major.minor.patch), modelling both
// classic semver and CalVer (YYYY.M.D). Mirror of deploycontrol/semver.go.
type semver struct{ major, minor, patch int }

// parseSemver parses a clean three-part numeric version, rejecting anything
// release.sh would (wrong segment count, empty/negative/non-numeric segment,
// or a pre-release/build suffix). Identical contract to deploycontrol.
func parseSemver(s string) (semver, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return semver{}, fmt.Errorf("invalid version %q: empty", s)
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("invalid version %q: want three-part X.Y.Z (got %d segments)", s, len(parts))
	}
	nums := make([]int, 3)
	for idx, p := range parts {
		if p == "" {
			return semver{}, fmt.Errorf("invalid version %q: empty segment", s)
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, fmt.Errorf("invalid version %q: segment %q is not a non-negative integer", s, p)
		}
		nums[idx] = n
	}
	return semver{major: nums[0], minor: nums[1], patch: nums[2]}, nil
}

// String renders the canonical X.Y.Z form.
func (v semver) String() string { return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch) }

// bump returns the version with one part incremented. major/minor zero the
// lower parts; patch (the default, including "") increments patch. Mirror of
// deploycontrol/semver.go's bump.
func (v semver) bump(part string) (semver, error) {
	switch part {
	case "major":
		return semver{major: v.major + 1}, nil
	case "minor":
		return semver{major: v.major, minor: v.minor + 1}, nil
	case "", "patch":
		return semver{major: v.major, minor: v.minor, patch: v.patch + 1}, nil
	default:
		return semver{}, fmt.Errorf("invalid bump %q: want major, minor, or patch", part)
	}
}

// handleSuggestNextVersion parses `current`, bumps by `bump`, and returns one
// MemoryNode whose payload carries `current`, `bump` (the part actually
// applied), and `next` (the bumped version). A malformed version or bump part
// is a hard error -- a version decision must never silently fabricate a tag.
func (i *Integration) handleSuggestNextVersion(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	current := strings.TrimSpace(asString(args["current"]))
	if current == "" {
		return nil, fmt.Errorf("deployversion.suggestNextVersion: required arg \"current\" is missing")
	}
	part := strings.TrimSpace(asString(args["bump"]))
	if part == "" {
		part = "patch"
	}

	v, err := parseSemver(current)
	if err != nil {
		return nil, fmt.Errorf("deployversion.suggestNextVersion: %w", err)
	}
	bumped, err := v.bump(part)
	if err != nil {
		return nil, fmt.Errorf("deployversion.suggestNextVersion: %w", err)
	}

	payload := map[string]any{
		"current": v.String(),
		"bump":    part,
		"next":    bumped.String(),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("deployversion.suggestNextVersion: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("deployversion:next:%s:%s:%d", v.String(), bumped.String(), time.Now().UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   b,
	}}, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
