package pipelines

// Spec is the pipeline: block of memql-package.yaml (design record D3, D7).
//
// component/packages decodes the whole manifest with ONE strict decoder --
// unknown keys refused, one document, the formatVersion check -- and hands
// this package the typed block, which is why these types carry yaml tags but
// this module imports no YAML package. The json tags match the yaml names so
// the manifest round-trips through an analysis report unchanged.
//
// Decoding checks SHAPE only. Whether the block MEANS anything -- a stage that
// needs a later stage, a need outside the closed set, a bucket nobody declared
// -- is Validate's question, asked when a run compiles. Keeping the two apart
// is deliberate: a typo in a pipeline must fail that pipeline's run with a
// typed refusal (D9), not refuse every deploy of the source it sits in.
type Spec struct {
	// Workflow selects an installed MemQL automation over the compiled steps.
	// Empty selects the sealed core workflow; the manifest never supplies code.
	Workflow string `yaml:"workflow,omitempty" json:"workflow,omitempty"`
	// Platform optionally pins container OS/architecture across the pipeline.
	Platform string `yaml:"platform,omitempty" json:"platform,omitempty"`
	// Image is the toolchain image every command step runs in, by digest.
	Image string `yaml:"image,omitempty" json:"image,omitempty"`
	// Services are the sidecars a step may name, by name.
	Services map[string]Service `yaml:"services,omitempty" json:"services,omitempty"`
	// Caches names the default caches a step inherits (go, npm).
	Caches []string    `yaml:"caches,omitempty" json:"caches,omitempty"`
	Select *Select     `yaml:"select,omitempty" json:"select,omitempty"`
	Stages []StageSpec `yaml:"stages" json:"stages"`
}

// Select declares package-selection facts and path buckets (D8). Sealed core
// DSL chooses coverage and applicability; the runtime reads the Go import
// graph and matches paths as mechanics, so no repository ships a selection
// script.
type Select struct {
	// Go is the Go selection strategy; SelectImportGraph is the one there is.
	Go string `yaml:"go,omitempty" json:"go,omitempty"`
	// DBGated are the directory trees whose packages need a database:
	// `only: db-gated` keeps them, `only: not-db-gated` drops them.
	DBGated []string `yaml:"dbGated,omitempty" json:"dbGated,omitempty"`
	// Full are path globs whose change selects everything, on top of the
	// built-in ones (go.mod, go.sum, go.work, go.work.sum, the manifest).
	Full []string `yaml:"full,omitempty" json:"full,omitempty"`
	// Buckets are named path globs a non-Go step gates on with
	// `when: { bucket: <name> }`.
	Buckets map[string][]string `yaml:"buckets,omitempty" json:"buckets,omitempty"`
}

// StageSpec is one stage. Stages run one after another in the order written;
// the steps inside a stage run at once (D7).
type StageSpec struct {
	// RunAfterFailure still runs this stage when an earlier stage failed. It
	// does not erase failures or bypass cancellation, authorization or leases.
	RunAfterFailure bool   `yaml:"runAfterFailure,omitempty" json:"runAfterFailure,omitempty"`
	Name            string `yaml:"name" json:"name"`
	// Needs names earlier stages this one depends on.
	Needs []string `yaml:"needs,omitempty" json:"needs,omitempty"`
	// On restricts the stage to events (pull_request, merge_group, push,
	// release) or modes (affected, full). Empty means every run.
	On []string `yaml:"on,omitempty" json:"on,omitempty"`
	// Channel makes this a notify stage: it names a v1:pipelines:channel and
	// carries no steps (D16).
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty"`
	// Links are the notify stage's own links, shown in its message beside the
	// run page: the docs a release's announcement points to. Only a notify
	// stage carries them.
	Links []Link     `yaml:"links,omitempty" json:"links,omitempty"`
	Steps []StepSpec `yaml:"steps,omitempty" json:"steps,omitempty"`
}

// StepSpec is one step: a shell command in the image's working copy, with
// the same contract as a deployable's build.command. There is no step
// language.
type StepSpec struct {
	// CPUMilli requests a bounded CPU reservation for the cluster command.
	// Zero uses the operator's namespace default; 1000 means one CPU.
	CPUMilli int `yaml:"cpuMilli,omitempty" json:"cpuMilli,omitempty"`

	// ImageBuild asks the cluster's fixed rootless builder for an OCI archive.
	// It cannot carry a shell command, services, caches or publication secrets.
	ImageBuild *ImageBuild `yaml:"imageBuild,omitempty" json:"imageBuild,omitempty"`
	// MemoryMiB requests a bounded memory reservation for the cluster command.
	// Zero uses the operator's namespace default; services retain their defaults.
	MemoryMiB int `yaml:"memoryMiB,omitempty" json:"memoryMiB,omitempty"`
	// Image overrides the pipeline's container image for this step only.
	Image string `yaml:"image,omitempty" json:"image,omitempty"`
	// Placement selects cluster or fleet; omitted follows native execution or host needs.
	Placement string `yaml:"placement,omitempty" json:"placement,omitempty"`
	// Execution defaults to container. Native explicitly requests a host toolchain.
	Execution string `yaml:"execution,omitempty" json:"execution,omitempty"`
	// Platform overrides the pipeline platform; fleet steps must name one.
	Platform string `yaml:"platform,omitempty" json:"platform,omitempty"`
	Name     string `yaml:"name" json:"name"`
	Run      string `yaml:"run,omitempty" json:"run,omitempty"`
	// Packages selects Go packages into MEMQL_PACKAGES: PackagesAffected or
	// PackagesAll. Empty means the step selects none.
	Packages string `yaml:"packages,omitempty" json:"packages,omitempty"`
	// Only narrows the packages to the db-gated trees, or to everything else.
	Only string `yaml:"only,omitempty" json:"only,omitempty"`
	// Shards splits the packages over up to this many steps, by the timing
	// table.
	Shards int   `yaml:"shards,omitempty" json:"shards,omitempty"`
	When   *When `yaml:"when,omitempty" json:"when,omitempty"`
	// Needs is the environment hint: a need set true routes the step to a
	// fleet machine that offers it. A map rather than a list because D7
	// writes it `needs: { docker: true }`; a stage's needs is a different
	// thing (the stages it waits for).
	Needs map[string]bool `yaml:"needs,omitempty" json:"needs,omitempty"`
	// Services name entries of the pipeline's services this step needs.
	Services []string `yaml:"services,omitempty" json:"services,omitempty"`
	// Caches overrides the pipeline defaults. An explicit empty list disables
	// them, allowing native build steps beside cached container test steps.
	// A pointer preserves that empty override through JSON/YAML round trips.
	Caches *[]string `yaml:"caches,omitempty" json:"caches,omitempty"`
	// Timeout is a Go duration ("20m"); empty means DefaultStepTimeout.
	Timeout   string   `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Artifacts []string `yaml:"artifacts,omitempty" json:"artifacts,omitempty"`
	// Secrets are globalSecret NAMES the step's environment receives,
	// resolved only when the pipeline's owner allowed them.
	Secrets []string `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	// ImagePullSecret names an owner-allowed Docker config JSON secret. Only
	// the cluster image puller receives it; it is never a command environment.
	ImagePullSecret string `yaml:"imagePullSecret,omitempty" json:"imagePullSecret,omitempty"`
}

// When gates a step on a path bucket.
type When struct {
	Bucket string `yaml:"bucket" json:"bucket"`
}

// The closed vocabularies a spec draws on.
const (
	SelectImportGraph = "import-graph"

	PackagesAffected = "affected"
	PackagesAll      = "all"

	OnlyDBGated    = "db-gated"
	OnlyNotDBGated = "not-db-gated"
)

// Link is a labelled URL in a notification: one a manifest's notify stage
// declares (Docs), or one of the run's Library files.
type Link struct {
	Label string `json:"label" yaml:"label"`
	URL   string `json:"url" yaml:"url"`
}
