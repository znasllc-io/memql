package main

// `memql promote-site` -- the operator path to artifact promotion
// (epic memql#3748 / memql#3768).
//
// WHY A SUBCOMMAND ALONGSIDE THE gRPC MESSAGE. PromoteSiteMsg is the surface a
// client speaks (the portal's Deployments view, memql#3733, is the one being
// built for it). This is the surface an OPERATOR speaks, and it exists for the
// same reason `pat mint` and `enrolment-token mint` do: somebody who can exec
// inside a pod already holds the cluster's secrets, so access to the process is
// the authorization, and it works when no client is to hand and when nobody has
// yet been granted anything.
//
// It runs the promote directly rather than dialling the bff, which is also what
// makes it usable on a cluster whose bff is the thing being fixed.
//
// UNTAGGED, like `pat`: the promote needs the database and nothing else --
// specifically no identity issuer and no transport -- so it runs wherever an
// operator can exec the binary.
//
// Everything the components log goes to stderr, and the RESULT goes to stdout,
// so a caller's `$(kubectl exec ...)` capture holds the bundle reference and
// only the bundle reference. Same discipline as `pat mint`.

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/znasllc-io/memql/app"
	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/core/common"
)

func runPromoteSiteSubcommand(args []string) int {
	fs := flag.NewFlagSet("promote-site", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		siteId    = fs.String("site-id", "", "the site row id, which is the SAME in both environments")
		from      = fs.String("from", "", "source environment to read the bundle reference from")
		to        = fs.String("to", "", "target environment to write it to")
		bundleRef = fs.String("bundle-ref", "", "pin this reference instead of reading one (the ROLLBACK form)")
		hostname  = fs.String("hostname", "", "required only when the site does not exist in the target yet, in which case it is CREATED")
		prefix    = fs.String("schema-prefix", edge.DefaultEnvironmentSchemaPrefix, "prefix an environment's schema name is composed from")
	)
	if err := fs.Parse(args); err != nil {
		printPromoteSiteUsage()
		return 2
	}
	if strings.TrimSpace(*siteId) == "" || strings.TrimSpace(*to) == "" {
		fmt.Fprintln(os.Stderr, "promote-site: --site-id and --to are required")
		printPromoteSiteUsage()
		return 2
	}
	if strings.TrimSpace(*from) == "" && strings.TrimSpace(*bundleRef) == "" {
		fmt.Fprintln(os.Stderr, "promote-site: give --from to promote from an environment, or --bundle-ref to pin a value (the rollback form)")
		printPromoteSiteUsage()
		return 2
	}

	deps, application, _, code := bootstrapPromoteSiteEngine("promote-site")
	if code != 0 {
		return code
	}
	defer stopPATDependencies(deps)

	db := application.BunDB()
	if db == nil {
		fmt.Fprintln(os.Stderr, "promote-site: no database handle after bootstrap")
		return 1
	}

	target, err := edge.SchemaFor(*prefix, *to)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promote-site: target %v\n", err)
		return 2
	}

	ctx := context.Background()
	promoter := edge.NewPromoter(db)

	var res edge.PromoteResult
	if strings.TrimSpace(*bundleRef) != "" {
		res, err = promoter.SetBundleRef(ctx, target, *siteId, *bundleRef)
	} else {
		var source string
		if source, err = edge.SchemaFor(*prefix, *from); err != nil {
			fmt.Fprintf(os.Stderr, "promote-site: source %v\n", err)
			return 2
		}
		res, err = promoter.Promote(ctx, source, target, *siteId, *hostname)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "promote-site: %v\n", err)
		return 1
	}

	switch {
	case res.NoOp:
		fmt.Fprintf(os.Stderr, "promote-site: %s already serves %s in %s -- no new version written\n",
			res.SiteID, res.BundleRef, target)
	case res.Created:
		fmt.Fprintf(os.Stderr, "promote-site: created %s in %s at %s\n", res.SiteID, target, res.BundleRef)
	default:
		fmt.Fprintf(os.Stderr, "promote-site: %s in %s moved %s -> %s (roll back with --bundle-ref=%s)\n",
			res.SiteID, target, res.PreviousBundleRef, res.BundleRef, res.PreviousBundleRef)
	}
	// stdout carries the value and nothing else.
	fmt.Println(res.BundleRef)
	return 0
}

func printPromoteSiteUsage() {
	fmt.Fprintln(os.Stderr, `usage: memql promote-site --site-id <id> --to <environment> [--from <environment> | --bundle-ref <ref>]

Move a site's bundle reference from one environment to another, in one
transaction. The bundle itself is immutable and versioned by prefix in shared
object storage, so ONLY the reference moves and no upload occurs.

Rollback is the same command with the previous value:
  memql promote-site --site-id <id> --to prod --bundle-ref <previous>

Promoting a site the target does not have yet CREATES it, in which case
--hostname is required: a production hostname cannot be derived from a staging
one, because staging lives under the cluster's domain and production lives
wherever the customer's DNS points.

Requires the database environment (MEMQL_DATABASE_DSN). Typically run via
  kubectl exec -n <namespace> deploy/bff -- /app/memql promote-site ...
The bundle reference written is printed to stdout; everything else is stderr.`)
}

// bootstrapPromoteSiteEngine is bootstrapPATEngine's shape, returning the App
// as well because the promote needs the database HANDLE rather than the engine:
// it is one transaction naming two schemas, which no engine call can express
// (the engine reaches exactly one schema -- the connection's search path decides
// which, and that is the environment boundary itself).
//
// It reuses depsUpToEngine for the same reason pat mint does: starting the
// dependencies that follow the engine would fatal-validate on config this
// subcommand does not need and abort the promote.
func bootstrapPromoteSiteEngine(prefix string) ([]common.Dependency, *app.App, *slog.Logger, int) {
	if err := applySubcommandEnv(prefix); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, nil, nil, 1
	}

	logger := mustCreateCLILogger()
	application := app.Build(logger, resolveVersionFn(), app.Overrides{})

	selected, ok := depsUpToEngine(application.Dependencies)
	if !ok {
		fmt.Fprintf(os.Stderr, "%s: engine dependency not present in this build\n", prefix)
		return nil, nil, logger, 1
	}
	deps := make([]common.Dependency, 0, len(selected))
	for _, d := range selected {
		d.Start(context.Background())
		deps = append(deps, d)
	}
	return deps, application, logger, 0
}
