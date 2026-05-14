package identity

import "encoding/json"

// parseRegisteredClientsJSON parses the IDENTITY_REGISTERED_CLIENTS
// env-var payload. Kept in its own file so the env-loading code in
// config.go stays readable.
func parseRegisteredClientsJSON(raw string) ([]RegisteredClient, error) {
	var out []RegisteredClient
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}
