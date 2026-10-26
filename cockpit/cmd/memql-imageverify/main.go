// Command memql-imageverify is the fixed operator-installed image verification
// service. It has no general command runner or account/credential operations.
package main

import (
	"github.com/znasllc-io/memql/cockpit/internal/worker/imageverify"
	"os"
)

func main() {
	if handled, code := imageverify.InternalMain(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(2)
}
