// Package voice owns the canonical voice catalog and the gender-bucketed
// auto-assignment + provider-resolution path used by Polyphon and the
// agent reply pipeline. The catalog is hardcoded on purpose -- voices
// change rarely, they're shared across all tenants, and seeding /
// validation overhead would buy nothing. Per-tenant voice customisation
// (if ever needed) will get its own concept.
//
// Two surfaces:
//
//   - PickVoiceForGender(gender, exclude) -- returns a canonical voice
//     name suitable for an agent of that gender, biased away from voices
//     already used by the same owner's other agents. Called by the
//     frontend at agent creation time via the `voicePickForGender`
//     builtin.
//   - ResolveVoice(canonical, provider) -- returns the provider-specific
//     voice ID. Called by the cognition handler before forwarding TTS to
//     the Bridge Agent, and by the bridge / TTS clients at synthesis
//     time. Falls back to a sensible default if the canonical name is
//     unknown so the audio path never goes silent.
package voice

import (
	"os"
	"strings"
)

// ActiveProvider returns the currently active TTS / ASR provider name,
// applying the same auto-selection rule the bridge-agent and the
// engine's STT selector use:
//
//  1. POLYPHON_VOICE_PROVIDER explicit (non-empty) -> honor it.
//  2. Default -> "openai".
//
// Every node in the cluster (cognition, voice, bridge-agent) must
// agree on the active provider, otherwise voice resolution on one
// side produces ids the other side can't synthesize. Hardcoding
// "openai" anywhere is a bug -- always go through this helper.
func ActiveProvider() string {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("POLYPHON_VOICE_PROVIDER"))); v != "" {
		return v
	}
	return "openai"
}

// CanonicalVoice is one entry in the gender-bucketed catalog.
type CanonicalVoice struct {
	Name   string // canonical name -- lowercased, stable across providers
	Gender string // "male" | "female"
	// PreferredProviderHint records which provider's voice was the
	// inspiration for the canonical entry. Not used at resolution
	// time -- we pick the per-provider id from the maps below -- but
	// it documents intent.
	PreferredProviderHint string
}

// FemaleVoices is the canonical female catalog. Order matters: the
// auto-assigner walks this list when picking a fresh voice for a new
// agent and prefers earlier entries (so a tenant's first three female
// agents get distinct, well-known voices before we start cycling).
var FemaleVoices = []CanonicalVoice{
	{Name: "alto", Gender: "female", PreferredProviderHint: "openai"},
	{Name: "soprano", Gender: "female", PreferredProviderHint: "openai"},
	{Name: "mezzo", Gender: "female", PreferredProviderHint: "openai"},
	{Name: "lyric", Gender: "female", PreferredProviderHint: "openai"},
	{Name: "aria", Gender: "female", PreferredProviderHint: "openai"},
	{Name: "cadence", Gender: "female", PreferredProviderHint: "openai"},
	{Name: "harmony", Gender: "female", PreferredProviderHint: "openai"},
	{Name: "nova", Gender: "female", PreferredProviderHint: "openai"},
}

// MaleVoices is the canonical male catalog. Same ordering rule as
// FemaleVoices.
var MaleVoices = []CanonicalVoice{
	{Name: "tenor", Gender: "male", PreferredProviderHint: "openai"},
	{Name: "baritone", Gender: "male", PreferredProviderHint: "openai"},
	{Name: "bass", Gender: "male", PreferredProviderHint: "openai"},
	{Name: "echo", Gender: "male", PreferredProviderHint: "openai"},
	{Name: "anchor", Gender: "male", PreferredProviderHint: "openai"},
	{Name: "marcus", Gender: "male", PreferredProviderHint: "openai"},
	{Name: "drake", Gender: "male", PreferredProviderHint: "openai"},
	{Name: "atlas", Gender: "male", PreferredProviderHint: "openai"},
}

// GAVoiceCanonical is the hardcoded canonical voice the General
// Assistant gets seeded with. The GA is provisioned with gender
// "female" by default, so this lands in the female bucket. The user
// can rename the GA but cannot edit its voice (per Q7's no-user-edit
// rule); changing the GA's voice means changing this constant and
// re-running the provision automation.
const GAVoiceCanonical = "alto"

// openAIVoiceMap maps canonical names to OpenAI TTS voice IDs.
// OpenAI's catalog: alloy, nova, coral, sage, echo, shimmer (plus
// fable, onyx in the realtime API). We pick the closest timbre per
// canonical entry; any unmapped canonical falls back to the
// gender-appropriate default.
var openAIVoiceMap = map[string]string{
	// female bucket
	"alto":    "nova",
	"soprano": "shimmer",
	"mezzo":   "coral",
	"lyric":   "sage",
	"aria":    "shimmer",
	"cadence": "nova",
	"harmony": "coral",
	"nova":    "nova",
	// male bucket
	"tenor":    "echo",
	"baritone": "onyx",
	"bass":     "onyx",
	"echo":     "echo",
	"anchor":   "echo",
	"marcus":   "onyx",
	"drake":    "echo",
	"atlas":    "onyx",
}

// openAIRealtimeVoiceMap maps canonical names to gpt-realtime GA voice
// ids. The Realtime API accepts its OWN voice set, which is NOT the same
// as the standard TTS catalog: the GA realtime voices are alloy, ash,
// ballad, cedar, coral, echo, marin, sage, shimmer, verse. Notably the
// TTS-only ids "nova" and "onyx" (used by openAIVoiceMap) are NOT valid
// realtime voices, so realtime MUST resolve through this dedicated map
// rather than sharing openAIVoiceMap -- otherwise a session would be
// handed a voice id the Realtime API rejects.
//
// marin and cedar are OpenAI's two newest GA realtime voices and the
// recommended defaults (marin female-leaning, cedar male-leaning); the
// rest of the bucket is filled from the gender-appropriate GA set to keep
// each agent's timbre distinct. Any unmapped canonical falls back to the
// gender-appropriate GA default (see ResolveVoice).
var openAIRealtimeVoiceMap = map[string]string{
	// female bucket
	"alto":    "marin",
	"soprano": "shimmer",
	"mezzo":   "coral",
	"lyric":   "sage",
	"aria":    "coral",
	"cadence": "marin",
	"harmony": "sage",
	"nova":    "shimmer",
	// male bucket
	"tenor":    "cedar",
	"baritone": "ash",
	"bass":     "ash",
	"echo":     "echo",
	"anchor":   "verse",
	"marcus":   "ballad",
	"drake":    "cedar",
	"atlas":    "verse",
}

// providerDefaults is the per-provider fallback voice when the
// canonical name is unknown. Picked to be safe + inoffensive for
// either gender.
var providerDefaults = map[string]string{
	"openai": "alloy",
	// marin is OpenAI's recommended GA realtime voice -- a good,
	// configurable default per #483. Cedar is its male-leaning sibling.
	"openai-realtime": "marin",
}

// canonicalDefaults are the catalog defaults we hand back when
// PickVoiceForGender exhausts the bucket (every voice is already
// taken). At that point we cycle from the head of the bucket -- the
// "no-collision" rule degrades to "best effort, may collide." A user
// with 9+ female agents will start to see voice repeats, which is the
// least-bad outcome.
var canonicalDefaults = map[string]string{
	"female": "alto",
	"male":   "tenor",
}

// PickVoiceForGender returns a canonical voice name for the given
// gender, biased away from the supplied exclude list. Returns the
// catalog default for the gender if every voice in the bucket is
// excluded.
//
// gender is normalised case-insensitively. Anything other than
// "female" or "male" falls back to the female bucket -- the GA's
// default -- rather than erroring; agent creation stays write-through.
func PickVoiceForGender(gender string, exclude []string) string {
	bucket := bucketForGender(gender)

	excludedSet := make(map[string]struct{}, len(exclude))
	for _, name := range exclude {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		excludedSet[key] = struct{}{}
	}

	for _, v := range bucket {
		if _, taken := excludedSet[strings.ToLower(v.Name)]; taken {
			continue
		}
		return v.Name
	}

	// Bucket exhausted -- cycle back to the head.
	if defaultName, ok := canonicalDefaults[normalisedGender(gender)]; ok {
		return defaultName
	}
	return canonicalDefaults["female"]
}

// ResolveVoice maps a canonical voice name + provider to the
// provider-specific voice ID. Returns the provider's default voice if
// the canonical name is unknown, so the audio path never goes silent.
//
// Provider name is normalised case-insensitively; "openai-realtime"
// resolves through its own GA realtime voice set (marin/cedar/...),
// which differs from the standard TTS catalog. Empty canonical falls
// back to the provider default.
func ResolveVoice(canonical, provider string) string {
	canon := strings.ToLower(strings.TrimSpace(canonical))
	prov := strings.ToLower(strings.TrimSpace(provider))

	if canon == "" {
		return providerDefault(prov)
	}

	switch prov {
	case "openai", "":
		if id, ok := openAIVoiceMap[canon]; ok && id != "" {
			return id
		}
		return providerDefault("openai")
	case "openai-realtime":
		// Realtime has its own GA voice set (marin/cedar/...); an unknown
		// canonical falls back to a gender-appropriate GA voice so the
		// session never receives a voice id the Realtime API rejects.
		if id, ok := openAIRealtimeVoiceMap[canon]; ok && id != "" {
			return id
		}
		switch CanonicalGender(canon) {
		case "male":
			return "cedar"
		default:
			return providerDefault("openai-realtime")
		}
	default:
		// Unknown provider -- hand back the canonical name and let
		// the provider client decide. Prevents callers from getting
		// an empty string and hitting a TTS error downstream.
		return canon
	}
}

// AllCanonicalNames returns every canonical voice name in declaration
// order, female bucket first then male. Used by callers that need the
// full catalog (UI rendering, audit, doc generation).
func AllCanonicalNames() []string {
	out := make([]string, 0, len(FemaleVoices)+len(MaleVoices))
	for _, v := range FemaleVoices {
		out = append(out, v.Name)
	}
	for _, v := range MaleVoices {
		out = append(out, v.Name)
	}
	return out
}

// IsCanonical reports whether the given name (case-insensitive) is a
// known canonical voice in either bucket.
func IsCanonical(name string) bool {
	target := strings.ToLower(strings.TrimSpace(name))
	if target == "" {
		return false
	}
	for _, v := range FemaleVoices {
		if strings.ToLower(v.Name) == target {
			return true
		}
	}
	for _, v := range MaleVoices {
		if strings.ToLower(v.Name) == target {
			return true
		}
	}
	return false
}

// CanonicalGender returns the gender bucket of the named canonical
// voice, or "" if unknown.
func CanonicalGender(name string) string {
	target := strings.ToLower(strings.TrimSpace(name))
	for _, v := range FemaleVoices {
		if strings.ToLower(v.Name) == target {
			return "female"
		}
	}
	for _, v := range MaleVoices {
		if strings.ToLower(v.Name) == target {
			return "male"
		}
	}
	return ""
}

func bucketForGender(gender string) []CanonicalVoice {
	switch normalisedGender(gender) {
	case "male":
		return MaleVoices
	default:
		return FemaleVoices
	}
}

func normalisedGender(gender string) string {
	g := strings.ToLower(strings.TrimSpace(gender))
	if g == "male" {
		return "male"
	}
	return "female"
}

func providerDefault(provider string) string {
	if v, ok := providerDefaults[provider]; ok {
		return v
	}
	return providerDefaults["openai"]
}
