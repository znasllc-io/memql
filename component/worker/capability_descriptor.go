package worker

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// CapabilityDescriptor is the structured capability self-description
// a worker MAY attach to its Register message (memql#1330,
// computed cockpit-side by memql-cockpit#162/#169). It tells the
// agent runtime which workerComputer actions the connected build can
// actually dispatch, so dispatch can degrade or refuse up front
// without a live worker round-trip.
//
// The wire encoding is the Register.capability_descriptor_json proto
// field: this struct serialized as JSON. Unknown JSON fields are
// tolerated (additive evolution does not bump schemaVersion).
type CapabilityDescriptor struct {
	Platform             string   `json:"platform"`
	DisplayServer        string   `json:"displayServer"`
	ComputerUseAvailable bool     `json:"computerUseAvailable"`
	Actions              []string `json:"actions"`
	SchemaVersion        int      `json:"schemaVersion"`
}

const (
	// CapabilityDescriptorSchemaVersion is the only schema version
	// this server admits. Bump in lockstep with the cockpit's
	// capabilitySchemaVersion when the shape changes incompatibly.
	CapabilityDescriptorSchemaVersion = 1

	// maxCapabilityDescriptorBytes caps the raw JSON size so a
	// misbehaving worker can't park megabytes in the registry / DB.
	maxCapabilityDescriptorBytes = 4096

	// maxCapabilityDescriptorActions caps the advertised action list.
	maxCapabilityDescriptorActions = 64
)

// capabilityNamePattern constrains platform + action identifiers:
// short, printable, no whitespace / quoting / control characters.
var capabilityNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// validDisplayServers is the closed displayServer enum, mirroring
// the cockpit's detectDisplayServer outputs.
var validDisplayServers = map[string]struct{}{
	"quartz":  {},
	"x11":     {},
	"wayland": {},
	"none":    {},
}

// ParseCapabilityDescriptor validates and decodes the optional
// capability_descriptor_json Register field. An empty input returns
// (nil, nil) -- the descriptor is strictly optional and workers that
// don't send one register exactly as before. Garbage (oversized,
// non-JSON, wrong schemaVersion, unknown displayServer, malformed
// action names) is rejected with a descriptive error.
func ParseCapabilityDescriptor(raw string) (*CapabilityDescriptor, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > maxCapabilityDescriptorBytes {
		return nil, fmt.Errorf("capability descriptor: %d bytes exceeds %d byte cap", len(raw), maxCapabilityDescriptorBytes)
	}
	var d CapabilityDescriptor
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, fmt.Errorf("capability descriptor: invalid JSON: %w", err)
	}
	if d.SchemaVersion != CapabilityDescriptorSchemaVersion {
		return nil, fmt.Errorf("capability descriptor: unsupported schemaVersion %d (want %d)", d.SchemaVersion, CapabilityDescriptorSchemaVersion)
	}
	if _, ok := validDisplayServers[d.DisplayServer]; !ok {
		return nil, fmt.Errorf("capability descriptor: unknown displayServer %q (want quartz|x11|wayland|none)", d.DisplayServer)
	}
	if !capabilityNamePattern.MatchString(d.Platform) {
		return nil, fmt.Errorf("capability descriptor: invalid platform %q", d.Platform)
	}
	if len(d.Actions) > maxCapabilityDescriptorActions {
		return nil, fmt.Errorf("capability descriptor: %d actions exceeds cap of %d", len(d.Actions), maxCapabilityDescriptorActions)
	}
	seen := make(map[string]struct{}, len(d.Actions))
	for _, a := range d.Actions {
		if !capabilityNamePattern.MatchString(a) {
			return nil, fmt.Errorf("capability descriptor: invalid action name %q", a)
		}
		if _, dup := seen[a]; dup {
			return nil, fmt.Errorf("capability descriptor: duplicate action %q", a)
		}
		seen[a] = struct{}{}
	}
	if d.Actions == nil {
		// Keep downstream JSON shape stable: "actions": [] -- never null.
		d.Actions = []string{}
	}
	return &d, nil
}

// AsMap returns the descriptor as a generic map for engine mutation
// args / payload persistence. Nil receiver returns nil.
func (d *CapabilityDescriptor) AsMap() map[string]any {
	if d == nil {
		return nil
	}
	actions := make([]any, 0, len(d.Actions))
	for _, a := range d.Actions {
		actions = append(actions, a)
	}
	return map[string]any{
		"platform":             d.Platform,
		"displayServer":        d.DisplayServer,
		"computerUseAvailable": d.ComputerUseAvailable,
		"actions":              actions,
		"schemaVersion":        d.SchemaVersion,
	}
}

// capabilityDescriptorFromMap decodes a persisted payload object
// back into the typed struct. Returns nil for empty/absent input or
// anything that doesn't validate -- a corrupted stored descriptor
// degrades to "no descriptor" rather than failing the read path.
func capabilityDescriptorFromMap(m map[string]any) *CapabilityDescriptor {
	if len(m) == 0 {
		return nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	d, err := ParseCapabilityDescriptor(string(raw))
	if err != nil {
		return nil
	}
	return d
}
