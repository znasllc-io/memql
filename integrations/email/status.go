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
	Configured  string       `json:"configured"`
	Health      string       `json:"health"`
	Detail      string       `json:"detail"`
	Mode        string       `json:"mode"`
	Settings    []Setting    `json:"settings"`
	Credentials []Credential `json:"credentials"`
	Probed      bool         `json:"probed"`
}

// emailSlots names every env var the email lane reads, with the prose the
// portal renders beside it. Kept as one table so the report, the docs and the
// resolver cannot drift on what the lane actually consults.
type slotSpec struct {
	name    string
	envVar  string
	legacy  string
	purpose string
	secret  bool
}

func graphSlotSpecs() []slotSpec {
	keys := DefaultGraphEnvKeys()
	legacy := LegacyGraphEnvKeys()
	return []slotSpec{
		{name: "tenantId", envVar: keys.TenantId, legacy: legacy.TenantId, purpose: "Entra tenant the app registration lives in. An identifier, not a secret."},
		{name: "clientId", envVar: keys.ClientId, legacy: legacy.ClientId, purpose: "Application (client) id of the Entra app registration. An identifier, not a secret."},
		{name: "senderAddress", envVar: keys.SenderAddr, legacy: legacy.SenderAddr, purpose: "The mailbox mail is sent AS. Bound to the credential's Graph permission -- changing it is a deliverability decision."},
		{name: "fromName", envVar: keys.FromName, legacy: legacy.FromName, purpose: "Display name on the From header. Purely presentational; a campaign may override it per send."},
		{name: "clientSecret", envVar: keys.ClientSecret, legacy: legacy.ClientSecret, purpose: "Client-credentials secret for the app registration.", secret: true},
	}
}

func smtpSlotSpecs() []slotSpec {
	keys := DefaultEnvKeys()
	return []slotSpec{
		{name: "smtpHost", envVar: keys.Host, purpose: "Relay hostname."},
		{name: "smtpPort", envVar: keys.Port, purpose: "Relay port. Defaults to 587 (STARTTLS submission) when unset."},
		{name: "smtpUsername", envVar: keys.Username, purpose: "Relay username. An identifier, not a secret."},
		{name: "smtpFromAddress", envVar: keys.FromAddr, purpose: "Envelope and header From address."},
		{name: "smtpFromName", envVar: keys.FromName, purpose: "Display name on the From header."},
		{name: "smtpPassword", envVar: keys.Password, purpose: "Relay password.", secret: true},
	}
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
	// mode is what NewSenderFromEnv + LazySender.resolve would settle on for
	// this process, reproduced here rather than guessed. "graph" | "smtp" |
	// "log".
	mode string
	// modeSource says which lane supplied the winning configuration.
	modeSource string
}

// describer resolves slots from the two tiers the email lane reads, in the
// order the lane reads them.
type describer struct {
	reader        env.EnvReader
	resolveVar    VariableResolver
	resolveSecret SecretResolver
}

func newDescriber(sender Sender) *describer {
	d := &describer{reader: env.NewEnvReader("")}
	if lazy, ok := sender.(*LazySender); ok && lazy != nil {
		d.resolveVar = lazy.resolveVar
		d.resolveSecret = lazy.resolveSecret
	}
	return d
}

// fromEnv returns the first non-empty environment value across the primary
// and legacy names.
func (d *describer) fromEnv(names ...string) string {
	for _, name := range names {
		if name == "" {
			continue
		}
		if v, ok := d.reader.String(name); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// fromRows returns the first non-empty stored value. Variables and secrets
// are separate stores; `secret` picks which one is consulted.
func (d *describer) fromRows(ctx context.Context, secret bool, names ...string) string {
	var resolve func(context.Context, string) (string, error)
	switch {
	case secret && d.resolveSecret != nil:
		resolve = d.resolveSecret
	case !secret && d.resolveVar != nil:
		resolve = d.resolveVar
	default:
		return ""
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		if v, err := resolve(ctx, name); err == nil && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// slot resolves one slot to (value, source).
func (d *describer) slot(ctx context.Context, spec slotSpec) (string, string) {
	if v := d.fromEnv(spec.envVar, spec.legacy); v != "" {
		return v, SourceEnv
	}
	if v := d.fromRows(ctx, spec.secret, spec.envVar, spec.legacy); v != "" {
		if spec.secret {
			return v, SourceGlobalSecret
		}
		return v, SourceGlobalVariable
	}
	return "", SourceUnset
}

// resolve reproduces the sender-selection algorithm (NewSenderFromEnv then
// LazySender.resolve) and reports which lane wins.
//
// It is a REPRODUCTION rather than a call into the real path on purpose: the
// real path caches its answer behind a sync.Once on the first Send, and a
// status read must neither trigger that resolution early nor be answered by a
// cache that predates a credential the operator has since seeded.
func (d *describer) resolve(ctx context.Context) resolvedConfig {
	out := resolvedConfig{mode: "log", modeSource: SourceUnset}

	graphSpecs := graphSlotSpecs()
	values := make(map[string]string, len(graphSpecs))
	sources := make(map[string]string, len(graphSpecs))
	for _, spec := range graphSpecs {
		v, src := d.slot(ctx, spec)
		values[spec.name] = v
		sources[spec.name] = src
	}
	out.graph = GraphConfig{
		TenantId:     values["tenantId"],
		ClientId:     values["clientId"],
		ClientSecret: values["clientSecret"],
		SenderAddr:   values["senderAddress"],
		FromName:     values["fromName"],
	}

	smtpSpecs := smtpSlotSpecs()
	svalues := make(map[string]string, len(smtpSpecs))
	ssources := make(map[string]string, len(smtpSpecs))
	for _, spec := range smtpSpecs {
		v, src := d.slot(ctx, spec)
		svalues[spec.name] = v
		ssources[spec.name] = src
	}
	port := svalues["smtpPort"]
	if port == "" {
		port = "587"
	}
	out.smtp = SMTPConfig{
		Host:     svalues["smtpHost"],
		Port:     port,
		Username: svalues["smtpUsername"],
		Password: svalues["smtpPassword"],
		FromAddr: svalues["smtpFromAddress"],
		FromName: svalues["smtpFromName"],
	}

	// The four required Graph slots, all from one lane. The real resolver
	// tries env wholesale first and only then the stored rows, so a
	// configuration split across the two does NOT resolve -- and the report
	// has to say that rather than showing five present slots and a log-only
	// sender with no explanation.
	graphRequired := []string{"tenantId", "clientId", "clientSecret", "senderAddress"}
	if laneComplete(sources, graphRequired, SourceEnv) {
		out.mode, out.modeSource = "graph", SourceEnv
		return out
	}
	smtpRequired := []string{"smtpHost", "smtpFromAddress"}
	if laneComplete(ssources, smtpRequired, SourceEnv) {
		out.mode, out.modeSource = "smtp", SourceEnv
		return out
	}
	if laneComplete(sources, graphRequired, SourceGlobalVariable, SourceGlobalSecret) {
		out.mode, out.modeSource = "graph", SourceGlobalVariable
		return out
	}
	if laneComplete(ssources, smtpRequired, SourceGlobalVariable, SourceGlobalSecret) {
		out.mode, out.modeSource = "smtp", SourceGlobalVariable
		return out
	}
	return out
}

// laneComplete reports whether every required slot resolved, and did so from
// one of the allowed sources.
func laneComplete(sources map[string]string, required []string, allowed ...string) bool {
	for _, name := range required {
		src := sources[name]
		match := false
		for _, a := range allowed {
			if src == a {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}

// emailReport builds the email integration's line, optionally running a live
// probe. `probe` is opt-in because it costs a network round trip to a third
// party and an operations console re-reads on every navigation.
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
	}

	for _, spec := range append(graphSlotSpecs(), smtpSlotSpecs()...) {
		value, source := d.slot(ctx, spec)
		if spec.secret {
			report.Credentials = append(report.Credentials, Credential{
				Name:    spec.name,
				Present: source != SourceUnset,
				Source:  source,
				EnvVar:  spec.envVar,
				Purpose: spec.purpose,
				Rotate:  rotateCommand(spec.envVar),
			})
			continue
		}
		report.Settings = append(report.Settings, Setting{
			Name:    spec.name,
			Value:   value,
			Source:  source,
			EnvVar:  spec.envVar,
			Purpose: spec.purpose,
			// A setting the environment supplies cannot be overridden from
			// the graph -- the resolver reads env first and stops. Saying so
			// is the difference between a disabled field and a silent no-op.
			Editable: source != SourceEnv,
		})
	}

	if cfg.mode == "log" {
		report.Configured = AnswerNo
		report.Health = HealthDegraded
		report.Detail = "No sender is configured, so the integration is running in log-only mode: every send succeeds, writes a line to the node log, and delivers nothing. " +
			"Configure Microsoft Graph (tenant id, client id, client secret, sender address) or SMTP (host and from-address), all four from the same source -- the resolver takes a lane wholesale from the environment, or wholesale from stored rows, and will not mix them."
		return report
	}

	report.Configured = AnswerYes
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
	report.Detail += " " + detail
	return report
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
			Detail:       "Registered on this node. This integration publishes no configuration self-report, so whether its credentials resolved is not knowable from here.",
			Settings:     []Setting{},
			Credentials:  []Credential{},
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
// Owner and admin only. The reply carries no secret, but it does carry the
// operational shape of the deployment -- which mailbox it sends as, which
// relay it talks to, which credentials are seeded -- and the probe reaches
// out to a third party on the caller's say-so. Neither belongs to every
// authenticated reader. Builtins cannot express this gate in the DSL (the
// annotation set is @description/@enabled/@disabled/@executor/@alias/@args/
// @sdk, and the coarse gRPC data-plane gate classifies every builtin as a
// read), so Go is the only place it can live.
func statusAuthorized(ctx context.Context) error {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return fmt.Errorf("email.status: no authenticated caller")
	}
	if ac.Role == auth.RoleOwner || ac.Role == auth.RoleAdmin {
		return nil
	}
	return fmt.Errorf("email.status: role %q may not read integration configuration (owner or admin required)", string(ac.Role))
}
