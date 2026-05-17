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
	_ "github.com/znasllc-io/memql/integrations/database"
	_ "github.com/znasllc-io/memql/integrations/email"
	_ "github.com/znasllc-io/memql/integrations/embedding"
	_ "github.com/znasllc-io/memql/integrations/fileprocessor"
	_ "github.com/znasllc-io/memql/integrations/gcs"
	_ "github.com/znasllc-io/memql/integrations/identity"
	_ "github.com/znasllc-io/memql/integrations/knowledge"
	_ "github.com/znasllc-io/memql/integrations/liveavatar"
	_ "github.com/znasllc-io/memql/integrations/liveknowledge"
	_ "github.com/znasllc-io/memql/integrations/router"
	_ "github.com/znasllc-io/memql/integrations/similarity"
	_ "github.com/znasllc-io/memql/integrations/training"
	_ "github.com/znasllc-io/memql/integrations/voice"
)
