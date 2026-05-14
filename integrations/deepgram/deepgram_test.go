package deepgram

import (
	"net/url"
	"strings"
	"testing"
)

func TestConfig_ValidateAppliesDefaults(t *testing.T) {
	c := Config{APIKey: "dg-test-key"}
	if err := c.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if c.ASRModel != "nova-3" {
		t.Errorf("ASRModel default = %q, want %q", c.ASRModel, "nova-3")
	}
	if c.TTSModel != "aura-2-thalia-en" {
		t.Errorf("TTSModel default = %q, want %q", c.TTSModel, "aura-2-thalia-en")
	}
	if c.Language != "en-US" {
		t.Errorf("Language default = %q, want %q", c.Language, "en-US")
	}
	if c.BaseURL != "wss://api.deepgram.com" {
		t.Errorf("BaseURL default = %q, want %q", c.BaseURL, "wss://api.deepgram.com")
	}
}

func TestConfig_ValidateRejectsMissingAPIKey(t *testing.T) {
	c := Config{}
	err := c.validate()
	if err == nil {
		t.Fatal("validate accepted empty APIKey; want error")
	}
	if !strings.Contains(err.Error(), "APIKey") {
		t.Errorf("error message = %q, want APIKey in it", err.Error())
	}
}

func TestConfig_AuthHeaderValueFormatsTokenScheme(t *testing.T) {
	c := Config{APIKey: "dg-secret"}
	if got := c.authHeaderValue(); got != "Token dg-secret" {
		t.Errorf("authHeaderValue = %q, want %q", got, "Token dg-secret")
	}
}

func TestConfig_ASRStreamURLBakesQueryParams(t *testing.T) {
	c := Config{APIKey: "k"}
	got, err := c.asrStreamURL("")
	if err != nil {
		t.Fatalf("asrStreamURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Scheme != "wss" {
		t.Errorf("scheme = %q, want wss", u.Scheme)
	}
	if u.Host != "api.deepgram.com" {
		t.Errorf("host = %q, want api.deepgram.com", u.Host)
	}
	if u.Path != "/v1/listen" {
		t.Errorf("path = %q, want /v1/listen", u.Path)
	}

	q := u.Query()
	cases := map[string]string{
		"model":            "nova-3",
		"language":         "en-US",
		"encoding":         "linear16",
		"sample_rate":      "16000",
		"channels":         "1",
		"interim_results":  "true",
		"vad_events":       "true",
		"smart_format":     "true",
		"endpointing":      "300",  // default Config.EndpointingMs
		"utterance_end_ms": "1000", // default Config.UtteranceEndMs
	}
	for k, want := range cases {
		if got := q.Get(k); got != want {
			t.Errorf("query[%s] = %q, want %q", k, got, want)
		}
	}
}

func TestConfig_ASRStreamURLHonorsEndpointingOverride(t *testing.T) {
	c := Config{APIKey: "k", EndpointingMs: 800}
	got, err := c.asrStreamURL("")
	if err != nil {
		t.Fatalf("asrStreamURL: %v", err)
	}
	u, _ := url.Parse(got)
	if v := u.Query().Get("endpointing"); v != "800" {
		t.Errorf("endpointing override = %q, want 800", v)
	}
}

func TestConfig_ASRStreamURLDisablesUtteranceEndMode(t *testing.T) {
	// -1 sentinel disables; validate() clamps to 0 which means
	// "omit from URL." Useful for operators who want to rely only
	// on the client watchdog (uncommon).
	c := Config{APIKey: "k", UtteranceEndMs: -1}
	got, err := c.asrStreamURL("")
	if err != nil {
		t.Fatalf("asrStreamURL: %v", err)
	}
	u, _ := url.Parse(got)
	if u.Query().Has("utterance_end_ms") {
		t.Errorf("utterance_end_ms present when Config set -1; want absent")
	}
}

func TestConfig_ValidateAppliesClientEOUTimeoutDefault(t *testing.T) {
	c := Config{APIKey: "k"}
	if err := c.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if c.ClientEOUTimeoutMs != 2500 {
		t.Errorf("ClientEOUTimeoutMs default = %d, want 2500", c.ClientEOUTimeoutMs)
	}
}

func TestConfig_ValidateHonorsClientEOUTimeoutOverride(t *testing.T) {
	c := Config{APIKey: "k", ClientEOUTimeoutMs: 5000}
	if err := c.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if c.ClientEOUTimeoutMs != 5000 {
		t.Errorf("ClientEOUTimeoutMs = %d, want 5000", c.ClientEOUTimeoutMs)
	}
}

func TestConfig_ValidateAllowsClientEOUTimeoutDisable(t *testing.T) {
	c := Config{APIKey: "k", ClientEOUTimeoutMs: -1}
	if err := c.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if c.ClientEOUTimeoutMs != 0 {
		t.Errorf("ClientEOUTimeoutMs = %d, want 0 (disabled)", c.ClientEOUTimeoutMs)
	}
}

func TestConfig_ASRStreamURLPerCallLanguageOverride(t *testing.T) {
	c := Config{APIKey: "k", Language: "en-US"}
	got, err := c.asrStreamURL("es-MX")
	if err != nil {
		t.Fatalf("asrStreamURL: %v", err)
	}
	u, _ := url.Parse(got)
	if got := u.Query().Get("language"); got != "es-MX" {
		t.Errorf("language override = %q, want %q", got, "es-MX")
	}
}

func TestConfig_ASRStreamURLHonorsBaseURLOverride(t *testing.T) {
	c := Config{APIKey: "k", BaseURL: "wss://localhost:8443"}
	got, err := c.asrStreamURL("")
	if err != nil {
		t.Fatalf("asrStreamURL: %v", err)
	}
	u, _ := url.Parse(got)
	if u.Host != "localhost:8443" {
		t.Errorf("host = %q, want localhost:8443", u.Host)
	}
}

func TestTTSClient_SpeakURLPromotesWSSToHTTPS(t *testing.T) {
	c, err := NewTTSClient(Config{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewTTSClient: %v", err)
	}
	got, err := c.speakURL("aura-2-thalia-en", "opus", "ogg")
	if err != nil {
		t.Fatalf("speakURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Scheme != "https" {
		t.Errorf("scheme = %q, want https (wss promoted)", u.Scheme)
	}
	if u.Host != "api.deepgram.com" {
		t.Errorf("host = %q, want api.deepgram.com", u.Host)
	}
	if u.Path != "/v1/speak" {
		t.Errorf("path = %q, want /v1/speak", u.Path)
	}
}

func TestTTSClient_SpeakURLOggOpusOmitsSampleRate(t *testing.T) {
	c, err := NewTTSClient(Config{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewTTSClient: %v", err)
	}
	got, err := c.speakURL("aura-2-thalia-en", "opus", "ogg")
	if err != nil {
		t.Fatalf("speakURL: %v", err)
	}
	u, _ := url.Parse(got)
	q := u.Query()
	if q.Get("encoding") != "opus" {
		t.Errorf("encoding = %q, want opus", q.Get("encoding"))
	}
	if q.Get("container") != "ogg" {
		t.Errorf("container = %q, want ogg", q.Get("container"))
	}
	if q.Get("model") != "aura-2-thalia-en" {
		t.Errorf("model = %q, want aura-2-thalia-en", q.Get("model"))
	}
	if q.Has("sample_rate") {
		t.Errorf("sample_rate must be omitted for opus+ogg; got %q", q.Get("sample_rate"))
	}
}

func TestTTSClient_SpeakURLLinear16IncludesSampleRate(t *testing.T) {
	c, err := NewTTSClient(Config{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewTTSClient: %v", err)
	}
	got, err := c.speakURL("aura-2-thalia-en", "linear16", "")
	if err != nil {
		t.Fatalf("speakURL: %v", err)
	}
	u, _ := url.Parse(got)
	q := u.Query()
	if q.Get("sample_rate") != "16000" {
		t.Errorf("sample_rate = %q, want 16000", q.Get("sample_rate"))
	}
	if q.Has("container") {
		t.Errorf("container must be omitted when caller passes empty; got %q", q.Get("container"))
	}
}

func TestTTSClient_ResolveModelPassesThroughAura2Ids(t *testing.T) {
	c, err := NewTTSClient(Config{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewTTSClient: %v", err)
	}
	cases := []string{
		"aura-2-thalia-en",
		"aura-2-arcas-en",
		"AURA-2-ORION-EN", // case-insensitive prefix check
	}
	for _, in := range cases {
		got := c.resolveModel(in)
		if got != in {
			t.Errorf("resolveModel(%q) = %q, want pass-through", in, got)
		}
	}
}

func TestTTSClient_ResolveModelMapsCanonicalNames(t *testing.T) {
	c, err := NewTTSClient(Config{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewTTSClient: %v", err)
	}
	// Known canonical name -> deepgramVoiceMap entry.
	if got := c.resolveModel("alto"); got != "aura-2-thalia-en" {
		t.Errorf("resolveModel(alto) = %q, want aura-2-thalia-en", got)
	}
	if got := c.resolveModel("tenor"); got != "aura-2-arcas-en" {
		t.Errorf("resolveModel(tenor) = %q, want aura-2-arcas-en", got)
	}
}

func TestTTSClient_ResolveModelRescuesOpenAIVoiceIds(t *testing.T) {
	c, err := NewTTSClient(Config{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewTTSClient: %v", err)
	}
	// "nova" is canonical female -> aura-2-thalia-en (deepgramVoiceMap).
	// This is the actual regression: cognition with a stale provider
	// view shipped "nova" as the model name and Deepgram 400'd.
	if got := c.resolveModel("nova"); !strings.HasPrefix(got, "aura-") {
		t.Errorf("resolveModel(nova) = %q, want aura-2-* fallback", got)
	}
	// "alloy" is an OpenAI-only voice id; not in the canonical catalog.
	// ResolveVoice's gender-bucket fallback handles it -- result must
	// still be a valid Deepgram voice.
	if got := c.resolveModel("alloy"); !strings.HasPrefix(got, "aura-") {
		t.Errorf("resolveModel(alloy) = %q, want aura-2-* fallback", got)
	}
}

func TestTTSClient_ResolveModelDefaultsOnEmpty(t *testing.T) {
	c, err := NewTTSClient(Config{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewTTSClient: %v", err)
	}
	got := c.resolveModel("")
	if got != "aura-2-thalia-en" {
		t.Errorf("resolveModel(empty) = %q, want aura-2-thalia-en", got)
	}
}
