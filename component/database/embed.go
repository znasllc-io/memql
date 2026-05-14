package database

import (
	"io/fs"
	"testing/fstest"
)

// Concepts was the active concept-schema FS. Pass 3 of the DSL
// restructure migration retired the legacy walk over
// dsl/v1/concepts/; concepts now load via LoadUnifiedConcepts in
// component/memql (reading from dsl/<domain>/<entity>.memql) and
// merge into the global registry. This variable stays as a typed
// empty FS so any code path still referencing it returns no
// content (the legacy walk effectively no-ops).
var Concepts fs.FS = fstest.MapFS{}
