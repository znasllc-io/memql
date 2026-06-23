package email

import (
	"context"
	"fmt"
	"log/slog"
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
// plug-in registration time, BEFORE `make secrets-seed` populates
// v1:platform:globalVariable + globalSecret rows on a freshly
// rebuilt cluster. Without lazy resolution, the identity binary
// would lock in LogSender on cold-boot and require a manual restart
// after seeding to pick up Graph / SMTP credentials.
//
// Resolution order on first Send:
//
//  1. Env-resolved Sender (passed in from NewSenderFromEnv) -- if
//     it's a real GraphSender / SMTPSender, use that. Production
//     path: env is always set at startup.
//  2. memQL globalVariable / globalSecret rows -- dev path. The
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
func (l *LazySender) Send(ctx context.Context, msg Message) error {
	if l == nil {
		return fmt.Errorf("email: lazy sender not initialized")
	}
	l.once.Do(func() { l.resolved = l.resolve(ctx) })
	if l.resolved == nil {
		// Defensive -- resolve always returns at least a LogSender.
		l.resolved = NewLogSender(l.logger)
	}
	return l.resolved.Send(ctx, msg)
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

	if sender := l.tryGraphFromMemql(ctx); sender != nil {
		return sender
	}
	if sender := l.trySMTPFromMemql(ctx); sender != nil {
		return sender
	}

	if l.logger != nil {
		l.logger.Info("email: no Graph or SMTP credentials in env or memql, using LogSender")
	}
	return l.envResolved // LogSender baseline
}

func (l *LazySender) tryGraphFromMemql(ctx context.Context) Sender {
	keys := DefaultGraphEnvKeys()
	legacy := LegacyGraphEnvKeys()

	tenantId := l.firstNonEmptyVar(ctx, keys.TenantId, legacy.TenantId)
	clientId := l.firstNonEmptyVar(ctx, keys.ClientId, legacy.ClientId)
	sender := l.firstNonEmptyVar(ctx, keys.SenderAddr, legacy.SenderAddr)
	fromName := l.firstNonEmptyVar(ctx, keys.FromName, legacy.FromName)
	clientSecret := l.firstNonEmptySecret(ctx, keys.ClientSecret, legacy.ClientSecret)

	if tenantId == "" || clientId == "" || clientSecret == "" || sender == "" {
		return nil
	}
	if l.logger != nil {
		l.logger.Info("email: using Microsoft Graph sender (resolved from memql global rows)",
			"sender", sender,
			"tenantId", tenantId)
	}
	return NewGraphSender(GraphConfig{
		TenantId:     tenantId,
		ClientId:     clientId,
		ClientSecret: clientSecret,
		SenderAddr:   sender,
		FromName:     fromName,
	}, nil, l.logger)
}

func (l *LazySender) trySMTPFromMemql(ctx context.Context) Sender {
	keys := DefaultEnvKeys()

	host := l.firstNonEmptyVar(ctx, keys.Host)
	if host == "" {
		return nil
	}
	fromAddr := l.firstNonEmptyVar(ctx, keys.FromAddr)
	if fromAddr == "" {
		if l.logger != nil {
			l.logger.Warn("email: SMTP_HOST seeded but SMTP_FROM_ADDR missing; cannot construct SMTPSender")
		}
		return nil
	}
	port := l.firstNonEmptyVar(ctx, keys.Port)
	if port == "" {
		port = "587"
	}
	username := l.firstNonEmptyVar(ctx, keys.Username)
	password := l.firstNonEmptySecret(ctx, keys.Password)
	fromName := l.firstNonEmptyVar(ctx, keys.FromName)

	if l.logger != nil {
		l.logger.Info("email: using SMTP sender (resolved from memql global rows)",
			"host", host, "fromAddr", fromAddr)
	}
	return NewSMTPSender(SMTPConfig{
		Host:     host,
		Port:     port,
		Username: username,
		Password: password,
		FromAddr: fromAddr,
		FromName: fromName,
	}, l.logger)
}

func (l *LazySender) firstNonEmptyVar(ctx context.Context, names ...string) string {
	if l.resolveVar == nil {
		return ""
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		v, err := l.resolveVar(ctx, name)
		if err == nil && v != "" {
			return v
		}
	}
	return ""
}

func (l *LazySender) firstNonEmptySecret(ctx context.Context, names ...string) string {
	if l.resolveSecret == nil {
		return ""
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		v, err := l.resolveSecret(ctx, name)
		if err == nil && v != "" {
			return v
		}
	}
	return ""
}
