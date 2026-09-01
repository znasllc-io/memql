package email

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// VariableResolver resolves a named global plaintext variable from
// v1:platform:globalVariable. Mirrors the signature exposed by
// memql.PluginContext.ResolveSystemVariable so the email plug-in
// doesn't need to import the memql package directly.
type VariableResolver func(ctx context.Context, name string) (string, error)

// SecretResolver resolves a named global encrypted secret from
// v1:platform:globalSecret. Mirrors PluginContext.ResolveSystemSecret.
type SecretResolver func(ctx context.Context, name string) (string, error)

// LazySender is a Sender that defers credential resolution until the
// first Send call, then caches the resulting concrete Sender for the
// lifetime of the process.
//
// Why this exists: the env-driven NewSenderFromEnv path runs at
// plug-in registration time, BEFORE `go run ./scripts/secrets seed`
// populates v1:platform:globalVariable + globalSecret rows on a freshly
// rebuilt cluster. Without lazy resolution, the identity binary
// would lock in LogSender on cold-boot and require a manual restart
// after seeding to pick up Graph / SMTP credentials.
//
// Resolution order on first Send:
//
//  1. Env-resolved Sender (passed in from NewSenderFromEnv) -- if
//     it's a real GraphSender / SMTPSender, use that. Production
//     path: env is always set at startup.
//  2. MemQL globalVariable / globalSecret rows -- dev path. The
//     resolver pulls EMAIL_AZURE_* + MEMQL_EMAIL_SENDER + (optionally)
//     MEMQL_EMAIL_FROM_NAME for Graph; falls back to SMTP_HOST + sibling
//     vars + SMTP_PASSWORD if Graph isn't fully configured.
//  3. LogSender fallback -- everything's empty.
//
// Subsequent Send calls reuse the resolved Sender directly. If
// credentials change the operator must restart the binary; that's
// the same posture as env-only setups.
type LazySender struct {
	logger        *slog.Logger
	envResolved   Sender // the Sender returned by NewSenderFromEnv
	resolveVar    VariableResolver
	resolveSecret SecretResolver

	once     sync.Once
	resolved Sender
}

// NewLazySender wraps the env-resolved Sender with a fallback
// resolver that pulls config from memql global rows on first Send.
//
// envResolved must not be nil; pass NewLogSender(...) when there is
// no env-resolved Sender to delegate to.
func NewLazySender(envResolved Sender, resolveVar VariableResolver, resolveSecret SecretResolver, logger *slog.Logger) *LazySender {
	return &LazySender{
		logger:        logger,
		envResolved:   envResolved,
		resolveVar:    resolveVar,
		resolveSecret: resolveSecret,
	}
}

// Send delegates to the resolved Sender. Resolution runs at most
// once across the process lifetime.
//
// `as` is forwarded UNINTERPRETED. Which identities a transport can honour
// is a fact about the transport -- Graph accepts any mailbox its credential
// is scoped to, SMTP exactly one -- so the decision belongs to the concrete
// Sender this resolves to, and a check here would have to guess which one
// that will be before it has resolved.
func (l *LazySender) Send(ctx context.Context, msg Message, as SendAs) error {
	if l == nil {
		return fmt.Errorf("email: lazy sender not initialized")
	}
	return l.Resolve(ctx).Send(ctx, msg, as)
}

// Resolve returns the concrete Sender this wrapper stands for, running the
// one-time resolution if it has not run yet. Never nil.
//
// Exported for the NDR poller (memql#4824), which needs to know whether this
// node's sender is Graph before any message has been sent -- a node that
// only READS the mailbox would otherwise sit behind an unresolved lazy
// wrapper forever and conclude it had no Graph sender. Asking is the same
// act a send performs, so it runs the same sync.Once and produces the same
// answer; it is not a second resolution path.
func (l *LazySender) Resolve(ctx context.Context) Sender {
	l.once.Do(func() { l.resolved = l.resolve(ctx) })
	if l.resolved == nil {
		// Defensive -- resolve always returns at least a LogSender.
		l.resolved = NewLogSender(l.logger)
	}
	return l.resolved
}

// resolve picks the best Sender available, in priority order. Env
// wins because it's the production posture; only when the env
// resolver returned a LogSender (i.e. no Graph + no SMTP
// configured) do we try memql.
func (l *LazySender) resolve(ctx context.Context) Sender {
	// If env produced a real Sender (Graph / SMTP), keep it.
	if l.envResolved != nil {
		if _, isLog := l.envResolved.(*LogSender); !isLog {
			return l.envResolved
		}
	}

	if l.resolveVar == nil && l.resolveSecret == nil {
		return l.envResolved // LogSender from caller
	}

	// ONE walk over the declared manifest, rather than a hand-written
	// attempt per lane (memql#4825). The lane list, which slots each needs
	// and which of them are secret are all declared in emailconfig.go, and
	// the status reporter walks the same declaration -- so the console
	// cannot show a complete lane beside a sender that did not resolve.
	//
	// Env is passed as nil deliberately: this tier runs only after the env
	// tier already lost, and letting it re-read the environment would let a
	// half-migrated configuration resolve here by borrowing the very values
	// NewSenderFromEnv had already declined to use.
	res := resolveEmailConfig(ctx, ConfigResolver{Vars: l.resolveVar, Secrets: l.resolveSecret})
	if sender := senderFor(res, l.logger); sender != nil {
		return sender
	}

	// A configuration that is PRESENT but split across tiers is a distinct
	// state from an absent one, and it used to be silent: the operator saw
	// every value seeded and a log-only sender. The manifest can tell them
	// apart, so this says which it is.
	for _, lane := range res.Lanes {
		if l.logger == nil {
			continue
		}
		switch {
		case lane.Split:
			l.logger.Warn("email: a lane's settings are present but split across tiers, so none of them is used",
				"lane", lane.Lane.Name,
				"hint", "a lane is taken WHOLE from the environment or WHOLE from stored rows, never mixed")
		case lane.Partial:
			// The generalization of the old "SMTP_HOST seeded but
			// SMTP_FROM_ADDR missing" line. It now fires for whichever lane
			// somebody started, and names every value it is short of --
			// rather than for one hand-picked pair on one hand-picked lane.
			l.logger.Warn("email: a lane is partly configured and cannot be used",
				"lane", lane.Lane.Name,
				"missing", strings.Join(lane.Missing, ", "))
		}
	}

	// The baseline is whatever the caller passed, which on an install that
	// must deliver mail is a LogSender that refuses every Send (email.go).
	// Reached only when the boot gate did not fire -- a node that never
	// materialized the plug-in, or a credential seeded into rows that has
	// since been cleared -- so the honest line here is not "using LogSender".
	if l.logger != nil {
		if DeliveryRequired() {
			l.logger.Error("email: no Graph or SMTP credentials in env or memql; sends will be REFUSED",
				"error", RefuseLogOnly("send"))
		} else {
			l.logger.Info("email: no Graph or SMTP credentials in env or memql, using LogSender")
		}
	}
	return l.envResolved // LogSender baseline
}
