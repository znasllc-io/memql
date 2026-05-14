package metadata

import (
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/oschwald/geoip2-golang"
)

// GeoIPResolver resolves IP addresses to geographic information using MaxMind GeoLite2.
// Thread-safe. Gracefully degrades if the database file is not available.
type GeoIPResolver struct {
	reader *geoip2.Reader
	mu     sync.RWMutex
}

// NewGeoIPResolver creates a GeoIP resolver. If dbPath is empty or the file cannot
// be opened, the resolver returns nil results (no error, graceful degradation).
func NewGeoIPResolver(dbPath string, logger *slog.Logger) *GeoIPResolver {
	if dbPath == "" {
		if logger != nil {
			logger.Info("geoip: no database path configured, geo metadata will be empty")
		}
		return &GeoIPResolver{}
	}

	reader, err := geoip2.Open(dbPath)
	if err != nil {
		if logger != nil {
			logger.Warn("geoip: failed to open database, geo metadata will be empty", "path", dbPath, "error", err)
		}
		return &GeoIPResolver{}
	}

	if logger != nil {
		logger.Info("geoip: database loaded", "path", dbPath)
	}
	return &GeoIPResolver{reader: reader}
}

// Resolve returns geographic metadata for an IP address.
// Returns nil if the IP is invalid, private, or if no database is loaded.
func (g *GeoIPResolver) Resolve(ipStr string) map[string]string {
	if g == nil || g.reader == nil || ipStr == "" {
		return nil
	}

	ip := net.ParseIP(ipStr)
	if ip == nil {
		return nil
	}

	// Skip private/loopback IPs
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return nil
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	record, err := g.reader.City(ip)
	if err != nil {
		return nil
	}

	result := make(map[string]string)

	if record.Country.IsoCode != "" {
		result["geo.country"] = record.Country.IsoCode
	}
	if len(record.Subdivisions) > 0 && record.Subdivisions[0].Names["en"] != "" {
		result["geo.region"] = record.Subdivisions[0].Names["en"]
	}
	if record.City.Names["en"] != "" {
		result["geo.city"] = record.City.Names["en"]
	}
	if record.Location.TimeZone != "" {
		result["geo.timezone"] = record.Location.TimeZone
	}
	if record.Location.Latitude != 0 || record.Location.Longitude != 0 {
		result["geo.lat"] = fmt.Sprintf("%.4f", record.Location.Latitude)
		result["geo.lng"] = fmt.Sprintf("%.4f", record.Location.Longitude)
	}

	return result
}

// Close closes the GeoIP database reader.
func (g *GeoIPResolver) Close() error {
	if g == nil || g.reader == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reader.Close()
}
