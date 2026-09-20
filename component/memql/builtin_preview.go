package memql

import (
	"context"
	"fmt"
)

type builtinPreviewContextKey struct{}

// WithBuiltinPreview keeps the preview restriction on nested engine calls,
// including a builtin invoked from inside a query or a logic.
func WithBuiltinPreview(ctx context.Context) context.Context {
	return context.WithValue(ctx, builtinPreviewContextKey{}, true)
}

// CheckBuiltinPreview admits executors whose implementations only inspect
// metadata or compute a value. A capability grant is authorization, not a
// guarantee that a handler has no side effects. Unknown and integration
// executors therefore require a classification before previews can run them.
func CheckBuiltinPreview(executor string) error {
	switch executor {
	case BuiltinExecutorConcepts, BuiltinExecutorMemqlDocs,
		BuiltinExecutorMemqlGrammar, BuiltinExecutorMemqlVocabulary,
		BuiltinExecutorValidate, BuiltinExecutorFunctions, BuiltinExecutorTools,
		BuiltinExecutorHelp, BuiltinExecutorShapeTemplates, BuiltinExecutorShapeHelp,
		BuiltinExecutorContentId, BuiltinExecutorPreviewInsert,
		BuiltinExecutorServiceVersion:
		return nil
	default:
		return fmt.Errorf("dry-run refused builtin executor %q: it is not classified as side-effect free", executor)
	}
}
