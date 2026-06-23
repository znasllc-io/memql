package genesis

import (
	"log/slog"
	"os"
	"sort"
)

// LegacyAliases maps each NEW (MEMQL_-prefixed) env var name to the
// LEGACY name it replaced in Epic 7 workstream 7.3 (memql#2106). It is
// the inverse of scripts/rename/env_rename_rules.json (legacy -> new).
//
// The boot-time shim ApplyLegacyEnvAliases consults this map to keep an
// operator's pre-7.3 .env / sealed envelope / k8s Secret working: when a
// new name is unset but its legacy alias is present, the legacy value is
// copied onto the new name (set-if-absent, new wins).
//
// IMPORTANT: the legacy names appear here ONLY as map VALUES (string
// literals), never as a literal argument to os.Getenv / os.LookupEnv.
// The envscan forward-drift scanner keys off those call sites with a
// literal argument, so the legacy names are not treated as code reads --
// they are documented as deprecated registry aliases (manifest.yaml
// legacy-alias section) and resolved here via the loop variable.
var LegacyAliases = map[string]string{
	"MEMQL_AI_ANTHROPIC_API_KEY":                            "MEMQL_SI_ANTHROPIC_API_KEY",
	"MEMQL_AI_OPENAI_API_KEY":                               "MEMQL_SI_OPENAI_API_KEY",
	"MEMQL_AI_OPENAI_PROJECT_ID":                            "MEMQL_SI_OPENAI_PROJECT_ID",
	"MEMQL_ANAM_API_KEY":                                    "ANAM_API_KEY",
	"MEMQL_ANTHROPIC_API_KEY":                               "ANTHROPIC_API_KEY",
	"MEMQL_DATABASE_DSN":                                    "MEMORY_NODES_DATABASE_DSN",
	"MEMQL_DEFAULT_AGENT_PROVIDER":                          "DEFAULT_AGENT_PROVIDER",
	"MEMQL_EMAIL_AZURE_CLIENT_ID":                           "EMAIL_AZURE_CLIENT_ID",
	"MEMQL_EMAIL_AZURE_CLIENT_SECRET":                       "EMAIL_AZURE_CLIENT_SECRET",
	"MEMQL_EMAIL_AZURE_TENANT_ID":                           "EMAIL_AZURE_TENANT_ID",
	"MEMQL_EMAIL_FROM_NAME":                                 "EMAIL_FROM_NAME",
	"MEMQL_EMAIL_SENDER":                                    "EMAIL_SENDER",
	"MEMQL_EMAIL_SUPPRESS_OWNER_BOOTSTRAP":                  "MAIL_SUPPRESS_OWNER_BOOTSTRAP",
	"MEMQL_IDENTITY_ACCESS_REQUEST_NOTIFY_EMAILS":           "IDENTITY_ACCESS_REQUEST_NOTIFY_EMAILS",
	"MEMQL_IDENTITY_ACCESS_REQUEST_NOTIFY_THROTTLE_MINUTES": "IDENTITY_ACCESS_REQUEST_NOTIFY_THROTTLE_MINUTES",
	"MEMQL_IDENTITY_ACCESS_TOKEN_TTL_SECONDS":               "IDENTITY_ACCESS_TOKEN_TTL_SECONDS",
	"MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY":                    "IDENTITY_ALLOW_EPHEMERAL_KEY",
	"MEMQL_IDENTITY_AUDIT_LOG_RETENTION_DAYS":               "IDENTITY_AUDIT_LOG_RETENTION_DAYS",
	"MEMQL_IDENTITY_BASE_URL":                               "IDENTITY_BASE_URL",
	"MEMQL_IDENTITY_BOOTSTRAP_DOMAIN":                       "IDENTITY_BOOTSTRAP_DOMAIN",
	"MEMQL_IDENTITY_BOOTSTRAP_INTERNAL_DEFAULT_ROLE":        "IDENTITY_BOOTSTRAP_INTERNAL_DEFAULT_ROLE",
	"MEMQL_IDENTITY_BOOTSTRAP_INTERNAL_DOMAINS":             "IDENTITY_BOOTSTRAP_INTERNAL_DOMAINS",
	"MEMQL_IDENTITY_BOOTSTRAP_NOTIFY_EMAILS":                "IDENTITY_BOOTSTRAP_NOTIFY_EMAILS",
	"MEMQL_IDENTITY_BOOTSTRAP_ORG_NAME":                     "IDENTITY_BOOTSTRAP_ORG_NAME",
	"MEMQL_IDENTITY_BOOTSTRAP_OWNER_BIRTHDATE":              "IDENTITY_BOOTSTRAP_OWNER_BIRTHDATE",
	"MEMQL_IDENTITY_BOOTSTRAP_OWNER_EMAIL":                  "IDENTITY_BOOTSTRAP_OWNER_EMAIL",
	"MEMQL_IDENTITY_BOOTSTRAP_OWNER_FIRST_NAME":             "IDENTITY_BOOTSTRAP_OWNER_FIRST_NAME",
	"MEMQL_IDENTITY_BOOTSTRAP_OWNER_GENDER":                 "IDENTITY_BOOTSTRAP_OWNER_GENDER",
	"MEMQL_IDENTITY_BOOTSTRAP_OWNER_LAST_NAME":              "IDENTITY_BOOTSTRAP_OWNER_LAST_NAME",
	"MEMQL_IDENTITY_BOOTSTRAP_OWNER_PHONE":                  "IDENTITY_BOOTSTRAP_OWNER_PHONE",
	"MEMQL_IDENTITY_BOOTSTRAP_OWNER_PRIMARY_ROLE":           "IDENTITY_BOOTSTRAP_OWNER_PRIMARY_ROLE",
	"MEMQL_IDENTITY_BOOTSTRAP_REGISTRATION_DOMAINS":         "IDENTITY_BOOTSTRAP_REGISTRATION_DOMAINS",
	"MEMQL_IDENTITY_BOOTSTRAP_REGISTRATION_MODE":            "IDENTITY_BOOTSTRAP_REGISTRATION_MODE",
	"MEMQL_IDENTITY_BRAND_ICON_DATA_URI":                    "IDENTITY_BRAND_ICON_DATA_URI",
	"MEMQL_IDENTITY_BRAND_LOGO_DATA_URI":                    "IDENTITY_BRAND_LOGO_DATA_URI",
	"MEMQL_IDENTITY_BRAND_NAME":                             "IDENTITY_BRAND_NAME",
	"MEMQL_IDENTITY_BRAND_PRIMARY_COLOR":                    "IDENTITY_BRAND_PRIMARY_COLOR",
	"MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS":                   "IDENTITY_CORS_ALLOWED_ORIGINS",
	"MEMQL_IDENTITY_DATA_EXPORT_RATE_LIMIT_HOURS":           "IDENTITY_DATA_EXPORT_RATE_LIMIT_HOURS",
	"MEMQL_IDENTITY_DELETION_COOLDOWN_DAYS":                 "IDENTITY_DELETION_COOLDOWN_DAYS",
	"MEMQL_IDENTITY_DISPOSABLE_EMAIL_BLOCKLIST_ENABLED":     "IDENTITY_DISPOSABLE_EMAIL_BLOCKLIST_ENABLED",
	"MEMQL_IDENTITY_EMAIL_VALIDATION_PROVIDER":              "IDENTITY_EMAIL_VALIDATION_PROVIDER",
	"MEMQL_IDENTITY_ENABLED":                                "IDENTITY_ENABLED",
	"MEMQL_IDENTITY_INTERNAL_DEFAULT_ROLE":                  "IDENTITY_INTERNAL_DEFAULT_ROLE",
	"MEMQL_IDENTITY_INTERNAL_DOMAINS":                       "IDENTITY_INTERNAL_DOMAINS",
	"MEMQL_IDENTITY_INVITATION_TTL_DAYS":                    "IDENTITY_INVITATION_TTL_DAYS",
	"MEMQL_IDENTITY_JWKS_OVERLAP_HOURS":                     "IDENTITY_JWKS_OVERLAP_HOURS",
	"MEMQL_IDENTITY_JWKS_URL":                               "IDENTITY_JWKS_URL",
	"MEMQL_IDENTITY_JWT_AUDIENCE":                           "IDENTITY_JWT_AUDIENCE",
	"MEMQL_IDENTITY_KEY_DIR":                                "IDENTITY_KEY_DIR",
	"MEMQL_IDENTITY_KEY_ENCRYPTION_KEY":                     "IDENTITY_KEY_ENCRYPTION_KEY",
	"MEMQL_IDENTITY_KEY_ROTATION_DAYS":                      "IDENTITY_KEY_ROTATION_DAYS",
	"MEMQL_IDENTITY_MAGIC_LINK_TTL_SECONDS":                 "IDENTITY_MAGIC_LINK_TTL_SECONDS",
	"MEMQL_IDENTITY_MX_VALIDATION_ENABLED":                  "IDENTITY_MX_VALIDATION_ENABLED",
	"MEMQL_IDENTITY_OAUTH_DCR_ENABLED":                      "IDENTITY_OAUTH_DCR_ENABLED",
	"MEMQL_IDENTITY_PRIVACY_OVERRIDE_PATH":                  "IDENTITY_PRIVACY_OVERRIDE_PATH",
	"MEMQL_IDENTITY_RATE_LIMIT_PER_IP_PER_HOUR":             "IDENTITY_RATE_LIMIT_PER_IP_PER_HOUR",
	"MEMQL_IDENTITY_REFRESH_COOKIE_SAMESITE":                "IDENTITY_REFRESH_COOKIE_SAMESITE",
	"MEMQL_IDENTITY_REFRESH_TOKEN_TTL_SECONDS":              "IDENTITY_REFRESH_TOKEN_TTL_SECONDS",
	"MEMQL_IDENTITY_REGISTERED_CLIENTS":                     "IDENTITY_REGISTERED_CLIENTS",
	"MEMQL_IDENTITY_REGISTRATION_DOMAINS":                   "IDENTITY_REGISTRATION_DOMAINS",
	"MEMQL_IDENTITY_REGISTRATION_MODE":                      "IDENTITY_REGISTRATION_MODE",
	"MEMQL_IDENTITY_RISK_THRESHOLD":                         "IDENTITY_RISK_THRESHOLD",
	"MEMQL_IDENTITY_SESSION_IDLE_DAYS":                      "IDENTITY_SESSION_IDLE_DAYS",
	"MEMQL_IDENTITY_SESSION_MAX_DAYS":                       "IDENTITY_SESSION_MAX_DAYS",
	"MEMQL_IDENTITY_SIGNING_KEY_B64":                        "IDENTITY_SIGNING_KEY_B64",
	"MEMQL_IDENTITY_TOS_OVERRIDE_PATH":                      "IDENTITY_TOS_OVERRIDE_PATH",
	"MEMQL_IDENTITY_TURNSTILE_SECRET":                       "IDENTITY_TURNSTILE_SECRET",
	"MEMQL_IDENTITY_TURNSTILE_SITE_KEY":                     "IDENTITY_TURNSTILE_SITE_KEY",
	"MEMQL_IDENTITY_VERIFIER_AUDIENCE":                      "IDENTITY_VERIFIER_AUDIENCE",
	"MEMQL_IDENTITY_VERIFIER_BASE_URL":                      "IDENTITY_VERIFIER_BASE_URL",
	"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER":               "IDENTITY_VERIFIER_EXPECTED_ISSUER",
	"MEMQL_IDENTITY_VERIFIER_JWKS_FETCH_TIMEOUT_SECONDS":    "IDENTITY_VERIFIER_JWKS_FETCH_TIMEOUT_SECONDS",
	"MEMQL_IDENTITY_VERIFIER_JWKS_REFRESH_SECONDS":          "IDENTITY_VERIFIER_JWKS_REFRESH_SECONDS",
	"MEMQL_IDENTITY_VERIFIER_JWKS_URL":                      "IDENTITY_VERIFIER_JWKS_URL",
	"MEMQL_OPENAI_API_KEY":                                  "OPENAI_API_KEY",
	"MEMQL_OPERATOR_AGENT_PROVIDER":                         "OPERATOR_AGENT_PROVIDER",
	"MEMQL_OPERATOR_APP_PROFILES_DIR":                       "OPERATOR_APP_PROFILES_DIR",
	"MEMQL_POLYPHON_BRIDGE_AGENT_URL":                       "POLYPHON_BRIDGE_AGENT_URL",
	"MEMQL_POLYPHON_LIVEKIT_API_KEY":                        "POLYPHON_LIVEKIT_API_KEY",
	"MEMQL_POLYPHON_LIVEKIT_API_SECRET":                     "POLYPHON_LIVEKIT_API_SECRET",
	"MEMQL_POLYPHON_LIVEKIT_PUBLIC_URL":                     "POLYPHON_LIVEKIT_PUBLIC_URL",
	"MEMQL_POLYPHON_LIVEKIT_URL":                            "POLYPHON_LIVEKIT_URL",
	"MEMQL_POLYPHON_OPENAI_ASR_MODEL":                       "POLYPHON_OPENAI_ASR_MODEL",
	"MEMQL_POLYPHON_OPENAI_TTS_MODEL":                       "POLYPHON_OPENAI_TTS_MODEL",
	"MEMQL_POLYPHON_OPENAI_TTS_VOICE":                       "POLYPHON_OPENAI_TTS_VOICE",
	"MEMQL_POLYPHON_OPENAI_VAD_SILENCE_MS":                  "POLYPHON_OPENAI_VAD_SILENCE_MS",
	"MEMQL_POLYPHON_PREDICTION_ENGINE_URL":                  "POLYPHON_PREDICTION_ENGINE_URL",
	"MEMQL_POLYPHON_VOICE_PROVIDER":                         "POLYPHON_VOICE_PROVIDER",
	"MEMQL_SIMLI_API_KEY":                                   "SIMLI_API_KEY",
}

// legacyByLegacyName is the reverse lookup (legacy name -> new name),
// built once from LegacyAliases. Used by PresentWithLegacy.
var legacyByNewName = LegacyAliases

// ApplyLegacyEnvAliases bridges pre-7.3 (memql#2106) env var names to
// the MEMQL_ convention at boot. For every (new, legacy) pair: if the
// new name is unset/empty in the process environment AND the legacy
// name is set, the legacy value is copied onto the new name
// (set-if-absent -- a value already on the new name always wins).
//
// It emits exactly one deprecation warning per legacy var it actually
// bridges. It is idempotent: a second call finds the new name already
// populated and does nothing.
//
// Call this AFTER the genesis envelope auto-load and the repo-root .env
// override have painted the environment, but BEFORE any component reads
// its config, so a legacy value from any of those layers is honored.
func ApplyLegacyEnvAliases(logger *slog.Logger) {
	// Deterministic order so the warning log is stable run-to-run.
	news := make([]string, 0, len(LegacyAliases))
	for newName := range LegacyAliases {
		news = append(news, newName)
	}
	sort.Strings(news)

	for _, newName := range news {
		legacy := LegacyAliases[newName]
		if os.Getenv(newName) != "" {
			continue // new name already set -- new wins
		}
		legacyVal, ok := os.LookupEnv(legacy)
		if !ok {
			continue // legacy not set -- nothing to bridge
		}
		if err := os.Setenv(newName, legacyVal); err != nil {
			if logger != nil {
				logger.Warn("failed to apply legacy env alias",
					"legacy", legacy, "new", newName, "err", err)
			}
			continue
		}
		if logger != nil {
			logger.Warn("deprecated env var is set; use the MEMQL_ name instead",
				"legacy", legacy, "use", newName)
		}
	}
}

// PresentWithLegacy reports whether the env-var name (a NEW
// MEMQL_-prefixed name) is satisfied by the have-set, either directly
// or via its legacy alias. Used by the seal floor check so an operator
// .env carrying legacy names still seals.
func PresentWithLegacy(have map[string]bool, name string) bool {
	if have[name] {
		return true
	}
	if legacy, ok := legacyByNewName[name]; ok && have[legacy] {
		return true
	}
	return false
}
