package email

// delivery.go -- the gate that stops mail from FAILING UPWARD (memql#4477).
//
// The email lane degrades rather than failing. With no credentials
// NewSenderFromEnv used to select a LogSender, which writes the message to
// the pod log and returns nil, and every layer above then reported success:
// the setup wizard said the link was sent, and the audit row recorded
// `magic_link_issued` with `outcome=success`. The only evidence was a human
// not receiving mail, which is indistinguishable from a spam filter.
//
// That is a worse failure than the silent kind. A pod that hangs eventually
// gets investigated; a green success does not. On the first real cloud
// bring-up the cluster owner could not claim their own cluster and nothing
// anywhere said why.
//
// So: log-only stays exactly as it is on a local install, where it is the
// intended behaviour, and is REFUSED on an install where nobody can have
// meant it. The discriminator is MEMQL_DOMAIN -- the one seeded value every
// node already derives its issuer, CORS origins and redirect URIs from, so
// this introduces no new fact for an operator to keep in sync.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/env"
)

const (
	// DomainEnv is the install's public hostname suffix. Set on every node
	// (component/envregistry/manifest.yaml, scope node) and delivered to
	// local pods as the single key of the memql-domain ConfigMap.
	DomainEnv = "MEMQL_DOMAIN"

	// AllowLogOnlyEnv is the break-glass. It exists because a domain is NOT
	// a reliable statement about intent in one direction: `make up
	// DOMAIN=lab.example.com` stands up a genuinely local parity cluster on
	// a name that looks like production, and refusing to boot it would be a
	// regression for a developer who never wanted mail. Set it to true to
	// say log-only is deliberate here; the boot log says so out loud, and
	// the portal's integration report keeps calling the lane degraded.
	//
	// The default is deliberately the strict direction. An operator who has
	// to type this has decided something; an operator who never heard of
	// mail configuration gets told.
	AllowLogOnlyEnv = "MEMQL_EMAIL_ALLOW_LOG_ONLY"
)

// ErrLogOnlyRefused is the sentinel every refusal wraps, so a caller can tell
// "this install will not pretend" apart from a real delivery failure.
var ErrLogOnlyRefused = errors.New("email: log-only mode refused on an install that must deliver mail")

// IsLocalDomain reports whether a MEMQL_DOMAIN value names an install where
// log-only mail is the intended behaviour rather than a misconfiguration.
//
// Three shapes count as local, matching the convention the rest of the tree
// already uses (component/identity's isLocalHost):
//
//  1. The empty string. A process with no domain configured is not a cloud
//     install -- it is a unit test, a CLI tool, or a developer's shell.
//  2. A literal loopback name, or anything under the RFC 6761 `.localhost`
//     TLD. `memql.localhost` is the local overlay's committed default and
//     resolves to loopback by specification.
//  3. The `*.local.<domain>` dev wildcard this repo locks in, where the
//     SECOND label is exactly `local`.
//
// This deliberately does NOT reuse component/identity's predicate, and the
// duplication is the smaller cost. That one takes a URL host with a port and
// answers "must this posture encrypt keys at rest"; a near-miss question with
// a different answer is exactly the trap its own isLocalHost /
// isSingleProcessHost split was written to warn about. Reaching for it would
// also add a module edge from integrations/email to component/identity, which
// is the wrong direction.
func IsLocalDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" {
		return true
	}
	switch d {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	if strings.HasSuffix(d, ".localhost") {
		return true
	}
	labels := strings.Split(d, ".")
	return len(labels) >= 3 && labels[1] == "local"
}

// DeliveryRequired reports whether this process must really deliver mail --
// that is, whether selecting a log-only sender here would be a defect rather
// than a choice.
func DeliveryRequired() bool {
	reader := env.NewEnvReader("")
	domain, _ := reader.String(DomainEnv)

	// A malformed value is NOT an opt-out. ParseBool fails on "yes" and on a
	// typo, and reading that as permission would turn a fat-fingered
	// break-glass into exactly the silence this gate removes.
	if allow, err := reader.OptionalBool(AllowLogOnlyEnv); err == nil && allow != nil && *allow {
		return false
	}
	return !IsLocalDomain(domain)
}

// RefuseLogOnly builds the refusal. `stage` names where the install gave up
// ("boot", "send") so a log line places it without a stack trace.
//
// The message names every way out, because a refusal that only says no
// reproduces the silence it replaced -- the operator on the bring-up this
// came from had no string to search for.
func RefuseLogOnly(stage string) error {
	reader := env.NewEnvReader("")
	domain, _ := reader.String(DomainEnv)
	if domain == "" {
		domain = "(unset)"
	}
	g := DefaultGraphEnvKeys()
	s := DefaultEnvKeys()
	return fmt.Errorf(
		"%w at %s: %s=%q is not a local name, so mail written to the log would be reported as delivered and nobody would learn otherwise. "+
			"Configure Microsoft Graph (%s, %s, %s, %s -- all four) or SMTP (%s and %s), "+
			"or set %s=true to state that log-only is deliberate on this install",
		ErrLogOnlyRefused, stage, DomainEnv, domain,
		g.TenantId, g.ClientId, g.ClientSecret, g.SenderAddr,
		s.Host, s.FromAddr,
		AllowLogOnlyEnv,
	)
}

// refusalSendError wraps the refusal as a PERMANENT SendError so the campaign
// drain treats it correctly. Nothing about waiting makes an unconfigured
// sender configured, and a retryable classification would park every message
// on an install that is never going to send one.
//
// Cause carries the sentinel through, so both questions a caller can ask --
// "may I retry this?" via email.IsPermanent, and "is this the log-only
// refusal?" via errors.Is -- are answerable from the one value.
func refusalSendError(stage string) error {
	err := RefuseLogOnly(stage)
	return &SendError{Permanent: true, Detail: err.Error(), Cause: err}
}
