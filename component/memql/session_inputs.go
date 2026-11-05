package memql

// session_inputs.go -- the Library files a tool-using step hands its app
// session (the `session` door, app_session_provider.go).
//
// A step that runs whole in an app reads its material from the session's
// workspace, and the cockpit fills that workspace from the handover's Inputs
// (Library artifact ids it pulls before the app starts). Nothing between the
// step and the handover carries them -- the model seam takes a prompt, its data
// and tool names -- so the step puts them on the context, as it puts its acting
// agent there, and the session door reads them where it builds the handover.
//
// They ride only a handover that OFFERS TOOLS, which is the handover the door
// holds to the owned run and the acting agent before anything is opened: a
// person's files reach a machine only for a step that is theirs.

import (
	"context"
	"strings"
)

type sessionInputsKey struct{}

// WithSessionInputs returns a child context carrying the Library artifact ids a
// step's app session is handed, each once and in order. A list with none clears
// whatever an outer frame carried.
func WithSessionInputs(ctx context.Context, artifactIDs []string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ids := make([]string, 0, len(artifactIDs))
	seen := make(map[string]bool, len(artifactIDs))
	for _, id := range artifactIDs {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return context.WithValue(ctx, sessionInputsKey{}, ids)
}

// SessionInputsFromContext returns a copy of the artifact ids WithSessionInputs
// put on ctx, or nil when there are none.
func SessionInputsFromContext(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	ids, _ := ctx.Value(sessionInputsKey{}).([]string)
	if len(ids) == 0 {
		return nil
	}
	return append([]string(nil), ids...)
}
