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
	_ "github.com/znasllc-io/memql/integrations/actionsearch"
	_ "github.com/znasllc-io/memql/integrations/avatardirect"
	_ "github.com/znasllc-io/memql/integrations/chat"
	_ "github.com/znasllc-io/memql/integrations/dailyspace"
	_ "github.com/znasllc-io/memql/integrations/database"
	_ "github.com/znasllc-io/memql/integrations/deployversion"
	_ "github.com/znasllc-io/memql/integrations/email"
	_ "github.com/znasllc-io/memql/integrations/embedding"
	_ "github.com/znasllc-io/memql/integrations/fileprocessor"
	_ "github.com/znasllc-io/memql/integrations/harnessrecall"
	_ "github.com/znasllc-io/memql/integrations/harnesstrace"
	_ "github.com/znasllc-io/memql/integrations/identity"
	_ "github.com/znasllc-io/memql/integrations/knowledge"
	// sitePublish lives in the ROOT module, not integrations/, because it uses
	// component/edge's Publisher and integrations is its own module that the root
	// already requires -- importing edge from there made the module graph a cycle
	// (memql#4345). Blank-imported HERE, beside the integration it split from, so
	// the registration reaches exactly the same binaries it did before the move.
	_ "github.com/znasllc-io/memql/component/sitepublish"
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
	_ "github.com/znasllc-io/memql/integrations/shopify"
	_ "github.com/znasllc-io/memql/integrations/similarity"
	_ "github.com/znasllc-io/memql/integrations/telephony"
	_ "github.com/znasllc-io/memql/integrations/telephony/telnyx"
	_ "github.com/znasllc-io/memql/integrations/timeutil"
	_ "github.com/znasllc-io/memql/integrations/voice"
	_ "github.com/znasllc-io/memql/integrations/workbench"
)
