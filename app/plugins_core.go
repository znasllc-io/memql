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
	// product-agnostic features. `chat` (the single-utterance-stream
	// recentChat capability, operating on the core v1:cognition:utterance)
	// is re-homed here from the product pack -- any product references
	// integration.chat.recentChat with zero product Go. The remaining Epic-3
	// (memql#1902) product integrations -- avatardirect, dailyspace, training
	// -- are still pack-registered pending their own absorption PRs (#2485);
	// avatarvendor STAYS here (shared vendor-REST core the voice-agent imports).
	_ "github.com/znasllc-io/memql/integrations/actionsearch"
	_ "github.com/znasllc-io/memql/integrations/chat"
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
