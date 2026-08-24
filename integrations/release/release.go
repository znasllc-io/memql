// Package release cuts a version of MemQL itself: it creates the git tag and
// publishes the GitHub Release, which is what fires the image-build cascade
// (dispatch-engine-images-on-release.yml, #2519 -> build-engine-images.yml with
// the bare version).
//
// IT DOES NOT BUILD OR PUSH IMAGES, does not touch the repo-root VERSION file
// (deliberately stale, and nothing here reads it), and does not go near
// scripts/release/release.sh, whose --push path stays break-glass. Cutting is
// the one manual half of an otherwise automatic pipeline, and this package is
// exactly that half.
//
// THE OWNER ASK WAS "only the owners (role) have permissions to cut a new
// version", and the gate that delivers it is in cut.go, in Go, before any
// network call. Read the note there before changing anything about the order
// of operations.
//
// Design record:
// docs/superpowers/specs/2026-08-23-release-cut-automation-design.md.
// Operator runbook: docs/public/operate/release-cutting.md.
package release

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// resultConcept is the synthetic, never-persisted MemoryNode concept the
// capabilities return -- the same convention integrations/timeutil and
// integrations/deployversion use for a computed answer that is not a row.
//
// The DURABLE record is v1:cluster:releaseCut, written separately by the
// store. This is only the shape a DSL caller receives, and the two are
// deliberately different: the row is history keyed by version, the result is
// one call's answer including things never stored (the plan a dry run
// computed, the per-image presence a check saw).
const resultConcept = "integration:release:result"

// Integration is the DSL-facing capability set.
type Integration struct {
	logger   *slog.Logger
	github   *Client
	registry *RegistryChecker
	store    *Store
	resolver resolver
}

// NewIntegration wires the pieces. Every collaborator is a field rather than a
// package-level singleton so a test can swap the GitHub base URL, the registry
// base URL and the resolver chain independently -- which is what lets the
// whole surface be exercised without a network.
func NewIntegration(logger *slog.Logger, engine memql.IntegrationEngineAccess, res resolver) *Integration {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Integration{
		logger:   logger,
		github:   NewClient(),
		registry: NewRegistryChecker(),
		store:    NewStore(engine),
		resolver: res,
	}
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "release" }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name: "releaseCut",
			Description: "Cut a new release: compute the next version from the repository's vX.Y.Z tags, " +
				"create the tag at main's head, and publish a GitHub Release, which is what fires the " +
				"image-build cascade. Owner role only, enforced before any network call.",
			Handler: i.handleCut,
			ArgsSchema: map[string]string{
				"bump":             "string (required) -- \"major\" | \"minor\" | \"patch\".",
				"notes":            "string (optional) -- prose prepended to GitHub's generated release notes.",
				"bumpExtensionPin": "boolean (optional) -- also open a PR bumping the VS Code extension's DEFAULT_STACK_TAG.",
				"dryRun":           "boolean (optional) -- compute the plan and create nothing.",
			},
		},
		{
			Name: "releaseCutStatus",
			Description: "Ask the container registry whether the images for a cut version exist yet. " +
				"All present moves the row to images_available; any absent leaves it dispatched; a check " +
				"that errored reports the error and changes nothing.",
			Handler: i.handleStatus,
			ArgsSchema: map[string]string{
				"version": "string (required) -- v1.2.3 or 1.2.3.",
			},
		},
	}
}

// handleCut adapts the DSL argument map to Cut.
func (i *Integration) handleCut(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	out, err := i.Cut(ctx, CutRequest{
		Bump:             asString(args["bump"]),
		Notes:            asString(args["notes"]),
		BumpExtensionPin: asBool(args["bumpExtensionPin"]),
		DryRun:           asBool(args["dryRun"]),
	})
	if err != nil {
		return nil, err
	}
	return resultNode("cut", out.Version, out)
}

// handleStatus adapts the DSL argument map to Status.
func (i *Integration) handleStatus(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	out, err := i.Status(ctx, asString(args["version"]))
	if err != nil {
		return nil, err
	}
	return resultNode("status", out.Version, out)
}

// resultNode wraps a value as the single synthetic node a capability returns.
func resultNode(kind, version string, payload any) ([]memorynodes.MemoryNode, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("release: encode the %s result: %w", kind, err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("release:%s:%s:%d", kind, version, time.Now().UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   encoded,
	}}, nil
}

// asBool reads a DSL boolean.
//
// It accepts the string spellings as well, because a value that has been
// through a JSON round trip or a hand-composed call string arrives as "true"
// rather than true -- and a checkbox that silently does nothing is worse than
// one that refuses.
func asBool(v any) bool {
	switch value := v.(type) {
	case bool:
		return value
	case string:
		return value == "true"
	default:
		return false
	}
}
