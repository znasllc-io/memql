package memql

import "github.com/znasllc-io/memql/core/id"

// cacheIdEngine is the shared id engine for cache key generation.
// Using a package-level instance enables ID caching across calls for performance.
var cacheIdEngine = id.New()
