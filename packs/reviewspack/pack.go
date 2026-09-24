// Package reviewspack is the client-agnostic reviews pack (memql#4139).
//
// It is a PACK, not core: dsl/todos, dsl/calendar, and dsl/campaigns stay
// engine domains and this package cannot shadow them. Registration is
// opt-in (Register, or the reviewspack build-tag init) the same way
// examples/referencepack loads.
package reviewspack

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"embed"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// Domain is the DSL namespace this pack owns.
const Domain = "reviews"

// ContractVersion is the Plugin SDK contract this pack was built against.
const ContractVersion = memql.PluginContractVersion

const integrationName = "reviews"

// PrincipalClient is the only expressible decidedBy kind.
const PrincipalClient = "client"

// DefaultEnabled is what this pack ships as when no v1:platform:packState
// row governs it (epic memql#5532, issue memql#5549).
//
// FALSE, and the reason is the shopper surface rather than anything about
// reviews: enabling this pack publishes a write endpoint and a public read
// on every deployable whose shopperForms is on. That is a decision an
// operator makes, not one an upgrade makes for them.
const DefaultEnabled = false

// ClosedCriteria is the closed moderation enum. No "other", no sentiment.
var ClosedCriteria = []string{"spam", "profanity", "harassment", "off_topic", "illegal", "copyright"}

//go:embed all:dsl
var packFS embed.FS

// Tree returns the pack's embedded .memql subtree.
func Tree() fs.FS {
	sub, err := fs.Sub(packFS, "dsl")
	if err != nil {
		panic("reviewspack: embedded dsl tree missing: " + err.Error())
	}
	return sub
}

// Provider is the pack's IntegrationProvider.
//
// IT HOLDS THE ENGINE, which the pack did not need until it gained a public
// read (issue memql#5553). Two of its capabilities now READ the graph
// before they write or answer -- moderation copies the storeId off the
// review it is acting on, and the public read consults this store's
// publicDisplay before it returns anything -- and neither question can be
// asked from a mutation body.
type Provider struct {
	engine memql.IntegrationEngineAccess
	// reader is the narrow row read published.go decides over. Separate
	// from engine above so the gate, the exclusion and the bound are
	// testable as functions over rows rather than over an engine envelope.
	reader rowReader
}

func (p *Provider) IntegrationName() string { return integrationName }

func (p *Provider) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "recordModerationAction",
			Description: "Append a closed-criterion moderation decision. Client principal only.",
			Handler:     p.recordModerationAction,
			ArgsSchema: map[string]string{
				"reviewId":      "string (required)",
				"criterion":     "string (required) - closed enum",
				"principalKind": "string (required) - must be client",
				"decidedBy":     "string (required) - client user id",
				"note":          "string (optional)",
			},
		},
		{
			Name:        "exportReview",
			Description: "Export a review including image files (bytes), not URLs.",
			Handler:     p.exportReview,
			ArgsSchema: map[string]string{
				"reviewId": "string (required)",
				"images":   "object (optional) - [{name, bytes}]",
			},
		},
		{
			Name:        "setPublicDisplay",
			Description: "Flip one store's public display toggle at runtime (data, not source).",
			Handler:     p.setPublicDisplay,
			ArgsSchema: map[string]string{
				"storeId":       "string (required) - the store this toggle is for",
				"publicDisplay": "bool (required)",
			},
		},
		{
			Name: "publishedForProduct",
			Description: "The public read: reviews a storefront may render for one product, " +
				"gated on this store's publicDisplay and excluding every review a moderation " +
				"decision hides.",
			Handler: p.publishedForProduct,
			ArgsSchema: map[string]string{
				"storeId":       "string (required) - the store to read",
				"productHandle": "string (required) - the product",
				"siteId":        "string (optional) - provenance",
				"limit":         "integer (optional) - default 50",
			},
		},
	}
}

// ClientMayModerate reports whether principalKind may author a moderation action.
// Provider operators are inexpressible: only "client" is admitted.
func ClientMayModerate(principalKind string) error {
	if strings.TrimSpace(principalKind) != PrincipalClient {
		return fmt.Errorf("reviews: decidedBy must be a Client principal")
	}
	return nil
}

// ValidCriterion reports whether criterion is in the closed enum.
func ValidCriterion(criterion string) error {
	c := strings.TrimSpace(criterion)
	for _, want := range ClosedCriteria {
		if c == want {
			return nil
		}
	}
	return fmt.Errorf("reviews: criterion %q is not in the closed enum", criterion)
}

func (p *Provider) recordModerationAction(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := ClientMayModerate(asString(args["principalKind"])); err != nil {
		return nil, err
	}
	if err := ValidCriterion(asString(args["criterion"])); err != nil {
		return nil, err
	}
	reviewID := asString(args["reviewId"])
	decidedBy := asString(args["decidedBy"])
	if reviewID == "" || decidedBy == "" {
		return nil, fmt.Errorf("reviews: reviewId and decidedBy are required")
	}
	// THE STORE IS READ OFF THE REVIEW, never supplied (design D9, issue
	// memql#5552). A decision must be scoped to the same store its subject
	// is on, or a read scoped to one store would go on showing a review the
	// other store's merchant had already hidden -- and an argument for it
	// would be a way to record a decision against somebody else's store.
	//
	// The read runs under the CALLER'S own actor, so a caller who cannot
	// read the review cannot moderate it either. That is a narrowing this
	// capability did not have before and it is the right one: the previous
	// version would happily append a decision naming a review id that did
	// not exist.
	storeID, err := p.storeOfReview(ctx, reviewID)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"reviewId":      reviewID,
		"storeId":       storeID,
		"criterion":     asString(args["criterion"]),
		"decidedBy":     decidedBy,
		"principalKind": PrincipalClient,
		"note":          asString(args["note"]),
	})
	if err != nil {
		return nil, fmt.Errorf("reviews: marshal moderation: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("reviews:moderation:%s:%d", reviewID, time.Now().UTC().UnixNano()),
		Concept:   "v1:reviews:moderationAction",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

func (p *Provider) exportReview(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	files, err := ExportImages(args["images"])
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"reviewId": asString(args["reviewId"]),
		"files":    files,
	})
	if err != nil {
		return nil, fmt.Errorf("reviews: marshal export: %w", err)
	}
	if strings.Contains(string(payload), "http://") || strings.Contains(string(payload), "https://") {
		return nil, fmt.Errorf("reviews: export must include image bytes, not URLs")
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("reviews:export:%s", asString(args["reviewId"])),
		Concept:   "v1:reviews:export",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

func (p *Provider) setPublicDisplay(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	on, ok := args["publicDisplay"].(bool)
	if !ok {
		return nil, fmt.Errorf("reviews: publicDisplay must be a bool")
	}
	// PER STORE (design D9). The id used to be the literal
	// "reviews:settings:display" -- one row for the whole cluster -- which
	// meant a merchant flipping reviews on for their live storefront also
	// flipped them on for the development store a candidate was being
	// exercised against, and for every other merchant this cluster serves.
	storeID := strings.TrimSpace(asString(args["storeId"]))
	if storeID == "" {
		return nil, fmt.Errorf("reviews: storeId is required; a display toggle is per store")
	}
	payload, err := json.Marshal(map[string]any{
		"storeId":       storeID,
		"publicDisplay": on,
	})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{
		ID:        SettingsRowID(storeID),
		Concept:   "v1:reviews:reviewSettings",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

// SettingsRowID derives the one settings row id for a store.
//
// DERIVED RATHER THAN GENERATED, so the row is a singleton per store and a
// second flip is a new VERSION of one logical row rather than a second row
// the read would have to choose between.
func SettingsRowID(storeID string) string {
	return "reviews:settings:" + strings.TrimSpace(storeID)
}

// storeOfReview reads the store a review belongs to, under the caller's own
// actor.
func (p *Provider) storeOfReview(ctx context.Context, reviewID string) (string, error) {
	rows, err := p.rowsFor(ctx, "reviewById", map[string]string{"reviewId": reviewID})
	if err != nil {
		return "", fmt.Errorf("reviews: resolving the review being moderated: %w", err)
	}
	if len(rows) == 0 {
		// REFUSED, not defaulted. A decision against a review nobody can
		// read is a row scoped to no store, which every storefront read
		// would then miss -- so it would hide nothing while looking like it
		// had.
		return "", fmt.Errorf("reviews: review %q is not readable by this caller, so it cannot be moderated", reviewID)
	}
	storeID := strings.TrimSpace(asString(rows[0]["storeId"]))
	if storeID == "" {
		return "", fmt.Errorf("reviews: review %q carries no storeId", reviewID)
	}
	return storeID, nil
}

// ExportedFile is one image in an export. Bytes are required.
type ExportedFile struct {
	Name  string `json:"name"`
	Bytes []byte `json:"bytes"`
}

// ExportImages converts an images arg into files. A URL-only entry is refused.
func ExportImages(raw any) ([]ExportedFile, error) {
	if raw == nil {
		return []ExportedFile{}, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("reviews: images must be a list")
	}
	out := make([]ExportedFile, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("reviews: each image must be an object")
		}
		name := asString(m["name"])
		if name == "" {
			return nil, fmt.Errorf("reviews: image name is required")
		}
		if url := asString(m["url"]); url != "" && m["bytes"] == nil {
			return nil, fmt.Errorf("reviews: export requires image bytes, not a URL")
		}
		b, err := asBytes(m["bytes"])
		if err != nil {
			return nil, err
		}
		if len(b) == 0 {
			return nil, fmt.Errorf("reviews: export requires image bytes, not a URL")
		}
		out = append(out, ExportedFile{Name: name, Bytes: b})
	}
	return out, nil
}

func asBytes(v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case []byte:
		return t, nil
	case string:
		return []byte(t), nil
	default:
		return nil, fmt.Errorf("reviews: image bytes must be a string or byte slice")
	}
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// NewProvider builds the pack IntegrationProvider.
func NewProvider(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
	return &Provider{engine: pctx.Engine, reader: &engineRowReader{engine: pctx.Engine}}, nil
}

// Register wires the pack into the engine registries.
//
// The registrations below include the two that promote this from an example
// to a pack a cluster can actually serve (epic memql#5532):
//
//   - RegisterPackDefault(domain, false) -- storefront packs ship DISABLED.
//     Reviews carries a shopper write path and a public read, and "absence
//     of a packState row means enabled" would switch both on for every
//     cluster that ever upgrades, with no row anywhere saying so. An
//     operator enables it in Cluster > Modules.
//   - RegisterShopperForm / RegisterShopperRead -- the declared shopper
//     surface. NOTHING ELSE on this pack is reachable without a bearer, and
//     these two are reachable only on a deployable whose shopperForms is
//     on.
//   - RegisterStorefrontPack -- a developer may flip it, not only the owner
//     (Connect Shopify design, D4).
func Register(domain string) {
	memqldsl.RegisterTree(domain, Tree())
	memqldsl.RegisterPackDefault(domain, DefaultEnabled)
	// A developer may turn it on and off (Connect Shopify design, D4). A
	// declaration of its own, never inferred from the default above.
	memqldsl.RegisterStorefrontPack(domain)
	registerShopperSurface()
	// Bind the Go half to the pack domain so a v1:platform:packState
	// disable skips the factory and the module inventory folds this
	// integration under its pack row (memql#4183). Contract packs register
	// the plugin under the domain name, so the pair is (domain, domain).
	memql.BindPluginToPack(domain, domain)
	memql.RegisterPluginForContract(domain, ContractVersion, NewProvider)
}
