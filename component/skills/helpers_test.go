package skills

import "time"

// timeoutAfterASecond backs the cycle test's guard. A cycle that does not
// terminate hangs the package rather than failing it, and `go test` reports
// that as a ten-minute panic with no useful name -- so the guard names it.
func timeoutAfterASecond() <-chan time.Time { return time.After(time.Second) }
