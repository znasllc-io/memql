//go:build agent || mcp

package app

import (
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/worker"
)

// workerAuditorForApp resolves an Auditor for the agent build (epic
// memql#5327, design D14).
//
// IT RETURNED NoopAuditor, and the comment that stood here explained why that
// was temporary: "the agent node typically does not own an identity
// AuditLogger (those live on the identity binary) ... the SlogAuditLogger gets
// wired during identity rollout when audit relays for non-identity nodes go
// in." The relay never landed and the no-op did, so every `worker_registered`
// and `worker_disconnected` the stream server emitted -- on every agent
// replica, for every machine, for the life of the feature -- went nowhere.
// Pairing a machine was audited; connecting one was not.
//
// No relay is needed. v1:identity:auditEvent is an ordinary graph concept and
// identity.EngineAuditSink writes it through createAuditEvent like any other
// Go writer -- which is exactly what component/packages does from a
// non-identity node. The slog stream stays the always-on destination and the
// row is best-effort beside it, which is SlogAuditLogger's own contract.
func workerAuditorForApp(a *App) worker.Auditor {
	if a == nil || a.engine == nil {
		// No engine means no row to write. The slog half would still work,
		// but a logger with no sink is what the no-op already was and
		// pretending otherwise helps nobody.
		return worker.NoopAuditor{}
	}
	return &worker.IdentityAuditor{
		Logger: a.Logger,
		AuditLogger: &identity.SlogAuditLogger{
			Logger: a.Logger,
			DB:     &identity.EngineAuditSink{Engine: a.engine, Logger: a.Logger},
		},
	}
}
