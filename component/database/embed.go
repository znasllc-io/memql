package database

import (
	"io/fs"
	"testing/fstest"
)

// Concepts was the active concept-schema FS for the legacy
// version-directory walk, which has been deleted (concepts now load
// exclusively via LoadUnifiedConcepts in component/memql, reading
// from dsl/<domain>/concepts.memql). The variable stays as a typed
// empty FS only because the concept seeder still walks it for
// legacy seed.memql sidecars; that walk no-ops over the empty FS.
var Concepts fs.FS = fstest.MapFS{}
