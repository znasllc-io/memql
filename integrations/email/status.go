package email

// status.go -- the email integration's self-report (memql#3323).
//
// The portal needs to answer two DIFFERENT questions about every integration
// and must never conflate them:
//
//	CONFIGURED  does this node have enough settings and credentials to do
//	            the job at all? Answerable from what is present, with no
//	            network call, and it is a boolean fact about the process.
//	HEALTHY     does the thing on the other end actually accept us? Only a
//	            round trip can answer that, and a "yes" goes stale.
//
// Keeping them apart matters here more than it would elsewhere, because the
// email integration DEGRADES RATHER THAN FAILING. With no credentials at all
// NewSenderFromEnv hands back a LogSender: every send returns nil, the log
// records a line, and nothing is delivered. A surface that only asked "did
// sendEmail error?" would report a perfectly healthy integration that has
// never delivered a message. So log-only is reported as its own state --
// configured=no, health=degraded -- and said out loud.
//
// # The no-secrets invariant
//
// Nothing this file produces may contain a credential value. Not the Graph
// client secret, not the SMTP password, not the ciphertext of either. What it
// reports instead, per credential slot, is:
//
//	present      whether a value resolved at all
//	source       env / globalVariable / globalSecret / unset -- WHERE it came
//	             from, which is what an operator needs to know to change it
//	envVar       the environment variable that supplies it
//	rotate       the command that rotates it
//
// The invariant is not left to reviewer attention: probe output is passed
// through redactSecrets before it leaves, and status_test.go plants
// recognisable values in every credential slot, serializes the entire reply,
// and fails if any of them appears anywhere in it.
//
// # Why the settings are separable from the secrets at all
//
// This is the whole reason "configurable from the portal" is possible without
// a secret-writing path. The lazy resolver (lazy.go) already reads the
// non-secret half of the configuration out of v1:platform:globalVariable
// rows -- sender address, from-name, tenant id, client id, SMTP host / port /
// username -- and only the client secret and the SMTP password out of
// v1:platform:globalSecret. Those rows are ordinary graph state with an
// existing mutation (setGlobalVariable), so the portal can edit the non-secret
// half through a path that already exists, and the secret half stays where it
// is: sealed, write-only from the operator's shell.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/env"
)

// Slot sources. A slot's source names where the value came from, which is
// what decides how an operator changes it.
const (
	// SourceUnset -- no value anywhere.
	SourceUnset = "unset"
	// SourceEnv -- an OS environment variable, i.e. the bootstrap envelope.
	// Changing it is a redeploy.
	SourceEnv = "env"
	// SourceGlobalVariable -- a v1:platform:globalVariable row. Plaintext,
	// editable from the portal.
	SourceGlobalVariable = "globalVariable"
	// SourceGlobalSecret -- a v1:platform:globalSecret row. Sealed under
	// MEMQL_MASTER_KEY; readable only by the resolver, never by a client.
	SourceGlobalSecret = "globalSecret"
)

// Tri-state answers. Strings rather than booleans because "we do not know"
// is a real and common answer -- most integrations publish no self-report --
// and a boolean would force it to be rendered as "no".
const (
	AnswerYes     = "yes"
	AnswerNo      = "no"
	AnswerUnknown = "unknown"
)

// Health verdicts.
const (
	// HealthUnknown -- nobody has asked. The honest default: an integration
	// that has never been probed and has never been used has no health.
	HealthUnknown = "unknown"
	// HealthHealthy -- a live probe succeeded.
	HealthHealthy = "healthy"
	// HealthUnhealthy -- a live probe failed.
	HealthUnhealthy = "unhealthy"
	// HealthDegraded -- running, answering, and not doing the job. The
	// log-only sender is the whole reason this state exists.
	HealthDegraded = "degraded"
)

// Setting is one NON-SECRET configuration value. Its value is rendered.
type Setting struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Source  string `json:"source"`
	EnvVar  string `json:"envVar"`
	Purpose string `json:"purpose"`
	// Lane names which way-of-being-configured this slot belongs to
	// ("graph" / "smtp"). A settings surface groups by it: showing eleven
	// flat fields invites an operator to fill in half of each lane, which is
	// the one arrangement that resolves to nothing.
	Lane string `json:"lane"`
	// Required reports whether the lane can work without it.
	Required bool `json:"required"`
	// Reason is the sentence to render under the field when something is
	// wrong with it, and empty when nothing is.
	Reason string `json:"reason,omitempty"`
	// Editable reports whether the portal may write this setting through
	// setGlobalVariable. False for a value the resolver only ever reads out
	// of the environment.
	Editable bool `json:"editable"`
}

// Credential is one SECRET slot. It carries no value and never will --
// see the file header.
type Credential struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
	Source  string `json:"source"`
	EnvVar  string `json:"envVar"`
	Purpose string `json:"purpose"`
	// Lane / Required / Reason carry the same meaning they do on Setting.
	Lane     string `json:"lane"`
	Required bool   `json:"required"`
	Reason   string `json:"reason,omitempty"`
	// Rotate is the operator command that changes this credential. The
	// portal shows it instead of offering a field, because there is
	// deliberately no path from a browser to a secret write.
	Rotate string `json:"rotate"`
}

// IntegrationReport is one integration's line in the registry roll-call.
type IntegrationReport struct {
	Name string `json:"name"`
	// Registered is always true for a row that appears at all -- the list is
	// built from the plug-in registry. Carried explicitly so the UI can say
	// "registered" without inferring it from presence.
	Registered   bool     `json:"registered"`
	Capabilities []string `json:"capabilities"`
	// Configured / Health are AnswerUnknown / HealthUnknown for every
	// integration that publishes no self-report, which today is all of them
	// except email. That is reported as ignorance, never as a failure.
	Configured string `json:"configured"`
	Health     string `json:"health"`
	// State is the SINGLE machine-readable verdict a surface paints a badge
	// from: needs_configuration | configured | unhealthy (memql#4825). It is
	// not a third opinion beside Configured and Health -- it is the two
	// combined, because "configured but the provider refuses us" is neither
	// of those words on its own. AnswerUnknown-equivalent is the EMPTY
	// string: an integration that publishes no self-report has no state, and
	// an unknown value must be read as unknown rather than as any member of
	// the set.
	State string `json:"state"`
	// Reasons is why State is not `configured`, one entry per thing a person
	// would have to do. Empty when there is nothing to say.
	Reasons     []ConfigReason `json:"reasons"`
	Detail      string         `json:"detail"`
	Mode        string         `json:"mode"`
	Settings    []Setting      `json:"settings"`
	Credentials []Credential   `json:"credentials"`
	Probed      bool           `json:"probed"`
}

// rotateCommand is the operator instruction the portal shows next to a secret
// slot. There is no browser path to a secret write and there is not going to
// be one, so the honest thing to render is the command that does the job.
func rotateCommand(envVar string) string {
	return "make secret-set NAME=" + envVar + " VALUE=<new value> SCOPE=global"
}

// resolvedConfig is the internal, VALUE-BEARING resolution. It never leaves
// this file: the probe reads it, the report is built from presence alone.
type resolvedConfig struct {
	graph GraphConfig
	smtp  SMTPConfig
	// resolution is the manifest walk this was folded from -- kept so the
	// report can name what is MISSING as well as what resolved, which is the
	// half a value-only struct cannot carry.
	resolution ConfigResolution
	// mode is what NewSenderFromEnv + LazySender.resolve would settle on for
	// this process, reproduced here rather than guessed. "graph" | "smtp" |
	// "log".
	mode string
	// modeSource says which lane supplied the winning configuration.
	modeSource string
}

// describer resolves the manifest from the two tiers the email lane reads,
// in the order the lane reads them.
type describer struct {
	resolver ConfigResolver
}

func newDescriber(sender Sender) *describer {
	reader := env.NewEnvReader("")
	d := &describer{resolver: ConfigResolver{
		Env: func(name string) (string, bool) { return reader.String(name) },
	}}
	if lazy, ok := sender.(*LazySender); ok && lazy != nil {
		d.resolver.Vars = lazy.resolveVar
		d.resolver.Secrets = lazy.resolveSecret
	}
	return d
}

// slot resolves one slot to (value, source).
func (d *describer) slot(ctx context.Context, spec ConfigSlot) (string, string) {
	return d.resolver.ResolveSlot(ctx, spec)
}

// resolve reproduces the sender-selection algorithm (NewSenderFromEnv then
// LazySender.resolve) and reports which lane wins.
//
// It is a REPRODUCTION rather than a call into the real path on purpose: the
// real path caches its answer behind a sync.Once on the first Send, and a
// status read must neither trigger that resolution early nor be answered by a
// cache that predates a credential the operator has since seeded.
//
// What it no longer reproduces is the LIST (memql#4825). Both walks now run
// over EmailConfigManifest, so the reporter and the resolver cannot disagree
// about which slots exist, which are secret, or which a lane requires --
// while the walk itself, which is the part that had to stay separate, still
// is.
func (d *describer) resolve(ctx context.Context) resolvedConfig {
	res := resolveEmailConfig(ctx, d.resolver)
	out := resolvedConfig{
		resolution: res,
		graph:      graphConfigFrom(res),
		smtp:       smtpConfigFrom(res),
		mode:       "log",
		modeSource: SourceUnset,
	}
	if res.Active != "" {
		out.mode, out.modeSource = res.Active, res.ActiveSource
	}
	return out
}

// emailReport builds the email integration's line, optionally running a live
// probe. `probe` is opt-in because it costs a network round trip to a third
// party and an operations console re-reads on every navigation.
//
// # State and reasons (memql#4825)
//
// The report carries a machine-readable State and, when it is not
// StateConfigured, the REASONS -- one per required slot that resolved
// nowhere, plus one per lane whose values are split across tiers. An OS
// Settings surface renders from those rather than from the prose Detail,
// because prose is for a person to read and a control that highlights the
// field responsible needs the field's name.
//
// Configured / Health survive alongside State and are not redundant with it.
// Configured answers "is there enough to work with", Health answers "does the
// far end accept us", and State is the SINGLE verdict a surface paints a
// badge from -- which needs the two combined, since a configured integration
// whose probe failed is neither of the two words on its own.
func (i *Integration) emailReport(ctx context.Context, probe bool) IntegrationReport {
	d := newDescriber(i.sender)
	cfg := d.resolve(ctx)

	report := IntegrationReport{
		Name:         "email",
		Registered:   true,
		Capabilities: capabilityNames(i.Capabilities()),
		Mode:         cfg.mode,
		Probed:       probe,
		Settings:     []Setting{},
		Credentials:  []Credential{},
		Reasons:      cfg.resolution.Reasons(),
	}
	if report.Reasons == nil {
		report.Reasons = []ConfigReason{}
	}

	for _, lane := range cfg.resolution.Lanes {
		for _, resolved := range lane.Slots {
			spec := resolved.Slot
			if spec.Secret {
				report.Credentials = append(report.Credentials, Credential{
					Name:     spec.Name,
					Present:  resolved.Source != SourceUnset,
					Source:   resolved.Source,
					EnvVar:   spec.EnvVar,
					Purpose:  spec.Purpose,
					Lane:     lane.Lane.Name,
					Required: spec.Required,
					Reason:   slotReason(lane, resolved),
					Rotate:   rotateCommand(spec.EnvVar),
				})
				continue
			}
			report.Settings = append(report.Settings, Setting{
				Name:     spec.Name,
				Value:    resolved.Value,
				Source:   resolved.Source,
				EnvVar:   spec.EnvVar,
				Purpose:  spec.Purpose,
				Lane:     lane.Lane.Name,
				Required: spec.Required,
				Reason:   slotReason(lane, resolved),
				// A setting the environment supplies cannot be overridden from
				// the graph -- the resolver reads env first and stops. Saying so
				// is the difference between a disabled field and a silent no-op.
				Editable: resolved.Source != SourceEnv,
			})
		}
	}

	if cfg.mode == "log" {
		report.Configured = AnswerNo
		report.State = StateNeedsConfiguration
		// Two different facts share this one resolved mode, and the console
		// must not render them the same way (memql#4477). On a local install
		// log-only is what was asked for, and DEGRADED -- running, answering,
		// not delivering -- is the honest word. On an install that must
		// really deliver mail every send is now REFUSED, which is a broken
		// integration, and an amber row there reads as a known dev posture.
		if DeliveryRequired() {
			report.Health = HealthUnhealthy
			// UNHEALTHY, not needs_configuration. The distinction is the
			// whole of memql#4477: this install is not merely unconfigured,
			// it is actively refusing every send, and a surface that painted
			// it the same amber as a fresh cluster would say "finish setup"
			// about a cluster whose mail is broken.
			report.State = StateUnhealthy
			report.Reasons = append(report.Reasons, ConfigReason{
				Code:   ReasonRefused,
				Detail: RefuseLogOnly("send").Error(),
			})
			report.Detail = "No sender is configured and this install must deliver mail, so every send is refused rather than written to the log. " +
				RefuseLogOnly("send").Error() + ". " +
				"Both lanes must resolve from ONE source -- the resolver takes Graph or SMTP wholesale from the environment, or wholesale from stored rows, and will not mix them."
			return report
		}
		report.Health = HealthDegraded
		report.Detail = "No sender is configured, so the integration is running in log-only mode: every send succeeds, writes a line to the node log, and delivers nothing. " +
			"Configure Microsoft Graph (tenant id, client id, client secret, sender address) or SMTP (host and from-address), all four from the same source -- the resolver takes a lane wholesale from the environment, or wholesale from stored rows, and will not mix them."
		return report
	}

	report.Configured = AnswerYes
	report.State = StateConfigured
	report.Detail = fmt.Sprintf("Sending via %s, configured from %s.", cfg.mode, cfg.modeSource)
	if !probe {
		report.Health = HealthUnknown
		report.Detail += " Configuration is present; whether the provider accepts these credentials has not been checked. Run a check to find out."
		return report
	}

	ok, detail := runProbe(ctx, cfg)
	// Belt and braces: a provider error message is third-party text and the
	// only place a credential could plausibly be echoed back at us.
	detail = redactSecrets(detail, cfg.graph.ClientSecret, cfg.smtp.Password)
	if ok {
		report.Health = HealthHealthy
		report.Detail += " " + detail
		return report
	}
	report.Health = HealthUnhealthy
	report.State = StateUnhealthy
	report.Reasons = append(report.Reasons, ConfigReason{
		Code:   ReasonProbeFailed,
		Lane:   cfg.mode,
		Detail: detail,
	})
	report.Detail += " " + detail
	return report
}

// slotReason is the per-slot sentence a settings surface renders under the
// field. Empty for a slot that is fine, and empty for an OPTIONAL slot that
// is unset -- an optional value nobody set is a normal state, and marking it
// would train an operator to ignore the marks that mean something.
//
// It is built from the slot's DECLARATION only. Never from its value: a
// reason about a secret that quoted the secret would put it on the wire,
// which is the one thing this surface may not do (see the file header, and
// TestStatusNeverLeaksACredential, which sweeps the whole serialized reply).
func slotReason(lane LaneResolution, resolved SlotResolution) string {
	if !resolved.Slot.Required || resolved.Source != SourceUnset {
		if lane.Split && resolved.Slot.Required {
			return fmt.Sprintf("Present, but the %s lane's values are not all in the same place, so none of them is used. "+
				"Move them together: wholly into the environment, or wholly into stored settings.", lane.Lane.Name)
		}
		return ""
	}
	return fmt.Sprintf("Required by the %s lane and not set anywhere. Set %s.", lane.Lane.Name, resolved.Slot.EnvVar)
}

// runProbe performs a live reachability check that DOES NOT SEND MAIL.
//
// For Graph that is a client-credentials token acquisition: it exercises the
// tenant, the client id and the secret against the real authority, which is
// the whole credential chain short of the mailbox permission. For SMTP it is
// connect / EHLO / STARTTLS / AUTH / QUIT -- the same handshake a send does,
// stopping before MAIL FROM.
//
// Sending a test message would be a stronger check and is deliberately not
// done: it needs a recipient, that recipient is a real person's inbox, and a
// health check that mails somebody every time an operator opens a page is a
// worse failure than not knowing.
func runProbe(ctx context.Context, cfg resolvedConfig) (bool, string) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	switch cfg.mode {
	case "graph":
		sender := NewGraphSender(cfg.graph, &http.Client{Timeout: probeTimeout}, nil)
		if _, err := sender.getToken(probeCtx); err != nil {
			return false, "Live check failed: the Entra token endpoint refused these credentials -- " + err.Error()
		}
		return true, "Live check passed: Entra issued a Graph token for this app registration. That proves the tenant, client id and secret; it does not prove the mailbox permission, which only a real send exercises."
	case "smtp":
		if err := probeSMTP(probeCtx, cfg.smtp); err != nil {
			return false, "Live check failed: the relay handshake did not complete -- " + err.Error()
		}
		return true, "Live check passed: the relay accepted a connection, STARTTLS and authentication. No message was sent."
	default:
		return false, "Nothing to check: no sender is configured."
	}
}

// probeTimeout bounds one live check. Short on purpose -- this runs behind an
// operator clicking a button and a slow answer is worse than a retry.
const probeTimeout = 10 * time.Second

func probeSMTP(ctx context.Context, cfg SMTPConfig) error {
	addr := net.JoinHostPort(cfg.Host, cfg.Port)
	dialer := net.Dialer{Timeout: probeTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp greeting from %s: %w", cfg.Host, err)
	}
	defer func() { _ = client.Close() }()

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("starttls with %s: %w", cfg.Host, err)
		}
	}
	if cfg.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fmt.Errorf("authenticate as %s: %w", cfg.Username, err)
		}
	}
	return client.Quit()
}

// redactSecrets removes any of the given values from s. Values shorter than
// eight characters are skipped: a short "secret" is far more likely to be a
// substring of ordinary prose ("587", "true") than a real credential, and
// blanking those would corrupt the diagnostic without protecting anything.
func redactSecrets(s string, secrets ...string) string {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if len(secret) < 8 {
			continue
		}
		s = strings.ReplaceAll(s, secret, "<redacted>")
	}
	return s
}

func capabilityNames(caps []memql.IntegrationCapability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, c.Name)
	}
	return out
}

// registryRollCall lists every plug-in this binary registered.
//
// The list comes from memql.RegisteredPlugins() -- the init()-time factory
// registry -- because that is the honest answer to "what does this NODE
// have". Which plug-ins compile in is a build-tag decision, so the roll-call
// differs per node type, and a bff and an agent replica legitimately give
// different answers.
//
// WHAT IT DOES NOT COVER, stated rather than implied: the handful of
// integrations wired explicitly in app/integrations_*.go (cognition, agent,
// stt) because their dependencies sit outside PluginContext. They are not in
// this registry and therefore not in this list.
//
// Every entry except email reports configured/health as unknown. That is not
// a gap being papered over -- it is the accurate answer until an integration
// publishes a self-report of its own, and rendering it as "unknown" rather
// than "no" is exactly the configured-vs-working distinction this surface
// exists to keep straight.
func registryRollCall(emailLine IntegrationReport) []IntegrationReport {
	out := []IntegrationReport{}
	for _, reg := range memql.RegisteredPlugins() {
		if reg.Name == "email" {
			continue
		}
		out = append(out, IntegrationReport{
			Name:         reg.Name,
			Registered:   true,
			Capabilities: []string{},
			Configured:   AnswerUnknown,
			Health:       HealthUnknown,
			// No State, deliberately. The closed set has no member meaning
			// "nobody asked", and inventing one here would let a surface
			// paint a badge over an integration that has published nothing.
			// The empty string is what a renderer must treat as unknown.
			Detail:      "Registered on this node. This integration publishes no configuration self-report, so whether its credentials resolved is not knowable from here.",
			Reasons:     []ConfigReason{},
			Settings:    []Setting{},
			Credentials: []Credential{},
		})
	}
	// Sorted by name with email first: email is the one line with an answer,
	// and a stable order keeps the list from reshuffling between reads.
	sortReportsByName(out)
	return append([]IntegrationReport{emailLine}, out...)
}

func sortReportsByName(reports []IntegrationReport) {
	for i := 1; i < len(reports); i++ {
		for j := i; j > 0 && reports[j].Name < reports[j-1].Name; j-- {
			reports[j], reports[j-1] = reports[j-1], reports[j]
		}
	}
}

// statusAuthorized fails closed. A nil AccessContext means the auth
// middleware never ran or never resolved a caller, and the answer to "who is
// asking" is then nobody -- which must not be treated as a trusted internal
// call (component/auth's ActorEnvelopeMap makes the same choice, memql#2801).
//
// Owner, developer and admin. The reply carries no secret, but it does carry the
// operational shape of the deployment -- which mailbox it sends as, which
// relay it talks to, which credentials are seeded -- and the probe reaches
// out to a third party on the caller's say-so. Neither belongs to every
// authenticated reader.
//
// DEVELOPER joined them for memql#4826, and its absence was a real defect
// rather than a tightening. Program decision P6 gates integration
// configuration owner-or-developer -- wiring up what the cluster talks to is
// a developer's concern, administering PEOPLE is an admin's -- and the OS
// Settings section that renders this report declares exactly that role set.
// So the role the section exists FOR was the one role the engine refused, and
// the section would have rendered a refusal to its intended audience while
// serving somebody it does not offer itself to.
//
// Admin is KEPT rather than removed to match P6 exactly, and the asymmetry is
// deliberate: P6 is about who may CONFIGURE, and this is a read that carries
// no secret. Narrowing an existing read to make a sentence symmetrical would
// take a capability away from every admin in every deployment to fix a
// documentation shape, and the OS surface already declines to offer the
// section to them.
//
// Builtins cannot express this gate in the DSL (the
// annotation set is @description/@enabled/@disabled/@executor/@alias/@args/
// @sdk, and the coarse gRPC data-plane gate classifies every builtin as a
// read), so Go is the only place it can live.
func statusAuthorized(ctx context.Context) error {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return fmt.Errorf("email.status: no authenticated caller")
	}
	if ac.Role == auth.RoleOwner || ac.Role == auth.RoleAdmin || ac.Role == auth.RoleDeveloper {
		return nil
	}
	return fmt.Errorf("email.status: role %q may not read integration configuration (owner, developer or admin required)", string(ac.Role))
}
