package work

import "encoding/json"

// NormalizeApprovalSubject snapshots the exact JSON value storage will return.
// In particular map[string]string and map[string]any must describe the same
// approval. Copying also prevents a caller mutating the object after hashing it.
func NormalizeApprovalSubject(subject map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(subject)
	if err != nil {
		return nil, err
	}
	var normalized map[string]any
	err = json.Unmarshal(raw, &normalized)
	return normalized, err
}
