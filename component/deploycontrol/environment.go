// environment.go maps a console environment onto the surface it actually
// occupies in the cluster -- namespace, ArgoCD Application, overlay directory,
// and (for production) the environment its digests are promoted FROM.
//
// # What was repointed, and why it was wrong before
//
// Every address in this package used to be the literal `memql`: `kubectl -n
// argocd get app memql`, `kubectl -n memql get rollout`, `kubectl argo rollouts
// ... -n memql`. That was correct while `memql` was the ONE Application and the
// ONE namespace in the cloud estate -- the surface the product pack's overlay
// owned. It stopped being correct when epic memql#3748 / memql#3766 split the
// cloud into two namespaces (`memql-prod`, `memql-staging`) reconciled by two
// Applications (`memql-prod`, `memql-staging`) from one base.
//
// After that split the constant does not fail loudly, which is the reason this
// file exists rather than a search-and-replace. `kubectl -n memql get rollout`
// against a cluster with no `memql` namespace returns an EMPTY LIST and exit 0,
// so `GetDeploymentStatus(env="prod")` answered "no rollouts, gate unknown" for
// a production environment that was mid-rollout. A status surface that reports
// nothing is indistinguishable from a healthy one, and an operator reading it
// has no way to tell. #3766's author left this deliberately -- repointing it
// there would have broken the product flow for no gain while the product pack
// still owned `memql` -- and named this issue as where it lands.
//
// # The namespace and the Application share a name, and that is a coincidence
//
// `memql-prod` is both the namespace the workloads run in and the name of the
// Application that reconciles them. They are stated as two fields anyway. They
// answer different questions -- "where do the pods live" versus "what does an
// operator sync" -- and a future estate that renames one has no reason to
// rename the other. Deriving one from the other would make that rename a code
// change instead of a value change, which is the mistake this epic exists to
// stop repeating.
//
// # Local is not here, on purpose
//
// The local k3d cluster is ONE environment in the plain `memql` namespace and
// is deliberately not a deploy target: `validateDeploymentProvider` (driver.go)
// refuses the retired `docker-local` provider with a pointer to `make up`, and
// the design keeps that refusal explicitly ("`deploycontrol` still refuses
// local. Unchanged", virtual-environments design §5). So `SurfaceFor("local")`
// answers false like any other unknown value; there is no local row to find.
//
// # What a PROMOTE moves, and the one thing everybody expects it to move
//
// PromoteFrom is what makes engine promotion expressible as a value: production
// is promoted from staging, and staging is promoted from nowhere (its digests
// are cut on the GitHub build server and pinned into its overlay directly).
// Promotion is therefore "pin the production overlay's image digests to the
// ones staging is running" -- one edit, in one tree, reconciled by one ArgoCD,
// so a promote is ONE COMMIT rather than a copy between two estates.
//
// TRAINED CONSTRUCTS DO NOT CROSS, and this is the assumption most likely to be
// mistaken for a bug. A promoted construct (memql#3746) is a
// `v1:authoring:construct` ROW, and rows are separated by the connection: the
// two namespaces are put on different Postgres schema search paths
// (`memql_prod, public` / `memql_staging, public`, component/database/
// search_path.go), so a construct promoted in staging is a row in
// `memql_staging` and is genuinely ABSENT from production. An engine promote
// moves IMAGE DIGESTS in a git-tracked file. It performs no database write at
// all, so there is nothing in it that could carry a row across even in
// principle. The same is true of a product's DSL bundle in the other direction:
// the bundle is a data-only image mounted at MEMQL_DSL_PATH, so promoting it IS
// pinning a digest -- and it still carries no trained state, because trained
// state was never in the image.
//
// Training production is therefore an explicit act against production, never a
// side effect of promoting a bundle. Asserted rather than asserted-in-prose by
// TestEnginePromoteCarriesNoGraphState (promote_test.go) and, at the row level
// against a real database, TestPromotedConstructsDoNotCrossEnvironments
// (component/database/search_path_db_test.go).
package deploycontrol

import "path/filepath"

// EnvironmentSurface is everything the deploy console needs to know about WHERE
// one environment lives. Values only -- no behaviour branches on which one of
// these a caller holds, which is the property the parity standard is protecting
// (CLAUDE.md, "no `if env == \"...\"` in engine code").
type EnvironmentSurface struct {
	// ConsoleEnv is the console spelling: "staging" | "prod". This is what
	// ConsoleEnvFor (driver.go) normalizes a deployment record's environment
	// enum (staging | production | development) down to.
	ConsoleEnv string
	// Namespace is the Kubernetes namespace this environment's workloads run
	// in -- what `kubectl -n <ns>` must address.
	Namespace string
	// ArgoApp is the ArgoCD Application that reconciles it, in the `argocd`
	// namespace. Same string as Namespace today; see the header for why it is
	// not derived from it.
	ArgoApp string
	// OverlayDir is the kustomize overlay directory, relative to the repo root,
	// that is this environment's single image authority.
	OverlayDir string
	// PromoteFrom is the console env whose image digests this one is promoted
	// from, or "" when there is none. Only production has one; staging's
	// digests are cut on the build server and pinned into its overlay directly.
	PromoteFrom string
}

// environmentSurfaces is the closed set of deploy targets, keyed by console env.
//
// A map literal rather than a switch so the whole set reads as data, and listed
// rather than derived from the overlay directory for the reason the render gate
// gives (deploy/k8s/overlays/environments_test.go): the failure worth catching
// is a THIRD environment arriving and nobody deciding what it promotes from,
// which discovery would wave through.
var environmentSurfaces = map[string]EnvironmentSurface{
	"staging": {
		ConsoleEnv: "staging",
		Namespace:  "memql-staging",
		ArgoApp:    "memql-staging",
		OverlayDir: filepath.Join("deploy", "k8s", "overlays", "staging"),
		// Nothing. Staging is where a version arrives from the build server;
		// it is the SOURCE of a promote, never the target of one.
		PromoteFrom: "",
	},
	"prod": {
		ConsoleEnv:  "prod",
		Namespace:   "memql-prod",
		ArgoApp:     "memql-prod",
		OverlayDir:  filepath.Join("deploy", "k8s", "overlays", "prod"),
		PromoteFrom: "staging",
	},
}

// SurfaceFor resolves a console env ("staging" | "prod") to its surface.
// Reports false for anything else -- including "local" and "development", which
// are not deploy targets (see the header).
func SurfaceFor(consoleEnv string) (EnvironmentSurface, bool) {
	s, ok := environmentSurfaces[consoleEnv]
	return s, ok
}
