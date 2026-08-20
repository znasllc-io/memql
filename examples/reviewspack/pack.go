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
type Provider struct{}

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
			Description: "Flip the public display toggle at runtime (data, not source).",
			Handler:     p.setPublicDisplay,
			ArgsSchema: map[string]string{
				"publicDisplay": "bool (required)",
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

func (p *Provider) recordModerationAction(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
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
	payload, err := json.Marshal(map[string]any{
		"reviewId":      reviewID,
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
	payload, err := json.Marshal(map[string]any{"publicDisplay": on})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{
		ID:        "reviews:settings:display",
		Concept:   "v1:reviews:reviewSettings",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
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
	_ = pctx
	return &Provider{}, nil
}

// Register wires the pack into the engine registries.
func Register(domain string) {
	memqldsl.RegisterTree(domain, Tree())
	// Bind the Go half to the pack domain so a v1:platform:packState
	// disable skips the factory and the module inventory folds this
	// integration under its pack row (memql#4183). Contract packs register
	// the plugin under the domain name, so the pair is (domain, domain).
	memql.BindPluginToPack(domain, domain)
	memql.RegisterPluginForContract(domain, ContractVersion, NewProvider)
}
