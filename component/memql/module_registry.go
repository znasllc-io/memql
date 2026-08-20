package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/envregistry"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// module_registry.go assembles the MODULE INVENTORY -- the runtime answer to
// "what does this instance run" -- per the module-registry design
// (docs/superpowers/specs/2026-08-20-module-registry-design.md, sections
// 2-3). "Module" is the collective term over the locked extension kinds
// plus the node-type deployment units; it is deliberately not a fourth
// extension kind, and nothing here introduces a new registry to maintain.
// Every row is ASSEMBLED at request time from sources that already exist:
//
//   component  -> the envregistry manifest's component: vocabulary
//   integration-> RegisteredPlugins() + the live IntegrationRegistry
//   pack       -> dsl.ListPackDomains() + pack bindings + v1:platform:packState
//   node-type  -> v1:cluster:nodeType / :node / :deploymentNodeSpec graph rows
//
// PER-NODE VS CLUSTER HONESTY (design section 2.3): rows built from the
// answering binary's own registries and env carry ScopeNode; rows whose
// truth lives in the shared graph carry ScopeCluster. There is no
// cross-node fan-out in v1 -- a bff cannot enumerate what a cognition
// binary wired up, and the rows say so rather than pretending.
//
// SECRETS NEVER LEAVE THE ENGINE, in any form (design section 3): a
// manifest entry from the secrets list reports set/unset and nothing else.
// No masked prefix, no last-four, no reveal path. TestModuleEnvSurface
// NeverCarriesASecretValue asserts this structurally.

// Module kinds. Closed set; the wire surface carries these strings.
const (
	ModuleKindComponent   = "component"
	ModuleKindIntegration = "integration"
	ModuleKindPack        = "pack"
	ModuleKindNodeType    = "node-type"
)

// Module scopes (which truth tier a row's state is).
const (
	ModuleScopeNode    = "node"
	ModuleScopeCluster = "cluster"
)

// ModuleRow is one module as the inventory reports it.
type ModuleRow struct {
	Kind          string
	Name          string
	Description   string
	State         string
	StateDetail   string
	Scope         string
	EnvComponents []string
	FqnPrefixes   []string
	CodeReference string
}

// ModuleEnvVar is one manifest-declared environment variable on a module's
// detail surface. Secret entries carry NO value field content, ever.
type ModuleEnvVar struct {
	Name         string
	Description  string
	Secret       bool
	Scope        string
	RequiredFor  []string
	Set          bool
	Value        string
	DefaultValue string
}

// ModuleDetail is the detail answer: the row plus its env surface.
type ModuleDetail struct {
	Row     ModuleRow
	EnvVars []ModuleEnvVar
}

// moduleManifest caches the envregistry manifest for the process. The
// manifest is an embedded snapshot -- immutable per binary -- so one load
// serves every inventory call.
var (
	moduleManifestOnce sync.Once
	moduleManifestVal  *envregistry.Manifest
	moduleManifestErr  error
)

func moduleManifest() (*envregistry.Manifest, error) {
	moduleManifestOnce.Do(func() {
		moduleManifestVal, moduleManifestErr = envregistry.LoadManifest("")
	})
	return moduleManifestVal, moduleManifestErr
}

// SetDisabledPackDomains hands the boot-time pack disablement projection to
// the DSL layer (which owns the loaders' view of it -- see
// dsl/pack_enablement.go). App phase 3 calls this after reading
// v1:platform:packState and before Init runs a single loader.
func (e *MemQLEngine) SetDisabledPackDomains(domains []string) {
	memqldsl.SetDisabledPackDomains(domains)
}

// ---------------------------------------------------------------------------
// Authorization (design section 5)
// ---------------------------------------------------------------------------

// ModuleRefusal is a policy refusal the gRPC handler translates into the
// result payload's error fields. Codes are gRPC status ints named the way
// adminops names them, so this package carries no transport dependency.
type ModuleRefusal struct {
	Code    int
	Message string
}

const (
	moduleCodePermissionDenied = 7
	moduleCodeUnauthenticated  = 16
)

// moduleActor resolves the caller the same way adminops does: the
// stream-resolved AccessContext first, the legacy user context as
// fallback. nil AccessContext + no user context = unauthenticated.
func moduleActor(ctx context.Context) (auth.UserContext, bool) {
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		return auth.UserContext{ID: ac.UserId, Email: ac.PrimaryEmail, Role: ac.Role}, true
	}
	if uc, ok := auth.UserFromContext(ctx); ok {
		return uc, true
	}
	return auth.UserContext{}, false
}

// AuthorizeModuleRead gates the inventory's list + detail reads:
// owner or admin. Below that the surface effectively does not exist
// (the portal hides it entirely); the refusal rides the result payload,
// never a stream-fatal handler error. Reads are not audited, matching
// deploycontrol's read tier.
func AuthorizeModuleRead(ctx context.Context) *ModuleRefusal {
	actor, ok := moduleActor(ctx)
	if !ok {
		return &ModuleRefusal{Code: moduleCodeUnauthenticated,
			Message: "module inventory: no authenticated caller on this connection"}
	}
	if !auth.AtLeastAdmin(actor) {
		return &ModuleRefusal{Code: moduleCodePermissionDenied,
			Message: fmt.Sprintf("module inventory: reading modules requires the owner or admin role (you hold %q)", actor.Role)}
	}
	return nil
}

// AuthorizeSetPackEnabled gates the one write: owner only. The caller
// (component/grpc's handler) emits exactly one audit event per call,
// including refusals, with the verdict this returns; the DSL mutation's
// clusterOwner tier is the independent second layer under both.
func AuthorizeSetPackEnabled(ctx context.Context) (auth.UserContext, *ModuleRefusal) {
	actor, ok := moduleActor(ctx)
	if !ok {
		return actor, &ModuleRefusal{Code: moduleCodeUnauthenticated,
			Message: "set pack enabled: no authenticated caller on this connection"}
	}
	if !auth.IsOwner(actor) {
		return actor, &ModuleRefusal{Code: moduleCodePermissionDenied,
			Message: fmt.Sprintf("set pack enabled: flipping a pack requires the owner role (you hold %q)", actor.Role)}
	}
	return actor, nil
}

// ---------------------------------------------------------------------------
// Assembly
// ---------------------------------------------------------------------------

// ListModules assembles the full inventory. The caller has already passed
// AuthorizeModuleRead; this function does policy-free assembly.
func (e *MemQLEngine) ListModules(ctx context.Context) ([]ModuleRow, error) {
	manifest, err := moduleManifest()
	if err != nil {
		return nil, fmt.Errorf("module inventory: manifest load: %w", err)
	}

	var rows []ModuleRow
	rows = append(rows, componentModuleRows(manifest)...)

	packStates, err := e.packStatesForInventory(ctx)
	if err != nil {
		return nil, err
	}
	packRows, packBound := packModuleRows(packStates)
	rows = append(rows, packRows...)
	rows = append(rows, e.integrationModuleRows(manifest, packBound)...)

	nodeTypeRows, err := e.nodeTypeModuleRows(ctx, manifest)
	if err != nil {
		return nil, err
	}
	rows = append(rows, nodeTypeRows...)

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Kind != rows[j].Kind {
			return rows[i].Kind < rows[j].Kind
		}
		return rows[i].Name < rows[j].Name
	})
	return rows, nil
}

// ModuleDetail returns one module's row plus its env-var surface. Unknown
// (kind, name) returns nil detail, no error -- the handler translates that
// into a not-found result code.
func (e *MemQLEngine) ModuleDetail(ctx context.Context, kind, name string) (*ModuleDetail, error) {
	rows, err := e.ListModules(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Kind == kind && row.Name == name {
			manifest, mErr := moduleManifest()
			if mErr != nil {
				return nil, fmt.Errorf("module inventory: manifest load: %w", mErr)
			}
			return &ModuleDetail{Row: row, EnvVars: moduleEnvSurface(manifest, row.EnvComponents)}, nil
		}
	}
	return nil, nil
}

// PackStateSnapshot exposes the live v1:platform:packState read to the
// wire layer (the SetPackEnabled handler reports prior state honestly).
// Same reader boot uses, same interpretation.
func (e *MemQLEngine) PackStateSnapshot(ctx context.Context) (map[string]PackStateRow, error) {
	return e.packStatesForInventory(ctx)
}

// packStatesForInventory reads the live v1:platform:packState rows when a
// database is available. A pre-database engine (unit tests over pure
// assembly) reports packs with no persisted state, which reads as
// all-enabled -- exactly what absence of rows means.
func (e *MemQLEngine) packStatesForInventory(ctx context.Context) (map[string]PackStateRow, error) {
	db := e.database()
	if db == nil {
		return map[string]PackStateRow{}, nil
	}
	states, err := ReadPackStates(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("module inventory: %w", err)
	}
	return states, nil
}

// componentModuleRows enumerates the manifest's component vocabulary. The
// design picked this granularity deliberately: it is a real, maintained,
// closed set that already joins to the env-var surface, which is what the
// detail page exists to show. Components are not switchable and the
// inventory does not pretend otherwise.
func componentModuleRows(manifest *envregistry.Manifest) []ModuleRow {
	seen := map[string]struct{}{}
	var rows []ModuleRow
	for _, entry := range manifest.AllEntries() {
		comp := strings.TrimSpace(entry.Component)
		if comp == "" {
			continue
		}
		if _, dup := seen[comp]; dup {
			continue
		}
		seen[comp] = struct{}{}
		rows = append(rows, ModuleRow{
			Kind:          ModuleKindComponent,
			Name:          comp,
			Description:   "Engine subsystem (envregistry manifest component).",
			State:         "built_in",
			StateDetail:   "components are engine internals; not switchable",
			Scope:         ModuleScopeNode,
			EnvComponents: []string{comp},
		})
	}
	return rows
}

// packModuleRows builds one row per registered non-embedded pack domain.
// Returns the rows plus the set of plugin names folded under pack rows, so
// the integration enumeration can skip them.
func packModuleRows(states map[string]PackStateRow) ([]ModuleRow, map[string]struct{}) {
	bound := map[string]struct{}{}
	var rows []ModuleRow
	for _, domain := range memqldsl.ListPackDomains() {
		if domain.Origin == "embedded" {
			continue
		}
		name := domain.Name

		// Cluster-scope desired state from the graph; node-scope boot
		// outcome from the loaders' set. The two can disagree between a
		// flip and this node's restart, and the row says so.
		enabled := true
		detailParts := []string{}
		if st, ok := states[name]; ok {
			enabled = st.Enabled
			if strings.TrimSpace(st.Reason) != "" {
				detailParts = append(detailParts, "reason: "+st.Reason)
			}
		}
		loadedInert := "loaded on this node"
		if memqldsl.PackDomainDisabled(name) {
			loadedInert = "mounted-inert on this node (behavioral constructs not loaded)"
		}
		state := "enabled"
		if !enabled {
			state = "disabled"
		}
		bootMatchesDesired := enabled != memqldsl.PackDomainDisabled(name)
		if !bootMatchesDesired {
			detailParts = append(detailParts,
				"state changed since this node booted; restart required to apply")
		}
		detailParts = append(detailParts, loadedInert)

		plugins := PluginsForPackDomain(name)
		for _, p := range plugins {
			bound[p] = struct{}{}
		}
		prefixes := make([]string, 0, len(plugins))
		for _, p := range plugins {
			prefixes = append(prefixes, "integration."+p+".")
		}

		rows = append(rows, ModuleRow{
			Kind:        ModuleKindPack,
			Name:        name,
			Description: fmt.Sprintf("Pack domain (%s), %d file(s).", domain.Origin, domain.FileCount),
			State:       state,
			StateDetail: strings.Join(detailParts, "; "),
			Scope:       ModuleScopeCluster,
			FqnPrefixes: prefixes,
		})
	}
	return rows, bound
}

// integrationModuleRows enumerates this binary's integrations: every
// registered plugin not folded under a pack, plus every live provider the
// app wired explicitly (cognition, agent, stt -- the PluginContext-exempt
// tier). State is DERIVED, never stored: active means materialized on this
// node, opted_out means the factory declined (nil, nil) for a documented
// dependency reason.
func (e *MemQLEngine) integrationModuleRows(manifest *envregistry.Manifest, packBound map[string]struct{}) []ModuleRow {
	manifestComponents := map[string]struct{}{}
	for _, entry := range manifest.AllEntries() {
		if c := strings.TrimSpace(entry.Component); c != "" {
			manifestComponents[c] = struct{}{}
		}
	}

	live := map[string]struct{}{}
	if e != nil && e.integrations != nil {
		for _, name := range e.integrations.ProviderNames() {
			live[name] = struct{}{}
		}
	}

	seen := map[string]struct{}{}
	var rows []ModuleRow
	addRow := func(name, registrationState string) {
		if _, dup := seen[name]; dup {
			return
		}
		if _, isPacks := packBound[name]; isPacks {
			return
		}
		seen[name] = struct{}{}

		state := "opted_out"
		detail := "compiled in; factory declined (dependency not configured on this node)"
		if _, ok := live[name]; ok {
			state = "active"
			detail = "materialized on this node"
		} else if registrationState == "wired" {
			// A live-registry-only provider that has since gone away cannot
			// happen (registration is boot-time), so this branch is unreachable;
			// kept for the map's completeness.
			detail = "wired explicitly by the app phase"
		}

		var envComponents []string
		if _, ok := manifestComponents[name]; ok {
			envComponents = []string{name}
		}

		rows = append(rows, ModuleRow{
			Kind:          ModuleKindIntegration,
			Name:          name,
			State:         state,
			StateDetail:   detail,
			Scope:         ModuleScopeNode,
			EnvComponents: envComponents,
			FqnPrefixes:   []string{"integration." + name + "."},
		})
	}

	for _, reg := range RegisteredPlugins() {
		addRow(reg.Name, "registered")
	}
	// Explicitly wired providers (no RegisterPlugin entry) surface from the
	// live registry: on the answering binary they are as real as the
	// self-registered tier.
	for name := range live {
		addRow(name, "wired")
	}
	return rows
}

// clusterConceptRow is the minimal decoded shape of a latest-version graph
// row the node-type assembly reads.
type clusterConceptRow struct {
	ID        string
	CreatedAt time.Time
	Payload   map[string]any
}

// latestRowsByConcept reads the newest version of every row of a concept,
// raw, mirroring ReadPackStates. Used for the v1:cluster:* topology
// concepts. Missing relation reads as empty (a cluster that has never
// registered a node simply has no node-type rows).
//
// staged-data: MUST-NOT-GATE -- the module inventory reports the cluster's
// operating state to owner/admin callers; withholding a topology row would
// report a node type as absent while the mesh runs it. The v1:cluster:*
// concepts this reads are system-written bookkeeping, never authored
// staged data, and the surface is already role-gated above this read.
func latestRowsByConcept(ctx context.Context, db *bun.DB, conceptID string) ([]clusterConceptRow, error) {
	if db == nil {
		return nil, nil
	}
	var rows []struct {
		ID        string          `bun:"id"`
		CreatedAt time.Time       `bun:"createdAt"`
		Payload   json.RawMessage `bun:"payload"`
	}
	err := db.NewRaw(
		`SELECT DISTINCT ON (id) id, "createdAt", payload
		   FROM "MemoryNodes"
		  WHERE concept = ?
		  ORDER BY id, "createdAt" DESC`,
		conceptID,
	).Scan(ctx, &rows)
	if err != nil {
		if isMissingRelation(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("module inventory: read %s: %w", conceptID, err)
	}
	out := make([]clusterConceptRow, 0, len(rows))
	for _, r := range rows {
		payload := map[string]any{}
		if len(r.Payload) > 0 {
			// Undecodable topology bookkeeping should not take the whole
			// inventory down; the row simply contributes no fields.
			_ = json.Unmarshal(r.Payload, &payload)
		}
		out = append(out, clusterConceptRow{ID: r.ID, CreatedAt: r.CreatedAt, Payload: payload})
	}
	return out, nil
}

func payloadString(p map[string]any, key string) string {
	if v, ok := p[key].(string); ok {
		return v
	}
	return ""
}

// nodeTypeModuleRows builds one row per node type the graph knows
// (v1:cluster:nodeType), with live health from v1:cluster:node, replicas
// from the newest v1:cluster:deploymentNodeSpec per node type, and the
// voice-style credential gate evaluated from the manifest's per-node-type
// requiredness against the ANSWERING node's env -- honest because every
// Deployment shares the memql-secrets envFrom by deploy convention, and
// labeled node-scope-derived in the detail because that sharing is a
// convention, not an engine guarantee.
func (e *MemQLEngine) nodeTypeModuleRows(ctx context.Context, manifest *envregistry.Manifest) ([]ModuleRow, error) {
	db := e.database()
	if db == nil {
		return nil, nil
	}

	typeRows, err := latestRowsByConcept(ctx, db, "v1:cluster:nodeType")
	if err != nil {
		return nil, err
	}
	nodeRows, err := latestRowsByConcept(ctx, db, "v1:cluster:node")
	if err != nil {
		return nil, err
	}
	specRows, err := latestRowsByConcept(ctx, db, "v1:cluster:deploymentNodeSpec")
	if err != nil {
		return nil, err
	}

	// Live nodes per type, counting only rows whose health says the process
	// answers (or is on its way up).
	liveByType := map[string]int{}
	for _, n := range nodeRows {
		nt := payloadString(n.Payload, "nodeType")
		if nt == "" {
			continue
		}
		switch payloadString(n.Payload, "health") {
		case "healthy", "connecting", "degraded", "draining":
			liveByType[nt]++
		}
	}

	// Newest replica pin per node type across deployments.
	type replicaPin struct {
		replicas int
		at       time.Time
	}
	replicasByType := map[string]replicaPin{}
	for _, s := range specRows {
		nt := payloadString(s.Payload, "nodeType")
		if nt == "" {
			continue
		}
		replicas := -1
		if v, ok := s.Payload["replicas"].(float64); ok {
			replicas = int(v)
		}
		if replicas < 0 {
			continue
		}
		if prev, ok := replicasByType[nt]; !ok || s.CreatedAt.After(prev.at) {
			replicasByType[nt] = replicaPin{replicas: replicas, at: s.CreatedAt}
		}
	}

	var rows []ModuleRow
	for _, t := range typeRows {
		name := payloadString(t.Payload, "name")
		if name == "" {
			name = strings.TrimPrefix(t.ID, "v1:cluster:nodeType:")
		}
		if name == "" {
			continue
		}

		live := liveByType[name]
		pin, hasPin := replicasByType[name]

		missingCreds := missingRequiredEnvForNodeType(manifest, name)

		state := "not_deployed"
		var detail []string
		switch {
		case live > 0:
			state = "running"
			detail = append(detail, fmt.Sprintf("%d live node(s)", live))
		case hasPin && pin.replicas == 0:
			state = "scaled_to_zero"
			detail = append(detail, "pinned to 0 replicas")
		case len(missingCreds) > 0:
			state = "credential_gated"
		}
		if hasPin {
			detail = append(detail, fmt.Sprintf("replicas pinned to %d", pin.replicas))
		}
		if len(missingCreds) > 0 {
			detail = append(detail, fmt.Sprintf(
				"required env unset (evaluated on the answering node): %s",
				strings.Join(missingCreds, ", ")))
			if state == "scaled_to_zero" {
				// The voice case: the deploy layer holds the lane at zero
				// BECAUSE the credentials are absent (memql#2416). Report the
				// mechanism, keep it where it lives.
				state = "credential_gated"
			}
		}

		rows = append(rows, ModuleRow{
			Kind:          ModuleKindNodeType,
			Name:          name,
			Description:   payloadString(t.Payload, "description"),
			State:         state,
			StateDetail:   strings.Join(detail, "; "),
			Scope:         ModuleScopeCluster,
			CodeReference: payloadString(t.Payload, "codeReference"),
		})
	}
	return rows, nil
}

// missingRequiredEnvForNodeType lists manifest entries required for the
// node type that resolve unset on the answering node (no env value and no
// manifest default).
func missingRequiredEnvForNodeType(manifest *envregistry.Manifest, nodeType string) []string {
	var missing []string
	for _, entry := range manifest.AllEntries() {
		if !entry.RequiredFor(nodeType) {
			continue
		}
		if v, ok := os.LookupEnv(entry.Name); ok && strings.TrimSpace(v) != "" {
			continue
		}
		if strings.TrimSpace(entry.Default) != "" {
			continue
		}
		missing = append(missing, entry.Name)
	}
	sort.Strings(missing)
	return missing
}

// moduleEnvSurface joins the module's envComponents against the manifest.
// SECRET ENTRIES NEVER CARRY A VALUE -- set/unset is the whole answer for
// them; non-secret variables carry the resolved value (env first, manifest
// default second).
func moduleEnvSurface(manifest *envregistry.Manifest, envComponents []string) []ModuleEnvVar {
	if len(envComponents) == 0 {
		return nil
	}
	wanted := map[string]struct{}{}
	for _, c := range envComponents {
		wanted[c] = struct{}{}
	}

	secretNames := map[string]struct{}{}
	for _, s := range manifest.Secrets {
		secretNames[s.Name] = struct{}{}
	}

	var out []ModuleEnvVar
	for _, entry := range manifest.AllEntries() {
		if _, ok := wanted[strings.TrimSpace(entry.Component)]; !ok {
			continue
		}
		_, isSecret := secretNames[entry.Name]

		envVal, envSet := os.LookupEnv(entry.Name)
		set := (envSet && strings.TrimSpace(envVal) != "") || strings.TrimSpace(entry.Default) != ""

		v := ModuleEnvVar{
			Name:        entry.Name,
			Description: entry.Description,
			Secret:      isSecret,
			Scope:       entry.Scope,
			RequiredFor: append([]string(nil), entry.Required...),
			Set:         set,
		}
		if !isSecret {
			if envSet && strings.TrimSpace(envVal) != "" {
				v.Value = envVal
			} else {
				v.Value = entry.Default
			}
			v.DefaultValue = entry.Default
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
