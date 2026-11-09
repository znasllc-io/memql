package pipelines

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// SecuritySettingsVariable is the operator's explicit consent to weekly
// security stages and their remediation goals. An absent setting is off.
const SecuritySettingsVariable = "MEMQL_PIPELINES_SECURITY"

type SecuritySettings struct {
	Enabled bool `json:"enabled"`
}

func ParseSecuritySettings(raw string) SecuritySettings {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	if err := decoder.Decode(&fields); err != nil || len(fields) != 1 {
		return SecuritySettings{}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return SecuritySettings{}
	}
	for name, field := range fields {
		var enabled bool
		if !strings.EqualFold(name, "enabled") || json.Unmarshal(field, &enabled) != nil {
			return SecuritySettings{}
		}
		return SecuritySettings{Enabled: enabled}
	}
	return SecuritySettings{}
}
