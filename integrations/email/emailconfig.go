package email

// emailconfig.go -- the email integration's DECLARATION (memql#4825).
//
// This is the whole of what the email lane is configurable by. Three
// consumers read it and none of them holds a second copy:
//
//	NewSenderFromEnv   picks a Sender from the environment at plug-in
//	                   registration time
//	LazySender.resolve picks one from the stored rows on first use
//	(*Integration).emailReport  says what resolved, from where, and what is
//	                   missing -- walking the tiers itself rather than
//	                   calling either of the above, because their answer is
//	                   cached behind a sync.Once (status.go)
//
// Before this the list lived in two of those three, in different shapes, and
// the failure mode was a console showing every slot green beside a sender
// that had not resolved.
//
// Adding a value is now a change to ONE table, and the three consumers move
// with it. What is NOT free is teaching a lane how to build its client --
// senderFor below -- because that is where the values become a constructor's
// arguments, and no manifest can guess a constructor.

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// Slot names. Exported nowhere: a name is what the surfaces key on, and
// these constants exist so a typo is a compile error rather than a slot that
// silently resolves to "".
const (
	slotTenantID     = "tenantId"
	slotClientID     = "clientId"
	slotClientSecret = "clientSecret"
	slotSenderAddr   = "senderAddress"
	slotFromName     = "fromName"

	slotSMTPHost     = "smtpHost"
	slotSMTPPort     = "smtpPort"
	slotSMTPUsername = "smtpUsername"
	slotSMTPPassword = "smtpPassword"
	slotSMTPFrom     = "smtpFromAddress"
	slotSMTPFromName = "smtpFromName"

	// LaneGraph / LaneSMTP name the two ways this integration can be
	// configured. Also the values reported as the active MODE, which is why
	// they are exported: a surface renders them.
	LaneGraph = "graph"
	LaneSMTP  = "smtp"
)

// defaultSMTPPort is applied when the slot is unset. Declared here rather
// than as a slot default because a manifest default would have to be
// reported as a resolved VALUE, and "587, because nobody said otherwise" is
// not something the operator configured.
const defaultSMTPPort = "587"

// EmailConfigManifest is the declaration.
//
// Lane ORDER is preference order: Graph first, because it is the recommended
// transport and the only one that can send as more than one identity.
func EmailConfigManifest() ConfigManifest {
	keys := DefaultGraphEnvKeys()
	legacy := LegacyGraphEnvKeys()
	smtp := DefaultEnvKeys()
	return ConfigManifest{
		Integration: ComponentName,
		Lanes: []ConfigLane{
			{
				Name: LaneGraph,
				Description: "Microsoft Graph. The recommended transport: it authenticates as an Entra app registration rather than " +
					"as a mailbox password, and it is the only lane that can send as more than one identity.",
				Slots: []ConfigSlot{
					{Name: slotTenantID, EnvVar: keys.TenantId, Legacy: legacy.TenantId, Required: true,
						Purpose: "Entra tenant the app registration lives in. An identifier, not a secret."},
					{Name: slotClientID, EnvVar: keys.ClientId, Legacy: legacy.ClientId, Required: true,
						Purpose: "Application (client) id of the Entra app registration. An identifier, not a secret."},
					{Name: slotSenderAddr, EnvVar: keys.SenderAddr, Legacy: legacy.SenderAddr, Required: true,
						Purpose: "The mailbox mail is sent AS, and the mailbox the delivery-report reader reads. Bound to the credential's Graph permissions -- changing it is a deliverability decision."},
					{Name: slotFromName, EnvVar: keys.FromName, Legacy: legacy.FromName,
						Purpose: "Display name on the From header. Purely presentational; a campaign may override it per send."},
					{Name: slotClientSecret, EnvVar: keys.ClientSecret, Legacy: legacy.ClientSecret, Required: true, Secret: true,
						Purpose: "Client-credentials secret for the app registration."},
				},
			},
			{
				Name: LaneSMTP,
				Description: "A plain SMTP relay over STARTTLS. Single-identity by construction: AUTH binds the connection to one " +
					"mailbox, so a campaign naming a different sending identity is refused here.",
				Slots: []ConfigSlot{
					{Name: slotSMTPHost, EnvVar: smtp.Host, Required: true, Purpose: "Relay hostname."},
					{Name: slotSMTPPort, EnvVar: smtp.Port, Purpose: "Relay port. Defaults to " + defaultSMTPPort + " (STARTTLS submission) when unset."},
					{Name: slotSMTPUsername, EnvVar: smtp.Username, Purpose: "Relay username. An identifier, not a secret."},
					{Name: slotSMTPFrom, EnvVar: smtp.FromAddr, Required: true, Purpose: "Envelope and header From address."},
					{Name: slotSMTPFromName, EnvVar: smtp.FromName, Purpose: "Display name on the From header."},
					{Name: slotSMTPPassword, EnvVar: smtp.Password, Secret: true, Purpose: "Relay password."},
				},
			},
		},
	}
}

// graphSlotSpecs / smtpSlotSpecs are the manifest's two lanes, kept as named
// helpers because reading "the Graph slots" at a call site is clearer than
// indexing a lane list, and because a test sweeping every env var the lane
// consults wants exactly this.
func graphSlotSpecs() []ConfigSlot { return laneSlots(LaneGraph) }
func smtpSlotSpecs() []ConfigSlot  { return laneSlots(LaneSMTP) }

func laneSlots(name string) []ConfigSlot {
	lane, ok := EmailConfigManifest().Lane(name)
	if !ok {
		return nil
	}
	return lane.Slots
}

// envConfigResolver reads the environment only. The tier NewSenderFromEnv
// runs at plug-in registration time, before any graph row can be read.
func envConfigResolver() ConfigResolver {
	return ConfigResolver{Env: os.LookupEnv}
}

// graphConfigFrom / smtpConfigFrom turn a resolution into a constructor's
// arguments. The one part of the collapse a manifest cannot do for you: the
// slot names are declared above, but which field of GraphConfig each one
// fills is knowledge only this integration has.
func graphConfigFrom(res ConfigResolution) GraphConfig {
	return GraphConfig{
		TenantId:     res.Value(slotTenantID),
		ClientId:     res.Value(slotClientID),
		ClientSecret: res.Value(slotClientSecret),
		SenderAddr:   res.Value(slotSenderAddr),
		FromName:     res.Value(slotFromName),
	}
}

func smtpConfigFrom(res ConfigResolution) SMTPConfig {
	port := res.Value(slotSMTPPort)
	if strings.TrimSpace(port) == "" {
		port = defaultSMTPPort
	}
	return SMTPConfig{
		Host:     res.Value(slotSMTPHost),
		Port:     port,
		Username: res.Value(slotSMTPUsername),
		Password: res.Value(slotSMTPPassword),
		FromAddr: res.Value(slotSMTPFrom),
		FromName: res.Value(slotSMTPFromName),
	}
}

// senderFor builds the concrete Sender the resolution's ACTIVE lane names,
// or nil when no lane resolved whole.
//
// nil rather than a LogSender, deliberately: what to do when nothing resolved
// is a policy question with two different answers on two different installs
// (delivery.go), and it belongs to the caller that knows which stage it is at
// -- boot refuses, a send refuses, a status read reports. A helper that
// quietly substituted a LogSender would take that decision away from all
// three.
func senderFor(res ConfigResolution, logger *slog.Logger) Sender {
	switch res.Active {
	case LaneGraph:
		cfg := graphConfigFrom(res)
		if logger != nil {
			logger.Info("email: using Microsoft Graph sender",
				"sender", cfg.SenderAddr, "tenantId", cfg.TenantId, "configuredFrom", res.ActiveSource)
		}
		return NewGraphSender(cfg, nil, logger)
	case LaneSMTP:
		cfg := smtpConfigFrom(res)
		if logger != nil {
			logger.Info("email: using SMTP sender",
				"host", cfg.Host, "fromAddr", cfg.FromAddr, "configuredFrom", res.ActiveSource)
		}
		return NewSMTPSender(cfg, logger)
	default:
		return nil
	}
}

// resolveEmailConfig is the one-line form every consumer uses.
func resolveEmailConfig(ctx context.Context, r ConfigResolver) ConfigResolution {
	return EmailConfigManifest().Resolve(ctx, r)
}
