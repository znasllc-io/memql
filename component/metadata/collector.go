package metadata

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/visionarys-io/memql/component/auth"
)

// ServerMeta contains static server information set once at startup.
type ServerMeta struct {
	Region   string // Cloud region (e.g., "us-central1")
	NodeId   string // Cluster node ID
	NodeType string // Node type (e.g., "cognition", "agent", "planner", "bff")
	Version  string // Service version string
	Instance string // Cloud Run instance/revision ID or hostname
}

// Collector assembles metadata from context.Context, server identity, and GeoIP.
// Thread-safe and reusable across requests.
type Collector struct {
	server      ServerMeta
	geoResolver *GeoIPResolver
}

// NewCollector creates a metadata collector with static server metadata and optional GeoIP.
func NewCollector(server ServerMeta, geoDBPath string, logger *slog.Logger) *Collector {
	return &Collector{
		server:      server,
		geoResolver: NewGeoIPResolver(geoDBPath, logger),
	}
}

// Collect assembles metadata from all available context sources.
// Returns a sparse map -- only keys with non-empty values are included.
func (c *Collector) Collect(ctx context.Context) map[string]string {
	if c == nil {
		return nil
	}

	m := make(map[string]string, 32)

	// 1. Identity context
	c.collectIdentity(ctx, m)

	// 2. Request context
	c.collectRequest(ctx, m)

	// 3. Geographic context (from client IP)
	c.collectGeo(m)

	// 4. Server context (static)
	c.collectServer(m)

	// 5. Source context
	c.collectSource(ctx, m)

	// 6. SI context
	c.collectSI(ctx, m)

	// 7. Lineage context
	c.collectLineage(ctx, m)

	// Validate and enforce limits
	return Validate(m)
}

// collectIdentity extracts identity metadata from auth context.
func (c *Collector) collectIdentity(ctx context.Context, m map[string]string) {
	// Token info (subject)
	token := auth.TokenInfoFromContext(ctx)
	if token != nil {
		set(m, "identity.subject", token.Subject)
	}

	// User identity (display name, role, email)
	identity, err := auth.UserIdentityFromContext(ctx)
	if err == nil {
		displayName := strings.TrimSpace(identity.FirstName + " " + identity.LastName)
		set(m, "identity.displayName", displayName)
		set(m, "identity.role", identity.Role)
		set(m, "identity.email", identity.Email)
	}

	// Delegation context
	delegation, ok := auth.DelegationFromContext(ctx)
	if ok && delegation != nil {
		set(m, "identity.type", delegation.IdentityType)
		set(m, "identity.delegationId", delegation.DelegationId)
		set(m, "identity.guardianSubject", delegation.GuardianSubject)
		set(m, "identity.agentId", delegation.AgentId)
	} else if token != nil {
		set(m, "identity.type", "human")
	}
}

// collectRequest extracts request metadata from transport context.
func (c *Collector) collectRequest(ctx context.Context, m map[string]string) {
	rm := RequestMetaFromContext(ctx)
	if rm == nil {
		return
	}
	set(m, "request.id", rm.RequestId)
	set(m, "request.correlationId", rm.CorrelationId)
	set(m, "request.protocol", rm.Protocol)
	set(m, "request.method", rm.Method)
	set(m, "request.userAgent", rm.UserAgent)
	set(m, "geo.ip", rm.ClientIP)
	set(m, "platform.type", rm.Platform)
	set(m, "platform.appVersion", rm.AppVersion)
}

// collectGeo resolves geographic data from the client IP.
func (c *Collector) collectGeo(m map[string]string) {
	ip := m["geo.ip"]
	if ip == "" || c.geoResolver == nil {
		return
	}
	geo := c.geoResolver.Resolve(ip)
	for k, v := range geo {
		set(m, k, v)
	}
}

// collectServer adds static server metadata.
func (c *Collector) collectServer(m map[string]string) {
	set(m, "server.region", c.server.Region)
	set(m, "server.nodeId", c.server.NodeId)
	set(m, "server.nodeType", c.server.NodeType)
	set(m, "server.version", c.server.Version)
	set(m, "server.instance", c.server.Instance)
}

// collectSource extracts source/trigger metadata.
func (c *Collector) collectSource(ctx context.Context, m map[string]string) {
	sm := SourceMetaFromContext(ctx)
	if sm == nil {
		return
	}
	set(m, "source.system", sm.System)
	set(m, "source.component", sm.Component)
	set(m, "source.automationName", sm.AutomationName)
	set(m, "source.functionName", sm.FunctionName)
	set(m, "source.toolName", sm.ToolName)
	set(m, "source.trigger", sm.Trigger)
}

// collectSI extracts synthetic intelligence execution metadata.
func (c *Collector) collectSI(ctx context.Context, m map[string]string) {
	si := SIMetaFromContext(ctx)
	if si == nil {
		return
	}
	set(m, "si.agentId", si.AgentId)
	set(m, "si.agentName", si.AgentName)
	set(m, "si.provider", si.Provider)
	set(m, "si.model", si.Model)
	if si.TokensIn > 0 {
		m["si.tokensIn"] = fmt.Sprintf("%d", si.TokensIn)
	}
	if si.TokensOut > 0 {
		m["si.tokensOut"] = fmt.Sprintf("%d", si.TokensOut)
	}
	if si.LatencyMs > 0 {
		m["si.latencyMs"] = fmt.Sprintf("%d", si.LatencyMs)
	}
	if si.ToolCalls > 0 {
		m["si.toolCalls"] = fmt.Sprintf("%d", si.ToolCalls)
	}
}

// collectLineage extracts data provenance metadata.
func (c *Collector) collectLineage(ctx context.Context, m map[string]string) {
	lm := LineageMetaFromContext(ctx)
	if lm == nil {
		return
	}
	set(m, "lineage.causedBy", lm.CausedBy)
	set(m, "lineage.replyTo", lm.ReplyTo)
	set(m, "lineage.batchId", lm.BatchId)
	set(m, "lineage.importSource", lm.ImportSource)
}

// set adds a key-value pair to the map only if the value is non-empty.
func set(m map[string]string, key, value string) {
	v := strings.TrimSpace(value)
	if v != "" {
		m[key] = v
	}
}
