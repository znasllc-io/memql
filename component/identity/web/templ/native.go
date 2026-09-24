package webtempl

import "encoding/json"

// Layout's renderer and asset wiring are not part of the native UI contract.
func (d LayoutData) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Title     string
		BrandName string
	}{d.Title, d.BrandName})
}
