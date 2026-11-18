package calls

import (
	"fmt"
	"strings"
	"testing"
)

// The values every case starts from: a whole, valid set. The secrets are
// distinctive so a test can prove they never reach an error or a printout.
const (
	testAPISecret     = "api-secret-0123456789abcdefghijklmnopqrstuv"
	testWebhookSecret = "webhook-secret-0123456789abcdefghijklmnopqr"
)

func validMediaEnv() map[string]string {
	return map[string]string{
		EnvMediaProvider:      "docker-local",
		EnvMediaAPIURL:        "http://host.k3d.internal:7880",
		EnvMediaPublicURL:     "wss://media.memql.localhost:7443",
		EnvMediaSeats:         "50",
		EnvMediaAPIKey:        "APIroomkey",
		EnvMediaAPISecret:     testAPISecret,
		EnvMediaWebhookKey:    "APIwebhookkey",
		EnvMediaWebhookSecret: testWebhookSecret,
	}
}

func lookupIn(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

// requireNoSecretIn fails when text carries either secret of the valid set.
func requireNoSecretIn(t *testing.T, text string) {
	t.Helper()
	for _, secret := range []string{testAPISecret, testWebhookSecret} {
		if strings.Contains(text, secret) {
			t.Fatalf("a secret value reached text that can be logged: %q", text)
		}
	}
}

// TestMediaConfigAllOrNone holds the media target's environment to one set: all
// eight variables, which is a media server, or none, which is a cluster without
// calls. Anything in between refuses boot, naming every variable that is missing
// and never a value.
func TestMediaConfigAllOrNone(t *testing.T) {
	t.Run("none set is no media server and no error", func(t *testing.T) {
		cfg, present, err := MediaConfigFromEnv(lookupIn(map[string]string{}))
		if err != nil || present {
			t.Fatalf("an empty environment answered present=%v err=%v, want a cluster without calls", present, err)
		}
		if cfg != (MediaConfig{}) {
			t.Fatalf("an empty environment produced a config: %v", cfg)
		}
	})

	t.Run("blank values count as unset", func(t *testing.T) {
		env := map[string]string{}
		for _, name := range MediaEnvNames() {
			env[name] = "   "
		}
		if _, present, err := MediaConfigFromEnv(lookupIn(env)); err != nil || present {
			t.Fatalf("eight blank values answered present=%v err=%v, want none set", present, err)
		}
	})

	t.Run("all eight set is a media server", func(t *testing.T) {
		cfg, present, err := MediaConfigFromEnv(lookupIn(validMediaEnv()))
		if err != nil || !present {
			t.Fatalf("a whole set answered present=%v err=%v", present, err)
		}
		want := MediaConfig{
			Provider: "docker-local", APIURL: "http://host.k3d.internal:7880", PublicURL: "wss://media.memql.localhost:7443",
			Seats: 50, APIKey: "APIroomkey", APISecret: testAPISecret, WebhookKey: "APIwebhookkey", WebhookSecret: testWebhookSecret,
		}
		if cfg != want {
			t.Fatalf("the parsed config differs:\n got %v\nwant %v", cfg, want)
		}
	})

	t.Run("each variable alone refuses, naming the other seven", func(t *testing.T) {
		full := validMediaEnv()
		for _, only := range MediaEnvNames() {
			_, present, err := MediaConfigFromEnv(lookupIn(map[string]string{only: full[only]}))
			if err == nil || present {
				t.Fatalf("%s alone answered present=%v err=%v, want a refusal", only, present, err)
			}
			for _, other := range MediaEnvNames() {
				if other != only && !strings.Contains(err.Error(), other) {
					t.Errorf("%s alone: the refusal does not name the missing %s: %v", only, other, err)
				}
			}
			requireNoSecretIn(t, err.Error())
		}
	})

	t.Run("seven of eight refuses, naming the one", func(t *testing.T) {
		for _, missing := range MediaEnvNames() {
			env := validMediaEnv()
			delete(env, missing)
			_, present, err := MediaConfigFromEnv(lookupIn(env))
			if err == nil || present {
				t.Fatalf("all but %s answered present=%v err=%v, want a refusal", missing, present, err)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("the refusal does not name %s: %v", missing, err)
			}
			requireNoSecretIn(t, err.Error())
		}
	})

	t.Run("the set is exactly eight names", func(t *testing.T) {
		names := MediaEnvNames()
		if len(names) != 8 {
			t.Fatalf("the media set names %d variables, want 8: %v", len(names), names)
		}
		seen := map[string]bool{}
		for _, name := range names {
			if seen[name] || !strings.HasPrefix(name, "MEMQL_CALLS_MEDIA_") {
				t.Fatalf("the media set carries %q twice or outside MEMQL_CALLS_MEDIA_: %v", name, names)
			}
			seen[name] = true
		}
	})
}

// TestMediaConfigRefusesWhatItCannotUse: a whole set is still refused when a
// value cannot work, and the refusal names the variable, never the value.
func TestMediaConfigRefusesWhatItCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name, variable, value string
	}{
		{"a provider outside the closed set", EnvMediaProvider, "aws"},
		{"a provider in another case", EnvMediaProvider, "Hetzner"},
		{"an API URL with no scheme", EnvMediaAPIURL, "host.k3d.internal:7880"},
		{"an API URL with a scheme nothing serves", EnvMediaAPIURL, "ftp://media.example.test"},
		{"an API URL with no host", EnvMediaAPIURL, "http://"},
		{"a public URL a browser would reach without TLS", EnvMediaPublicURL, "ws://media.example.test"},
		{"a public URL that is not one", EnvMediaPublicURL, "media.example.test"},
		{"seats that are not a number", EnvMediaSeats, "many"},
		{"no seats", EnvMediaSeats, "0"},
		{"negative seats", EnvMediaSeats, "-5"},
		{"an API secret one short of 32 characters", EnvMediaAPISecret, strings.Repeat("s", 31)},
		{"a webhook secret one short of 32 characters", EnvMediaWebhookSecret, strings.Repeat("w", 31)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := validMediaEnv()
			env[tc.variable] = tc.value
			_, present, err := MediaConfigFromEnv(lookupIn(env))
			if err == nil || present {
				t.Fatalf("%s answered present=%v err=%v, want a refusal", tc.name, present, err)
			}
			if !strings.Contains(err.Error(), tc.variable) {
				t.Errorf("the refusal does not name %s: %v", tc.variable, err)
			}
			if tc.variable == EnvMediaAPISecret || tc.variable == EnvMediaWebhookSecret {
				if strings.Contains(err.Error(), tc.value) {
					t.Errorf("the refusal repeats the secret it refused: %v", err)
				}
			}
			requireNoSecretIn(t, err.Error())
		})
	}

	t.Run("both providers in the set are accepted and recorded as given", func(t *testing.T) {
		for _, provider := range []string{"docker-local", "hetzner"} {
			env := validMediaEnv()
			env[EnvMediaProvider] = provider
			cfg, present, err := MediaConfigFromEnv(lookupIn(env))
			if err != nil || !present || cfg.Provider != provider {
				t.Fatalf("provider %q answered %v present=%v err=%v", provider, cfg.Provider, present, err)
			}
		}
	})

	t.Run("an API secret of exactly 32 characters is enough", func(t *testing.T) {
		env := validMediaEnv()
		env[EnvMediaAPISecret] = strings.Repeat("s", 32)
		env[EnvMediaWebhookSecret] = strings.Repeat("w", 32)
		if _, present, err := MediaConfigFromEnv(lookupIn(env)); err != nil || !present {
			t.Fatalf("32-character secrets answered present=%v err=%v", present, err)
		}
	})

	t.Run("surrounding whitespace is not part of a value", func(t *testing.T) {
		env := validMediaEnv()
		for name, value := range env {
			env[name] = "  " + value + "\n"
		}
		cfg, present, err := MediaConfigFromEnv(lookupIn(env))
		if err != nil || !present || cfg.APISecret != testAPISecret || cfg.Seats != 50 {
			t.Fatalf("padded values answered %v present=%v err=%v", cfg, present, err)
		}
	})
}

// TestMediaConfigNeverPrintsASecret: a config is passed around and logged by
// people who will not think about what it holds, so every way of printing one
// leaves the two secrets out.
func TestMediaConfigNeverPrintsASecret(t *testing.T) {
	cfg, _, err := MediaConfigFromEnv(lookupIn(validMediaEnv()))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{cfg.String(), cfg.GoString(), cfg.LogValue().String(), fmt.Sprintf("%v %+v %#v %s", cfg, cfg, cfg, cfg)} {
		requireNoSecretIn(t, text)
		if !strings.Contains(text, "wss://media.memql.localhost:7443") {
			t.Errorf("a printed config should still say which server it names: %q", text)
		}
	}
	credential := MediaCredential{Key: cfg.APIKey, Secret: cfg.APISecret}
	for _, text := range []string{credential.String(), fmt.Sprintf("%v %+v %#v %s", credential, credential, credential, credential), credential.LogValue().String()} {
		requireNoSecretIn(t, text)
		if !strings.Contains(text, cfg.APIKey) {
			t.Errorf("a printed credential should still say which key it is: %q", text)
		}
	}
}
