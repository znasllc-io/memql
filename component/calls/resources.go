package calls

import (
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxResources          = 32
	MaxResourceLeases     = 64
	MaxActiveResources    = 2
	ResourceLeaseLifetime = 15 * time.Minute
)

// ResourceSpec describes output, never an input URL, shell command or credential.
// Device access stays with the owner's connector. The gateway reserves and bounds
// both output tracks even when an adapter only sends one.
type ResourceSpec struct {
	Label          string `json:"label"`
	Kind           string `json:"kind"`
	Audio          bool   `json:"audio"`
	Video          bool   `json:"video"`
	MaxWidth       int    `json:"maxWidth"`
	MaxHeight      int    `json:"maxHeight"`
	MaxFPS         int    `json:"maxFPS"`
	MaxBitrateKbps int    `json:"maxBitrateKbps"`
}

type ResourceRequest struct {
	Action           string        `json:"action"`
	ResourceID       string        `json:"resourceId,omitempty"`
	RoomID           string        `json:"roomId,omitempty"`
	ExpectedRevision int           `json:"expectedRevision,omitempty"`
	RequestID        string        `json:"requestId,omitempty"`
	Resource         *ResourceSpec `json:"resource,omitempty"`
}

type ResourceInfo struct {
	ResourceSpec
	ID         string     `json:"id"`
	Revision   int        `json:"revision"`
	Status     string     `json:"status"`
	RoomID     string     `json:"roomId,omitempty"`
	LeaseID    string     `json:"leaseId,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	HumanHold  bool       `json:"humanHold"`
	LastReason string     `json:"lastReason,omitempty"`
}

type ResourcePublication struct {
	ResourceID string    `json:"resourceId"`
	LeaseID    string    `json:"leaseId"`
	RoomID     string    `json:"roomId"`
	Identity   string    `json:"identity"`
	Protocol   string    `json:"protocol"`
	URL        string    `json:"url"`
	StreamKey  string    `json:"streamKey"`
	ExpiresAt  time.Time `json:"expiresAt"`
}
type ResourceCapabilities struct {
	Protocols        []string `json:"protocols"`
	MaxSources       int      `json:"maxSources"`
	MaxActivePerRoom int      `json:"maxActivePerRoom"`
	MaxLeaseSeconds  int      `json:"maxLeaseSeconds"`
	Available        bool     `json:"available"`
	Readiness        string   `json:"readiness"`
}
type ResourceResult struct {
	Resources    []ResourceInfo       `json:"resources"`
	Publication  *ResourcePublication `json:"publication,omitempty"`
	Capabilities ResourceCapabilities `json:"capabilities"`
	Reason       string               `json:"reason,omitempty"`
}

func ValidResourceSpec(r ResourceSpec) bool {
	// Stock ingress reserves and may publish both tracks. A false flag cannot
	// pretend to be an enforced single-track permission; the adapter may still
	// choose to capture/send only one of its two authorized tracks.
	return r.Audio && r.Video && ValidResourceOutputBounds(r)
}

// ValidResourceOutputBounds admits earlier metadata for inspection/removal,
// without expanding a source's track authority or allowing it to start.
func ValidResourceOutputBounds(r ResourceSpec) bool {
	switch r.Kind {
	case "desktop", "window", "terminal", "camera", "microphone", "synthetic":
	default:
		return false
	}
	if strings.TrimSpace(r.Label) != r.Label || r.Label == "" || utf8.RuneCountInString(r.Label) > 64 || strings.ContainsAny(r.Label, "\r\n\x00") || (!r.Audio && !r.Video) {
		return false
	}
	return r.MaxWidth >= 160 && r.MaxWidth <= 1280 && r.MaxHeight >= 90 && r.MaxHeight <= 720 && r.MaxFPS >= 1 && r.MaxFPS <= 15 && r.MaxBitrateKbps >= 64 && r.MaxBitrateKbps <= 1000
}

func ResourceScope(action string) string {
	switch action {
	case "list", "status":
		return "resources_read"
	case "register", "update", "unregister", "start":
		return "resources_publish"
	case "stop", "switch":
		return "resources_control"
	default:
		return ""
	}
}
