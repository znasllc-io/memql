package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/znasllc-io/memql/component/packages"
)

// dsl-fetch is the D5 fetcher mode (epic memql#4794): the init-container half
// of package DSL delivery.
//
// It reads the active-set pointer out of the cluster's own object storage and
// copies each named content-addressed tree into MEMQL_DSL_PATH, before the
// node boots. The node then mounts that directory exactly as it does today --
// dsl.MountRuntimeDomainsFromEnv is unchanged, and there is no hot-mount
// anywhere: the ROLL is the mechanism.
//
// It lives on the engine binary rather than in a separate image for one
// reason: the credentials, the container name and the blob client are already
// here, correctly configured, from the same secret every node envFroms. A
// second image would need its own copy of all three and could disagree with
// the engine about where the cluster's storage is.
//
// EXIT CODES ARE THE CONTRACT, because an init container's exit code is what
// decides whether the pod starts:
//
//	0  the trees are in place, OR there is no pointer at all
//	1  a pointer exists and could not be honoured, OR this container was
//	   never told where to look
//
// The asymmetry is deliberate. A cluster that has never deployed a package has
// no pointer, and that is the ordinary state -- refusing to boot over it would
// mean this component could never be applied before the first deploy. A
// pointer that EXISTS and cannot be read is the opposite: booting anyway
// brings the node up with silently-missing product DSL, which presents as a
// healthy cluster answering "function not found" to every call it used to
// serve.
//
// AN UNCONFIGURED CONTAINER IS THE SECOND CASE, NOT THE FIRST (memql#4933).
// It reads like the first -- nothing was found, nothing is wrong -- and for a
// long-running engine process that reading is right. Here it is not: this
// process exists only to fetch, so a fetcher that cannot say where to look has
// not found an empty cluster, it has failed to look at all. It exited 0 on
// every instance that applied the component as shipped, because the container
// name is not in the Secret the component envFroms, and each of those nodes
// booted healthy and served none of its packages' DSL.
func runDslFetchSubcommand(args []string) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			fmt.Fprintln(os.Stderr, "usage: memql dsl-fetch")
			fmt.Fprintln(os.Stderr)
			fmt.Fprintln(os.Stderr, "Copies every package DSL tree named by blob://"+packages.ActiveSetPath)
			fmt.Fprintln(os.Stderr, "into $MEMQL_DSL_PATH. Run as an init container before the node boots.")
			return 0
		}
	}

	root := os.Getenv("MEMQL_DSL_PATH")
	if root == "" {
		fmt.Fprintln(os.Stderr, "dsl-fetch: MEMQL_DSL_PATH is not set; there is nowhere to put the trees")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := packages.FetchActiveSet(ctx, root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dsl-fetch: %v\n", err)
		return 1
	}
	if res.PointerAbsent {
		fmt.Fprintln(os.Stderr, "dsl-fetch: no package pointer in object storage; nothing to fetch")
		return 0
	}
	fmt.Fprintf(os.Stderr, "dsl-fetch: copied %d domain(s), %d file(s) into %s\n",
		len(res.Domains), res.Files, root)
	return 0
}
