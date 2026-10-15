package memql

import (
	"context"

	"github.com/znasllc-io/memql/component/auth"
)

// PersonalDataPlaneEnforced admits only complete calls to the core personal
// mutations below. Creates stamp the verified actor; updates retain the engine's
// owner write guard. Raw writes, arbitrary namespace members, internal transcript
// writes and mixed expressions never acquire this exception.
func (e *MemQLEngine) PersonalDataPlaneEnforced(ctx context.Context, query string) bool {
	if e == nil || !e.GlobalDataCapable(ctx, auth.VerbRead) {
		return false
	}
	plan, err := e.parseWithFunctionsAmbient(query, e.functions, nil, false, auth.OriginFromContext(ctx), buildAmbientEnvelope(ctx, e), StagedScope{})
	if err != nil || plan == nil || plan.MutationCall == nil || plan.Root != nil || plan.LogicCall != nil || len(plan.Mutations) > 0 || len(plan.Relationships) > 0 || plan.Refine != nil || plan.IncludeBundle || plan.Depth != nil || !organizationPlainValues(plan.MutationCall.Args) {
		return false
	}
	fn, ok := e.functions.Lookup(plan.MutationCall.Name)
	return ok && auth.CanManageOwnAppData(ctx, personalMutationApp(fn))
}

func personalMutationApp(fn *Function) string {
	if fn == nil || fn.FunctionKind != "mutation" || fn.MutationTemplate == nil || fn.ServerOnly {
		return ""
	}
	concept, namespace, app := "", "library", "files"
	switch fn.Name {
	case "archiveArtifact", "restoreArtifact", "moveArtifactToFolder", "setArtifactAccounts":
		concept = "v1:library:artifact"
	case "archiveLibraryFile", "restoreLibraryFile":
		concept = "v1:library:file"
	case "createLibraryFolder", "renameLibraryFolder", "moveLibraryFolder", "archiveLibraryFolder", "restoreLibraryFolder", "deleteLibraryFolder":
		concept = "v1:library:folder"
	case "createLibraryWatchedFolder", "updateLibraryWatchedFolder", "setLibraryWatchedFolderStatus", "archiveLibraryWatchedFolder", "restoreLibraryWatchedFolder":
		concept = "v1:library:watchedFolder"
	case "createAskConversation":
		concept, namespace, app = "v1:os:askConversation", "os", "ask"
	default:
		return ""
	}
	if ConstructNamespaceForOrigin(fn.Origin) != namespace || fn.MutationTemplate.Concept != concept {
		return ""
	}
	return app
}
