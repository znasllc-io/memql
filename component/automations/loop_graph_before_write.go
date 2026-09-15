package automations

import "fmt"

// invalidateBeforeWriteFields keeps mutation literals from proving a trigger
// impossible when a synchronous hook can change those fields before storage.
// Hook bodies emit no event of their own; their effects belong to the original
// write's production. Conditions and hook filters stay conservative here.
func (b *graphBuilder) invalidateBeforeWriteFields(p *production) {
	if p.kind == kindPublish || p.concept == "" {
		return
	}
	changed := map[string]string{}
	allFields := ""
	for _, a := range b.nodes {
		hook := a.BeforeWrite
		if hook == nil || hook.Concept != p.concept {
			continue
		}
		// Timing is about whether the row already exists, not the DSL verb alone:
		// an insert naming an id can update a previous version and matches either.
		if hook.On == "create" && p.kind == "update" {
			continue
		}
		if hook.On == "update" && p.kind == "insert" && p.row.FirstVersion == triTrue {
			continue
		}
		var walk func([]*Step)
		walk = func(steps []*Step) {
			for _, s := range steps {
				if s == nil {
					continue
				}
				if s.Type == StepTypeFieldWrite {
					if s.FieldWrite == nil || s.FieldWrite.Field == "" {
						allFields = a.Name
					} else {
						changed[s.FieldWrite.Field] = a.Name
					}
				}
				if s.Block != nil {
					walk(s.Block.Steps)
				}
				if s.ForEach != nil {
					walk(s.ForEach.Do)
				}
				if s.Parallel != nil {
					walk(s.Parallel.Branches)
				}
			}
		}
		walk(a.Steps)
	}
	if allFields != "" {
		for field := range p.row.Fields {
			changed[field] = allFields
		}
		for field := range p.row.Absent {
			changed[field] = allFields
		}
		p.row.OthersKnown = false
	}
	if len(changed) == 0 && allFields == "" {
		return
	}
	if p.row.Unknown == nil {
		p.row.Unknown = map[string]bool{}
	}
	for field := range changed {
		delete(p.row.Fields, field)
		delete(p.row.Absent, field)
		p.row.Unknown[field] = true
	}
	previous := p.row.explain
	p.row.explain = func(field string) string {
		if hook, ok := changed[field]; ok {
			return fmt.Sprintf("before-write automation %s may replace %s before the original write publishes its event", hook, field)
		}
		if allFields != "" {
			return fmt.Sprintf("before-write automation %s has a field assignment the graph cannot resolve", allFields)
		}
		if previous != nil {
			return previous(field)
		}
		return ""
	}
}
