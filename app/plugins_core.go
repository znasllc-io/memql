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
	_ "github.com/znasllc-io/memql/integrations/library"
	_ "github.com/znasllc-io/memql/integrations/liveknowledge"
	_ "github.com/znasllc-io/memql/integrations/openairealtime"
	_ "github.com/znasllc-io/memql/integrations/rbac"
	_ "github.com/znasllc-io/memql/integrations/router"
	_ "github.com/znasllc-io/memql/integrations/similarity"
	_ "github.com/znasllc-io/memql/integrations/telephony"
	_ "github.com/znasllc-io/memql/integrations/telephony/telnyx"
	_ "github.com/znasllc-io/memql/integrations/timeutil"
	_ "github.com/znasllc-io/memql/integrations/voice"
	_ "github.com/znasllc-io/memql/integrations/workbench"
)
