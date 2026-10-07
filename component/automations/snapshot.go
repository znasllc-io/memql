package automations

import (
	"encoding/json"
	"fmt"
)

// Snapshot prepares an owned copy of a loaded definition. Scope hosts check
// and execute this copy so a loader reload cannot replace preflighted work.
// Derived trust and secret annotations are copied, never accepted from JSON.
func (l *Loader) Snapshot(source *Automation) (*Automation, error) {
	if source == nil {
		return nil, fmt.Errorf("cannot snapshot a nil automation")
	}
	data, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	owned, err := l.parseJSON(data, source.Origin)
	if err != nil {
		return nil, err
	}
	owned.Trusted, owned.Origin = source.Trusted, source.Origin
	var copySecrets func([]*ArgsField, []*ArgsField)
	copySecrets = func(dst, src []*ArgsField) {
		for i, field := range src {
			if field == nil || dst[i] == nil {
				continue
			}
			dst[i].Secret = field.Secret
			copySecrets(dst[i].Nested, field.Nested)
			if field.Items != nil && dst[i].Items != nil {
				copySecrets([]*ArgsField{dst[i].Items}, []*ArgsField{field.Items})
			}
		}
	}
	if source.Args != nil && owned.Args != nil {
		copySecrets(owned.Args.Fields, source.Args.Fields)
	}
	return owned, nil
}
