package app

// Blank-import core plug-in packages so their init() registrations run.
// Each imported package calls memql.RegisterPlugin in its init() and is
// materialized by a.materializePlugins at startup.
//
// Composition rule: every integration whose dependencies fit PluginContext
// (Logger, Engine, BunDB, VisionProvider, EmbeddingProviderByName,
// partition/variable resolvers) belongs here. Integrations that need deps
// outside that surface -- cognition, agent, stt -- continue to be wired
// explicitly in the build-tag-gated app/integrations_*.go files.
//
// Adding a new plug-in from a product branch: add a new package under
// integrations/ with an init() that calls memql.RegisterPlugin, then add a
// blank import in a build-tagged file alongside this one.
import (
	_ "github.com/znasllc-io/memql/integrations/agents"
	_ "github.com/znasllc-io/memql/integrations/auth"
	_ "github.com/znasllc-io/memql/integrations/azureblob"
	// Platform consolidation (#2472 Step 2/3, Decision 2): reusable
	// capabilities are absorbed back into the engine as GENERIC,
	// product-agnostic features. Re-homed here from the product pack (each
	// operates only on core concepts + engine packages, zero product Go):
	//   - `chat`         -- recentChat over the core v1:cognition:utterance stream
	//   - `dailyspace`   -- ensure/rollover a per-user daily space (v1:cognition:space)
	//   - `avatardirect` -- direct cloud-avatar session (rides the shared avatarvendor)
	// Any product references integration.<name>.* with zero product Go.
	// `training` (per-agent train pipeline) is still pack-registered pending its
	// own absorption PR (#2485) -- it references a product concept that must be
	// de-coupled first. avatarvendor STAYS here (shared vendor-REST core).
	_ "github.com/znasllc-io/memql/integrations/avatardirect"
	_ "github.com/znasllc-io/memql/integrations/chat"
	// Custom domains (epic memql#4805). Registered on every node type, like
	// release above and for the same reason: the two builtins are declared in
	// dsl/platform/builtins.memql, which every binary loads, and a capability
	// present in the DSL and absent from the registry is a boot-time
	// resolution failure. Gating it by build tag would additionally make the
	// `add` capability's availability depend on which replica a browser's
	// stream landed on.
	_ "github.com/znasllc-io/memql/integrations/customdomain"
	_ "github.com/znasllc-io/memql/integrations/dailyspace"
	_ "github.com/znasllc-io/memql/integrations/database"
	_ "github.com/znasllc-io/memql/integrations/deployversion"
	_ "github.com/znasllc-io/memql/integrations/email"
	_ "github.com/znasllc-io/memql/integrations/embedding"
	_ "github.com/znasllc-io/memql/integrations/fileprocessor"
	_ "github.com/znasllc-io/memql/integrations/harnessrecall"
	_ "github.com/znasllc-io/memql/integrations/worktrace"
	_ "github.com/znasllc-io/memql/integrations/identity"
	_ "github.com/znasllc-io/memql/integrations/knowledge"
	// sitePublish lives in the ROOT module, not integrations/, because it uses
	// component/edge's Publisher and integrations is its own module that the root
	// already requires -- importing edge from there made the module graph a cycle
	// (memql#4345). Blank-imported HERE, beside the integration it split from, so
	// the registration reaches exactly the same binaries it did before the move.
	_ "github.com/znasllc-io/memql/component/sitepublish"
	// packages lives in the ROOT module for exactly the reason sitePublish
	// does, and it is the same constraint one layer up: the deploy pipeline
	// publishes through component/edge's Publisher, and importing edge from
	// integrations is the module cycle CI's module-boundaries lane refuses
	// (epic memql#4794). Registered on every node type, because the builtins
	// are declared in the core DSL tree that every binary loads -- and an
	// integration registered NOWHERE only warns, while one registered with a
	// capability MISSING fails boot.
	_ "github.com/znasllc-io/memql/component/packages"
	// The log store's `logs` plug-in (epic memql#4893) lives in component/, in
	// the ROOT module, for the reason packages does: it is engine internals.
	// The eight builtins it executes are declared in dsl/observability, which
	// every binary loads, so a capability missing from the registry is a
	// boot-time resolution failure on every node type -- and it imports
	// integrations/azureblob for the archive, which the root module already
	// requires and integrations could not import back.
	_ "github.com/znasllc-io/memql/component/logstore"
	_ "github.com/znasllc-io/memql/integrations/library"
	_ "github.com/znasllc-io/memql/integrations/liveknowledge"
	_ "github.com/znasllc-io/memql/integrations/openairealtime"
	_ "github.com/znasllc-io/memql/integrations/rbac"
	// Cutting a release of MemQL itself (epic memql#4434). Registered on
	// every node type rather than gated to one: the builtins are declared in
	// dsl/cluster/builtins.memql, which every node loads, and a capability
	// present in the DSL and absent from the registry is a boot-time
	// resolution failure. It refuses harmlessly wherever nothing is seeded.
	_ "github.com/znasllc-io/memql/integrations/release"
	_ "github.com/znasllc-io/memql/integrations/router"
	// The sync runtime (epic memql#4378): the inbound dispatcher and the
	// backfill / reconciliation runners. The outbox DRAIN worker is not
	// here -- it needs the cluster execution guard, which is not on
	// PluginContext, so it is wired explicitly in app/engine.go beside
	// the campaigns worker for the same reason.
	_ "github.com/znasllc-io/memql/component/datasync"
	// Event-email rules (memql#4829). It registers the ordinary way for its
	// three capabilities, and the App additionally hands it the concrete
	// engine plus the authored-runtime deps through emailrules.Bind, because
	// activation is a method on *MemQLEngine and the deps are assembled from
	// the live App -- neither is on PluginContext. Until that Bind happens the
	// two activation capabilities refuse with a sentence naming the reason,
	// which on a node type that runs no authored runtime is the permanent and
	// correct answer.
	_ "github.com/znasllc-io/memql/component/emailrules"
	_ "github.com/znasllc-io/memql/component/sitetraffic"
	_ "github.com/znasllc-io/memql/integrations/shopify"
	_ "github.com/znasllc-io/memql/integrations/similarity"
	_ "github.com/znasllc-io/memql/integrations/telephony"
	_ "github.com/znasllc-io/memql/integrations/telephony/telnyx"
	_ "github.com/znasllc-io/memql/integrations/timeutil"
	_ "github.com/znasllc-io/memql/integrations/voice"
	_ "github.com/znasllc-io/memql/integrations/workbench"
)
