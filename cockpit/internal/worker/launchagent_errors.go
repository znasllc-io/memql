package worker

import "errors"

// ErrNoLaunchAgent is returned on platforms where the wizard's
// auto-install isn't supported. Defined on every build so callers
// can `errors.Is(err, ErrNoLaunchAgent)` without build tags.
var ErrNoLaunchAgent = errors.New("auto-start install is only available on macOS in this build")
