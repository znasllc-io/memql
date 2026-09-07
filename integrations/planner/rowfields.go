package planner

// rowfields.go -- the small row-reading helpers the surviving files share.
//
// They lived in train_specialist_dispatch.go and
// embed_domain_items_dispatch.go, which memql#5051 deletes -- and
// responsibility_intake.go and the authoring transcript, which both STAY, both
// use them. A helper whose only home is a file being deleted is the shape that
// turns a clean deletion into a broken build, so they get a home of their own.

// mapField pulls a nested object field off a row map, tolerating both
// map[string]any and the absence of the field.
func mapField(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

// intFromAny reads an int-valued field, tolerating the float64 a decoded JSON
// number arrives as.
//
// It deliberately does NOT reach for core/num: this is a small ordering key
// read from a row the engine wrote, not a payload narrowing that has to
// declare an out-of-range answer.
func intFromAny(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}
